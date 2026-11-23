package reference

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/reference/referencedb"
)

// Service reads the reference lists.
type Service struct {
	reads *referencedb.Queries
}

// NewService returns the service on database.
func NewService(database *db.DB) (*Service, error) {
	if database == nil {
		return nil, errors.New("reference: the service needs a database")
	}
	return &Service{reads: referencedb.New(database.Pool())}, nil
}

// Version is the data version the tables hold, 0 before the first sync.
// A handler reads it before the list: if a sync lands in between, the
// list is newer than its ETag and the next conditional request gets the
// new list, never the other way round.
func (s *Service) Version(ctx context.Context) (int32, error) {
	v, err := s.reads.GetDataVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("reference: read the data version: %w", err)
	}
	return v, nil
}

// Countries lists the countries in use.
func (s *Service) Countries(ctx context.Context) ([]Item, error) {
	rows, err := s.reads.ListCountries(ctx)
	if err != nil {
		return nil, fmt.Errorf("reference: list the countries: %w", err)
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, Item{ID: r.ID, Name: r.Name})
	}
	return items, nil
}

// Universities lists the universities in use.
func (s *Service) Universities(ctx context.Context) ([]Item, error) {
	rows, err := s.reads.ListUniversities(ctx)
	if err != nil {
		return nil, fmt.Errorf("reference: list the universities: %w", err)
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, Item{ID: r.ID, Name: r.Name})
	}
	return items, nil
}

// Cities lists the cities of a country in use.
func (s *Service) Cities(ctx context.Context, countryID uuid.UUID) ([]Item, error) {
	rows, err := s.reads.ListCities(ctx, countryID)
	if err != nil {
		return nil, fmt.Errorf("reference: list the cities: %w", err)
	}
	out := make([]childRow, len(rows))
	for i, r := range rows {
		out[i] = childRow(r)
	}
	return children(out)
}

// Districts lists the districts of a city in use.
func (s *Service) Districts(ctx context.Context, cityID uuid.UUID) ([]Item, error) {
	rows, err := s.reads.ListDistricts(ctx, cityID)
	if err != nil {
		return nil, fmt.Errorf("reference: list the districts: %w", err)
	}
	out := make([]childRow, len(rows))
	for i, r := range rows {
		out[i] = childRow(r)
	}
	return children(out)
}

// Faculties lists the faculties of a university in use.
func (s *Service) Faculties(ctx context.Context, universityID uuid.UUID) ([]Item, error) {
	rows, err := s.reads.ListFaculties(ctx, universityID)
	if err != nil {
		return nil, fmt.Errorf("reference: list the faculties: %w", err)
	}
	out := make([]childRow, len(rows))
	for i, r := range rows {
		out[i] = childRow(r)
	}
	return children(out)
}

// Departments lists the departments of a faculty in use.
func (s *Service) Departments(ctx context.Context, facultyID uuid.UUID) ([]Item, error) {
	rows, err := s.reads.ListDepartments(ctx, facultyID)
	if err != nil {
		return nil, fmt.Errorf("reference: list the departments: %w", err)
	}
	out := make([]childRow, len(rows))
	for i, r := range rows {
		out[i] = childRow(r)
	}
	return children(out)
}

// childRow is a row of a parent LEFT JOIN child query; the four list
// queries return the same shape.
type childRow struct {
	ID   uuid.NullUUID
	Name *string
}

// children turns the rows of a parent LEFT JOIN child query into a list:
// no row is a parent that is missing or retired (REFERENCE_ITEM_NOT_FOUND),
// a row without a child is a parent in use without children.
func children(rows []childRow) ([]Item, error) {
	if len(rows) == 0 {
		return nil, apperr.New(apperr.KindNotFound, CodeReferenceItemNotFound)
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		if !r.ID.Valid || r.Name == nil {
			continue
		}
		items = append(items, Item{ID: r.ID.UUID, Name: *r.Name})
	}
	return items, nil
}
