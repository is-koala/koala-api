package address

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/integrations/googlemaps"
	"github.com/is-koala/koala-api/internal/platform/apperr"
)

func testSearch(t *testing.T, cities ...string) *search {
	t.Helper()
	d, err := referencedata.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPlaces(d)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, len(cities))
	for i, c := range cities {
		ids[i] = uuid.MustParse(c)
	}
	area, err := NewServiceArea(p, ids)
	if err != nil {
		t.Fatal(err)
	}
	return &search{places: p, area: area}
}

const (
	istanbulID = "e3eecc83-7fa1-4405-bc61-04dd36656ebd"
	ankaraID   = "453a1765-7a7f-4f30-b6e7-7922b5eb9883"
)

func component(long, short string, types ...string) googlemaps.Component {
	return googlemaps.Component{Long: long, Short: short, Types: types}
}

// A provider's place becomes our components (ADR-0039 Karar 2): the city
// and district are reference rows, the mahalle loses its suffix, the
// postal code is five digits or nothing, the free lines are cut to the
// contract's bounds; nothing of the provider's formatted text exists to
// be carried (MED-008).
func TestResolvedComponents(t *testing.T) {
	t.Parallel()
	x := testSearch(t, istanbulID)
	long := ""
	for range 200 {
		long += "a"
	}
	place := googlemaps.Place{ID: "ChIJ-x", Components: []googlemaps.Component{
		component("12", "12", "street_number"),
		component(long, "Moda Cd.", "route"),
		component("Caferağa Mahallesi", "Caferağa Mh.", "administrative_area_level_4", "political"),
		component("Kadıköy", "Kadıköy", "administrative_area_level_2", "political"),
		component("İstanbul", "İstanbul", "administrative_area_level_1", "political"),
		component("Türkiye", "TR", "country", "political"),
		component("34710-x", "34710-x", "postal_code"),
		component("Moda Konutları", "Moda Konutları", "premise"),
	}}
	r, err := x.resolved(place, &Point{Lat: 41, Lng: 29}, SourceAutocomplete)
	if err != nil {
		t.Fatal(err)
	}
	if r.CityID != uuid.MustParse(istanbulID) || r.DistrictID == nil || r.DistrictID.String() != "464757c9-1827-499d-8b58-562758181579" {
		t.Errorf("references = %+v", r)
	}
	if r.Neighborhood == nil || *r.Neighborhood != "Caferağa" {
		t.Errorf("neighborhood = %v", r.Neighborhood)
	}
	if r.Thoroughfare == nil || len([]rune(*r.Thoroughfare)) != maxThoroughfare {
		t.Errorf("thoroughfare not cut to the contract's bound")
	}
	if r.PostalCode != nil {
		t.Errorf("postal code %q kept", *r.PostalCode)
	}
	if r.SiteName == nil || *r.SiteName != "Moda Konutları" || r.Source != SourceAutocomplete {
		t.Errorf("site name and source = %v, %s", r.SiteName, r.Source)
	}
	if r.PlaceID == nil || *r.PlaceID != "ChIJ-x" || r.Point == nil {
		t.Errorf("MED-008: place id and point = %+v", r)
	}
}

// An unmatched district is left to the caller; a city outside the area
// or a foreign country is ADDRESS_OUTSIDE_SERVICE_AREA (400). A second
// city in the area resolves.
func TestResolvedServiceArea(t *testing.T) {
	t.Parallel()
	ankaraPlace := googlemaps.Place{Components: []googlemaps.Component{
		component("Çankaya", "Çankaya", "administrative_area_level_2"),
		component("Ankara", "Ankara", "administrative_area_level_1"),
		component("Türkiye", "TR", "country"),
	}}
	if _, err := testSearch(t, istanbulID).resolved(ankaraPlace, nil, SourceReverse); !outside(err) {
		t.Errorf("Ankara under İstanbul's area: %v", err)
	}
	if r, err := testSearch(t, istanbulID, ankaraID).resolved(ankaraPlace, nil, SourceReverse); err != nil || r.DistrictID == nil {
		t.Errorf("Ankara under two cities: %+v, %v", r, err)
	}
	unknown := googlemaps.Place{Components: []googlemaps.Component{
		component("Yeni İlçe", "Yeni İlçe", "administrative_area_level_2"),
		component("İstanbul", "İstanbul", "administrative_area_level_1"),
		component("Türkiye", "TR", "country"),
	}}
	if r, err := testSearch(t, istanbulID).resolved(unknown, nil, SourceReverse); err != nil || r.DistrictID != nil {
		t.Errorf("an unknown district: %+v, %v; want no district and no error", r, err)
	}
	foreign := googlemaps.Place{Components: []googlemaps.Component{
		component("İstanbul", "İstanbul", "administrative_area_level_1"),
		component("Greece", "GR", "country"),
	}}
	if _, err := testSearch(t, istanbulID).resolved(foreign, nil, SourceReverse); !outside(err) {
		t.Errorf("a foreign country: %v", err)
	}
}

func outside(err error) bool {
	var e *apperr.Error
	return errors.As(err, &e) && e.Code == CodeOutsideServiceArea && e.Kind == apperr.KindInvalid
}

// A candidate id is an index the session holds, nothing else.
func TestCandidateIndex(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]int{"c_0": 0, "c_4": 4} {
		if got, ok := candidateIndex(id, 5); !ok || got != want {
			t.Errorf("%s = %d, %v", id, got, ok)
		}
	}
	for _, id := range []string{"c_5", "c_", "c_-1", "x_1", "c_12345", "ChIJ-moda", "c_1a"} {
		if _, ok := candidateIndex(id, 5); ok {
			t.Errorf("%s accepted", id)
		}
	}
}

// TestOnlyReserveSpends holds what the type of spent cannot: within this
// package a spent value is made only in reserve, after the budget counted
// it. It reads the package's sources for every way to make one: a
// literal, a zero value (var x spent[…], a value, not a pointer), new and
// a conversion. A provider call paid with a hand-made unit would pass
// every budget unseen. The demo sources prove each form is seen.
func TestOnlyReserveSpends(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range spentMakers(t, name, string(src)) {
			found++
			if m.fn != "reserve" || m.form != "literal" {
				t.Errorf("%s: %s makes a spent unit (%s); only reserve may, by a literal", m.pos, m.fn, m.form)
			}
		}
	}
	if found == 0 {
		t.Fatal("no spent literal found; the check reads the wrong files")
	}
	demo := `package address
func a() { var x spent[detailsTag]; _ = x }
func b() { _ = new(spent[detailsTag]) }
func c() { _ = spent[detailsTag](spent[detailsTag]{}) }
func d() { var p *spent[detailsTag]; _ = p }
`
	var forms []string
	for _, m := range spentMakers(t, "demo.go", demo) {
		forms = append(forms, m.fn+" "+m.form)
	}
	if want := []string{"a zero value", "b new", "c conversion", "c literal"}; !slices.Equal(forms, want) {
		t.Errorf("the demo's spent makers = %q, want %q (a pointer is no unit)", forms, want)
	}
}

// spentMaker is one place a spent value is made.
type spentMaker struct {
	pos, fn, form string
}

// spentMakers lists the places src makes a spent value, in source order.
func spentMakers(t *testing.T, name, src string) []spentMaker {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	isSpent := func(e ast.Expr) bool {
		idx, ok := e.(*ast.IndexExpr)
		if !ok {
			return false
		}
		id, ok := idx.X.(*ast.Ident)
		return ok && id.Name == "spent"
	}
	var out []spentMaker
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			form := ""
			switch x := n.(type) {
			case *ast.CompositeLit:
				if isSpent(x.Type) {
					form = "literal"
				}
			case *ast.ValueSpec:
				if x.Type != nil && isSpent(x.Type) {
					form = "zero value"
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isSpent(x.Args[0]) {
					form = "new"
				} else if isSpent(x.Fun) {
					form = "conversion"
				}
			}
			if form != "" {
				out = append(out, spentMaker{pos: fset.Position(n.Pos()).String(), fn: fn.Name.Name, form: form})
			}
			return true
		})
	}
	return out
}
