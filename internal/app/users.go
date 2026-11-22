package app

import (
	"context"
	"fmt"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/users"
)

// usersHandlers is the module's handler type, named here so that files
// with a local variable called users (the authentication service) need
// not import the package.
type usersHandlers = users.HTTP

// wireUsers builds the profile and education module (ADR-0036). A profile
// patch that sends an address or a number starts its verifications through
// the verification flows themselves, so the start's rules have one home;
// education records are deleted along the soft-delete catalog.
func wireUsers(database *db.DB, verifications *authn.Verifications, clk clock.Clock) (*users.HTTP, error) {
	cat, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		return nil, fmt.Errorf("wire users: %w", err)
	}
	svc, err := users.NewService(database, verifications, softdelete.New(cat), clk)
	if err != nil {
		return nil, fmt.Errorf("wire users: %w", err)
	}
	return users.NewHTTP(svc), nil
}

// The profile and education operations, forwarded to users.HTTP: its type
// is named HTTP like authn's, so it cannot be embedded next to it.

func (s strictServer) GetMyProfile(ctx context.Context, req apiv1.GetMyProfileRequestObject) (apiv1.GetMyProfileResponseObject, error) {
	return s.users.GetMyProfile(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) UpdateMyProfile(ctx context.Context, req apiv1.UpdateMyProfileRequestObject) (apiv1.UpdateMyProfileResponseObject, error) {
	return s.users.UpdateMyProfile(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ListMyEducations(ctx context.Context, req apiv1.ListMyEducationsRequestObject) (apiv1.ListMyEducationsResponseObject, error) {
	return s.users.ListMyEducations(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) CreateMyEducation(ctx context.Context, req apiv1.CreateMyEducationRequestObject) (apiv1.CreateMyEducationResponseObject, error) {
	return s.users.CreateMyEducation(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ReplaceMyEducation(ctx context.Context, req apiv1.ReplaceMyEducationRequestObject) (apiv1.ReplaceMyEducationResponseObject, error) {
	return s.users.ReplaceMyEducation(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) DeleteMyEducation(ctx context.Context, req apiv1.DeleteMyEducationRequestObject) (apiv1.DeleteMyEducationResponseObject, error) {
	return s.users.DeleteMyEducation(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}
