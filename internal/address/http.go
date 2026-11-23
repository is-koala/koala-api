package address

import (
	"context"
	"errors"
	"net/http"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
	"github.com/is-koala/koala-api/internal/platform/jsontime"
)

// HTTP implements the address operations of the generated strict server
// (api/openapi.yaml, tag address; ADR-0039). Every one is the caller's
// own: an address or a session is looked up by id and owner together, so
// another user's is the same 404 as one that never existed.
type HTTP struct {
	svc *Service
}

// NewHTTP returns the handlers.
func NewHTTP(svc *Service) *HTTP {
	return &HTTP{svc: svc}
}

// errNoPrincipal means a protected handler ran without the middleware's
// principal: a wiring bug, answered 500.
var errNoPrincipal = errors.New("address: handler reached without an authenticated principal")

// defaultPageSize is the contract's default limit (components/pagination).
const defaultPageSize = 20

// clientKey is the client address the lookup budget is kept for: an IPv4
// address or an IPv6 /64, as the rate limits key it.
func clientKey(ctx context.Context) string {
	return authn.ClientKey(httpx.ClientIPFrom(ctx))
}

// ListMyAddresses lists the caller's addresses.
func (h *HTTP) ListMyAddresses(ctx context.Context, req apiv1.ListMyAddressesRequestObject) (apiv1.ListMyAddressesResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	limit, cursor := defaultPageSize, ""
	if req.Params.Limit != nil {
		limit = int(*req.Params.Limit)
	}
	if req.Params.Cursor != nil {
		cursor = *req.Params.Cursor
	}
	page, err := h.svc.List(ctx, p, limit, cursor)
	if err != nil {
		return nil, err
	}
	items := make([]apiv1.Address, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, ownView(page.Items[i]))
	}
	return apiv1.ListMyAddresses200JSONResponse{Items: items, NextCursor: page.NextCursor, PrevCursor: page.PrevCursor}, nil
}

// CreateMyAddress adds an address for the caller. The answer is stored for
// the Idempotency-Key in the address's transaction (ADR-0034 Karar 4).
func (h *HTTP) CreateMyAddress(ctx context.Context, req apiv1.CreateMyAddressRequestObject) (apiv1.CreateMyAddressResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	var resp apiv1.CreateMyAddress201JSONResponse
	_, err := h.svc.Create(ctx, p, input(req.Body), func(ctx context.Context, q db.Querier, a Address) error {
		location := "/v1/me/addresses/" + a.ID.String()
		resp = apiv1.CreateMyAddress201JSONResponse{
			Body:    ownView(a),
			Headers: apiv1.CreateMyAddress201ResponseHeaders{Location: &location},
		}
		return idempotency.Commit(ctx, q, resp.VisitCreateMyAddressResponse)
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GetMyAddress returns one of the caller's addresses.
func (h *HTTP) GetMyAddress(ctx context.Context, req apiv1.GetMyAddressRequestObject) (apiv1.GetMyAddressResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	a, err := h.svc.Get(ctx, p, req.AddressID)
	if err != nil {
		return nil, err
	}
	return apiv1.GetMyAddress200JSONResponse(ownView(a)), nil
}

// ReplaceMyAddress rewrites one of the caller's addresses.
func (h *HTTP) ReplaceMyAddress(ctx context.Context, req apiv1.ReplaceMyAddressRequestObject) (apiv1.ReplaceMyAddressResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	a, err := h.svc.Replace(ctx, p, req.AddressID, input(req.Body))
	if err != nil {
		return nil, err
	}
	return apiv1.ReplaceMyAddress200JSONResponse(ownView(a)), nil
}

// DeleteMyAddress deletes one of the caller's addresses.
func (h *HTTP) DeleteMyAddress(ctx context.Context, req apiv1.DeleteMyAddressRequestObject) (apiv1.DeleteMyAddressResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.svc.Delete(ctx, p, req.AddressID); err != nil {
		return nil, err
	}
	return apiv1.DeleteMyAddress204Response{}, nil
}

// StartAddressSearchSession opens a search session of the caller.
func (h *HTTP) StartAddressSearchSession(ctx context.Context, _ apiv1.StartAddressSearchSessionRequestObject) (apiv1.StartAddressSearchSessionResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	s, err := h.svc.StartSession(ctx, p, clientKey(ctx))
	if err != nil {
		return nil, err
	}
	location := "/v1/me/address-search-sessions/" + s.ID.String() + "/suggestions"
	return apiv1.StartAddressSearchSession201JSONResponse{
		Body:    apiv1.AddressSearchSession{ID: s.ID, ExpiresAt: jsontime.From(s.ExpiresAt)},
		Headers: apiv1.StartAddressSearchSession201ResponseHeaders{Location: &location},
	}, nil
}

// ListAddressSuggestions returns the suggestions of the caller's session.
func (h *HTTP) ListAddressSuggestions(ctx context.Context, req apiv1.ListAddressSuggestionsRequestObject) (apiv1.ListAddressSuggestionsResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	got, err := h.svc.Suggest(ctx, p, req.SessionID, req.Params.Query, clientKey(ctx))
	if err != nil {
		return nil, err
	}
	items := make([]apiv1.AddressSuggestion, 0, len(got))
	for _, s := range got {
		items = append(items, apiv1.AddressSuggestion{
			CandidateID: s.CandidateID, PrimaryText: s.PrimaryText, SecondaryText: s.SecondaryText,
		})
	}
	return apiv1.ListAddressSuggestions200JSONResponse{Items: items}, nil
}

// ResolveAddressCandidate resolves a candidate of the caller's session.
func (h *HTTP) ResolveAddressCandidate(ctx context.Context, req apiv1.ResolveAddressCandidateRequestObject) (apiv1.ResolveAddressCandidateResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	r, err := h.svc.Resolve(ctx, p, req.SessionID, req.CandidateID, clientKey(ctx))
	if err != nil {
		return nil, err
	}
	return apiv1.ResolveAddressCandidate200JSONResponse(resolvedView(r)), nil
}

// ReverseLookupAddress turns the caller's map pin into components.
func (h *HTTP) ReverseLookupAddress(ctx context.Context, req apiv1.ReverseLookupAddressRequestObject) (apiv1.ReverseLookupAddressResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	var at Point
	if req.Body != nil {
		at = Point{Lat: req.Body.Latitude, Lng: req.Body.Longitude}
	}
	r, err := h.svc.Reverse(ctx, p, at, clientKey(ctx))
	if err != nil {
		return nil, err
	}
	return apiv1.ReverseLookupAddress200JSONResponse(resolvedView(r)), nil
}

// StrictMiddleware returns the strict server middleware of the address
// operations: a refusal of the lookup budget that carries a wait gets its
// Retry-After, which the contract's TooManyRequests promises.
func (h *HTTP) StrictMiddleware() apiv1.StrictMiddlewareFunc {
	return func(f apiv1.StrictHandlerFunc, _ string) apiv1.StrictHandlerFunc {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			resp, err := f(ctx, w, r, request)
			if wait, ok := RetryAfter(err); ok {
				httpx.SetRetryAfter(w, wait)
			}
			return resp, err
		}
	}
}

// ownView is the owner's view of an address: the coordinate exact
// (MED-009). Point.Render is the one place a coordinate is rounded; the
// views of other callers (a company's address, a job's) come with their
// modules and pass their own precision.
func ownView(a Address) apiv1.Address {
	out := apiv1.Address{
		ID:                   a.ID,
		City:                 apiv1.ReferenceItem{ID: a.City.ID, Name: a.City.Name},
		District:             apiv1.ReferenceItem{ID: a.District.ID, Name: a.District.Name},
		Neighborhood:         a.Neighborhood,
		Thoroughfare:         a.Thoroughfare,
		BuildingNumber:       a.BuildingNumber,
		BuildingName:         a.BuildingName,
		SiteName:             a.SiteName,
		Block:                a.Block,
		Floor:                a.Floor,
		UnitNumber:           a.UnitNumber,
		PostalCode:           a.PostalCode,
		AdditionalDirections: a.AdditionalDirections,
		DisplayLine:          a.DisplayLine(),
		CreatedAt:            jsontime.From(a.CreatedAt),
	}
	if a.Point != nil {
		r := a.Point.Render(PrecisionExact)
		out.Coordinates = &apiv1.Coordinates{
			Latitude: r.Lat, Longitude: r.Lng, Precision: apiv1.CoordinatesPrecision(r.Precision),
		}
	}
	return out
}

// resolvedView is the contract's ResolvedAddress. Its coordinate is given
// raw, not through Point.Render: after a reverse geocode it is the
// caller's own pin, after a resolve the location of the place the caller
// chose, and in both the caller is the one the address will belong to,
// who sees it exact (MED-009).
func resolvedView(r Resolved) apiv1.ResolvedAddress {
	out := apiv1.ResolvedAddress{
		CityID: r.CityID, DistrictID: r.DistrictID, Neighborhood: r.Neighborhood, Thoroughfare: r.Thoroughfare,
		BuildingNumber: r.BuildingNumber, SiteName: r.SiteName, PostalCode: r.PostalCode, ProviderPlaceID: r.PlaceID,
		Source: apiv1.AddressSource(r.Source),
	}
	if r.Point != nil {
		lat, lng := r.Point.Lat, r.Point.Lng
		out.Latitude, out.Longitude = &lat, &lng
	}
	return out
}

// input is the validated body as the service takes it.
func input(b *apiv1.AddressInput) Input {
	if b == nil {
		return Input{}
	}
	in := Input{
		CityID: b.CityID, DistrictID: b.DistrictID,
		Lines: Lines{
			Neighborhood: b.Neighborhood, Thoroughfare: b.Thoroughfare, BuildingNumber: b.BuildingNumber,
			BuildingName: b.BuildingName, SiteName: b.SiteName, Block: b.Block, Floor: b.Floor, UnitNumber: b.UnitNumber,
			PostalCode: b.PostalCode, AdditionalDirections: b.AdditionalDirections,
		},
		ProviderPlaceID: b.ProviderPlaceID,
	}
	if b.Source != nil {
		src := Source(*b.Source)
		in.Source = &src
	}
	if b.Coordinates != nil {
		in.Point = &Point{Lat: b.Coordinates.Latitude, Lng: b.Coordinates.Longitude}
	}
	return in
}
