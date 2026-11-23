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

// Provider is the map provider (internal/integrations/googlemaps: the
// Google client or the canned driver). It is called only through metered,
// whose every method takes the spent unit of its own call.
type Provider interface {
	Autocomplete(ctx context.Context, input, token string, restriction *googlemaps.Rect) ([]googlemaps.Prediction, error)
	Details(ctx context.Context, placeID, token string) (googlemaps.Place, error)
	Reverse(ctx context.Context, at googlemaps.LatLng) (googlemaps.Place, error)
	RegionBounds(ctx context.Context, name, countryCode string) (googlemaps.Rect, error)
}

// The unit tags, one per kind of unit: spent[T] is a unit of T's kind.
type (
	sessionTag      struct{}
	autocompleteTag struct{}
	detailsTag      struct{}
	reverseTag      struct{}
	boundsTag       struct{}
)

// unitTag is the set of the unit tags.
type unitTag interface {
	sessionTag | autocompleteTag | detailsTag | reverseTag | boundsTag
}

// unitOf is the unit of tag T.
func unitOf[T unitTag]() Unit {
	var tag T
	switch any(tag).(type) {
	case sessionTag:
		return UnitSession
	case autocompleteTag:
		return UnitAutocomplete
	case detailsTag:
		return UnitDetails
	case reverseTag:
		return UnitReverse
	default:
		return UnitBounds
	}
}

// spent is a unit of kind T reserved before a provider call (ADR-0039
// Karar 7). Only reserve makes one, and each metered call takes the spent
// unit of its own kind: a details call cannot ride on a session's unit nor
// an autocomplete request on another's. The compiler holds the kinds
// apart; that no code of this package builds a spent value by hand is
// held by review (TestOnlyReserveSpends reads the source for it).
type spent[T unitTag] struct{}

// lookupLockClass is the first key of the lookup budget's advisory lock
// (LockLookups): "KA".
const lookupLockClass = 0x4b41

// The service area's bounds in memory (ADR-0039 Karar 3).
const (
	// boundsTTL is how long a city's bounds are kept: a day, far inside
	// the 30 days the provider's terms allow for coordinates.
	boundsTTL = 24 * time.Hour
	// boundsRetry is the first wait after a failed lookup; it doubles with
	// every failure in a row, up to boundsRetryMax.
	boundsRetry    = 10 * time.Minute
	boundsRetryMax = 24 * time.Hour
	// boundsTimeout bounds one refresh, whatever the request that started
	// it.
	boundsTimeout = 20 * time.Second
)

// cityBox is one service area city's bounds as the cache holds them.
type cityBox struct {
	rect     *googlemaps.Rect
	until    time.Time // rect is valid before it
	retryAt  time.Time // after a failure, not tried before it
	failures int
}

// search is the address search's state beyond the database: the service
// area, its cities' bounds in memory and the budget.
type search struct {
	s      *Service
	places *Places
	area   ServiceArea
	budget Budget

	mu    sync.Mutex
	boxes map[uuid.UUID]*cityBox
	// flight is open while one refresh of the bounds runs: the others
	// wait for it instead of starting their own.
	flight chan struct{}
}

func newSearch(s *Service, cfg Config) *search {
	b := cfg.Budget.withDefaults()
	s.metrics.capped.Set(float64(b.PerDay))
	return &search{s: s, places: cfg.Places, area: cfg.Area, budget: b, boxes: map[uuid.UUID]*cityBox{}}
}

func providerUnavailable() error {
	return apperr.New(apperr.KindUpstream, CodeProviderUnavailable)
}

func sessionNotFound() error {
	return apperr.New(apperr.KindNotFound, CodeSessionNotFound)
}

// outsideServiceArea is a domain rule's refusal of a well-formed request:
// 400, like EDUCATION_PERIOD_INVALID (style guide, ADR-0034).
func outsideServiceArea() error {
	return apperr.New(apperr.KindInvalid, CodeOutsideServiceArea)
}

// lookupLimited is ADDRESS_LOOKUP_LIMIT_REACHED with the wait until the
// window frees a unit, never below RetryFloor.
func lookupLimited(wait time.Duration) error {
	return &limitError{err: apperr.New(apperr.KindRateLimited, CodeLookupLimitReached), wait: max(wait, RetryFloor)}
}

// limitError carries the Retry-After of a spent budget.
type limitError struct {
	err  error
	wait time.Duration
}

func (e *limitError) Error() string { return e.err.Error() }

func (e *limitError) Unwrap() error { return e.err }

// RetryAfter is how long the caller of a refused lookup waits.
func RetryAfter(err error) (time.Duration, bool) {
	var l *limitError
	if errors.As(err, &l) {
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

// reserve spends one unit of kind T in q's transaction (ADR-0039 Karar
// 7), under the budget's lock, in the order the buckets of ADR-0033 Karar
// 4 take: the caller's daily budget first, so a caller over it spends
// nothing of the client address's budget, which a carrier NAT shares;
// then the client address's hourly budget; then the service's daily cap,
// which every unit counts the same, whatever the call (lesson of #21). A
// refusal writes nothing. The service's own bounds lookup has no caller
// and counts only on the cap.
//
// Every refusal is 429 ADDRESS_LOOKUP_LIMIT_REACHED with the wait until
// the window that refused frees a unit: the cap is the provider's bill,
// not the provider's failure, so it is no 5xx.
//
//nolint:unparam // spent's value is always the zero value: its type, the unit's kind, is what a provider call checks
func reserve[T unitTag](ctx context.Context, x *search, q *addressdb.Queries, c *caller, now time.Time) (spent[T], error) {
	unit := unitOf[T]()
	if err := q.LockLookups(ctx, lookupLockClass); err != nil {
		return spent[T]{}, fmt.Errorf("address: lock the lookup budget: %w", err)
	}
	if c != nil {
		since := now.Add(-UserWindow)
		n, err := q.CountUserLookups(ctx, addressdb.CountUserLookupsParams{UserID: c.user, Since: since})
		if err != nil {
			return spent[T]{}, fmt.Errorf("address: count the user's lookups: %w", err)
		}
		if n >= int64(x.budget.PerUser) {
			x.s.metrics.reserved(unit, outcomeUserLimited)
			oldest, err := q.OldestUserLookup(ctx, addressdb.OldestUserLookupParams{UserID: c.user, Since: since})
			if err != nil {
				return spent[T]{}, fmt.Errorf("address: read the user's oldest lookup: %w", err)
			}
			return spent[T]{}, lookupLimited(oldest.Add(UserWindow).Sub(now))
		}
		since = now.Add(-ClientWindow)
		n, err = q.CountClientLookups(ctx, addressdb.CountClientLookupsParams{ClientKey: c.client, Since: since})
		if err != nil {
			return spent[T]{}, fmt.Errorf("address: count the client's lookups: %w", err)
		}
		if n >= int64(x.budget.PerClient) {
			x.s.metrics.reserved(unit, outcomeClientLimited)
			oldest, err := q.OldestClientLookup(ctx, addressdb.OldestClientLookupParams{ClientKey: c.client, Since: since})
			if err != nil {
				return spent[T]{}, fmt.Errorf("address: read the client's oldest lookup: %w", err)
			}
			return spent[T]{}, lookupLimited(oldest.Add(ClientWindow).Sub(now))
		}
	}
	since := now.Add(-ServiceWindow)
	n, err := q.CountAllLookups(ctx, since)
	if err != nil {
		return spent[T]{}, fmt.Errorf("address: count the lookups: %w", err)
	}
	if n >= int64(x.budget.PerDay) {
		x.s.metrics.reserved(unit, outcomeCapped)
		oldest, err := q.OldestLookup(ctx, since)
		if err != nil {
			return spent[T]{}, fmt.Errorf("address: read the oldest lookup: %w", err)
		}
		return spent[T]{}, lookupLimited(oldest.Add(ServiceWindow).Sub(now))
	}
	row := addressdb.InsertLookupParams{ID: uuid.Must(uuid.NewV7()), Unit: string(unit), CreatedAt: now}
	if c != nil {
		client := c.client
		row.UserID = uuid.NullUUID{UUID: c.user, Valid: true}
		row.ClientKey = &client
	}
	if err := q.InsertLookup(ctx, row); err != nil {
		return spent[T]{}, fmt.Errorf("address: spend the lookup: %w", err)
	}
	x.s.metrics.reserved(unit, outcomeReserved)
	return spent[T]{}, nil
}

// metered is the provider behind the budget: each call takes the spent
// unit of its own kind, and counts its outcome. Without a provider (no
// key in local and test) every call is ADDRESS_PROVIDER_UNAVAILABLE.
type metered struct {
	p       Provider
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

func (m metered) autocomplete(ctx context.Context, _ spent[autocompleteTag], input, token string, r *googlemaps.Rect,
) ([]googlemaps.Prediction, error) {
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

func (m metered) details(ctx context.Context, _ spent[detailsTag], placeID, token string) (googlemaps.Place, error) {
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

func (m metered) reverse(ctx context.Context, _ spent[reverseTag], at googlemaps.LatLng) (googlemaps.Place, error) {
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

func (m metered) regionBounds(ctx context.Context, _ spent[boundsTag], name, country string) (googlemaps.Rect, error) {
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
// one unit of the budget, reserved with the row. The provider's session
// token is a random UUIDv4, tied to nothing of the caller (ADR-0039 Karar
// 9). Without a provider the start is refused at once, before it spends
// anything.
func (s *Service) StartSession(ctx context.Context, p authn.UserPrincipal, client string) (Session, error) {
	if s.provider == nil {
		return Session{}, s.metered().unwired(callAutocomplete)
	}
	now := s.clock.Now()
	out := Session{ID: uuid.Must(uuid.NewV7()), ExpiresAt: now.Add(SessionTTL)}
	c := callerOf(p, client)
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		if _, err := reserve[sessionTag](ctx, s.search, q, &c, now); err != nil {
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
// (MED-010). It runs before any budget is counted, so a refusal for the
// budget never tells a session apart from a missing one.
func openSession(ctx context.Context, q *addressdb.Queries, userID, id uuid.UUID, now time.Time) (addressdb.LockSessionRow, error) {
	row, err := q.LockSession(ctx, addressdb.LockSessionParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return addressdb.LockSessionRow{}, sessionNotFound()
	}
	if err != nil {
		return addressdb.LockSessionRow{}, fmt.Errorf("address: lock the session: %w", err)
	}
	if !now.Before(row.ExpiresAt) || row.ResolvedAt != nil {
		return addressdb.LockSessionRow{}, sessionNotFound()
	}
	return row, nil
}

// Suggest returns the provider's suggestions for query in the caller's
// session. A query shorter than MinQueryRunes once trimmed reaches no one,
// spends nothing and gets none. Every request that reaches the provider is
// a unit of the budget (the provider bills it), and a session makes at
// most SuggestionRequestsPerSession of them; past that the caller opens a
// new session (429 with RetryFloor). The place ids are kept in the
// session, the candidate is their index; the display lines are not kept.
func (s *Service) Suggest(ctx context.Context, p authn.UserPrincipal, sessionID uuid.UUID, query, client string) ([]Suggestion, error) {
	query = strings.TrimSpace(query)
	now := s.clock.Now()
	c := callerOf(p, client)
	var token uuid.UUID
	var paid *spent[autocompleteTag]
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		row, err := openSession(ctx, q, p.UserID, sessionID, now)
		if err != nil {
			return err
		}
		if utf8.RuneCountInString(query) < MinQueryRunes {
			return nil
		}
		if row.SuggestionRequests >= SuggestionRequestsPerSession {
			return lookupLimited(RetryFloor)
		}
		sp, err := reserve[autocompleteTag](ctx, s.search, q, &c, now)
		if err != nil {
			return err
		}
		if err := q.CountSuggestionRequest(ctx, sessionID); err != nil {
			return fmt.Errorf("address: count the request: %w", err)
		}
		token, paid = row.ProviderToken, &sp
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("address: suggest: %w", err)
	}
	if paid == nil {
		return []Suggestion{}, nil
	}
	predictions, err := s.metered().autocomplete(ctx, *paid, query, token.String(), s.search.restriction(ctx))
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
// place details call; the details call is one unit. Should the provider
// call fail, the end is taken back so the caller may try again (the unit
// stays spent). A candidate the session never offered is the session's
// 404: a candidate id means nothing outside its session.
func (s *Service) Resolve(ctx context.Context, p authn.UserPrincipal, sessionID uuid.UUID, candidateID, client string) (Resolved, error) {
	now := s.clock.Now()
	c := callerOf(p, client)
	var placeID string
	var token uuid.UUID
	var paid spent[detailsTag]
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		row, err := openSession(ctx, q, p.UserID, sessionID, now)
		if err != nil {
			return err
		}
		i, ok := candidateIndex(candidateID, len(row.Candidates))
		if !ok {
			return sessionNotFound()
		}
		sp, err := reserve[detailsTag](ctx, s.search, q, &c, now)
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
	return s.search.resolved(place, at, SourceAutocomplete)
}

// Reverse turns the caller's map pin into our components: one unit, and
// only the coordinate goes to the provider (ADR-0039 Karar 9). A point
// outside the service area's known bounds goes nowhere and spends nothing
// (Java's IstanbulServiceArea.contains before the call). The point
// answered is the caller's pin, not the provider's geometry: the pin is
// the caller's own datum. A point no address matches, or one outside the
// service area's cities, is ADDRESS_OUTSIDE_SERVICE_AREA.
func (s *Service) Reverse(ctx context.Context, p authn.UserPrincipal, at Point, client string) (Resolved, error) {
	if s.provider == nil {
		return Resolved{}, s.metered().unwired(callReverse)
	}
	pin := googlemaps.LatLng{Lat: at.Lat, Lng: at.Lng}
	if box := s.search.restriction(ctx); box != nil && !box.Contains(pin) {
		return Resolved{}, outsideServiceArea()
	}
	now := s.clock.Now()
	c := callerOf(p, client)
	var paid spent[reverseTag]
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		sp, err := reserve[reverseTag](ctx, s.search, addressdb.New(tx), &c, now)
		paid = sp
		return err
	})
	if err != nil {
		return Resolved{}, fmt.Errorf("address: reverse: %w", err)
	}
	place, err := s.metered().reverse(ctx, paid, pin)
	if errors.Is(err, googlemaps.ErrNoResult) {
		return Resolved{}, outsideServiceArea()
	}
	if err != nil {
		return Resolved{}, err
	}
	own := at
	return s.search.resolved(place, &own, SourceReverse)
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
// _3, else sublocality_level_1 when it is not the district; the site name
// from premise.
func (x *search) resolved(place googlemaps.Place, at *Point, source Source) (Resolved, error) {
	long, short := components(place.Components)
	if !strings.EqualFold(first(short, "country"), x.area.Country()) {
		return Resolved{}, outsideServiceArea()
	}
	cityID, ok := x.places.City(x.area.Country(), first(long, "administrative_area_level_1", "locality"))
	if !ok || !x.area.Has(cityID) {
		return Resolved{}, outsideServiceArea()
	}
	district := first(long, "administrative_area_level_2", "sublocality_level_1")
	out := Resolved{CityID: cityID, Point: at, Source: source}
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
	out.SiteName = clip(first(long, "premise"), maxSiteName)
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

// restriction is the service area's rectangle: the union of its cities'
// bounds, looked up from the provider by the cities' reference names and
// kept per city for a day (ADR-0039 Karar 3). It is nil (Türkiye only)
// while any city's bounds cannot be had; the resolve's city check stays
// the authority either way.
//
// A refresh is single-flight: the first caller that finds a city due runs
// it, on a context of its own that the request's cancellation does not
// end, and the others wait for it (or for their own deadline). No lock is
// held while the provider is called. Each lookup is a unit of the
// service's cap; a city whose lookup fails is not tried again before its
// backoff (boundsRetry, doubling up to a day), so a city the provider
// never answers spends one unit per backoff, not one per request, and the
// other cities keep their bounds.
func (x *search) restriction(ctx context.Context) *googlemaps.Rect {
	x.mu.Lock()
	now := x.s.clock.Now()
	due := x.due(now)
	if len(due) == 0 {
		defer x.mu.Unlock()
		return x.union(now)
	}
	ch, leader := x.flight, x.flight == nil
	if leader {
		ch = make(chan struct{})
		x.flight = ch
	}
	x.mu.Unlock()
	if leader {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boundsTimeout)
		x.refresh(refreshCtx, due)
		cancel()
		x.mu.Lock()
		x.flight = nil
		close(ch)
		x.mu.Unlock()
	} else {
		select {
		case <-ch:
		case <-ctx.Done():
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.union(x.s.clock.Now())
}

// due lists the cities whose bounds are missing or stale and not in a
// backoff; the caller holds x.mu.
func (x *search) due(now time.Time) []uuid.UUID {
	var out []uuid.UUID
	for _, id := range x.area.Cities() {
		b := x.boxes[id]
		if b == nil || ((b.rect == nil || !now.Before(b.until)) && !now.Before(b.retryAt)) {
			out = append(out, id)
		}
	}
	return out
}

// union is the union of the cities' valid bounds, nil when one has none;
// the caller holds x.mu.
func (x *search) union(now time.Time) *googlemaps.Rect {
	var out *googlemaps.Rect
	for _, id := range x.area.Cities() {
		b := x.boxes[id]
		if b == nil || b.rect == nil || !now.Before(b.until) {
			return nil
		}
		r := *b.rect
		if out != nil {
			r = out.Union(r)
		}
		out = &r
	}
	return out
}

// refresh looks the due cities' bounds up, one unit each, and records
// each outcome under x.mu.
func (x *search) refresh(ctx context.Context, due []uuid.UUID) {
	for _, id := range due {
		name, country, _ := x.places.CityName(id)
		r, err := x.cityBounds(ctx, titleTR(name), country)
		now := x.s.clock.Now()
		x.mu.Lock()
		b := x.boxes[id]
		if b == nil {
			b = &cityBox{}
			x.boxes[id] = b
		}
		if err != nil {
			b.failures++
			b.retryAt = now.Add(backoff(b.failures))
		} else {
			b.rect, b.until, b.failures, b.retryAt = &r, now.Add(boundsTTL), 0, time.Time{}
		}
		x.mu.Unlock()
	}
}

// backoff is the wait after the n-th failure in a row.
func backoff(n int) time.Duration {
	d := boundsRetry
	for i := 1; i < n && d < boundsRetryMax; i++ {
		d *= 2
	}
	return min(d, boundsRetryMax)
}

// cityBounds reserves a bounds unit and looks the city up.
func (x *search) cityBounds(ctx context.Context, name, country string) (googlemaps.Rect, error) {
	var paid spent[boundsTag]
	err := x.s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		sp, err := reserve[boundsTag](ctx, x, addressdb.New(tx), nil, x.s.clock.Now())
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
