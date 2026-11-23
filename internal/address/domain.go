// Package address serves the caller's addresses and the address search
// against the map provider (ADR-0029 Karar 8, ADR-0039): /v1/me/addresses,
// /v1/me/address-search-sessions and /v1/me/address-lookups/reverse, and
// the profile's primary address link (users.address_id, ADR-0036 Karar 5).
//
// What is kept of the provider is the place id and our own components
// (MED-008): the city and the district are our reference rows, matched
// from the provider's names here, never stored as the provider wrote them;
// the free lines are what the caller confirms. The provider's formatted
// address and raw answer have no column and no field to land in. The
// displayed line is built from our components (DisplayLine).
//
// Coordinates are stored at full precision; the projection that renders
// them decides what a caller sees (MED-009, Point.Render). Every provider
// call is bought with a unit of the lookup budget, reserved before the call
// in one place (reserve): the caller's daily budget, then the client
// address's hourly budget, then the service's daily cap (ADR-0039 Karar 7).
package address

import (
	"bytes"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// Error codes. Each is in api/components/problem.yaml's ErrorCode.
const (
	// CodeAddressNotFound: the caller has no such address; another user's
	// looks the same.
	CodeAddressNotFound apperr.Code = "ADDRESS_NOT_FOUND"
	// CodeSessionNotFound: no such search session, expired, ended by a
	// resolve or another user's; all look the same (MED-010).
	CodeSessionNotFound apperr.Code = "ADDRESS_SEARCH_SESSION_NOT_FOUND"
	// CodeProviderUnavailable: the provider did not answer usefully or is
	// not configured.
	CodeProviderUnavailable apperr.Code = "ADDRESS_PROVIDER_UNAVAILABLE"
	// CodeReferenceNotFound: no such city, or no such district of the city.
	CodeReferenceNotFound apperr.Code = "REFERENCE_ITEM_NOT_FOUND"
	// CodeOutsideServiceArea: the point or the chosen suggestion is not in
	// a city of the service area, or matches no address (400, a domain
	// rule's refusal like EDUCATION_PERIOD_INVALID).
	CodeOutsideServiceArea apperr.Code = "ADDRESS_OUTSIDE_SERVICE_AREA"
	// CodeAddressLimitReached: the caller has AddressesPerUser addresses.
	CodeAddressLimitReached apperr.Code = "ADDRESS_LIMIT_REACHED"
	// CodeLookupLimitReached: the caller's daily budget, the client
	// address's hourly budget or the service's daily cap is spent, or the
	// session made SuggestionRequestsPerSession requests. One code for all
	// four: the client's remedy is the same (wait, or enter the address by
	// hand) and the answer does not tell which bound a caller hit.
	CodeLookupLimitReached apperr.Code = "ADDRESS_LOOKUP_LIMIT_REACHED"
)

// Codes lists every code this package defines, for the contract test.
func Codes() []apperr.Code {
	return []apperr.Code{
		CodeAddressNotFound, CodeSessionNotFound, CodeProviderUnavailable, CodeReferenceNotFound,
		CodeOutsideServiceArea, CodeAddressLimitReached, CodeLookupLimitReached,
	}
}

// AddressesPerUser is how many addresses a user may have (ADR-0039 Karar
// 4): a home, a work place and a few more; the cap bounds what one account
// can write and what the list loads.
const AddressesPerUser = 10

// The search session (ADR-0029 Karar 8, ADR-0039 Karar 6).
const (
	// SessionTTL is how long a session lives.
	SessionTTL = 15 * time.Minute
	// SuggestionRequestsPerSession bounds the autocomplete requests of one
	// session. The provider bills the first 12 autocomplete requests of a
	// session that ends in an Essentials place details call one by one,
	// and every request of an abandoned session; each is a unit of the
	// budget (UnitAutocomplete). Twelve is the session's billed part, and a
	// client that debounces needs about six to twelve per address typed.
	SuggestionRequestsPerSession = 12
	// MaxSuggestions is the contract's page (AddressSuggestionList).
	MaxSuggestions = 10
	// MinQueryRunes is the shortest query that reaches the provider, after
	// trimming; a shorter one is answered with no suggestions, free.
	MinQueryRunes = 3
)

// Unit is a unit of the lookup budget (ADR-0039 Karar 7): each provider
// call the provider bills, and the session's opening, which bills nothing
// but bounds how many sessions a caller churns through. With the
// Essentials field mask the provider bills every autocomplete request of
// a session (up to 12 when it ends in place details, all of them when it
// is abandoned), so each autocomplete request is a unit; a typical address
// entry is 1 session + 6–12 autocomplete + 1 details = 8–14 units.
type Unit string

// Units (address_lookups_unit_check).
const (
	UnitSession      Unit = "SESSION"
	UnitAutocomplete Unit = "AUTOCOMPLETE"
	UnitDetails      Unit = "DETAILS"
	UnitReverse      Unit = "REVERSE"
	// UnitBounds is the service's own lookup of a service area city's
	// bounds; it has no caller and counts only on the service's cap.
	UnitBounds Unit = "BOUNDS"
)

// The lookup budget's defaults and windows (ADR-0039 Karar 7), in units
// of a typical entry's 14 at most: a user ten entries a day (150), a client
// address — a carrier NAT shares one among many users — about forty an
// hour (600), the service about two hundred a day (3000). The values are
// configurable (config.Maps); they were not measured.
const (
	DefaultLookupsPerUser   = 150
	DefaultLookupsPerClient = 600
	DefaultLookupsPerDay    = 3000
	UserWindow              = 24 * time.Hour
	ClientWindow            = time.Hour
	ServiceWindow           = 24 * time.Hour
	// RetryFloor is the shortest Retry-After a refusal of the budget
	// answers; a refusal of the session's request bound answers it too,
	// since a new session may start at once.
	RetryFloor = time.Second
)

// Budget is the lookup budget's three bounds.
type Budget struct {
	PerUser   int
	PerClient int
	PerDay    int
}

// withDefaults fills the unset bounds.
func (b Budget) withDefaults() Budget {
	if b.PerUser <= 0 {
		b.PerUser = DefaultLookupsPerUser
	}
	if b.PerClient <= 0 {
		b.PerClient = DefaultLookupsPerClient
	}
	if b.PerDay <= 0 {
		b.PerDay = DefaultLookupsPerDay
	}
	return b
}

// Precision is how exactly a rendered coordinate shows (MED-009).
type Precision string

// Precisions (CoordinatesPrecision).
const (
	PrecisionExact       Precision = "EXACT"
	PrecisionApproximate Precision = "APPROXIMATE"
)

// approximateDecimals is the decimals an approximate coordinate keeps: a
// grid of 0.01°, about 1.1 km north to south and 0.85 km east to west in
// Türkiye. A grid, not a random offset: the same address always lands on
// the same cell, so averaging many answers learns nothing more.
const approximateDecimals = 2

// Point is a coordinate as stored: full precision.
type Point struct {
	Lat, Lng float64
}

// Rendered is a coordinate as one caller sees it.
type Rendered struct {
	Lat, Lng  float64
	Precision Precision
}

// Render is the one place a stored coordinate is rounded for a caller
// (MED-009, [[job-location-rule]]): exact for the owner and for a company
// address shown to the one allowed to see it, approximate for an
// individual's job and on the map. The stored value is never rounded.
func (p Point) Render(precision Precision) Rendered {
	if precision != PrecisionApproximate {
		return Rendered{Lat: p.Lat, Lng: p.Lng, Precision: PrecisionExact}
	}
	scale := math.Pow10(approximateDecimals)
	return Rendered{
		Lat: math.Round(p.Lat*scale) / scale, Lng: math.Round(p.Lng*scale) / scale, Precision: PrecisionApproximate,
	}
}

// Reference is a reference row an address names.
type Reference struct {
	ID   uuid.UUID
	Name string
}

// Lines are an address's free lines; nil is empty.
type Lines struct {
	Neighborhood         *string
	Thoroughfare         *string
	BuildingNumber       *string
	BuildingName         *string
	SiteName             *string
	Block                *string
	Floor                *string
	UnitNumber           *string
	PostalCode           *string
	AdditionalDirections *string
}

// Source is where an address's components and coordinate came from
// (AddressSource, ADR-0039 Karar 10).
type Source string

// Sources (addresses_components_source_check).
const (
	// SourceManual: the caller's own entry; the coordinate is their pin.
	SourceManual Source = "MANUAL"
	// SourceAutocomplete: a resolved suggestion; the components and the
	// coordinate are the provider's.
	SourceAutocomplete Source = "AUTOCOMPLETE"
	// SourceReverse: a reverse geocode; the components are the provider's,
	// the coordinate the caller's pin.
	SourceReverse Source = "REVERSE_GEOCODE"
)

// The sources of a coordinate (addresses_coordinate_source_check).
const (
	coordinateUserPin  = "USER_PIN"
	coordinateProvider = "PROVIDER"
)

// coordinateSource is where a coordinate of the source came from: the
// provider's only for a resolved suggestion. A pin the caller moved after
// a resolve stays PROVIDER: the column is kept for a cleanup, and it errs
// on the side that removes too much rather than keeps too much.
func (s Source) coordinateSource() string {
	if s == SourceAutocomplete {
		return coordinateProvider
	}
	return coordinateUserPin
}

// sourceOf is the source an input declares, or the conservative default:
// an address with a provider's place id came from a suggestion.
func sourceOf(declared *Source, placeID *string) Source {
	if declared != nil {
		return *declared
	}
	if placeID != nil {
		return SourceAutocomplete
	}
	return SourceManual
}

// Address is one of the caller's addresses as stored.
type Address struct {
	ID       uuid.UUID
	City     Reference
	District Reference
	Lines
	ProviderPlaceID *string
	// Point is nil when the address has no coordinate.
	Point *Point
	// ComponentsSource and CoordinateSource are the stored sources
	// (ADR-0039 Karar 10); CoordinateSource is empty without a coordinate.
	ComponentsSource Source
	CoordinateSource string
	CreatedAt        time.Time
}

// DisplayLine is the address's shown line, built from our components only
// (MED-008): "Moda Caddesi 12/7, Moda Konutları B Blok, Caferağa, Kadıköy,
// İstanbul". The street part is the thoroughfare with the building number
// and, after it, the unit; then the site and its block; the reference
// names are shown in Turkish title case.
func (a Address) DisplayLine() string {
	var parts []string
	street := deref(a.Thoroughfare)
	if n := deref(a.BuildingNumber); n != "" {
		street = strings.TrimSpace(street + " " + n)
		if u := deref(a.UnitNumber); u != "" {
			street += "/" + u
		}
	}
	site := deref(a.SiteName)
	if b := deref(a.Block); b != "" {
		if !strings.Contains(strings.ToLowerSpecial(unicode.TurkishCase, b), "blok") {
			b += " Blok"
		}
		site = strings.TrimSpace(site + " " + b)
	}
	for _, p := range []string{street, site, deref(a.Neighborhood), titleTR(a.District.Name), titleTR(a.City.Name)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// titleTR is a reference name ("KADIKÖY", "AFYONKARAHİSAR") in Turkish
// title case ("Kadıköy", "Afyonkarahisar"): I and İ lower to ı and i.
func titleTR(s string) string {
	words := strings.Fields(strings.ToLowerSpecial(unicode.TurkishCase, s))
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.TurkishCase.ToUpper(r)) + w[n:]
	}
	return strings.Join(words, " ")
}

// Input is the whole of an address as the caller sends it.
type Input struct {
	CityID     uuid.UUID
	DistrictID uuid.UUID
	Lines
	ProviderPlaceID *string
	// Source is the source the caller declares, nil when not sent
	// (sourceOf gives the default).
	Source *Source
	Point  *Point
}

// normalized is in with every free line trimmed and an empty one cleared.
func (in Input) normalized() Input {
	out := in
	for _, f := range []**string{
		&out.Neighborhood, &out.Thoroughfare, &out.BuildingNumber, &out.BuildingName, &out.SiteName, &out.Block,
		&out.Floor, &out.UnitNumber, &out.PostalCode, &out.AdditionalDirections, &out.ProviderPlaceID,
	} {
		*f = trimmed(*f)
	}
	return out
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

// precedes reports whether the address a comes before b in the list:
// newest first, by the UUIDv7 ids as PostgreSQL orders uuid
// (db/queries/address ListAddresses).
func precedes(a, b uuid.UUID) bool { return bytes.Compare(a[:], b[:]) > 0 }

// Resolved is a provider's place in our components (ResolvedAddress).
// District is nil when the provider's district matches none of the city's
// (ADR-0039 Karar 2): the caller picks it from the list.
type Resolved struct {
	CityID     uuid.UUID
	DistrictID *uuid.UUID
	// The free lines a provider gives: neighborhood, thoroughfare,
	// building number, site name (its premise), postal code. The rest
	// stays empty.
	Neighborhood   *string
	Thoroughfare   *string
	BuildingNumber *string
	SiteName       *string
	PostalCode     *string
	PlaceID        *string
	// Point is the provider's location for a resolve, the caller's own pin
	// for a reverse geocode.
	Point  *Point
	Source Source
}

// The contract's bounds of the free lines a resolve fills (AddressInput):
// a resolved value is cut to them, so it can be posted as it is.
const (
	maxNeighborhood   = 120
	maxThoroughfare   = 160
	maxBuildingNumber = 30
	maxSiteName       = 120
	maxPlaceID        = 255
)

// clip is s cut to n runes; nil when empty.
func clip(s string, n int) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return &s
}

// postalCode is a Turkish postal code (five digits) or nil.
func postalCode(s string) *string {
	s = strings.TrimSpace(s)
	if len(s) != 5 {
		return nil
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return nil
		}
	}
	return &s
}

// neighborhoodSuffixes are the endings the provider adds to a mahalle's
// name; ours is the bare name ("Caferağa", AddressInput's example).
func neighborhoodSuffixes() []string {
	return []string{" Mahallesi", " Mah.", " Mh.", " Mah", " Mh"}
}

// neighborhoodName is the provider's mahalle without its suffix, compared
// rune by rune in Turkish lower case.
func neighborhoodName(s string) string {
	rs := []rune(strings.TrimSpace(s))
	for _, suf := range neighborhoodSuffixes() {
		sr := []rune(suf)
		if len(rs) <= len(sr) {
			continue
		}
		tail := strings.ToLowerSpecial(unicode.TurkishCase, string(rs[len(rs)-len(sr):]))
		if tail == strings.ToLowerSpecial(unicode.TurkishCase, suf) {
			return strings.TrimSpace(string(rs[:len(rs)-len(sr)]))
		}
	}
	return string(rs)
}
