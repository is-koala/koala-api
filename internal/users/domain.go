// Package users serves the caller's own profile and education records
// (ADR-0029 Karar 11, ADR-0036): GET and PATCH /v1/me/profile and
// /v1/me/educations.
//
// The profile is the users row past its identity core (authn owns the
// sign-in fields). Two of its fields are not written here: a new e-mail
// address or phone number starts a verification instead, through the one
// path every start takes (authn.Verifications.Reauthenticate and StartTx),
// and only the confirmation writes the value (USR-014). A patch is atomic:
// the fields, the verifications it starts and the profile sync intent
// commit together or not at all.
//
// The caller reads their own data: the staff PII gate (pii.Gate, a grant
// and an access log row per read) is about a staff member reading
// somebody else's, and is not part of this module (ADR-0036 Karar 4).
package users

import (
	"bytes"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// Error codes. Each is in api/components/problem.yaml's ErrorCode.
const (
	// CodeFieldImmutable: a patch changes the name or the birth date of a
	// user whose identity is verified (USR-024).
	CodeFieldImmutable apperr.Code = "USER_FIELD_IMMUTABLE"
	// CodeEducationNotFound: the caller has no such education record;
	// another user's looks the same (USR-028).
	CodeEducationNotFound apperr.Code = "EDUCATION_NOT_FOUND"
	// CodeEducationPeriodInvalid: the end year is before the start year.
	CodeEducationPeriodInvalid apperr.Code = "EDUCATION_PERIOD_INVALID"
	// CodeEducationLimitReached: the caller has EducationsPerUser records.
	CodeEducationLimitReached apperr.Code = "EDUCATION_LIMIT_REACHED"
	// CodeReferenceNotFound: no such university, no such faculty of the
	// university, or no such department of the faculty.
	CodeReferenceNotFound apperr.Code = "REFERENCE_ITEM_NOT_FOUND"
)

// Codes lists every code this package defines, for the contract test.
func Codes() []apperr.Code {
	return []apperr.Code{
		CodeFieldImmutable, CodeEducationNotFound, CodeEducationPeriodInvalid, CodeEducationLimitReached,
		CodeReferenceNotFound,
	}
}

// EducationsPerUser is how many education records a user may have
// (ADR-0036 Karar 6): a profile lists a few, and the cap bounds what one
// account can write and what the list loads.
const EducationsPerUser = 20

// Birth dates before this are refused (users_birth_date_check).
const earliestBirthYear = 1900

// userActive is the status of a user who may change their profile
// (users_status_check).
const userActive = "ACTIVE"

// identityMask is the mask of a verified identity number: no digit shows,
// the last ones included (ADR-0029 Karar 9, USR-027), so the mask needs
// no number at all.
const identityMask = "***********"

// Profile is the caller's profile as stored.
type Profile struct {
	ID        uuid.UUID
	Email     string
	FirstName string
	LastName  string
	// Phone is the verified number in E.164; nil until one is confirmed.
	Phone          *string
	BirthDate      *time.Time
	Gender         *string
	JobTitle       *string
	About          *string
	Qualifications *string
	Interests      *string
	Employer       bool
	EmailVerified  bool
	PhoneVerified  bool
	// IdentityVerified freezes the name and the birth date (USR-024).
	IdentityVerified bool
	CreatedAt        time.Time
}

// MaskedIdentityNumber is the identity number as its owner sees it: nil
// until the identity is verified, then fully blanked.
func (p Profile) MaskedIdentityNumber() *string {
	if !p.IdentityVerified {
		return nil
	}
	m := identityMask
	return &m
}

// Field is one clearable member of a JSON Merge Patch (RFC 7396): Set is
// false when the member was not sent; a sent member with a nil Value
// clears the field.
type Field[T any] struct {
	Set   bool
	Value *T
}

// Patch is a profile patch. FirstName and LastName cannot be cleared.
// Email and PhoneNumber are not written: each starts a verification when
// it differs from the value on file, and either needs CurrentPassword
// (ADR-0033 Karar 1b).
type Patch struct {
	FirstName      *string
	LastName       *string
	BirthDate      Field[time.Time]
	Gender         Field[string]
	JobTitle       Field[string]
	About          Field[string]
	Qualifications Field[string]
	Interests      Field[string]

	Email           *string
	PhoneNumber     *string
	CurrentPassword string

	// The links to the media and address modules (ADR-0036 Karar 5).
	ProfilePicture Field[uuid.UUID]
	CV             Field[uuid.UUID]
	Address        Field[uuid.UUID]
}

// contactChange reports whether the patch sends an address or a number.
func (p Patch) contactChange() bool { return p.Email != nil || p.PhoneNumber != nil }

// links reports whether the patch links a media or an address: a member
// sent with an id. Clearing one (null) links nothing.
func (p Patch) links() bool {
	return p.ProfilePicture.Value != nil || p.CV.Value != nil || p.Address.Value != nil
}

// apply returns cur with the patch's profile fields written. Names are
// trimmed and may not be empty; a text field cleared or sent empty is
// cleared. A verified identity freezes the name and the birth date: a
// patch that would change one is USER_FIELD_IMMUTABLE, one that sends the
// value on file changes nothing (USR-024). The gender is not frozen
// (USR-029).
func (p Patch) apply(cur Profile) (Profile, error) {
	next := cur
	immutable := apperr.New(apperr.KindConflict, CodeFieldImmutable)
	if p.FirstName != nil {
		next.FirstName = strings.TrimSpace(*p.FirstName)
	}
	if p.LastName != nil {
		next.LastName = strings.TrimSpace(*p.LastName)
	}
	if p.BirthDate.Set {
		next.BirthDate = p.BirthDate.Value
	}
	if cur.IdentityVerified &&
		(next.FirstName != cur.FirstName || next.LastName != cur.LastName || !sameDate(next.BirthDate, cur.BirthDate)) {
		return Profile{}, immutable
	}
	next.Gender = text(p.Gender, cur.Gender)
	next.JobTitle = text(p.JobTitle, cur.JobTitle)
	next.About = text(p.About, cur.About)
	next.Qualifications = text(p.Qualifications, cur.Qualifications)
	next.Interests = text(p.Interests, cur.Interests)
	return next, nil
}

// fieldsChanged reports whether the stored profile fields differ.
func fieldsChanged(a, b Profile) bool {
	return a.FirstName != b.FirstName || a.LastName != b.LastName || !sameDate(a.BirthDate, b.BirthDate) ||
		!sameText(a.Gender, b.Gender) || !sameText(a.JobTitle, b.JobTitle) || !sameText(a.About, b.About) ||
		!sameText(a.Qualifications, b.Qualifications) || !sameText(a.Interests, b.Interests)
}

// text is a clearable text field after a patch: unchanged when not sent,
// cleared when sent null or blank, else the trimmed value.
func text(f Field[string], cur *string) *string {
	if !f.Set {
		return cur
	}
	if f.Value == nil {
		return nil
	}
	v := strings.TrimSpace(*f.Value)
	if v == "" {
		return nil
	}
	return &v
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameDate(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// nameInvalid reports whether a trimmed name is empty or too long
// (1–100 characters; the contract's bounds, which a value of spaces
// passes).
func nameInvalid(name string) bool {
	n := utf8.RuneCountInString(name)
	return n < 1 || n > 100
}

// birthDateInvalid reports whether a birth date is in the future or
// before earliestBirthYear.
func birthDateInvalid(d, now time.Time) bool {
	return d.After(now) || d.Year() < earliestBirthYear
}

// Reference is one row of the academic reference data.
type Reference struct {
	ID   uuid.UUID
	Name string
}

// Education is one of the caller's education records.
type Education struct {
	ID         uuid.UUID
	University Reference
	Faculty    *Reference
	Department *Reference
	Degree     string
	StartYear  int32
	EndYear    *int32
	CreatedAt  time.Time
}

// EducationInput is the whole of an education record as the caller sends
// it. The contract requires a faculty with a department.
type EducationInput struct {
	UniversityID uuid.UUID
	FacultyID    *uuid.UUID
	DepartmentID *uuid.UUID
	Degree       string
	StartYear    int32
	EndYear      *int32
}

// check applies the period rule: an ongoing education has no end year,
// and an end year is not before the start year (USR-028).
func (in EducationInput) check() error {
	if in.EndYear != nil && *in.EndYear < in.StartYear {
		return apperr.New(apperr.KindInvalid, CodeEducationPeriodInvalid)
	}
	return nil
}

// educationKey is an education's place in the list: the latest end first,
// an ongoing one (no end year) leading, then the start year and the id,
// all descending (db/queries/users/educations.sql ListEducations).
type educationKey struct {
	end   int64
	start int64
	id    uuid.UUID
}

// ongoingEnd is the end key of an ongoing education, after every year the
// contract allows.
const ongoingEnd int64 = 9999

func keyOf(e Education) educationKey {
	end := ongoingEnd
	if e.EndYear != nil {
		end = int64(*e.EndYear)
	}
	return educationKey{end: end, start: int64(e.StartYear), id: e.ID}
}

// precedes reports whether a comes before b in the list. Ids compare by
// their bytes, as PostgreSQL orders uuid.
func (a educationKey) precedes(b educationKey) bool {
	switch {
	case a.end != b.end:
		return a.end > b.end
	case a.start != b.start:
		return a.start > b.start
	default:
		return bytes.Compare(a.id[:], b.id[:]) > 0
	}
}
