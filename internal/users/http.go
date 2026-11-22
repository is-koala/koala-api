package users

import (
	"context"
	"errors"
	"time"

	"github.com/oapi-codegen/nullable"
	"github.com/oapi-codegen/runtime/types"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
	"github.com/is-koala/koala-api/internal/platform/jsontime"
)

// HTTP implements the profile and education operations of the generated
// strict server (api/openapi.yaml, tag users; ADR-0036).
type HTTP struct {
	svc *Service
}

// NewHTTP returns the handlers.
func NewHTTP(svc *Service) *HTTP {
	return &HTTP{svc: svc}
}

// errNoPrincipal means a protected handler ran without the middleware's
// principal: a wiring bug, answered 500.
var errNoPrincipal = errors.New("users: handler reached without an authenticated principal")

// defaultPageSize is the contract's default limit (components/pagination).
const defaultPageSize = 20

// GetMyProfile returns the caller's profile.
func (h *HTTP) GetMyProfile(ctx context.Context, _ apiv1.GetMyProfileRequestObject) (apiv1.GetMyProfileResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	pr, err := h.svc.Profile(ctx, p)
	if err != nil {
		return nil, err
	}
	return apiv1.GetMyProfile200JSONResponse(profile(pr)), nil
}

// UpdateMyProfile applies a profile patch and returns the profile and
// the verifications it started.
func (h *HTTP) UpdateMyProfile(ctx context.Context, req apiv1.UpdateMyProfileRequestObject) (apiv1.UpdateMyProfileResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	pr, started, err := h.svc.Update(ctx, p, patchOf(req.Body))
	if err != nil {
		return nil, err
	}
	verifications := make([]apiv1.Verification, 0, len(started))
	for i := range started {
		verifications = append(verifications, verification(started[i]))
	}
	return apiv1.UpdateMyProfile200JSONResponse{Profile: profile(pr), PendingVerifications: verifications}, nil
}

// ListMyEducations lists the caller's education records.
func (h *HTTP) ListMyEducations(ctx context.Context, req apiv1.ListMyEducationsRequestObject) (apiv1.ListMyEducationsResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	limit, cursor := int32(defaultPageSize), ""
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if req.Params.Cursor != nil {
		cursor = *req.Params.Cursor
	}
	page, err := h.svc.Educations(ctx, p, limit, cursor)
	if err != nil {
		return nil, err
	}
	items := make([]apiv1.Education, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, education(page.Items[i]))
	}
	return apiv1.ListMyEducations200JSONResponse{Items: items, NextCursor: page.NextCursor, PrevCursor: page.PrevCursor}, nil
}

// CreateMyEducation adds an education record for the caller. The answer
// is stored for the Idempotency-Key in the record's transaction (ADR-0034
// Karar 4): a retry whose first answer was lost gets the same record.
func (h *HTTP) CreateMyEducation(ctx context.Context, req apiv1.CreateMyEducationRequestObject) (apiv1.CreateMyEducationResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	var resp apiv1.CreateMyEducation201JSONResponse
	_, err := h.svc.CreateEducation(ctx, p, educationInput(req.Body), func(ctx context.Context, q db.Querier, e Education) error {
		location := "/v1/me/educations/" + e.ID.String()
		resp = apiv1.CreateMyEducation201JSONResponse{
			Body:    education(e),
			Headers: apiv1.CreateMyEducation201ResponseHeaders{Location: &location},
		}
		return idempotency.Commit(ctx, q, resp.VisitCreateMyEducationResponse)
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ReplaceMyEducation rewrites one of the caller's education records.
func (h *HTTP) ReplaceMyEducation(ctx context.Context, req apiv1.ReplaceMyEducationRequestObject) (apiv1.ReplaceMyEducationResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	e, err := h.svc.ReplaceEducation(ctx, p, req.EducationID, educationInput(req.Body))
	if err != nil {
		return nil, err
	}
	return apiv1.ReplaceMyEducation200JSONResponse(education(e)), nil
}

// DeleteMyEducation deletes one of the caller's education records.
func (h *HTTP) DeleteMyEducation(ctx context.Context, req apiv1.DeleteMyEducationRequestObject) (apiv1.DeleteMyEducationResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.svc.DeleteEducation(ctx, p, req.EducationID); err != nil {
		return nil, err
	}
	return apiv1.DeleteMyEducation204Response{}, nil
}

// profile is the caller's own view of the profile (USR-027): the phone
// masked like a verification's target, the identity number fully blanked.
// The profile picture and the CV are the bound media's ids (ADR-0036
// Karar 10). The links of later modules answer null or an empty list until
// they land (Karar 5): primary address, IBAN (Faz 6) and company
// memberships.
func profile(pr Profile) apiv1.UserProfile {
	out := apiv1.UserProfile{
		ID: pr.ID, Email: pr.Email, FirstName: pr.FirstName, LastName: pr.LastName,
		JobTitle: pr.JobTitle, About: pr.About, Qualifications: pr.Qualifications, Interests: pr.Interests,
		Employer: pr.Employer, EmailVerified: pr.EmailVerified, PhoneVerified: pr.PhoneVerified,
		IdentityVerified: pr.IdentityVerified, MaskedIdentityNumber: pr.MaskedIdentityNumber(),
		ProfilePictureID: pr.ProfilePicture, CvID: pr.CV,
		Companies: []apiv1.CompanyMembership{}, CreatedAt: jsontime.From(pr.CreatedAt),
	}
	if pr.Phone != nil {
		masked := authn.MaskPhone(*pr.Phone)
		out.PhoneNumber = &masked
	}
	if pr.BirthDate != nil {
		d := types.Date{Time: *pr.BirthDate}
		out.BirthDate = &d
	}
	if pr.Gender != nil {
		g := apiv1.Gender(*pr.Gender)
		out.Gender = &g
	}
	return out
}

func verification(v authn.Verification) apiv1.Verification {
	out := apiv1.Verification{
		ID: v.ID, Channel: apiv1.VerificationChannel(v.Channel), Status: apiv1.VerificationStatus(v.Status),
		MaskedTarget: v.MaskedTarget, RemainingAttempts: v.RemainingAttempts,
		CreatedAt: jsontime.From(v.CreatedAt), ExpiresAt: jsontime.From(v.ExpiresAt),
	}
	if v.ConfirmedAt != nil {
		t := jsontime.From(*v.ConfirmedAt)
		out.ConfirmedAt = &t
	}
	return out
}

func education(e Education) apiv1.Education {
	out := apiv1.Education{
		ID: e.ID, University: apiv1.ReferenceItem{ID: e.University.ID, Name: e.University.Name},
		Degree: apiv1.EducationDegree(e.Degree), StartYear: e.StartYear, EndYear: e.EndYear,
		CreatedAt: jsontime.From(e.CreatedAt),
	}
	if e.Faculty != nil {
		out.Faculty = &apiv1.ReferenceItem{ID: e.Faculty.ID, Name: e.Faculty.Name}
	}
	if e.Department != nil {
		out.Department = &apiv1.ReferenceItem{ID: e.Department.ID, Name: e.Department.Name}
	}
	return out
}

// patchOf is the validated patch body as the service takes it. The
// generated body has a nullable member per clearable field: not sent,
// null, or a value.
func patchOf(b *apiv1.UserProfilePatch) Patch {
	if b == nil {
		return Patch{}
	}
	pt := Patch{
		FirstName: b.FirstName, LastName: b.LastName, Email: b.Email, PhoneNumber: b.PhoneNumber,
		BirthDate: dateField(b.BirthDate), Gender: field(b.Gender), JobTitle: field(b.JobTitle),
		About: field(b.About), Qualifications: field(b.Qualifications), Interests: field(b.Interests),
		ProfilePicture: field(b.ProfilePictureID), CV: field(b.CvID), Address: field(b.AddressID),
	}
	if b.CurrentPassword != nil {
		pt.CurrentPassword = *b.CurrentPassword
	}
	return pt
}

// field reads a nullable member by its documented representation
// (map[true]T a value, map[false]T null, empty not sent).
func field[T any](n nullable.Nullable[T]) Field[T] {
	if len(n) == 0 {
		return Field[T]{}
	}
	v, ok := n[true]
	if !ok {
		return Field[T]{Set: true}
	}
	return Field[T]{Set: true, Value: &v}
}

func dateField(n nullable.Nullable[types.Date]) Field[time.Time] {
	f := field(n)
	if f.Value == nil {
		return Field[time.Time]{Set: f.Set}
	}
	d := f.Value.Time
	return Field[time.Time]{Set: true, Value: &d}
}

func educationInput(b *apiv1.EducationInput) EducationInput {
	if b == nil {
		return EducationInput{}
	}
	return EducationInput{
		UniversityID: b.UniversityID, FacultyID: b.FacultyID, DepartmentID: b.DepartmentID,
		Degree: string(b.Degree), StartYear: b.StartYear, EndYear: b.EndYear,
	}
}
