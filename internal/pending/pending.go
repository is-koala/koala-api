// Package pending serves the operations whose contract is frozen but
// whose handler has not landed (ADR-0029 §1).
//
// Freezing a whole area's contract at once means the generated strict
// server interface grows ahead of the handlers. An operation nobody
// implements would stop the package from compiling, so Server implements
// every frozen operation with one answer: 501 NOT_IMPLEMENTED. The route
// exists and its contract binds; only the implementation is missing,
// which is why this is not the 404 of an unknown route.
//
// The contract marks each of these operations with `x-not-implemented`.
// internal/app.TestNotImplementedOperationsAreDeclared holds the two
// sides equal in both directions: a handler that landed while the marker
// stayed, and a marker that went while the stub stayed, are each red.
// 501 is not written as a response of its own in the contract: adding it
// and removing it when the handler lands would change the operation's
// status set, which the style guide calls an incompatible change, so it
// rides the operation's default response.
package pending

import (
	"context"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// CodeNotImplemented is the stable code of a frozen operation.
const CodeNotImplemented apperr.Code = "NOT_IMPLEMENTED"

// Codes lists the error codes this package returns; the contract test
// holds it against api/components/problem.yaml (ADR-0028).
func Codes() []apperr.Code { return []apperr.Code{CodeNotImplemented} }

// Operations lists the frozen operations as "METHOD /pattern", sorted.
// It is the code side of the two-way binding with the contract's
// `x-not-implemented`; internal/app holds it against both the contract
// and the methods Server actually has.
func Operations() []string {
	return []string{
		"DELETE /v1/companies/{companyId}/contacts/{userId}",
		"DELETE /v1/companies/{companyId}/follow",
		"DELETE /v1/companies/{companyId}/images/{imageId}",
		"DELETE /v1/companies/{companyId}/logo",
		"GET /v1/admin/companies",
		"GET /v1/admin/companies/{companyId}",
		"GET /v1/admin/identity-verifications",
		"GET /v1/admin/identity-verifications/{reviewId}",
		"GET /v1/admin/identity-verifications/{reviewId}/documents/{documentId}",
		"GET /v1/admin/users",
		"GET /v1/admin/users/{userId}",
		"GET /v1/admin/users/{userId}/identity-reveals",
		"GET /v1/companies/{companyId}",
		"GET /v1/companies/{companyId}/contact-invitations",
		"GET /v1/companies/{companyId}/contacts",
		"GET /v1/companies/{companyId}/images",
		"GET /v1/contact-invitations/{invitationId}",
		"GET /v1/me/followed-companies",
		"GET /v1/me/identity-documents",
		"GET /v1/users/{userId}",
		"PATCH /v1/companies/{companyId}",
		"POST /v1/admin/companies/{companyId}/approve",
		"POST /v1/admin/companies/{companyId}/reject",
		"POST /v1/admin/identity-verifications/{reviewId}/approve",
		"POST /v1/admin/identity-verifications/{reviewId}/reject",
		"POST /v1/companies",
		"POST /v1/companies/{companyId}/contact-invitations",
		"POST /v1/companies/{companyId}/images",
		"POST /v1/contact-invitations/{invitationId}/accept",
		"POST /v1/contact-invitations/{invitationId}/decline",
		"POST /v1/contact-invitations/{invitationId}/revoke",
		"POST /v1/me/identity-documents",
		"PUT /v1/companies/{companyId}/follow",
		"PUT /v1/companies/{companyId}/images",
		"PUT /v1/companies/{companyId}/logo",
	}
}

// Server implements the frozen part of the generated strict server
// interface. internal/app embeds it, so the operations no module
// implements answer 501 instead of failing to compile.
type Server struct{}

// notImplemented is the one error every frozen operation returns.
func notImplemented() error { return apperr.New(apperr.KindNotImplemented, CodeNotImplemented) }

// AcceptCompanyInvitation answers POST /v1/contact-invitations/{invitationId}/accept with 501 until its handler lands.
func (Server) AcceptCompanyInvitation(context.Context, apiv1.AcceptCompanyInvitationRequestObject) (apiv1.AcceptCompanyInvitationResponseObject, error) {
	return nil, notImplemented()
}

// AddCompanyImage answers POST /v1/companies/{companyId}/images with 501 until its handler lands.
func (Server) AddCompanyImage(context.Context, apiv1.AddCompanyImageRequestObject) (apiv1.AddCompanyImageResponseObject, error) {
	return nil, notImplemented()
}

// ApproveCompany answers POST /v1/admin/companies/{companyId}/approve with 501 until its handler lands.
func (Server) ApproveCompany(context.Context, apiv1.ApproveCompanyRequestObject) (apiv1.ApproveCompanyResponseObject, error) {
	return nil, notImplemented()
}

// ApproveIdentityReview answers POST /v1/admin/identity-verifications/{reviewId}/approve with 501 until its handler lands.
func (Server) ApproveIdentityReview(context.Context, apiv1.ApproveIdentityReviewRequestObject) (apiv1.ApproveIdentityReviewResponseObject, error) {
	return nil, notImplemented()
}

// CreateCompany answers POST /v1/companies with 501 until its handler lands.
func (Server) CreateCompany(context.Context, apiv1.CreateCompanyRequestObject) (apiv1.CreateCompanyResponseObject, error) {
	return nil, notImplemented()
}

// DeclineCompanyInvitation answers POST /v1/contact-invitations/{invitationId}/decline with 501 until its handler lands.
func (Server) DeclineCompanyInvitation(context.Context, apiv1.DeclineCompanyInvitationRequestObject) (apiv1.DeclineCompanyInvitationResponseObject, error) {
	return nil, notImplemented()
}

// FollowCompany answers PUT /v1/companies/{companyId}/follow with 501 until its handler lands.
func (Server) FollowCompany(context.Context, apiv1.FollowCompanyRequestObject) (apiv1.FollowCompanyResponseObject, error) {
	return nil, notImplemented()
}

// GetCompany answers GET /v1/companies/{companyId} with 501 until its handler lands.
func (Server) GetCompany(context.Context, apiv1.GetCompanyRequestObject) (apiv1.GetCompanyResponseObject, error) {
	return nil, notImplemented()
}

// GetCompanyForStaff answers GET /v1/admin/companies/{companyId} with 501 until its handler lands.
func (Server) GetCompanyForStaff(context.Context, apiv1.GetCompanyForStaffRequestObject) (apiv1.GetCompanyForStaffResponseObject, error) {
	return nil, notImplemented()
}

// GetCompanyInvitation answers GET /v1/contact-invitations/{invitationId} with 501 until its handler lands.
func (Server) GetCompanyInvitation(context.Context, apiv1.GetCompanyInvitationRequestObject) (apiv1.GetCompanyInvitationResponseObject, error) {
	return nil, notImplemented()
}

// GetIdentityReview answers GET /v1/admin/identity-verifications/{reviewId} with 501 until its handler lands.
func (Server) GetIdentityReview(context.Context, apiv1.GetIdentityReviewRequestObject) (apiv1.GetIdentityReviewResponseObject, error) {
	return nil, notImplemented()
}

// GetIdentityReviewDocument answers GET /v1/admin/identity-verifications/{reviewId}/documents/{documentId} with 501 until its handler lands.
func (Server) GetIdentityReviewDocument(context.Context, apiv1.GetIdentityReviewDocumentRequestObject) (apiv1.GetIdentityReviewDocumentResponseObject, error) {
	return nil, notImplemented()
}

// GetMyIdentityReview answers GET /v1/me/identity-documents with 501 until its handler lands.
func (Server) GetMyIdentityReview(context.Context, apiv1.GetMyIdentityReviewRequestObject) (apiv1.GetMyIdentityReviewResponseObject, error) {
	return nil, notImplemented()
}

// GetPublicProfile answers GET /v1/users/{userId} with 501 until its handler lands.
func (Server) GetPublicProfile(context.Context, apiv1.GetPublicProfileRequestObject) (apiv1.GetPublicProfileResponseObject, error) {
	return nil, notImplemented()
}

// GetUserForStaff answers GET /v1/admin/users/{userId} with 501 until its handler lands.
func (Server) GetUserForStaff(context.Context, apiv1.GetUserForStaffRequestObject) (apiv1.GetUserForStaffResponseObject, error) {
	return nil, notImplemented()
}

// InviteCompanyContact answers POST /v1/companies/{companyId}/contact-invitations with 501 until its handler lands.
func (Server) InviteCompanyContact(context.Context, apiv1.InviteCompanyContactRequestObject) (apiv1.InviteCompanyContactResponseObject, error) {
	return nil, notImplemented()
}

// ListCompaniesForStaff answers GET /v1/admin/companies with 501 until its handler lands.
func (Server) ListCompaniesForStaff(context.Context, apiv1.ListCompaniesForStaffRequestObject) (apiv1.ListCompaniesForStaffResponseObject, error) {
	return nil, notImplemented()
}

// ListCompanyContacts answers GET /v1/companies/{companyId}/contacts with 501 until its handler lands.
func (Server) ListCompanyContacts(context.Context, apiv1.ListCompanyContactsRequestObject) (apiv1.ListCompanyContactsResponseObject, error) {
	return nil, notImplemented()
}

// ListCompanyImages answers GET /v1/companies/{companyId}/images with 501 until its handler lands.
func (Server) ListCompanyImages(context.Context, apiv1.ListCompanyImagesRequestObject) (apiv1.ListCompanyImagesResponseObject, error) {
	return nil, notImplemented()
}

// ListCompanyInvitations answers GET /v1/companies/{companyId}/contact-invitations with 501 until its handler lands.
func (Server) ListCompanyInvitations(context.Context, apiv1.ListCompanyInvitationsRequestObject) (apiv1.ListCompanyInvitationsResponseObject, error) {
	return nil, notImplemented()
}

// ListIdentityRevealsForStaff answers GET /v1/admin/users/{userId}/identity-reveals with 501 until its handler lands.
func (Server) ListIdentityRevealsForStaff(context.Context, apiv1.ListIdentityRevealsForStaffRequestObject) (apiv1.ListIdentityRevealsForStaffResponseObject, error) {
	return nil, notImplemented()
}

// ListIdentityReviews answers GET /v1/admin/identity-verifications with 501 until its handler lands.
func (Server) ListIdentityReviews(context.Context, apiv1.ListIdentityReviewsRequestObject) (apiv1.ListIdentityReviewsResponseObject, error) {
	return nil, notImplemented()
}

// ListMyFollowedCompanies answers GET /v1/me/followed-companies with 501 until its handler lands.
func (Server) ListMyFollowedCompanies(context.Context, apiv1.ListMyFollowedCompaniesRequestObject) (apiv1.ListMyFollowedCompaniesResponseObject, error) {
	return nil, notImplemented()
}

// ListUsersForStaff answers GET /v1/admin/users with 501 until its handler lands.
func (Server) ListUsersForStaff(context.Context, apiv1.ListUsersForStaffRequestObject) (apiv1.ListUsersForStaffResponseObject, error) {
	return nil, notImplemented()
}

// RejectCompany answers POST /v1/admin/companies/{companyId}/reject with 501 until its handler lands.
func (Server) RejectCompany(context.Context, apiv1.RejectCompanyRequestObject) (apiv1.RejectCompanyResponseObject, error) {
	return nil, notImplemented()
}

// RejectIdentityReview answers POST /v1/admin/identity-verifications/{reviewId}/reject with 501 until its handler lands.
func (Server) RejectIdentityReview(context.Context, apiv1.RejectIdentityReviewRequestObject) (apiv1.RejectIdentityReviewResponseObject, error) {
	return nil, notImplemented()
}

// RemoveCompanyContact answers DELETE /v1/companies/{companyId}/contacts/{userId} with 501 until its handler lands.
func (Server) RemoveCompanyContact(context.Context, apiv1.RemoveCompanyContactRequestObject) (apiv1.RemoveCompanyContactResponseObject, error) {
	return nil, notImplemented()
}

// RemoveCompanyImage answers DELETE /v1/companies/{companyId}/images/{imageId} with 501 until its handler lands.
func (Server) RemoveCompanyImage(context.Context, apiv1.RemoveCompanyImageRequestObject) (apiv1.RemoveCompanyImageResponseObject, error) {
	return nil, notImplemented()
}

// RemoveCompanyLogo answers DELETE /v1/companies/{companyId}/logo with 501 until its handler lands.
func (Server) RemoveCompanyLogo(context.Context, apiv1.RemoveCompanyLogoRequestObject) (apiv1.RemoveCompanyLogoResponseObject, error) {
	return nil, notImplemented()
}

// ReplaceCompanyGallery answers PUT /v1/companies/{companyId}/images with 501 until its handler lands.
func (Server) ReplaceCompanyGallery(context.Context, apiv1.ReplaceCompanyGalleryRequestObject) (apiv1.ReplaceCompanyGalleryResponseObject, error) {
	return nil, notImplemented()
}

// RevokeCompanyInvitation answers POST /v1/contact-invitations/{invitationId}/revoke with 501 until its handler lands.
func (Server) RevokeCompanyInvitation(context.Context, apiv1.RevokeCompanyInvitationRequestObject) (apiv1.RevokeCompanyInvitationResponseObject, error) {
	return nil, notImplemented()
}

// SetCompanyLogo answers PUT /v1/companies/{companyId}/logo with 501 until its handler lands.
func (Server) SetCompanyLogo(context.Context, apiv1.SetCompanyLogoRequestObject) (apiv1.SetCompanyLogoResponseObject, error) {
	return nil, notImplemented()
}

// SubmitMyIdentityDocuments answers POST /v1/me/identity-documents with 501 until its handler lands.
func (Server) SubmitMyIdentityDocuments(context.Context, apiv1.SubmitMyIdentityDocumentsRequestObject) (apiv1.SubmitMyIdentityDocumentsResponseObject, error) {
	return nil, notImplemented()
}

// UnfollowCompany answers DELETE /v1/companies/{companyId}/follow with 501 until its handler lands.
func (Server) UnfollowCompany(context.Context, apiv1.UnfollowCompanyRequestObject) (apiv1.UnfollowCompanyResponseObject, error) {
	return nil, notImplemented()
}

// UpdateCompany answers PATCH /v1/companies/{companyId} with 501 until its handler lands.
func (Server) UpdateCompany(context.Context, apiv1.UpdateCompanyRequestObject) (apiv1.UpdateCompanyResponseObject, error) {
	return nil, notImplemented()
}
