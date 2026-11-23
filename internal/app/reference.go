package app

import (
	"context"
	"fmt"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/reference"
)

// wireReference builds the reference lists (ADR-0037). Their rows come
// from db/referencedata through `koala migrate up`; the server only reads.
func wireReference(database *db.DB) (*reference.HTTP, error) {
	svc, err := reference.NewService(database)
	if err != nil {
		return nil, fmt.Errorf("wire reference: %w", err)
	}
	return reference.NewHTTP(svc), nil
}

// The reference operations, forwarded to reference.HTTP: its type is
// named HTTP like authn's, so it cannot be embedded next to it.

func (s strictServer) ListCountries(ctx context.Context, req apiv1.ListCountriesRequestObject) (apiv1.ListCountriesResponseObject, error) {
	return s.reference.ListCountries(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ListCities(ctx context.Context, req apiv1.ListCitiesRequestObject) (apiv1.ListCitiesResponseObject, error) {
	return s.reference.ListCities(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ListDistricts(ctx context.Context, req apiv1.ListDistrictsRequestObject) (apiv1.ListDistrictsResponseObject, error) {
	return s.reference.ListDistricts(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ListUniversities(ctx context.Context, req apiv1.ListUniversitiesRequestObject) (apiv1.ListUniversitiesResponseObject, error) {
	return s.reference.ListUniversities(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ListFaculties(ctx context.Context, req apiv1.ListFacultiesRequestObject) (apiv1.ListFacultiesResponseObject, error) {
	return s.reference.ListFaculties(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ListDepartments(ctx context.Context, req apiv1.ListDepartmentsRequestObject) (apiv1.ListDepartmentsResponseObject, error) {
	return s.reference.ListDepartments(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}
