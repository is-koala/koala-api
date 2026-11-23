package fakes_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/fakes"
)

func googlePlace() fakes.GooglePlace {
	return fakes.GooglePlace{
		ID: "ChIJ-fake", MainText: "Moda Caddesi", SecondaryText: "Kadıköy/İstanbul, Türkiye",
		FormattedAddress: "Moda Cd. No:12, 34710 Kadıköy/İstanbul, Türkiye",
		Components:       []fakes.GoogleComponent{{Long: "Kadıköy", Short: "Kadıköy", Types: []string{"administrative_area_level_2"}}},
		Lat:              40.9876, Lng: 29.0254,
	}
}

// Place Details needs a field mask, as the service documents, and sends
// the formatted address only when the mask asks for it.
func TestGoogleMapsDetailsNeedsAFieldMask(t *testing.T) {
	t.Parallel()
	f := fakes.NewGoogleMaps(t, "k-0123456789")
	f.AddPlace(googlePlace())
	key := http.Header{"X-Goog-Api-Key": {"k-0123456789"}}
	status, body := call(t, f.Client(), http.MethodGet, f.URL()+f.DetailsPath("ChIJ-fake"), key, "")
	if status != http.StatusBadRequest || !strings.Contains(string(body), "INVALID_ARGUMENT") {
		t.Errorf("no mask = %d %s", status, body)
	}
	masked := http.Header{"X-Goog-Api-Key": {"k-0123456789"}, "X-Goog-Fieldmask": {"id,addressComponents,location"}}
	status, body = call(t, f.Client(), http.MethodGet, f.URL()+f.DetailsPath("ChIJ-fake"), masked, "")
	if status != http.StatusOK || strings.Contains(string(body), "formattedAddress") || !strings.Contains(string(body), "longText") {
		t.Errorf("masked = %d %s", status, body)
	}
	all := http.Header{"X-Goog-Api-Key": {"k-0123456789"}, "X-Goog-Fieldmask": {"*"}}
	if _, body = call(t, f.Client(), http.MethodGet, f.URL()+f.DetailsPath("ChIJ-fake"), all, ""); !strings.Contains(string(body), "formattedAddress") {
		t.Errorf("all fields = %s", body)
	}
	status, body = call(t, f.Client(), http.MethodGet, f.URL()+f.DetailsPath("ChIJ-none"), masked, "")
	if status != http.StatusNotFound || !strings.Contains(string(body), "NOT_FOUND") {
		t.Errorf("unknown = %d %s", status, body)
	}
}

// A wrong key is refused the way each service refuses it: Places with 403
// PERMISSION_DENIED, Geocoding with REQUEST_DENIED under HTTP 200.
func TestGoogleMapsRefusesAWrongKey(t *testing.T) {
	t.Parallel()
	f := fakes.NewGoogleMaps(t, "k-0123456789")
	status, body := call(t, f.Client(), http.MethodPost, f.URL()+f.AutocompletePath(),
		http.Header{"X-Goog-Api-Key": {"other"}}, `{"input":"moda"}`)
	if status != http.StatusForbidden || !strings.Contains(string(body), "PERMISSION_DENIED") {
		t.Errorf("places = %d %s", status, body)
	}
	status, body = call(t, f.Client(), http.MethodGet, f.URL()+f.GeocodePath()+"?latlng=40,29&key=other", nil, "")
	if status != http.StatusOK || !strings.Contains(string(body), "REQUEST_DENIED") {
		t.Errorf("geocode = %d %s", status, body)
	}
}

// A reverse geocode answers a plus code first and the street address
// second, with the formatted address the real service sends; components
// filtering answers a region's bounds.
func TestGoogleMapsGeocode(t *testing.T) {
	t.Parallel()
	f := fakes.NewGoogleMaps(t, "k-0123456789")
	f.AddPlace(googlePlace())
	f.AddRegion("TR", "İstanbul", fakes.GoogleRect{South: 40.8, West: 27.9, North: 41.6, East: 29.9})
	status, body := call(t, f.Client(), http.MethodGet, f.URL()+f.GeocodePath()+"?latlng=40.9877,29.0255&key=k-0123456789", nil, "")
	m := decode(t, body)
	results, _ := m["results"].([]any)
	if status != http.StatusOK || m["status"] != "OK" || len(results) != 2 {
		t.Fatalf("reverse = %d %s", status, body)
	}
	_, body = call(t, f.Client(), http.MethodGet, f.URL()+f.GeocodePath()+"?latlng=0,0&key=k-0123456789", nil, "")
	if m := decode(t, body); m["status"] != "ZERO_RESULTS" {
		t.Errorf("nothing near = %s", body)
	}
	_, body = call(t, f.Client(), http.MethodGet,
		f.URL()+f.GeocodePath()+"?components=country:TR%7Cadministrative_area:%C4%B0stanbul&key=k-0123456789", nil, "")
	if !strings.Contains(string(body), `"northeast"`) {
		t.Errorf("bounds = %s", body)
	}
	_, body = call(t, f.Client(), http.MethodGet, f.URL()+f.GeocodePath()+"?key=k-0123456789", nil, "")
	if m := decode(t, body); m["status"] != "INVALID_REQUEST" {
		t.Errorf("no query = %s", body)
	}
}
