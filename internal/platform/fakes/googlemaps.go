package fakes

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The Google Maps Platform endpoints the address search calls (Places API
// (New) and the Geocoding API, their documentation).
const (
	googleAutocompletePath = "/v1/places:autocomplete"
	googleDetailsPrefix    = "/v1/places/"
	googleGeocodePath      = "/maps/api/geocode/json"
)

// GooglePlace is a place the fake knows: what Place Details answers for
// its id, what Autocomplete offers when the input matches its lines, and
// what a reverse geocode near its location finds. FormattedAddress is
// Google's own line, which the fake sends whenever the real service would
// (a field mask that asks for it, every geocode result), so a test can
// show the client never keeps it.
type GooglePlace struct {
	ID               string
	MainText         string
	SecondaryText    string
	FormattedAddress string
	Components       []GoogleComponent
	Lat, Lng         float64
}

// GoogleComponent is an address component.
type GoogleComponent struct {
	Long, Short string
	Types       []string
}

// GoogleRect is a rectangle: its south-west and north-east corners.
type GoogleRect struct {
	South, West, North, East float64
}

// GoogleMaps is a fake of the two services, both on one server:
//   - POST /v1/places:autocomplete takes {input, sessionToken, …} with the
//     X-Goog-Api-Key header and answers {"suggestions": [{"placePrediction":
//     {place, placeId, text, structuredFormat: {mainText, secondaryText},
//     types}}]} for the places whose lines hold every word of the input,
//     inside a locationRestriction rectangle when one is sent. Under a field
//     mask other than "*" only placeId and structuredFormat are sent.
//   - GET /v1/places/{id} needs X-Goog-FieldMask (400 INVALID_ARGUMENT
//     without it, as documented) and answers the place's id,
//     addressComponents ({longText, shortText, types, languageCode}) and
//     location ({latitude, longitude}); formattedAddress only when the mask
//     names it or is "*". An unknown id is 404 NOT_FOUND.
//   - GET /maps/api/geocode/json with latlng answers the place within about
//     a kilometer (the result carries formatted_address and geometry as
//     the real one does), else ZERO_RESULTS; with components
//     "country:XX|administrative_area:Name" the region's bounds; without
//     either, INVALID_REQUEST. The key travels in the key query parameter.
//
// A wrong key is 403 PERMISSION_DENIED on Places and REQUEST_DENIED with
// HTTP 200 on Geocoding, as the services answer. Places errors have the
// Google API error body ({"error": {code, message, status}}); responses
// for quota and outages are built with GooglePlacesError and
// GoogleGeocodeStatus and enqueued.
type GoogleMaps struct {
	*Server
	key string

	mu      sync.Mutex
	places  []GooglePlace
	regions map[string]GoogleRect // "XX|Name" → bounds
}

// NewGoogleMaps starts a fake that accepts the API key key; it stops at
// cleanup.
func NewGoogleMaps(tb testing.TB, key string) *GoogleMaps {
	tb.Helper()
	f := &GoogleMaps{key: key, regions: map[string]GoogleRect{}}
	f.Server = newServer(tb, "googlemaps", f.check, f.fallback)
	return f
}

// AutocompletePath is the Autocomplete (New) path.
func (f *GoogleMaps) AutocompletePath() string { return googleAutocompletePath }

// DetailsPath is the Place Details path of id.
func (f *GoogleMaps) DetailsPath(id string) string { return googleDetailsPrefix + id }

// GeocodePath is the Geocoding API's path.
func (f *GoogleMaps) GeocodePath() string { return googleGeocodePath }

// AddPlace makes p known.
func (f *GoogleMaps) AddPlace(p GooglePlace) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.places = append(f.places, p)
}

// AddRegion makes the bounds of a first-level administrative area known.
func (f *GoogleMaps) AddRegion(countryCode, name string, r GoogleRect) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regions[strings.ToUpper(countryCode)+"|"+name] = r
}

// GooglePlacesError is a Places API error: status is the HTTP status and
// canonical Google's status (RESOURCE_EXHAUSTED with 429 for the quota,
// PERMISSION_DENIED with 403 for a key, INTERNAL with 500, UNAVAILABLE
// with 503).
func GooglePlacesError(status int, canonical string) Response {
	return JSON(status, map[string]any{"error": map[string]any{
		"code": status, "message": "fake " + canonical, "status": canonical,
	}})
}

// GoogleGeocodeStatus is a Geocoding answer with status and no result
// (OVER_QUERY_LIMIT, REQUEST_DENIED, UNKNOWN_ERROR…); the service answers
// them with HTTP 200.
func GoogleGeocodeStatus(status string) Response {
	return JSON(http.StatusOK, map[string]any{
		"status": status, "error_message": "fake " + status, "results": []any{},
	})
}

// check refuses a foreign key before anything else.
func (f *GoogleMaps) check(r Request) (Response, bool) {
	switch {
	case r.Path == googleGeocodePath:
		if r.Query.Get("key") != f.key {
			return GoogleGeocodeStatus("REQUEST_DENIED"), true
		}
	case r.Path == googleAutocompletePath || strings.HasPrefix(r.Path, googleDetailsPrefix):
		if r.Header.Get("X-Goog-Api-Key") != f.key {
			return GooglePlacesError(http.StatusForbidden, "PERMISSION_DENIED"), true
		}
	}
	return Response{}, false
}

func (f *GoogleMaps) fallback(r Request) (Response, bool) {
	switch {
	case r.Method == http.MethodPost && r.Path == googleAutocompletePath:
		return f.autocomplete(r), true
	case r.Method == http.MethodGet && strings.HasPrefix(r.Path, googleDetailsPrefix):
		return f.details(r), true
	case r.Method == http.MethodGet && r.Path == googleGeocodePath:
		return f.geocode(r), true
	}
	return Response{}, false
}

// googleSessionToken is a session token's documented form: URL- and
// filename-safe base64 characters.
var googleSessionToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (f *GoogleMaps) autocomplete(r Request) Response {
	var req struct {
		Input               string   `json:"input"`
		SessionToken        *string  `json:"sessionToken"`
		IncludedRegionCodes []string `json:"includedRegionCodes"`
		LocationRestriction *struct {
			Rectangle *struct {
				Low  struct{ Latitude, Longitude float64 } `json:"low"`
				High struct{ Latitude, Longitude float64 } `json:"high"`
			} `json:"rectangle"`
		} `json:"locationRestriction"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil || strings.TrimSpace(req.Input) == "" {
		return GooglePlacesError(http.StatusBadRequest, "INVALID_ARGUMENT")
	}
	if req.SessionToken != nil && !googleSessionToken.MatchString(*req.SessionToken) {
		return GooglePlacesError(http.StatusBadRequest, "INVALID_ARGUMENT")
	}
	if len(req.IncludedRegionCodes) > 15 {
		return GooglePlacesError(http.StatusBadRequest, "INVALID_ARGUMENT")
	}
	var box *GoogleRect
	if lr := req.LocationRestriction; lr != nil && lr.Rectangle != nil {
		box = &GoogleRect{
			South: lr.Rectangle.Low.Latitude, West: lr.Rectangle.Low.Longitude,
			North: lr.Rectangle.High.Latitude, East: lr.Rectangle.High.Longitude,
		}
	}
	full := r.Header.Get("X-Goog-FieldMask") == "" || r.Header.Get("X-Goog-FieldMask") == "*"
	words := strings.Fields(strings.ToLower(req.Input))
	suggestions := []any{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.places {
		text := strings.ToLower(p.MainText + " " + p.SecondaryText)
		if !allIn(text, words) || (box != nil && !inside(*box, p.Lat, p.Lng)) {
			continue
		}
		pred := map[string]any{
			"placeId": p.ID,
			"structuredFormat": map[string]any{
				"mainText": map[string]any{"text": p.MainText}, "secondaryText": map[string]any{"text": p.SecondaryText},
			},
		}
		if full {
			pred["place"] = "places/" + p.ID
			pred["text"] = map[string]any{"text": p.MainText + ", " + p.SecondaryText}
			pred["types"] = []string{"street_address", "geocode"}
		}
		suggestions = append(suggestions, map[string]any{"placePrediction": pred})
		if len(suggestions) == 5 { // the service's page
			break
		}
	}
	return JSON(http.StatusOK, map[string]any{"suggestions": suggestions})
}

func (f *GoogleMaps) details(r Request) Response {
	mask := r.Header.Get("X-Goog-FieldMask")
	if mask == "" {
		mask = r.Query.Get("fields")
	}
	if mask == "" {
		return GooglePlacesError(http.StatusBadRequest, "INVALID_ARGUMENT")
	}
	id := strings.TrimPrefix(r.Path, googleDetailsPrefix)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.places {
		if p.ID != id {
			continue
		}
		fields := map[string]bool{}
		for _, m := range strings.Split(mask, ",") {
			fields[strings.TrimSpace(m)] = true
		}
		all := fields["*"]
		out := map[string]any{}
		if all || fields["id"] {
			out["id"] = p.ID
		}
		if all || fields["addressComponents"] {
			comps := []any{}
			for _, c := range p.Components {
				comps = append(comps, map[string]any{
					"longText": c.Long, "shortText": c.Short, "types": c.Types, "languageCode": "tr",
				})
			}
			out["addressComponents"] = comps
		}
		if all || fields["location"] {
			out["location"] = map[string]any{"latitude": p.Lat, "longitude": p.Lng}
		}
		if all || fields["formattedAddress"] {
			out["formattedAddress"] = p.FormattedAddress
		}
		return JSON(http.StatusOK, out)
	}
	return GooglePlacesError(http.StatusNotFound, "NOT_FOUND")
}

// googleReverseRadius is how far, in degrees, a reverse geocode finds a
// place: about a kilometer.
const googleReverseRadius = 0.01

func (f *GoogleMaps) geocode(r Request) Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	if latlng := r.Query.Get("latlng"); latlng != "" {
		lat, lng, ok := parseLatLng(latlng)
		if !ok {
			return GoogleGeocodeStatus("INVALID_REQUEST")
		}
		for _, p := range f.places {
			if math.Abs(p.Lat-lat) > googleReverseRadius || math.Abs(p.Lng-lng) > googleReverseRadius {
				continue
			}
			comps := []any{}
			for _, c := range p.Components {
				comps = append(comps, map[string]any{"long_name": c.Long, "short_name": c.Short, "types": c.Types})
			}
			// A plus code result first, as the service often answers: the
			// street address is not always the first result.
			return JSON(http.StatusOK, map[string]any{"status": "OK", "results": []any{
				map[string]any{
					"place_id": "plus-" + p.ID, "types": []string{"plus_code"}, "formatted_address": "XXXX+XX",
					"address_components": []any{},
					"geometry":           map[string]any{"location": map[string]any{"lat": lat, "lng": lng}},
				},
				map[string]any{
					"place_id": p.ID, "types": []string{"street_address"}, "formatted_address": p.FormattedAddress,
					"address_components": comps,
					"geometry": map[string]any{
						"location": map[string]any{"lat": p.Lat, "lng": p.Lng}, "location_type": "ROOFTOP",
					},
				},
			}})
		}
		return JSON(http.StatusOK, map[string]any{"status": "ZERO_RESULTS", "results": []any{}})
	}
	if components := r.Query.Get("components"); components != "" {
		var country, area string
		for _, part := range strings.Split(components, "|") {
			k, v, _ := strings.Cut(part, ":")
			switch k {
			case "country":
				country = strings.ToUpper(v)
			case "administrative_area":
				area = v
			}
		}
		b, ok := f.regions[country+"|"+area]
		if !ok {
			return JSON(http.StatusOK, map[string]any{"status": "ZERO_RESULTS", "results": []any{}})
		}
		box := map[string]any{
			"northeast": map[string]any{"lat": b.North, "lng": b.East},
			"southwest": map[string]any{"lat": b.South, "lng": b.West},
		}
		return JSON(http.StatusOK, map[string]any{"status": "OK", "results": []any{map[string]any{
			"place_id": "region-" + country + "-" + area, "types": []string{"administrative_area_level_1", "political"},
			"formatted_address": area + ", " + country, "address_components": []any{},
			"geometry": map[string]any{"bounds": box, "viewport": box},
		}}})
	}
	return GoogleGeocodeStatus("INVALID_REQUEST")
}

func parseLatLng(s string) (lat, lng float64, ok bool) {
	a, b, found := strings.Cut(s, ",")
	if !found {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(a, 64)
	lng, err2 := strconv.ParseFloat(b, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return lat, lng, true
}

func inside(b GoogleRect, lat, lng float64) bool {
	return lat >= b.South && lat <= b.North && lng >= b.West && lng <= b.East
}

func allIn(text string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}
