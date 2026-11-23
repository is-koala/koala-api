package address

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/address/addressdb"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/integrations/googlemaps"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
)

// provider is the map provider (internal/integrations/googlemaps: the
// Google client or the canned driver). It is called only through metered,
// which takes a spent unit of the lookup budget.
type provider interface {
	Autocomplete(ctx context.Context, input, token string, restriction *googlemaps.Rect) ([]googlemaps.Prediction, error)
	Details(ctx context.Context, placeID, token string) (googlemaps.Place, error)
	Reverse(ctx context.Context, at googlemaps.LatLng) (googlemaps.Place, error)
	RegionBounds(ctx context.Context, name, countryCode string) (googlemaps.Rect, error)
}

// spent is a billed unit reserved before a provider call (ADR-0039 Karar
// 7). Its field is unexported and only reserve and an open session (whose
// start reserved the session's unit) make one: the provider cannot be
// reached past the budget, whatever path a later change adds.
type spent struct {
	unit Unit
}

// lookupLockClass is the first key of the lookup budget's advisory lock
// (LockLookups): "KA".
const lookupLockClass = 0x4b41

// boundsTTL is how long the service area's bounds are kept in memory: a
// day, far inside the 30 days the provider's terms allow for coordinates.
// boundsRetry is how long a failed lookup is not tried again.
const (
	boundsTTL   = 24 * time.Hour
	boundsRetry = 10 * time.Minute
)

// search is the address search's state beyond the database: the service
// area, its bounds cached in memory and the budget.
type search struct {
	s      *Service
	places *Places
	area   ServiceArea
	budget Budget

	mu          sync.Mutex
	bounds      *googlemaps.Rect
	boundsUntil time.Time
}

func newSearch(s *Service, cfg Config) *search {
	b := cfg.Budget.withDefaults()
	s.metrics.capped.Set(float64(b.PerDay))
	return &search{s: s, places: cfg.Places, area: cfg.Area, budget: b}
}

func providerUnavailable() error {
	return apperr.New(apperr.KindUpstream, CodeProviderUnavailable)
}

func sessionNotFound() error {
	return apperr.New(apperr.KindNotFound, CodeSessionNotFound)
}

func outsideServiceArea() error {
	return apperr.New(apperr.KindUnprocessable, CodeOutsideServiceArea)
}

// lookupLimited is ADDRESS_LOOKUP_LIMIT_REACHED with the wait until the
// window frees a unit (0: none to give).
func lookupLimited(wait time.Duration) error {
	return &limitError{err: apperr.New(apperr.KindRateLimited, CodeLookupLimitReached), wait: wait}
}

// limitError carries the Retry-After of a spent budget.
type limitError struct {
	err  error
	wait time.Duration
}

func (e *limitError) Error() string { return e.err.Error() }

func (e *limitError) Unwrap() error { return e.err }

// RetryAfter is how long the caller of a refused lookup waits, when the
// refusal says.
func RetryAfter(err error) (time.Duration, bool) {
	var l *limitError
	if errors.As(err, &l) && l.wait > 0 {
		return l.wait, true
	}
	return 0, false
}

// caller is who a unit is billed to: the user and the client address.
type caller struct {
	user   uuid.UUID
	client string
}

func callerOf(p authn.UserPrincipal, client string) caller {
	return caller{user: p.UserID, client: client}
}

// reserve spends one billed unit in q's transaction (ADR-0039 Karar 7),
// under the budget's lock, in the order the buckets of ADR-0033 Karar 4
// take: the caller's daily budget first, so a caller over it spends
// nothing of the client address's budget, which a carrier NAT shares;
// then the client address's hourly budget; then the service's daily cap,
// which every unit counts the same, whatever the call (lesson of #21). A
// refusal writes nothing. The service's own bounds lookup has no caller
// and counts only on the cap.
//
// The caller's and the address's refusals are 429 with the wait; the
// cap's is ADDRESS_PROVIDER_UNAVAILABLE: from the client's side the
// provider is out for the day, and the remedy is the same, entering the
// address by hand.
func (x *search) reserve(ctx context.Context, q *addressdb.Queries, c *caller, unit Unit, now time.Time) (spent, error) {
	if err := q.LockLookups(ctx, lookupLockClass); err != nil {
		return spent{}, fmt.Errorf("address: lock the lookup budget: %w", err)
	}
	if c != nil {
		since := now.Add(-UserWindow)
		n, err := q.CountUserLookups(ctx, addressdb.CountUserLookupsParams{UserID: c.user, Since: since})
		if err != nil {
			return spent{}, fmt.Errorf("address: count the user's lookups: %w", err)
		}
		if n >= int64(x.budget.PerUser) {
			x.s.metrics.reserved(unit, outcomeUserLimited)
			oldest, err := q.OldestUserLookup(ctx, addressdb.OldestUserLookupParams{UserID: c.user, Since: since})
			if err != nil {
				return spent{}, fmt.Errorf("address: read the user's oldest lookup: %w", err)
			}
			return spent{}, lookupLimited(oldest.Add(UserWindow).Sub(now))
		}
		since = now.Add(-ClientWindow)
		n, err = q.CountClientLookups(ctx, addressdb.CountClientLookupsParams{ClientKey: c.client, Since: since})
		if err != nil {
			return spent{}, fmt.Errorf("address: count the client's lookups: %w", err)
		}
		if n >= int64(x.budget.PerClient) {
			x.s.metrics.reserved(unit, outcomeClientLimited)
			oldest, err := q.OldestClientLookup(ctx, addressdb.OldestClientLookupParams{ClientKey: c.client, Since: since})
			if err != nil {
				return spent{}, fmt.Errorf("address: read the client's oldest lookup: %w", err)
			}
			return spent{}, lookupLimited(oldest.Add(ClientWindow).Sub(now))
		}
	}
	n, err := q.CountAllLookups(ctx, now.Add(-ServiceWindow))
	if err != nil {
		return spent{}, fmt.Errorf("address: count the lookups: %w", err)
	}
	if n >= int64(x.budget.PerDay) {
		x.s.metrics.reserved(unit, outcomeCapped)
		return spent{}, providerUnavailable()
	}
	row := addressdb.InsertLookupParams{ID: uuid.Must(uuid.NewV7()), Unit: string(unit), CreatedAt: now}
	if c != nil {
		client := c.client
		row.UserID = uuid.NullUUID{UUID: c.user, Valid: true}
		row.ClientKey = &client
	}
	if err := q.InsertLookup(ctx, row); err != nil {
		return spent{}, fmt.Errorf("address: spend the lookup: %w", err)
	}
	x.s.metrics.reserved(unit, outcomeReserved)
	return spent{unit: unit}, nil
}

// metered is the provider behind the budget: each call takes the spent
// unit that pays for it, and counts its outcome. Without a provider (no
// key in local and test) every call is ADDRESS_PROVIDER_UNAVAILABLE.
type metered struct {
	p       provider
	metrics *Metrics
	logger  *slog.Logger
}

// failed maps a provider error to the caller's answer and counts it. Only
// the reason is logged: never the query, the place or the coordinate.
func (m metered) failed(ctx context.Context, call string, err error) error {
	if errors.Is(err, googlemaps.ErrNoResult) {
		m.metrics.called(call, outcomeNoResult)
		return err
	}
	reason := googlemaps.ReasonOf(err)
	m.metrics.called(call, reason)
	m.logger.WarnContext(ctx, "address provider call failed", slog.String("call", call), slog.String("reason", reason))
	return providerUnavailable()
}

func (m metered) unwired(call string) error {
	m.metrics.called(call, outcomeUnwired)
	return providerUnavailable()
}

func (m metered) autocomplete(ctx context.Context, _ spent, input, token string, r *googlemaps.Rect) ([]googlemaps.Prediction, error) {
	if m.p == nil {
		return nil, m.unwired(callAutocomplete)
	}
	out, err := m.p.Autocomplete(ctx, input, token, r)
	if err != nil {
		return nil, m.failed(ctx, callAutocomplete, err)
	}
	m.metrics.called(callAutocomplete, outcomeOK)
	return out, nil
}

func (m metered) details(ctx context.Context, _ spent, placeID, token string) (googlemaps.Place, error) {
	if m.p == nil {
		return googlemaps.Place{}, m.unwired(callDetails)
	}
	out, err := m.p.Details(ctx, placeID, token)
	if err != nil {
		return googlemaps.Place{}, m.failed(ctx, callDetails, err)
	}
	m.metrics.called(callDetails, outcomeOK)
	return out, nil
}

func (m metered) reverse(ctx context.Context, _ spent, at googlemaps.LatLng) (googlemaps.Place, error) {
	if m.p == nil {
		return googlemaps.Place{}, m.unwired(callReverse)
	}
	out, err := m.p.Reverse(ctx, at)
	if err != nil {
		return googlemaps.Place{}, m.failed(ctx, callReverse, err)
	}
	m.metrics.called(callReverse, outcomeOK)
	return out, nil
}

func (m metered) regionBounds(ctx context.Context, _ spent, name, country string) (googlemaps.Rect, error) {
	if m.p == nil {
		return googlemaps.Rect{}, m.unwired(callBounds)
	}
	out, err := m.p.RegionBounds(ctx, name, country)
	if err != nil {
		return googlemaps.Rect{}, m.failed(ctx, callBounds, err)
	}
	m.metrics.called(callBounds, outcomeOK)
	return out, nil
}

func (s *Service) metered() metered {
	return metered{p: s.provider, metrics: s.metrics, logger: s.logger}
}

// Session is an open search session.
type Session struct {
	ID        uuid.UUID
	ExpiresAt time.Time
}

// StartSession opens a search session of the caller (ADR-0029 Karar 8):
// one billed unit of the budget, the session's, reserved with the row.
// The provider's session token is a random UUIDv4, tied to nothing of the
// caller (ADR-0039 Karar 9). Without a provider the start is refused at
// once, before it spends anything.
func (s *Service) StartSession(ctx context.Context, p authn.UserPrincipal, client string) (Session, error) {
	if s.provider == nil {
		return Session{}, s.metered().unwired(callAutocomplete)
	}
	now := s.clock.Now()
	out := Session{ID: uuid.Must(uuid.NewV7()), ExpiresAt: now.Add(SessionTTL)}
	c := callerOf(p, client)
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		if _, err := s.search.reserve(ctx, q, &c, UnitSession, now); err != nil {
			return err
		}
		err := q.InsertSession(ctx, addressdb.InsertSessionParams{
			ID: out.ID, UserID: p.UserID, ProviderToken: uuid.New(), CreatedAt: now, ExpiresAt: out.ExpiresAt,
		})
		if err != nil {
			return fmt.Errorf("address: open the session: %w", err)
		}
		return nil
	})
	if err != nil {
		return Session{}, fmt.Errorf("address: start a search session: %w", err)
	}
	return out, nil
}

// Suggestion is one suggestion of a session: its candidate id and the
// provider's two display lines, shown and never stored (MED-008).
type Suggestion struct {
	CandidateID   string
	PrimaryText   string
	SecondaryText string
}

// candidatePrefix starts a candidate id; the rest is the place id's index
// in the session.
const candidatePrefix = "c_"

// openSession locks one of the caller's sessions and checks it is alive:
// another user's, an expired one and one a resolve ended are the same 404
// (MED-010). The checks come before any budget is counted, so a refusal
// for the budget never tells a session apart from a missing one.
func openSession(ctx context.Context, q *addressdb.Queries, userID, id uuid.UUID, now time.Time) (addressdb.LockSessionRow, spent, error) {
	row, err := q.LockSession(ctx, addressdb.LockSessionParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return addressdb.LockSessionRow{}, spent{}, sessionNotFound()
	}
	if err != nil {
		return addressdb.LockSessionRow{}, spent{}, fmt.Errorf("address: lock the session: %w", err)
	}
	if !now.Before(row.ExpiresAt) || row.ResolvedAt != nil {
		return addressdb.LockSessionRow{}, spent{}, sessionNotFound()
	}
	return row, spent{unit: UnitSession}, nil
}

// Suggest returns the provider's suggestions for query in the caller's
// session. A query shorter than MinQueryRunes once trimmed reaches no one
// and gets none. A session makes at most SuggestionRequestsPerSession
// requests; they are billed with the session (its unit), and the bound is
// what an abandoned session can cost. The place ids are kept in the
// session, the candidate is their index; the display lines are not kept.
func (s *Service) Suggest(ctx context.Context, p authn.UserPrincipal, sessionID uuid.UUID, query string) ([]Suggestion, error) {
	query = strings.TrimSpace(query)
	now := s.clock.Now()
	var token uuid.UUID
	var paid spent
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		row, sp, err := openSession(ctx, q, p.UserID, sessionID, now)
		if err != nil {
			return err
		}
		if utf8.RuneCountInString(query) < MinQueryRunes {
			return nil
		}
		if row.SuggestionRequests >= SuggestionRequestsPerSession {
			return lookupLimited(0)
		}
		if err := q.CountSuggestionRequest(ctx, sessionID); err != nil {
			return fmt.Errorf("address: count the request: %w", err)
		}
		token, paid = row.ProviderToken, sp
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("address: suggest: %w", err)
	}
	if paid.unit == "" {
		return []Suggestion{}, nil
	}
	predictions, err := s.metered().autocomplete(ctx, paid, query, token.String(), s.search.restriction(ctx, now))
	if err != nil {
		return nil, err
	}
	if len(predictions) > MaxSuggestions {
		predictions = predictions[:MaxSuggestions]
	}
	if len(predictions) == 0 {
		return []Suggestion{}, nil
	}
	ids := make([]string, len(predictions))
	for i, pr := range predictions {
		ids[i] = pr.PlaceID
	}
	held, err := addressdb.New(s.db.Pool()).AppendCandidates(ctx, addressdb.AppendCandidatesParams{ID: sessionID, PlaceIds: ids})
	if err != nil {
		return nil, fmt.Errorf("address: keep the candidates: %w", err)
	}
	first := int(held) - len(ids)
	out := make([]Suggestion, len(predictions))
	for i, pr := range predictions {
		out[i] = Suggestion{
			CandidateID: candidatePrefix + strconv.Itoa(first+i), PrimaryText: pr.MainText, SecondaryText: pr.SecondaryText,
		}
	}
	return out, nil
}

// Resolve resolves a candidate of the caller's session into our
// components. The session ends here, as the provider's does with the
// place details call; the details call is one billed unit. Should the
// provider call fail, the end is taken back so the caller may try again
// (the unit stays spent). A candidate the session never offered is the
// session's 404: a candidate id means nothing outside its session.
func (s *Service) Resolve(ctx context.Context, p authn.UserPrincipal, sessionID uuid.UUID, candidateID, client string) (Resolved, error) {
	now := s.clock.Now()
	c := callerOf(p, client)
	var placeID string
	var token uuid.UUID
	var paid spent
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		row, _, err := openSession(ctx, q, p.UserID, sessionID, now)
		if err != nil {
			return err
		}
		i, ok := candidateIndex(candidateID, len(row.Candidates))
		if !ok {
			return sessionNotFound()
		}
		sp, err := s.search.reserve(ctx, q, &c, UnitDetails, now)
		if err != nil {
			return err
		}
		if err := q.EndSession(ctx, addressdb.EndSessionParams{ID: sessionID, ResolvedAt: now}); err != nil {
			return fmt.Errorf("address: end the session: %w", err)
		}
		placeID, token, paid = row.Candidates[i], row.ProviderToken, sp
		return nil
	})
	if err != nil {
		return Resolved{}, fmt.Errorf("address: resolve: %w", err)
	}
	place, err := s.metered().details(ctx, paid, placeID, token.String())
	if err != nil {
		reopenErr := addressdb.New(s.db.Pool()).ReopenSession(ctx, addressdb.ReopenSessionParams{ID: sessionID, ResolvedAt: now})
		if reopenErr != nil {
			s.logger.WarnContext(ctx, "address search session not reopened", slog.Any("error", reopenErr))
		}
		if errors.Is(err, googlemaps.ErrNoResult) {
			return Resolved{}, outsideServiceArea()
		}
		return Resolved{}, err
	}
	var at *Point
	if place.Location != nil {
		at = &Point{Lat: place.Location.Lat, Lng: place.Location.Lng}
	}
	return s.search.resolved(place, at)
}

// Reverse turns the caller's map pin into our components: one billed
// unit, and only the coordinate goes to the provider (ADR-0039 Karar 9).
// The point answered is the caller's pin, not the provider's geometry: the
// pin is the caller's own datum. A point no address matches, or one
// outside the service area's cities, is ADDRESS_OUTSIDE_SERVICE_AREA.
func (s *Service) Reverse(ctx context.Context, p authn.UserPrincipal, at Point, client string) (Resolved, error) {
	if s.provider == nil {
		return Resolved{}, s.metered().unwired(callReverse)
	}
	now := s.clock.Now()
	c := callerOf(p, client)
	var paid spent
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		sp, err := s.search.reserve(ctx, addressdb.New(tx), &c, UnitReverse, now)
		paid = sp
		return err
	})
	if err != nil {
		return Resolved{}, fmt.Errorf("address: reverse: %w", err)
	}
	place, err := s.metered().reverse(ctx, paid, googlemaps.LatLng{Lat: at.Lat, Lng: at.Lng})
	if errors.Is(err, googlemaps.ErrNoResult) {
		return Resolved{}, outsideServiceArea()
	}
	if err != nil {
		return Resolved{}, err
	}
	pin := at
	return s.search.resolved(place, &pin)
}

// candidateIndex reads "c_<n>" with n below held.
func candidateIndex(id string, held int) (int, bool) {
	digits, ok := strings.CutPrefix(id, candidatePrefix)
	if !ok || digits == "" || len(digits) > 4 {
		return 0, false
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	if n >= held {
		return 0, false
	}
	return n, true
}

// resolved maps a provider's place into our components (ADR-0039 Karar 2):
// the country must be the service area's, the city one of the area's
// cities (else ADDRESS_OUTSIDE_SERVICE_AREA); the district is matched in
// that city and left nil when it matches none. The free lines follow
// Java's choice of components (GoogleAddressComponents): the city from
// administrative_area_level_1 or locality, the district from
// administrative_area_level_2 or sublocality_level_1, the neighborhood
// from neighborhood, administrative_area_level_4, sublocality_level_2 or
// _3, else sublocality_level_1 when it is not the district.
func (x *search) resolved(place googlemaps.Place, at *Point) (Resolved, error) {
	long, short := components(place.Components)
	if !strings.EqualFold(first(short, "country"), x.area.Country()) {
		return Resolved{}, outsideServiceArea()
	}
	cityID, ok := x.places.City(x.area.Country(), first(long, "administrative_area_level_1", "locality"))
	if !ok || !x.area.Has(cityID) {
		return Resolved{}, outsideServiceArea()
	}
	district := first(long, "administrative_area_level_2", "sublocality_level_1")
	out := Resolved{CityID: cityID, Point: at}
	if id, ok := x.places.District(cityID, district); ok {
		out.DistrictID = &id
	}
	hood := first(long, "neighborhood", "administrative_area_level_4", "sublocality_level_2", "sublocality_level_3")
	if hood == "" {
		if sub := first(long, "sublocality_level_1"); sub != "" && nameKey(sub) != nameKey(district) {
			hood = sub
		}
	}
	out.Neighborhood = clip(neighborhoodName(hood), maxNeighborhood)
	out.Thoroughfare = clip(first(long, "route"), maxThoroughfare)
	out.BuildingNumber = clip(first(long, "street_number"), maxBuildingNumber)
	out.PostalCode = postalCode(first(long, "postal_code"))
	if place.ID != "" {
		out.PlaceID = clip(place.ID, maxPlaceID)
	}
	return out, nil
}

// components indexes a place's components by type, the first of each
// type winning, in long and short form.
func components(cs []googlemaps.Component) (long, short map[string]string) {
	long, short = map[string]string{}, map[string]string{}
	for _, c := range cs {
		for _, t := range c.Types {
			if _, ok := long[t]; !ok && strings.TrimSpace(c.Long) != "" {
				long[t] = c.Long
			}
			if _, ok := short[t]; !ok && strings.TrimSpace(c.Short) != "" {
				short[t] = c.Short
			}
		}
	}
	return long, short
}

func first(m map[string]string, types ...string) string {
	for _, t := range types {
		if v := strings.TrimSpace(m[t]); v != "" {
			return v
		}
	}
	return ""
}

// restriction is the autocomplete's location restriction: the union of
// the service area cities' bounds, looked up from the provider by the
// cities' reference names and kept a day (ADR-0039 Karar 3). Each lookup
// is a billed unit on the service's cap. While a city's bounds cannot be
// had the restriction is nil (Türkiye only) and the lookup is tried again
// after boundsRetry; the resolve's city check stays the authority either
// way.
func (x *search) restriction(ctx context.Context, now time.Time) *googlemaps.Rect {
	x.mu.Lock()
	defer x.mu.Unlock()
	if now.Before(x.boundsUntil) {
		return x.bounds
	}
	var union *googlemaps.Rect
	for _, id := range x.area.Cities() {
		name, country, _ := x.places.CityName(id)
		r, err := x.cityBounds(ctx, titleTR(name), country, now)
		if err != nil {
			x.bounds, x.boundsUntil = nil, now.Add(boundsRetry)
			return nil
		}
		if union == nil {
			union = &r
		} else {
			u := union.Union(r)
			union = &u
		}
	}
	x.bounds, x.boundsUntil = union, now.Add(boundsTTL)
	return union
}

// cityBounds reserves a bounds unit and looks the city up.
func (x *search) cityBounds(ctx context.Context, name, country string, now time.Time) (googlemaps.Rect, error) {
	var paid spent
	err := x.s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		sp, err := x.reserve(ctx, addressdb.New(tx), nil, UnitBounds, now)
		paid = sp
		return err
	})
	if err != nil {
		return googlemaps.Rect{}, fmt.Errorf("address: reserve the bounds lookup: %w", err)
	}
	r, err := x.s.metered().regionBounds(ctx, paid, name, country)
	if err != nil {
		return googlemaps.Rect{}, fmt.Errorf("address: look the bounds up: %w", err)
	}
	return r, nil
}

// Purge hard-deletes the expired sessions and the ledger rows past the
// longest window (M2, ADR-0039).
func (s *Service) Purge(ctx context.Context) (sessions, lookups int64, err error) {
	now := s.clock.Now()
	q := addressdb.New(s.db.Pool())
	sessions, err = q.PurgeSessions(ctx, now)
	if err != nil {
		return 0, 0, fmt.Errorf("address: purge the sessions: %w", err)
	}
	lookups, err = q.PurgeLookups(ctx, now.Add(-max(UserWindow, ServiceWindow, ClientWindow)))
	if err != nil {
		return sessions, 0, fmt.Errorf("address: purge the lookups: %w", err)
	}
	return sessions, lookups, nil
}
