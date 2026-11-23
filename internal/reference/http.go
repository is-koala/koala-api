package reference

import (
	"context"

	"github.com/is-koala/koala-api/internal/apiv1"
)

// HTTP implements the reference operations of the generated strict server
// (api/openapi.yaml, tag reference).
type HTTP struct {
	svc *Service
}

// NewHTTP returns the handlers.
func NewHTTP(svc *Service) *HTTP {
	return &HTTP{svc: svc}
}

// ListCountries lists the countries.
func (h *HTTP) ListCountries(ctx context.Context, _ apiv1.ListCountriesRequestObject) (apiv1.ListCountriesResponseObject, error) {
	items, err := h.svc.Countries(ctx)
	if err != nil {
		return nil, err
	}
	return apiv1.ListCountries200JSONResponse(list(items)), nil
}

// ListCities lists a country's cities.
func (h *HTTP) ListCities(ctx context.Context, req apiv1.ListCitiesRequestObject) (apiv1.ListCitiesResponseObject, error) {
	items, err := h.svc.Cities(ctx, req.CountryID)
	if err != nil {
		return nil, err
	}
	return apiv1.ListCities200JSONResponse(list(items)), nil
}

// ListDistricts lists a city's districts.
func (h *HTTP) ListDistricts(ctx context.Context, req apiv1.ListDistrictsRequestObject) (apiv1.ListDistrictsResponseObject, error) {
	items, err := h.svc.Districts(ctx, req.CityID)
	if err != nil {
		return nil, err
	}
	return apiv1.ListDistricts200JSONResponse(list(items)), nil
}

// ListUniversities lists the universities.
func (h *HTTP) ListUniversities(ctx context.Context, _ apiv1.ListUniversitiesRequestObject) (apiv1.ListUniversitiesResponseObject, error) {
	items, err := h.svc.Universities(ctx)
	if err != nil {
		return nil, err
	}
	return apiv1.ListUniversities200JSONResponse(list(items)), nil
}

// ListFaculties lists a university's faculties.
func (h *HTTP) ListFaculties(ctx context.Context, req apiv1.ListFacultiesRequestObject) (apiv1.ListFacultiesResponseObject, error) {
	items, err := h.svc.Faculties(ctx, req.UniversityID)
	if err != nil {
		return nil, err
	}
	return apiv1.ListFaculties200JSONResponse(list(items)), nil
}

// ListDepartments lists a faculty's departments.
func (h *HTTP) ListDepartments(ctx context.Context, req apiv1.ListDepartmentsRequestObject) (apiv1.ListDepartmentsResponseObject, error) {
	items, err := h.svc.Departments(ctx, req.FacultyID)
	if err != nil {
		return nil, err
	}
	return apiv1.ListDepartments200JSONResponse(list(items)), nil
}

// list is the contract's ReferenceItemList; an empty list is [], not null.
func list(items []Item) apiv1.ReferenceItemList {
	out := apiv1.ReferenceItemList{Items: make([]apiv1.ReferenceItem, 0, len(items))}
	for _, it := range items {
		out.Items = append(out.Items, apiv1.ReferenceItem{ID: it.ID, Name: it.Name})
	}
	return out
}
