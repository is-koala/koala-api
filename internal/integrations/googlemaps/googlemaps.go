// Package googlemaps calls the Google Maps Platform web services the
// address search needs (ADR-0039): Places API (New) Autocomplete and Place
// Details, billed per session with a session token, and the Geocoding API
// for a reverse geocode and for the bounds of a service area's city. Plain
// net/http, no SDK.
//
// What goes to Google is the least the call needs (ADR-0039 Karar 9): the
// text typed, a random session token tied to nothing of the user, a place
// id, or a coordinate. No user id, no client address, no header of the
// caller's request.
//
// What comes back is read through field masks and Go types that only hold
// what the address module keeps: a prediction's place id and its two
// display lines, a place's id, address components and location, a
// region's bounds. The formatted address and the rest of the answer are
// never decoded into anything the caller sees (MED-008).
//
// The API key is a secret: Places takes it in the X-Goog-Api-Key header;
// the Geocoding API documents only the key query parameter, so no error
// this package returns carries a request URL, and a transport error is
// reduced to its cause. Redirects are refused: one would carry the key to
// wherever it points.
package googlemaps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The services' origins.
const (
	DefaultPlacesURL    = "https://places.googleapis.com"
	DefaultGeocodingURL = "https://maps.googleapis.com"
)

// The endpoints, relative to their origin.
const (
	AutocompletePath = "/v1/places:autocomplete"
	// DetailsPathPrefix is followed by the place id.
	DetailsPathPrefix = "/v1/places/"
	GeocodePath       = "/maps/api/geocode/json"
)

// The field masks (X-Goog-FieldMask). A mask bounds both what Google bills
// (Place Details' Essentials SKU: id, addressComponents, location) and
// what it sends: the formatted address is never requested.
const (
	AutocompleteFieldMask = "suggestions.placePrediction.placeId," +
		"suggestions.placePrediction.structuredFormat.mainText.text," +
		"suggestions.placePrediction.structuredFormat.secondaryText.text"
	DetailsFieldMask = "id,addressComponents,location"
)

// The language and region of every call: Turkish names, Turkish results.
const (
	language = "tr"
	region   = "tr"
)

// timeout bounds one call. A request's own deadline (httpx.RequestTimeout)
// is shorter than the client's only when the request is nearly done.
const timeout = 8 * time.Second

// maxAnswer bounds the answer read from Google; an autocomplete or a
// details answer is a few kilobytes.
const maxAnswer = 256 << 10

// ErrNoResult is a successful call that found nothing: a geocode of a
// point with no address (ZERO_RESULTS).
var ErrNoResult = errors.New("googlemaps: no result")

// ErrUnavailable is every other failure: the provider refused the key or
// the quota, answered with an error or nothing readable, or did not answer
// in time. *Error carries why.
var ErrUnavailable = errors.New("googlemaps: provider unavailable")

// Reasons of an *Error, a finite set for the metric labels.
const (
	ReasonQuota       = "quota"
	ReasonDenied      = "denied"
	ReasonInvalid     = "invalid"
	ReasonUpstream    = "upstream"
	ReasonTransport   = "transport"
	ReasonUnreadable  = "unreadable"
	ReasonUnavailable = "unavailable"
)

// Error is a failed call: ErrUnavailable with the reason and the HTTP
// status (0 when none came). It carries no URL, no key and no free text of
// Google's.
type Error struct {
	Reason string
	Status int
	cause  error
}

// Error is the reason and the status, and a transport failure's cause.
func (e *Error) Error() string {
	msg := "googlemaps: " + e.Reason
	if e.Status != 0 {
		msg += " (HTTP " + strconv.Itoa(e.Status) + ")"
	}
	if e.cause != nil {
		msg += ": " + e.cause.Error()
	}
	return msg
}

// Unwrap makes every *Error an ErrUnavailable.
func (e *Error) Unwrap() error { return ErrUnavailable }

// ReasonOf is the reason of a failed call, ReasonUnavailable for an error
// this package did not make.
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ReasonUnavailable
}

// Config is the account and where the services are.
type Config struct {
	// APIKey is the server key; a secret.
	APIKey string
	// PlacesURL and GeocodingURL default to the services' origins; tests
	// point them at the fake.
	PlacesURL    string
	GeocodingURL string
	// HTTPClient is a client with timeout when nil.
	HTTPClient *http.Client
}

// Client calls the services.
type Client struct {
	key       string
	places    string
	geocoding string
	http      *http.Client
}

// errRedirect refuses every redirect.
var errRedirect = errors.New("a redirect is not followed")

// New checks cfg and returns the client. Nothing is sent until a call.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("googlemaps: the API key is required")
	}
	places, err := origin(cfg.PlacesURL, DefaultPlacesURL)
	if err != nil {
		return nil, err
	}
	geocoding, err := origin(cfg.GeocodingURL, DefaultGeocodingURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &Client{key: cfg.APIKey, places: places, geocoding: geocoding, http: client}, nil
}

func origin(raw, fallback string) (string, error) {
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("googlemaps: a base URL must be an absolute http or https URL")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// LatLng is a point.
type LatLng struct {
	Lat, Lng float64
}

// Rect is a rectangle: its south-west and north-east corners.
type Rect struct {
	South, West, North, East float64
}

// Union is the smallest rectangle that holds r and o. It does not wrap
// around the antimeridian, which no Turkish city crosses.
func (r Rect) Union(o Rect) Rect {
	return Rect{South: min(r.South, o.South), West: min(r.West, o.West), North: max(r.North, o.North), East: max(r.East, o.East)}
}

// Contains reports whether p is in r, edges included.
func (r Rect) Contains(p LatLng) bool {
	return p.Lat >= r.South && p.Lat <= r.North && p.Lng >= r.West && p.Lng <= r.East
}

// Prediction is one autocomplete suggestion: the place id and its two
// display lines, which the caller shows and never stores.
type Prediction struct {
	PlaceID       string
	MainText      string
	SecondaryText string
}

// Component is one address component of a place.
type Component struct {
	Long  string
	Short string
	Types []string
}

// Place is what the address module reads of a place: its id, its address
// components and its location (nil when Google gave none).
type Place struct {
	ID         string
	Components []Component
	Location   *LatLng
}

// Autocomplete returns the predictions for input in the session of token,
// restricted to the rectangle when there is one and to Turkey always.
func (c *Client) Autocomplete(ctx context.Context, input, token string, restriction *Rect) ([]Prediction, error) {
	body := map[string]any{
		"input": input, "sessionToken": token, "languageCode": language, "regionCode": region,
		"includedRegionCodes": []string{region}, "includeQueryPredictions": false,
	}
	if restriction != nil {
		body["locationRestriction"] = map[string]any{"rectangle": map[string]any{
			"low":  map[string]float64{"latitude": restriction.South, "longitude": restriction.West},
			"high": map[string]float64{"latitude": restriction.North, "longitude": restriction.East},
		}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("googlemaps: encode the autocomplete request: %w", err)
	}
	var answer struct {
		Suggestions []struct {
			PlacePrediction *struct {
				PlaceID          string `json:"placeId"`
				StructuredFormat struct {
					MainText      textField `json:"mainText"`
					SecondaryText textField `json:"secondaryText"`
				} `json:"structuredFormat"`
			} `json:"placePrediction"`
		} `json:"suggestions"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.places+AutocompletePath, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("googlemaps: build the autocomplete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-FieldMask", AutocompleteFieldMask)
	if err := c.callPlaces(req, &answer); err != nil {
		return nil, err
	}
	out := make([]Prediction, 0, len(answer.Suggestions))
	for _, s := range answer.Suggestions {
		p := s.PlacePrediction
		if p == nil || p.PlaceID == "" {
			continue // a query prediction, or nothing to resolve
		}
		out = append(out, Prediction{
			PlaceID: p.PlaceID, MainText: p.StructuredFormat.MainText.Text, SecondaryText: p.StructuredFormat.SecondaryText.Text,
		})
	}
	return out, nil
}

// textField is a localized text of the Places API.
type textField struct {
	Text string `json:"text"`
}

// Details returns the place of placeID in the session of token. The call
// ends the session at Google.
func (c *Client) Details(ctx context.Context, placeID, token string) (Place, error) {
	q := url.Values{"languageCode": {language}, "regionCode": {region}, "sessionToken": {token}}
	target := c.places + DetailsPathPrefix + url.PathEscape(placeID) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return Place{}, fmt.Errorf("googlemaps: build the details request: %w", err)
	}
	req.Header.Set("X-Goog-FieldMask", DetailsFieldMask)
	var answer struct {
		ID                string `json:"id"`
		AddressComponents []struct {
			LongText  string   `json:"longText"`
			ShortText string   `json:"shortText"`
			Types     []string `json:"types"`
		} `json:"addressComponents"`
		Location *struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"location"`
	}
	if err := c.callPlaces(req, &answer); err != nil {
		return Place{}, err
	}
	p := Place{ID: answer.ID}
	for _, a := range answer.AddressComponents {
		p.Components = append(p.Components, Component{Long: a.LongText, Short: a.ShortText, Types: a.Types})
	}
	if answer.Location != nil {
		p.Location = &LatLng{Lat: answer.Location.Latitude, Lng: answer.Location.Longitude}
	}
	return p, nil
}

// Reverse returns the most detailed place at p (ErrNoResult when there is
// none). Only the coordinate goes to Google.
func (c *Client) Reverse(ctx context.Context, p LatLng) (Place, error) {
	q := url.Values{
		"latlng": {strconv.FormatFloat(p.Lat, 'f', -1, 64) + "," + strconv.FormatFloat(p.Lng, 'f', -1, 64)},
	}
	results, err := c.geocode(ctx, q)
	if err != nil {
		return Place{}, err
	}
	best := results[0]
	for _, r := range results {
		if detailed(r.Types) {
			best = r
			break
		}
	}
	place := Place{ID: best.PlaceID}
	for _, a := range best.AddressComponents {
		place.Components = append(place.Components, Component{Long: a.LongName, Short: a.ShortName, Types: a.Types})
	}
	return place, nil
}

// detailed reports whether a geocode result is an address, not an area:
// Google lists results most specific first, but a plus code or an
// establishment may come before the street address.
func detailed(types []string) bool {
	for _, t := range types {
		switch t {
		case "street_address", "premise", "subpremise", "route":
			return true
		}
	}
	return false
}

// RegionBounds returns the bounds of the first-level administrative area
// (a Turkish il) named name in the country of countryCode: the service
// area's restriction is built from them (ADR-0039 Karar 3). Only a result
// typed administrative_area_level_1 counts: a components filter that
// falls back to a town or the country would bound the wrong area, so
// without one the lookup is ErrNoResult.
func (c *Client) RegionBounds(ctx context.Context, name, countryCode string) (Rect, error) {
	q := url.Values{"components": {"country:" + countryCode + "|administrative_area:" + name}}
	results, err := c.geocode(ctx, q)
	if err != nil {
		return Rect{}, err
	}
	i := slices.IndexFunc(results, func(r geocodeResult) bool { return slices.Contains(r.Types, "administrative_area_level_1") })
	if i < 0 {
		return Rect{}, ErrNoResult
	}
	g := results[i].Geometry
	box := g.Bounds
	if box == nil {
		box = g.Viewport
	}
	if box == nil {
		return Rect{}, &Error{Reason: ReasonUnreadable, Status: http.StatusOK}
	}
	return Rect{South: box.Southwest.Lat, West: box.Southwest.Lng, North: box.Northeast.Lat, East: box.Northeast.Lng}, nil
}

type geocodeBox struct {
	Northeast geocodePoint `json:"northeast"`
	Southwest geocodePoint `json:"southwest"`
}

type geocodePoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type geocodeResult struct {
	PlaceID           string   `json:"place_id"`
	Types             []string `json:"types"`
	AddressComponents []struct {
		LongName  string   `json:"long_name"`
		ShortName string   `json:"short_name"`
		Types     []string `json:"types"`
	} `json:"address_components"`
	Geometry struct {
		Bounds   *geocodeBox `json:"bounds"`
		Viewport *geocodeBox `json:"viewport"`
	} `json:"geometry"`
}

// geocode calls the Geocoding API with q and its language, region and
// key, and returns its results: at least one, else ErrNoResult.
func (c *Client) geocode(ctx context.Context, q url.Values) ([]geocodeResult, error) {
	q.Set("language", language)
	q.Set("region", region)
	q.Set("key", c.key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.geocoding+GeocodePath+"?"+q.Encode(), http.NoBody)
	if err != nil {
		// The URL holds the key: the error does not.
		return nil, errors.New("googlemaps: build the geocode request")
	}
	status, raw, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Status  string          `json:"status"`
		Results []geocodeResult `json:"results"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return nil, &Error{Reason: ReasonUnreadable, Status: status}
	}
	switch answer.Status {
	case "OK":
		if len(answer.Results) == 0 {
			return nil, ErrNoResult
		}
		return answer.Results, nil
	case "ZERO_RESULTS":
		return nil, ErrNoResult
	case "OVER_QUERY_LIMIT", "OVER_DAILY_LIMIT":
		return nil, &Error{Reason: ReasonQuota, Status: status}
	case "REQUEST_DENIED":
		return nil, &Error{Reason: ReasonDenied, Status: status}
	case "INVALID_REQUEST":
		return nil, &Error{Reason: ReasonInvalid, Status: status}
	default:
		return nil, &Error{Reason: ReasonUpstream, Status: status}
	}
}

// callPlaces sends a Places API (New) request with the key header and decodes
// a 200 answer into out. Any other status is an *Error by Google's
// canonical status: RESOURCE_EXHAUSTED is the quota, PERMISSION_DENIED and
// UNAUTHENTICATED the key, INVALID_ARGUMENT and NOT_FOUND the request.
func (c *Client) callPlaces(req *http.Request, out any) error {
	req.Header.Set("X-Goog-Api-Key", c.key)
	status, raw, err := c.do(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		var e struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e) // an unreadable error body is still an error
		return &Error{Reason: placesReason(status, e.Error.Status), Status: status}
	}
	if json.Unmarshal(raw, out) != nil {
		return &Error{Reason: ReasonUnreadable, Status: status}
	}
	return nil
}

func placesReason(status int, canonical string) string {
	switch {
	case canonical == "RESOURCE_EXHAUSTED" || status == http.StatusTooManyRequests:
		return ReasonQuota
	case canonical == "PERMISSION_DENIED" || canonical == "UNAUTHENTICATED" ||
		status == http.StatusForbidden || status == http.StatusUnauthorized:
		return ReasonDenied
	case canonical == "INVALID_ARGUMENT" || canonical == "NOT_FOUND" ||
		status == http.StatusBadRequest || status == http.StatusNotFound:
		return ReasonInvalid
	default:
		return ReasonUpstream
	}
}

// do sends req and reads at most maxAnswer bytes of the answer. A
// transport error is reduced to its cause: *url.Error's message holds the
// request URL, and a geocode URL holds the key.
func (c *Client) do(req *http.Request) (int, []byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, &Error{Reason: ReasonTransport, cause: errors.New(scrub(err.Error(), c.key))}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return resp.StatusCode, nil, &Error{Reason: ReasonTransport, Status: resp.StatusCode}
	}
	return resp.StatusCode, raw, nil
}

// scrub removes the key from a message, should a cause ever quote it.
func scrub(msg, key string) string {
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, "[key]")
}
