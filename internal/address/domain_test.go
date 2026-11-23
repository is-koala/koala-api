package address_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/address"
)

// Reference rows of db/referencedata.
var (
	istanbul = uuid.MustParse("e3eecc83-7fa1-4405-bc61-04dd36656ebd")
	kadikoy  = uuid.MustParse("464757c9-1827-499d-8b58-562758181579")
	sisli    = uuid.MustParse("3f3a88b0-947d-4bd9-9fc0-1fe45f5c27fb")
	fatih    = uuid.MustParse("a058819e-e2db-41d9-b7ae-245f1ec7b8b9")
	ankara   = uuid.MustParse("453a1765-7a7f-4f30-b6e7-7922b5eb9883")
	kazan    = uuid.MustParse("81c38c22-108f-400a-9cf2-cfb20f06645a")
	adiyaman = uuid.MustParse("94b50acc-2e84-4850-b2ff-b5a0e661bee6")
	merkez   = uuid.MustParse("95bc3fc8-1260-4c44-9498-b3ca06367da4")
)

func places(t *testing.T) *address.Places {
	t.Helper()
	d, err := referencedata.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := address.NewPlaces(d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The provider's names match our reference rows (ADR-0039 Karar 2): in
// Turkish case (İ/ı), folded to ASCII, with the provider's suffixes, the
// central district by every name the provider gives it, and the former
// names of the history table. A name that matches nothing is not guessed.
func TestPlacesMatchTheProvidersNames(t *testing.T) {
	t.Parallel()
	p := places(t)
	for _, tt := range []struct {
		city string
		want uuid.UUID
	}{
		{"İstanbul", istanbul}, {"ISTANBUL", istanbul}, {"istanbul", istanbul}, {"İstanbul İli", istanbul},
		{"Istanbul Province", istanbul}, {"  Adıyaman ", adiyaman}, {"Adiyaman", adiyaman},
	} {
		if got, ok := p.City("TR", tt.city); !ok || got != tt.want {
			t.Errorf("City(%q) = %v, %v; want %v", tt.city, got, ok, tt.want)
		}
	}
	for _, name := range []string{"Atlantis", ""} {
		if _, ok := p.City("TR", name); ok {
			t.Errorf("City(%q) matched", name)
		}
	}
	if _, ok := p.City("DE", "İstanbul"); ok {
		t.Error("a city matched in another country")
	}
	for _, tt := range []struct {
		city     uuid.UUID
		district string
		want     uuid.UUID
	}{
		{istanbul, "Kadıköy", kadikoy}, {istanbul, "KADIKÖY", kadikoy}, {istanbul, "Kadikoy", kadikoy},
		{istanbul, "Şişli", sisli}, {istanbul, "Sisli", sisli}, {istanbul, "Şişli İlçesi", sisli},
		{istanbul, "Eminönü", fatih}, {ankara, "Kazan", kazan},
		{adiyaman, "Merkez", merkez}, {adiyaman, "Adıyaman", merkez}, {adiyaman, "Adıyaman Merkez", merkez},
	} {
		if got, ok := p.District(tt.city, tt.district); !ok || got != tt.want {
			t.Errorf("District(%v, %q) = %v, %v; want %v", tt.city, tt.district, got, ok, tt.want)
		}
	}
	// A metropolitan city has no MERKEZ: the provider's "Merkez" there is
	// the caller's to pick, as an unknown name is; a district of another
	// city is not found under this one.
	for _, tt := range []struct {
		city     uuid.UUID
		district string
	}{{istanbul, "Merkez"}, {istanbul, "Çankaya"}, {istanbul, "Bilinmeyen"}, {ankara, "Eminönü"}} {
		if got, ok := p.District(tt.city, tt.district); ok {
			t.Errorf("District(%v, %q) = %v, want no match", tt.city, tt.district, got)
		}
	}
}

// The service area is checked against the reference data: an unknown city
// stops the server; two cities form one area; nothing names İstanbul in
// the code (KOALA_SERVICE_AREA_CITIES' default does).
func TestServiceArea(t *testing.T) {
	t.Parallel()
	p := places(t)
	if _, err := address.NewServiceArea(p, []uuid.UUID{uuid.New()}); err == nil {
		t.Error("an unknown city made a service area")
	}
	if _, err := address.NewServiceArea(p, nil); err == nil {
		t.Error("no city made a service area")
	}
	a, err := address.NewServiceArea(p, []uuid.UUID{istanbul, ankara, istanbul})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Has(istanbul) || !a.Has(ankara) || a.Has(adiyaman) || len(a.Cities()) != 2 || a.Country() != "TR" {
		t.Errorf("area = %v %s", a.Cities(), a.Country())
	}
}

// The shown line is built from our components only (MED-008), the
// reference names in Turkish title case.
func TestDisplayLine(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	a := address.Address{
		City: address.Reference{Name: "İSTANBUL"}, District: address.Reference{Name: "KADIKÖY"},
		Lines: address.Lines{Thoroughfare: s("Moda Caddesi"), BuildingNumber: s("12"), UnitNumber: s("7"), Neighborhood: s("Caferağa")},
	}
	if got := a.DisplayLine(); got != "Moda Caddesi 12/7, Caferağa, Kadıköy, İstanbul" {
		t.Errorf("line = %q", got)
	}
	a.Lines = address.Lines{}
	a.City.Name, a.District.Name = "AFYONKARAHİSAR", "MERKEZ"
	if got := a.DisplayLine(); got != "Merkez, Afyonkarahisar" {
		t.Errorf("bare line = %q", got)
	}
}

// MED-009: Render is the one place a stored coordinate is rounded; the
// exact view is the stored value and the approximate one a 0.01° grid.
func TestRender(t *testing.T) {
	t.Parallel()
	p := address.Point{Lat: 40.987654321, Lng: 29.025432109}
	if got := p.Render(address.PrecisionExact); got.Lat != p.Lat || got.Lng != p.Lng || got.Precision != address.PrecisionExact {
		t.Errorf("MED-009: exact = %+v", got)
	}
	got := p.Render(address.PrecisionApproximate)
	if got.Lat != 40.99 || got.Lng != 29.03 || got.Precision != address.PrecisionApproximate {
		t.Errorf("MED-009: approximate = %+v", got)
	}
	// The grid is stable: points of one cell render alike.
	if q := (address.Point{Lat: 40.9912, Lng: 29.0281}).Render(address.PrecisionApproximate); q.Lat != got.Lat || q.Lng != got.Lng {
		t.Errorf("same cell = %+v, want %+v", q, got)
	}
	// The stored value is never touched.
	if p.Lat != 40.987654321 {
		t.Error("render changed the point")
	}
}
