package googlemaps_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/integrations/googlemaps"
	"github.com/is-koala/koala-api/internal/platform/fakes"
)

const testKey = "test-maps-key-0123456789"

// moda is a place as Google answers it: a formatted address of its own,
// which the client never keeps.
func moda() fakes.GooglePlace {
	return fakes.GooglePlace{
		ID: "ChIJ-moda", MainText: "Moda Caddesi No:12", SecondaryText: "Caferağa, Kadıköy/İstanbul, Türkiye",
		FormattedAddress: "Caferağa, Moda Cd. No:12, 34710 Kadıköy/İstanbul, Türkiye",
		Components: []fakes.GoogleComponent{
			{Long: "12", Short: "12", Types: []string{"street_number"}},
			{Long: "Moda Caddesi", Short: "Moda Cd.", Types: []string{"route"}},
			{Long: "Caferağa Mahallesi", Short: "Caferağa", Types: []string{"administrative_area_level_4", "political"}},
			{Long: "Kadıköy", Short: "Kadıköy", Types: []string{"administrative_area_level_2", "political"}},
			{Long: "İstanbul", Short: "İstanbul", Types: []string{"administrative_area_level_1", "political"}},
			{Long: "Türkiye", Short: "TR", Types: []string{"country", "political"}},
			{Long: "34710", Short: "34710", Types: []string{"postal_code"}},
		},
		Lat: 40.9876, Lng: 29.0254,
	}
}

func newFake(t *testing.T) *fakes.GoogleMaps {
	t.Helper()
	f := fakes.NewGoogleMaps(t, testKey)
	f.AddPlace(moda())
	f.AddRegion("TR", "İstanbul", fakes.GoogleRect{South: 40.80, West: 27.97, North: 41.60, East: 29.95})
	return f
}

func newClient(t *testing.T, f *fakes.GoogleMaps, key string) *googlemaps.Client {
	t.Helper()
	c, err := googlemaps.New(googlemaps.Config{APIKey: key, PlacesURL: f.URL(), GeocodingURL: f.URL(), HTTPClient: f.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Autocomplete sends the input, the session token, Türkiye and the
// rectangle, with the key in its header and the field mask that asks for
// the place id and the two lines only; it reads those three.
func TestAutocomplete(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	c := newClient(t, f, testKey)
	box := googlemaps.Rect{South: 40.80, West: 27.97, North: 41.60, East: 29.95}
	got, err := c.Autocomplete(t.Context(), "moda caddesi", "6f1c8d0e-3a7b-4b8e-9d2f-1a2b3c4d5e6f", &box)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PlaceID != "ChIJ-moda" || got[0].MainText != "Moda Caddesi No:12" ||
		got[0].SecondaryText != "Caferağa, Kadıköy/İstanbul, Türkiye" {
		t.Fatalf("predictions = %+v", got)
	}
	reqs := f.RequestsTo(http.MethodPost, f.AutocompletePath())
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.Header.Get("X-Goog-Api-Key") != testKey || len(r.Query) != 0 {
		t.Errorf("the key travels as %q, query %v; want the header only", r.Header.Get("X-Goog-Api-Key"), r.Query)
	}
	if r.Header.Get("X-Goog-FieldMask") != googlemaps.AutocompleteFieldMask {
		t.Errorf("field mask = %q", r.Header.Get("X-Goog-FieldMask"))
	}
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["sessionToken"] != "6f1c8d0e-3a7b-4b8e-9d2f-1a2b3c4d5e6f" || body["languageCode"] != "tr" || body["locationRestriction"] == nil {
		t.Errorf("body = %s", r.Body)
	}
	// Data minimization (ADR-0039 Karar 9): the body carries nothing of
	// the caller.
	for k := range body {
		switch k {
		case "input", "sessionToken", "languageCode", "regionCode", "includedRegionCodes", "includeQueryPredictions",
			"locationRestriction":
		default:
			t.Errorf("the body carries %q", k)
		}
	}
	// Outside the rectangle nothing is offered.
	far := googlemaps.Rect{South: 38, West: 32, North: 39, East: 33}
	if got, err := c.Autocomplete(t.Context(), "moda", "t2", &far); err != nil || len(got) != 0 {
		t.Errorf("outside the rectangle = %+v, %v", got, err)
	}
}

// Details asks for id, address components and location only; the
// formatted address is never requested, so never read (MED-008).
func TestDetails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	p, err := newClient(t, f, testKey).Details(t.Context(), "ChIJ-moda", "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "ChIJ-moda" || len(p.Components) != 7 || p.Location == nil || p.Location.Lat != 40.9876 {
		t.Fatalf("place = %+v", p)
	}
	r := f.RequestsTo(http.MethodGet, f.DetailsPath("ChIJ-moda"))[0]
	if r.Header.Get("X-Goog-FieldMask") != googlemaps.DetailsFieldMask || strings.Contains(googlemaps.DetailsFieldMask, "formattedAddress") {
		t.Errorf("field mask = %q", r.Header.Get("X-Goog-FieldMask"))
	}
	if r.Query.Get("sessionToken") != "tok-1" || r.Query.Get("key") != "" {
		t.Errorf("query = %v", r.Query)
	}
	if _, err := newClient(t, f, testKey).Details(t.Context(), "ChIJ-none", "tok-2"); googlemaps.ReasonOf(err) != googlemaps.ReasonInvalid {
		t.Errorf("unknown place: %v", err)
	}
}

// Reverse sends the coordinate only and takes the street address, not the
// plus code Google lists first; nothing near is ErrNoResult.
func TestReverse(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	c := newClient(t, f, testKey)
	p, err := c.Reverse(t.Context(), googlemaps.LatLng{Lat: 40.98765, Lng: 29.02545})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "ChIJ-moda" || len(p.Components) != 7 {
		t.Fatalf("place = %+v", p)
	}
	r := f.RequestsTo(http.MethodGet, f.GeocodePath())[0]
	for k := range r.Query {
		switch k {
		case "latlng", "language", "region", "key":
		default:
			t.Errorf("the geocode carries %q", k)
		}
	}
	if _, err := c.Reverse(t.Context(), googlemaps.LatLng{}); !errors.Is(err, googlemaps.ErrNoResult) {
		t.Errorf("nothing near: %v", err)
	}
	b, err := c.RegionBounds(t.Context(), "İstanbul", "TR")
	if err != nil || b.South != 40.80 || b.East != 29.95 {
		t.Errorf("bounds = %+v, %v", b, err)
	}
}

// Every failure is ErrUnavailable with a reason from the finite set, and
// no error message carries the key, the request URL included.
func TestFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		key    string
		setup  func(f *fakes.GoogleMaps)
		call   func(ctx context.Context, c *googlemaps.Client) error
		reason string
	}{
		{"places quota", testKey, func(f *fakes.GoogleMaps) {
			f.Enqueue(http.MethodPost, f.AutocompletePath(), fakes.GooglePlacesError(http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"))
		}, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Autocomplete(ctx, "moda", "t", nil)
			return err
		}, googlemaps.ReasonQuota},
		{"places wrong key", "wrong-key-0123456789", nil, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Details(ctx, "ChIJ-moda", "t")
			return err
		}, googlemaps.ReasonDenied},
		{"places outage", testKey, func(f *fakes.GoogleMaps) {
			f.Enqueue(http.MethodGet, f.DetailsPath("ChIJ-moda"), fakes.GooglePlacesError(http.StatusServiceUnavailable, "UNAVAILABLE"))
		}, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Details(ctx, "ChIJ-moda", "t")
			return err
		}, googlemaps.ReasonUpstream},
		{"geocode quota", testKey, func(f *fakes.GoogleMaps) {
			f.Enqueue(http.MethodGet, f.GeocodePath(), fakes.GoogleGeocodeStatus("OVER_QUERY_LIMIT"))
		}, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Reverse(ctx, googlemaps.LatLng{Lat: 40.98765, Lng: 29.02545})
			return err
		}, googlemaps.ReasonQuota},
		{"geocode wrong key", "wrong-key-0123456789", nil, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Reverse(ctx, googlemaps.LatLng{Lat: 40.98765, Lng: 29.02545})
			return err
		}, googlemaps.ReasonDenied},
		{"geocode dropped", testKey, func(f *fakes.GoogleMaps) {
			f.Enqueue(http.MethodGet, f.GeocodePath(), fakes.Response{Drop: true})
		}, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Reverse(ctx, googlemaps.LatLng{Lat: 40.98765, Lng: 29.02545})
			return err
		}, googlemaps.ReasonTransport},
		{"unreadable", testKey, func(f *fakes.GoogleMaps) {
			f.Enqueue(http.MethodPost, f.AutocompletePath(), fakes.Response{Status: http.StatusOK, Body: []byte("<html>")})
		}, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Autocomplete(ctx, "moda", "t", nil)
			return err
		}, googlemaps.ReasonUnreadable},
		{"redirect", testKey, func(f *fakes.GoogleMaps) {
			f.Enqueue(http.MethodGet, f.GeocodePath(), fakes.Response{
				Status: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://example.com/elsewhere"}},
			})
		}, func(ctx context.Context, c *googlemaps.Client) error {
			_, err := c.Reverse(ctx, googlemaps.LatLng{Lat: 40.98765, Lng: 29.02545})
			return err
		}, googlemaps.ReasonTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			err := tt.call(t.Context(), newClient(t, f, tt.key))
			if !errors.Is(err, googlemaps.ErrUnavailable) || googlemaps.ReasonOf(err) != tt.reason {
				t.Fatalf("err = %v, want ErrUnavailable with reason %s", err, tt.reason)
			}
			if strings.Contains(err.Error(), tt.key) || strings.Contains(err.Error(), "key=") {
				t.Errorf("the error carries the key: %v", err)
			}
		})
	}
}

// A provider that does not answer is a transport failure within the
// client's timeout, and the context's deadline wins when shorter.
func TestTimeout(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.Enqueue(http.MethodPost, f.AutocompletePath(), fakes.Response{Hang: true})
	hc := f.Client()
	hc.Timeout = 50 * time.Millisecond
	c, err := googlemaps.New(googlemaps.Config{APIKey: testKey, PlacesURL: f.URL(), GeocodingURL: f.URL(), HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Autocomplete(t.Context(), "moda", "t", nil); googlemaps.ReasonOf(err) != googlemaps.ReasonTransport {
		t.Errorf("hang: %v", err)
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]googlemaps.Config{
		"no key":      {},
		"bad places":  {APIKey: "k", PlacesURL: "places.googleapis.com"},
		"bad geocode": {APIKey: "k", GeocodingURL: "ftp://maps.googleapis.com"},
	} {
		if _, err := googlemaps.New(cfg); err == nil {
			t.Errorf("%s: New() = nil error", name)
		}
	}
}

// The canned driver gives the shapes the client gives: the three cities'
// places, Merkez as Adıyaman's district, and ErrNoResult far from them.
func TestCanned(t *testing.T) {
	t.Parallel()
	var c googlemaps.Canned
	got, err := c.Autocomplete(t.Context(), "Atatürk Bulvarı", "t", nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("autocomplete = %+v, %v", got, err)
	}
	box := googlemaps.Rect{South: 38.95, West: 30.85, North: 40.60, East: 33.40}
	if got, _ := c.Autocomplete(t.Context(), "Atatürk Bulvarı", "t", &box); len(got) != 1 {
		t.Errorf("restricted to Ankara = %+v", got)
	}
	p, err := c.Details(t.Context(), "canned-adiyaman-ataturk-5", "t")
	if err != nil || p.Location == nil {
		t.Fatalf("details = %+v, %v", p, err)
	}
	if _, err := c.Reverse(t.Context(), googlemaps.LatLng{}); !errors.Is(err, googlemaps.ErrNoResult) {
		t.Errorf("reverse far away: %v", err)
	}
	if _, err := c.RegionBounds(t.Context(), "Ankara", "TR"); err != nil {
		t.Errorf("bounds: %v", err)
	}
}

func TestRectUnion(t *testing.T) {
	t.Parallel()
	a := googlemaps.Rect{South: 40, West: 28, North: 41, East: 29}
	b := googlemaps.Rect{South: 39, West: 32, North: 40, East: 33}
	u := a.Union(b)
	if u != (googlemaps.Rect{South: 39, West: 28, North: 41, East: 33}) {
		t.Errorf("union = %+v", u)
	}
	if !u.Contains(googlemaps.LatLng{Lat: 39.5, Lng: 32.5}) || u.Contains(googlemaps.LatLng{Lat: 38, Lng: 30}) {
		t.Error("contains")
	}
}
