package users

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/users/usersdb"
)

// educationLockClass is the first key of the two-key advisory lock of a
// user's education writes (LockEducationWrites): "KE".
const educationLockClass = 0x4b45

// educationLock is the second key of the lock: four of the user id's
// random bytes (12–15), big-endian. A UUIDv7 begins with its creation
// time, so its first bytes are shared by every user created in the same
// minute; the tail is random. Two users that share the key only wait for
// each other.
func educationLock(userID uuid.UUID) int32 {
	var k int32
	for _, b := range userID[12:16] {
		k = k<<8 | int32(b)
	}
	return k
}

// educationTable is the soft-deleted table of the records.
const educationTable = "educations"

func educationNotFound() error {
	return apperr.New(apperr.KindNotFound, CodeEducationNotFound)
}

// CreateEducation adds an education record for the caller. commit runs
// last in the transaction with the new record: the handler stores its
// answer for the Idempotency-Key there (ADR-0034 Karar 4).
//
// Lock order: the user's education write lock first, so concurrent creates
// cannot pass the cap (EducationsPerUser) together; then the user's active
// row (softdelete.LockActiveParent, FOR SHARE), since the record is a child
// of the user and a concurrent deletion of the account must not leave an
// active record of a deleted user (ADR-0036 Karar 6). No path takes the
// two the other way round.
func (s *Service) CreateEducation(ctx context.Context, p authn.UserPrincipal, in EducationInput,
	commit func(ctx context.Context, q db.Querier, e Education) error,
) (Education, error) {
	if err := in.check(); err != nil {
		return Education{}, err
	}
	id, now := newID(), s.clock.Now()
	var out Education
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := usersdb.New(tx)
		err := q.LockEducationWrites(ctx, usersdb.LockEducationWritesParams{
			LockClass: educationLockClass, LockKey: educationLock(p.UserID),
		})
		if err != nil {
			return fmt.Errorf("users: lock the education writes: %w", err)
		}
		err = softdelete.LockActiveParent(ctx, tx, "users", p.UserID)
		if errors.Is(err, softdelete.ErrNotActive) {
			return sessionInactive()
		}
		if err != nil {
			return fmt.Errorf("users: lock the user: %w", err)
		}
		n, err := q.CountEducations(ctx, p.UserID)
		if err != nil {
			return fmt.Errorf("users: count the educations: %w", err)
		}
		if n >= EducationsPerUser {
			return apperr.New(apperr.KindConflict, CodeEducationLimitReached)
		}
		if err := checkReferences(ctx, q, in, references{}); err != nil {
			return err
		}
		err = q.InsertEducation(ctx, usersdb.InsertEducationParams{
			ID: id, UserID: p.UserID, UniversityID: in.UniversityID, FacultyID: nullID(in.FacultyID),
			DepartmentID: nullID(in.DepartmentID), Degree: in.Degree, StartYear: in.StartYear, EndYear: in.EndYear,
			CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("users: insert the education: %w", err)
		}
		e, err := loadEducation(ctx, q, id, p.UserID)
		if err != nil {
			return err
		}
		out = e
		return commit(ctx, tx, e)
	})
	if err != nil {
		return Education{}, fmt.Errorf("users: create an education: %w", err)
	}
	return out, nil
}

// ReplaceEducation rewrites one of the caller's education records. The
// period is checked first, then the record: another user's record is the
// 404 of a missing one whatever the body says, so no reference check can
// tell it apart (USR-028).
func (s *Service) ReplaceEducation(ctx context.Context, p authn.UserPrincipal, id uuid.UUID, in EducationInput) (Education, error) {
	if err := in.check(); err != nil {
		return Education{}, err
	}
	var out Education
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := usersdb.New(tx)
		cur, err := q.LockEducation(ctx, usersdb.LockEducationParams{ID: id, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return educationNotFound()
		}
		if err != nil {
			return fmt.Errorf("users: lock the education: %w", err)
		}
		kept := references{university: cur.UniversityID, faculty: cur.FacultyID, department: cur.DepartmentID}
		if err := checkReferences(ctx, q, in, kept); err != nil {
			return err
		}
		_, err = q.ReplaceEducation(ctx, usersdb.ReplaceEducationParams{
			ID: id, UserID: p.UserID, UniversityID: in.UniversityID, FacultyID: nullID(in.FacultyID),
			DepartmentID: nullID(in.DepartmentID), Degree: in.Degree, StartYear: in.StartYear, EndYear: in.EndYear,
		})
		if err != nil {
			return fmt.Errorf("users: replace the education: %w", err)
		}
		e, err := loadEducation(ctx, q, id, p.UserID)
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	if err != nil {
		return Education{}, fmt.Errorf("users: replace an education: %w", err)
	}
	return out, nil
}

// DeleteEducation soft-deletes one of the caller's education records.
// Another user's, a deleted and a missing one are the same 404.
func (s *Service) DeleteEducation(ctx context.Context, p authn.UserPrincipal, id uuid.UUID) error {
	now := s.clock.Now()
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := usersdb.New(tx)
		_, err := q.LockEducation(ctx, usersdb.LockEducationParams{ID: id, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return educationNotFound()
		}
		if err != nil {
			return fmt.Errorf("users: lock the education: %w", err)
		}
		_, err = s.deleter.Delete(ctx, tx, softdelete.DeleteRequest{
			Table: educationTable, ID: id,
			Actor:  softdelete.Actor{Kind: softdelete.ActorUser, ID: p.UserID},
			Reason: softdelete.ReasonUserRequest, At: now,
		})
		if errors.Is(err, softdelete.ErrNotActive) {
			return educationNotFound()
		}
		if err != nil {
			return fmt.Errorf("users: delete the education: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("users: delete an education: %w", err)
	}
	return nil
}

// references are the reference rows a record names already: a retired
// one it keeps may stay (ADR-0036). The zero value is a new record's.
type references struct {
	university uuid.UUID
	faculty    uuid.NullUUID
	department uuid.NullUUID
}

// keeps reports whether id is the reference the record names already.
func keeps(kept uuid.NullUUID, id uuid.UUID) bool {
	return kept.Valid && kept.UUID == id
}

// checkReferences finds the university, the faculty under it and the
// department under the faculty. A retired row cannot be chosen anew, but a
// record keeps the one it names already (M4, ADR-0036). The reference rows
// are never deleted, so what is found here stays; the composite foreign
// keys hold the same in the database.
func checkReferences(ctx context.Context, q *usersdb.Queries, in EducationInput, kept references) error {
	_, err := q.GetUniversity(ctx, usersdb.GetUniversityParams{
		ID: in.UniversityID, AllowRetired: keeps(uuid.NullUUID{UUID: kept.university, Valid: kept.university != uuid.Nil}, in.UniversityID),
	})
	if err != nil {
		return referenceError(err)
	}
	if in.FacultyID != nil {
		_, err := q.GetFacultyOf(ctx, usersdb.GetFacultyOfParams{
			ID: *in.FacultyID, UniversityID: in.UniversityID, AllowRetired: keeps(kept.faculty, *in.FacultyID),
		})
		if err != nil {
			return referenceError(err)
		}
	}
	if in.DepartmentID != nil {
		if in.FacultyID == nil {
			// The contract requires a faculty with a department; this
			// keeps the rule should the schema ever drop it.
			return apperr.New(apperr.KindNotFound, CodeReferenceNotFound)
		}
		_, err := q.GetDepartmentOf(ctx, usersdb.GetDepartmentOfParams{
			ID: *in.DepartmentID, FacultyID: *in.FacultyID, AllowRetired: keeps(kept.department, *in.DepartmentID),
		})
		if err != nil {
			return referenceError(err)
		}
	}
	return nil
}

// referenceError is REFERENCE_ITEM_NOT_FOUND for a missing row and the
// wrapped error otherwise.
func referenceError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(apperr.KindNotFound, CodeReferenceNotFound)
	}
	return fmt.Errorf("users: find the reference row: %w", err)
}

func loadEducation(ctx context.Context, q *usersdb.Queries, id, userID uuid.UUID) (Education, error) {
	row, err := q.GetEducation(ctx, usersdb.GetEducationParams{ID: id, UserID: userID})
	if err != nil {
		return Education{}, fmt.Errorf("users: load the education: %w", err)
	}
	return educationOf(row), nil
}

func educationOf(r usersdb.GetEducationRow) Education {
	e := Education{
		ID: r.ID, University: Reference{ID: r.UniversityID, Name: r.UniversityName}, Degree: r.Degree,
		StartYear: r.StartYear, EndYear: r.EndYear, CreatedAt: r.CreatedAt,
	}
	if r.FacultyID.Valid && r.FacultyName != nil {
		e.Faculty = &Reference{ID: r.FacultyID.UUID, Name: *r.FacultyName}
	}
	if r.DepartmentID.Valid && r.DepartmentName != nil {
		e.Department = &Reference{ID: r.DepartmentID.UUID, Name: *r.DepartmentName}
	}
	return e
}

func nullID(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

// EducationPage is one page of the caller's education records and the
// cursors around it.
type EducationPage struct {
	Items      []Education
	NextCursor *string
	PrevCursor *string
}

// Educations returns a page of the caller's education records, the latest
// end first. The records of one user are few (EducationsPerUser), so all
// are read and the page is cut here, on the same keyset a query would use.
// A malformed cursor is CURSOR_INVALID.
func (s *Service) Educations(ctx context.Context, p authn.UserPrincipal, limit int32, cursor string) (EducationPage, error) {
	var at *educationCursor
	if cursor != "" {
		c, err := decodeEducationCursor(cursor)
		if err != nil {
			return EducationPage{}, err
		}
		at = &c
	}
	rows, err := s.reads.ListEducations(ctx, p.UserID)
	if err != nil {
		return EducationPage{}, fmt.Errorf("users: list the educations: %w", err)
	}
	all := make([]Education, 0, len(rows))
	for i := range rows {
		all = append(all, educationOf(usersdb.GetEducationRow(rows[i])))
	}
	return pageOf(all, int(limit), at), nil
}

// pageOf cuts one page of limit records out of all, in list order, after
// (older) or before (newer) the cursor's position; the position need not
// be a record that still exists.
func pageOf(all []Education, limit int, at *educationCursor) EducationPage {
	start, end := 0, min(len(all), limit)
	if at != nil {
		before, after := 0, 0
		for _, e := range all {
			k := keyOf(e)
			if k.precedes(at.key) {
				before++
			}
			if at.key.precedes(k) {
				after++
			}
		}
		if at.newer {
			end = before
			start = max(0, end-limit)
		} else {
			start = len(all) - after
			end = min(len(all), start+limit)
		}
	}
	page := EducationPage{Items: all[start:end]}
	if len(page.Items) == 0 {
		return page
	}
	if start > 0 {
		page.PrevCursor = educationCursor{newer: true, key: keyOf(page.Items[0])}.encode()
	}
	if end < len(all) {
		page.NextCursor = educationCursor{key: keyOf(page.Items[len(page.Items)-1])}.encode()
	}
	return page
}

// The keyset cursor of the list: its direction and the position (end key,
// start year, id), base64url under a version prefix. It is opaque to the
// client and carries no filter, because the list has none.
const (
	educationCursorPrefix = "edu1"
	educationCursorOlder  = "n"
	educationCursorNewer  = "p"
)

type educationCursor struct {
	newer bool
	key   educationKey
}

func (c educationCursor) encode() *string {
	dir := educationCursorOlder
	if c.newer {
		dir = educationCursorNewer
	}
	raw := strings.Join([]string{
		educationCursorPrefix, dir, strconv.FormatInt(c.key.end, 10), strconv.FormatInt(c.key.start, 10), c.key.id.String(),
	}, "|")
	s := base64.RawURLEncoding.EncodeToString([]byte(raw))
	return &s
}

func decodeEducationCursor(s string) (educationCursor, error) {
	invalid := apperr.New(apperr.KindInvalid, authn.CodeCursorInvalid)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return educationCursor{}, invalid
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 || parts[0] != educationCursorPrefix ||
		(parts[1] != educationCursorOlder && parts[1] != educationCursorNewer) {
		return educationCursor{}, invalid
	}
	end, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return educationCursor{}, invalid
	}
	start, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return educationCursor{}, invalid
	}
	id, err := uuid.Parse(parts[4])
	if err != nil {
		return educationCursor{}, invalid
	}
	return educationCursor{newer: parts[1] == educationCursorNewer, key: educationKey{end: end, start: start, id: id}}, nil
}
