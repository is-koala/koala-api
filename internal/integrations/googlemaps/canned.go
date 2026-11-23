package googlemaps

import (
	"context"
	"math"
	"net/http"
	"strings"
	"unicode"
)

// Canned is the fake driver (KOALA_GOOGLE_MAPS_DRIVER=fake, ADR-0039): it
// answers from a few Turkish places in three cities and calls nothing.
// local, test and a staging without a key run the whole address search
// on it; production refuses it (config.ServeMapsErrors). Its answers have
// the shapes the client returns: what the address module does with a
// Google answer it does with these.
//
// The places cover the cases the matching has to handle: a metropolitan
// district (Kadıköy, İstanbul), a second metropolitan city (Çankaya,
// Ankara), and a central district the provider calls "Merkez" (Adıyaman).
// internal/platform/fakes.GoogleMaps is the wire fake the client's own
// tests run against.
type Canned struct{}

// cannedPlace is one place of the canned set.
type cannedPlace struct {
	id, main, secondary string
	city, district      string
	neighborhood, route string
	number, postal      string
	at                  LatLng
}

func cannedPlaces() []cannedPlace {
	return []cannedPlace{
		{
			id: "canned-moda-caddesi-12", main: "Moda Caddesi No:12", secondary: "Caferağa, Kadıköy/İstanbul, Türkiye",
			city: "İstanbul", district: "Kadıköy", neighborhood: "Caferağa Mahallesi", route: "Moda Caddesi",
			number: "12", postal: "34710", at: LatLng{Lat: 40.9876, Lng: 29.0254},
		},
		{
			id: "canned-ataturk-bulvari-100", main: "Atatürk Bulvarı No:100", secondary: "Kızılay, Çankaya/Ankara, Türkiye",
			city: "Ankara", district: "Çankaya", neighborhood: "Kızılay Mahallesi", route: "Atatürk Bulvarı",
			number: "100", postal: "06420", at: LatLng{Lat: 39.9208, Lng: 32.8541},
		},
		{
			id: "canned-adiyaman-ataturk-5", main: "Atatürk Bulvarı No:5", secondary: "Merkez/Adıyaman, Türkiye",
			city: "Adıyaman", district: "Merkez", neighborhood: "Yeni Mahalle", route: "Atatürk Bulvarı",
			number: "5", postal: "02100", at: LatLng{Lat: 37.7648, Lng: 38.2786},
		},
	}
}

// cannedBounds are the canned cities' rectangles, wide enough to hold
// their place.
func cannedBounds() map[string]Rect {
	return map[string]Rect{
		"istanbul": {South: 40.80, West: 27.97, North: 41.60, East: 29.95},
		"ankara":   {South: 38.95, West: 30.85, North: 40.60, East: 33.40},
		"adiyaman": {South: 37.35, West: 37.45, North: 38.20, East: 39.25},
	}
}

// cannedRadius is how far, in degrees, a reverse geocode finds a canned
// place: about two kilometers.
const cannedRadius = 0.02

// Autocomplete returns the places whose lines hold every word of input,
// inside the restriction.
func (Canned) Autocomplete(_ context.Context, input, _ string, restriction *Rect) ([]Prediction, error) {
	words := strings.Fields(fold(input))
	var out []Prediction
	places := cannedPlaces()
	for i := range places {
		p := &places[i]
		text := fold(p.main + " " + p.secondary)
		if !containsAll(text, words) || (restriction != nil && !restriction.Contains(p.at)) {
			continue
		}
		out = append(out, Prediction{PlaceID: p.id, MainText: p.main, SecondaryText: p.secondary})
	}
	return out, nil
}

// Details returns the canned place of placeID; an unknown one is the
// provider's invalid request.
func (Canned) Details(_ context.Context, placeID, _ string) (Place, error) {
	places := cannedPlaces()
	for i := range places {
		if p := &places[i]; p.id == placeID {
			return p.place(true), nil
		}
	}
	return Place{}, &Error{Reason: ReasonInvalid, Status: http.StatusNotFound}
}

// Reverse returns the canned place nearest to at within cannedRadius.
func (Canned) Reverse(_ context.Context, at LatLng) (Place, error) {
	places := cannedPlaces()
	for i := range places {
		p := &places[i]
		if math.Abs(p.at.Lat-at.Lat) <= cannedRadius && math.Abs(p.at.Lng-at.Lng) <= cannedRadius {
			return p.place(false), nil
		}
	}
	return Place{}, ErrNoResult
}

// RegionBounds returns a canned city's rectangle.
func (Canned) RegionBounds(_ context.Context, name, countryCode string) (Rect, error) {
	if !strings.EqualFold(countryCode, "TR") {
		return Rect{}, ErrNoResult
	}
	r, ok := cannedBounds()[fold(name)]
	if !ok {
		return Rect{}, ErrNoResult
	}
	return r, nil
}

func (p *cannedPlace) place(location bool) Place {
	out := Place{ID: p.id, Components: []Component{
		{Long: p.number, Short: p.number, Types: []string{"street_number"}},
		{Long: p.route, Short: p.route, Types: []string{"route"}},
		{Long: p.neighborhood, Short: p.neighborhood, Types: []string{"administrative_area_level_4", "political"}},
		{Long: p.district, Short: p.district, Types: []string{"administrative_area_level_2", "political"}},
		{Long: p.city, Short: p.city, Types: []string{"administrative_area_level_1", "political"}},
		{Long: "Türkiye", Short: "TR", Types: []string{"country", "political"}},
		{Long: p.postal, Short: p.postal, Types: []string{"postal_code"}},
	}}
	if location {
		at := p.at
		out.Location = &at
	}
	return out
}

// fold is s in lower case with the Turkish letters folded to ASCII, for
// the canned matching only.
func fold(s string) string {
	lower := strings.ToLowerSpecial(unicode.TurkishCase, s)
	return strings.Map(func(r rune) rune {
		switch r {
		case 'ç':
			return 'c'
		case 'ğ':
			return 'g'
		case 'ı':
			return 'i'
		case 'ö':
			return 'o'
		case 'ş':
			return 's'
		case 'ü':
			return 'u'
		default:
			return r
		}
	}, lower)
}

func containsAll(text string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}
