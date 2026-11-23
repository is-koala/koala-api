package reference

import (
	"context"

	"github.com/is-koala/koala-api/internal/apiv1"
)

// HTTP implements the reference operations of the generated strict server
// (api/openapi.yaml, tag reference).
//
// Each handler reads the data version, then the list, then evaluates
// If-None-Match. The list comes before the precondition: a precondition is
// evaluated only for a request that would succeed (RFC 9110 §13.2.2), so a
// missing parent stays 404 whatever the client sends.
type HTTP struct {
	svc *Service
}

// NewHTTP returns the handlers.
func NewHTTP(svc *Service) *HTTP {
	return &HTTP{svc: svc}
}

// fresh reports whether the client's If-None-Match matches tag.
func fresh(ifNoneMatch *string, tag string) bool {
	return ifNoneMatch != nil && Matches(*ifNoneMatch, tag)
}

// ListCountries lists countries.
func (h *HTTP) ListCountries(ctx context.Context, req apiv1.ListCountriesRequestObject) (apiv1.ListCountriesResponseObject, error) {
	version, err := h.svc.Version(ctx)
	if err != nil {
		return nil, err
	}
	items, err := h.svc.Countries(ctx)
	if err != nil {
		return nil, err
	}
	tag := ETag(version)
	if fresh(req.Params.IfNoneMatch, tag) {
		return apiv1.ListCountries304Response{Headers: apiv1.ListCountries304ResponseHeaders{CacheControl: CacheControl, ETag: tag}}, nil
	}
	return apiv1.ListCountries200JSONResponse{
		Body: list(items), Headers: apiv1.ListCountries200ResponseHeaders{CacheControl: CacheControl, ETag: tag},
	}, nil
}

// ListCities lists a country's cities.
func (h *HTTP) ListCities(ctx context.Context, req apiv1.ListCitiesRequestObject) (apiv1.ListCitiesResponseObject, error) {
	version, err := h.svc.Version(ctx)
	if err != nil {
		return nil, err
	}
	items, err := h.svc.Cities(ctx, req.CountryID)
	if err != nil {
		return nil, err
	}
	tag := ETag(version)
	if fresh(req.Params.IfNoneMatch, tag) {
		return apiv1.ListCities304Response{Headers: apiv1.ListCities304ResponseHeaders{CacheControl: CacheControl, ETag: tag}}, nil
	}
	return apiv1.ListCities200JSONResponse{
		Body: list(items), Headers: apiv1.ListCities200ResponseHeaders{CacheControl: CacheControl, ETag: tag},
	}, nil
}

// ListDistricts lists a city's districts.
func (h *HTTP) ListDistricts(ctx context.Context, req apiv1.ListDistrictsRequestObject) (apiv1.ListDistrictsResponseObject, error) {
	version, err := h.svc.Version(ctx)
	if err != nil {
		return nil, err
	}
	items, err := h.svc.Districts(ctx, req.CityID)
	if err != nil {
		return nil, err
	}
	tag := ETag(version)
	if fresh(req.Params.IfNoneMatch, tag) {
		return apiv1.ListDistricts304Response{Headers: apiv1.ListDistricts304ResponseHeaders{CacheControl: CacheControl, ETag: tag}}, nil
	}
	return apiv1.ListDistricts200JSONResponse{
		Body: list(items), Headers: apiv1.ListDistricts200ResponseHeaders{CacheControl: CacheControl, ETag: tag},
	}, nil
}

// ListUniversities lists the universities.
func (h *HTTP) ListUniversities(ctx context.Context, req apiv1.ListUniversitiesRequestObject) (apiv1.ListUniversitiesResponseObject, error) {
	version, err := h.svc.Version(ctx)
	if err != nil {
		return nil, err
	}
	items, err := h.svc.Universities(ctx)
	if err != nil {
		return nil, err
	}
	tag := ETag(version)
	if fresh(req.Params.IfNoneMatch, tag) {
		return apiv1.ListUniversities304Response{Headers: apiv1.ListUniversities304ResponseHeaders{CacheControl: CacheControl, ETag: tag}}, nil
	}
	return apiv1.ListUniversities200JSONResponse{
		Body: list(items), Headers: apiv1.ListUniversities200ResponseHeaders{CacheControl: CacheControl, ETag: tag},
	}, nil
}

// ListFaculties lists a university's faculties.
func (h *HTTP) ListFaculties(ctx context.Context, req apiv1.ListFacultiesRequestObject) (apiv1.ListFacultiesResponseObject, error) {
	version, err := h.svc.Version(ctx)
	if err != nil {
		return nil, err
	}
	items, err := h.svc.Faculties(ctx, req.UniversityID)
	if err != nil {
		return nil, err
	}
	tag := ETag(version)
	if fresh(req.Params.IfNoneMatch, tag) {
		return apiv1.ListFaculties304Response{Headers: apiv1.ListFaculties304ResponseHeaders{CacheControl: CacheControl, ETag: tag}}, nil
	}
	return apiv1.ListFaculties200JSONResponse{
		Body: list(items), Headers: apiv1.ListFaculties200ResponseHeaders{CacheControl: CacheControl, ETag: tag},
	}, nil
}

// ListDepartments lists a faculty's departments.
func (h *HTTP) ListDepartments(ctx context.Context, req apiv1.ListDepartmentsRequestObject) (apiv1.ListDepartmentsResponseObject, error) {
	version, err := h.svc.Version(ctx)
	if err != nil {
		return nil, err
	}
	items, err := h.svc.Departments(ctx, req.FacultyID)
	if err != nil {
		return nil, err
	}
	tag := ETag(version)
	if fresh(req.Params.IfNoneMatch, tag) {
		return apiv1.ListDepartments304Response{Headers: apiv1.ListDepartments304ResponseHeaders{CacheControl: CacheControl, ETag: tag}}, nil
	}
	return apiv1.ListDepartments200JSONResponse{
		Body: list(items), Headers: apiv1.ListDepartments200ResponseHeaders{CacheControl: CacheControl, ETag: tag},
	}, nil
}

// list is the contract's ReferenceItemList; an empty list is [], not null.
func list(items []Item) apiv1.ReferenceItemList {
	out := apiv1.ReferenceItemList{Items: make([]apiv1.ReferenceItem, 0, len(items))}
	for _, it := range items {
		out.Items = append(out.Items, apiv1.ReferenceItem{ID: it.ID, Name: it.Name})
	}
	return out
}
