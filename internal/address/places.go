package address

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/db/referencedata"
)

// Places matches the provider's place names to our reference rows
// (ADR-0039 Karar 2): the provider writes "Kadıköy" and "İstanbul", the
// reference data "KADIKÖY" and "İSTANBUL".
//
// The match runs in Go over the reference data the binary carries
// (db/referencedata, the rows `koala migrate up` brings the tables in line
// with), not in SQL: the names' column has the ICU turkish collation, and
// an index on it would need a REINDEX at every ICU upgrade
// (TestNoIndexUsesTheTurkishCollation, ADR-0037). The data is a thousand
// rows; the maps are built once at wiring. A row the binary does not know
// (a newer database after a rollback) matches nothing, which is the safe
// answer: the caller picks the district from the list.
//
// A name is compared by its key: Turkish upper case (i → İ, ı → I), then
// folded to ASCII (Ç → C, Ğ → G, İ → I, Ö → O, Ş → S, Ü → U) and with its
// spaces collapsed, so "Kadıköy", "KADIKÖY" and "Kadikoy" are one key. Two
// rows of one parent that share a key are ambiguous and match nothing.
type Places struct {
	countries map[string]uuid.UUID // ISO code → id
	cities    map[uuid.UUID]map[string]uuid.UUID
	districts map[uuid.UUID]map[string]uuid.UUID
	// cityNames and cityCountries describe a city id.
	cityNames     map[uuid.UUID]string
	cityCountries map[uuid.UUID]string
}

// NewPlaces indexes the reference data's countries, cities and districts.
func NewPlaces(d *referencedata.Dataset) (*Places, error) {
	if d == nil {
		return nil, errors.New("address: places need the reference data")
	}
	p := &Places{
		countries: map[string]uuid.UUID{}, cities: map[uuid.UUID]map[string]uuid.UUID{},
		districts: map[uuid.UUID]map[string]uuid.UUID{}, cityNames: map[uuid.UUID]string{},
		cityCountries: map[uuid.UUID]string{},
	}
	codes := map[uuid.UUID]string{}
	for _, c := range d.Countries {
		p.countries[c.Code] = c.ID
		codes[c.ID] = c.Code
	}
	for _, c := range d.Cities {
		addKey(p.cities, c.Parent, nameKey(c.Name), c.ID)
		p.cityNames[c.ID] = c.Name
		p.cityCountries[c.ID] = codes[c.Parent]
	}
	for _, c := range d.Districts {
		addKey(p.districts, c.Parent, nameKey(c.Name), c.ID)
	}
	return p, nil
}

// addKey adds id under key of parent; a second row with the key makes it
// ambiguous (uuid.Nil).
func addKey(m map[uuid.UUID]map[string]uuid.UUID, parent uuid.UUID, key string, id uuid.UUID) {
	if m[parent] == nil {
		m[parent] = map[string]uuid.UUID{}
	}
	if _, dup := m[parent][key]; dup {
		m[parent][key] = uuid.Nil
		return
	}
	m[parent][key] = id
}

// CityName is a city's reference name and its country's ISO code.
func (p *Places) CityName(id uuid.UUID) (name, country string, ok bool) {
	name, ok = p.cityNames[id]
	return name, p.cityCountries[id], ok
}

// City is the city named name in the country of the ISO code.
func (p *Places) City(countryCode, name string) (uuid.UUID, bool) {
	country, ok := p.countries[strings.ToUpper(strings.TrimSpace(countryCode))]
	if !ok {
		return uuid.Nil, false
	}
	return found(p.cities[country], cityKey(name))
}

// District is the district of the city named name: the reference name, a
// former name (districtRenames), or the city's central district, which
// the reference data calls MERKEZ outside the metropolitan cities and the
// provider calls "Merkez", by the city's name, or "<city> Merkez".
func (p *Places) District(cityID uuid.UUID, name string) (uuid.UUID, bool) {
	key := nameKey(strings.TrimSuffix(nameKey(name), " ILCESI"))
	cityName, _, ok := p.CityName(cityID)
	if !ok {
		return uuid.Nil, false
	}
	city := nameKey(cityName)
	if renamed, ok := districtRenames()[city+"/"+key]; ok {
		key = renamed
	}
	if id, ok := found(p.districts[cityID], key); ok {
		return id, true
	}
	if key == centralDistrict || key == city || key == city+" "+centralDistrict {
		return found(p.districts[cityID], centralDistrict)
	}
	return uuid.Nil, false
}

// centralDistrict is the reference data's name of a non-metropolitan
// city's central district.
const centralDistrict = "MERKEZ"

// districtRenames are districts the provider may still name by a former
// name, keyed "<city key>/<former key>": a merger (Eminönü into Fatih,
// 2008) or a rename (Kazan to Kahramankazan, 2016; Eyüp to Eyüpsultan,
// 2017). It is history, not configuration; a district missing here is
// picked by the caller.
func districtRenames() map[string]string {
	return map[string]string{
		"ISTANBUL/EMINONU": "FATIH",
		"ISTANBUL/EYUP":    "EYUPSULTAN",
		"ANKARA/KAZAN":     "KAHRAMANKAZAN",
	}
}

func found(m map[string]uuid.UUID, key string) (uuid.UUID, bool) {
	id, ok := m[key]
	if !ok || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// cityKey is a provider's city name's key: without the " İli" or
// " Province" the provider may add.
func cityKey(name string) string {
	key := nameKey(name)
	for _, suf := range []string{" ILI", " PROVINCE"} {
		key = strings.TrimSuffix(key, suf)
	}
	return key
}

// nameKey is a name's comparison key: Turkish upper case, folded to ASCII,
// spaces collapsed.
func nameKey(s string) string {
	upper := strings.ToUpperSpecial(unicode.TurkishCase, strings.TrimSpace(s))
	folded := strings.Map(func(r rune) rune {
		switch r {
		case 'Ç':
			return 'C'
		case 'Ğ':
			return 'G'
		case 'İ':
			return 'I'
		case 'Ö':
			return 'O'
		case 'Ş':
			return 'S'
		case 'Ü':
			return 'U'
		case 'Â':
			return 'A'
		case 'Î':
			return 'I'
		case 'Û':
			return 'U'
		default:
			return r
		}
	}, upper)
	return strings.Join(strings.Fields(folded), " ")
}

// ServiceArea is the cities the address search serves (ADR-0039 Karar 3):
// KOALA_SERVICE_AREA_CITIES, İstanbul by default. A search, a resolve and
// a reverse lookup outside it are refused; an address entered by hand is
// not, since a user's own address does not depend on where Koala works.
type ServiceArea struct {
	cities []uuid.UUID
	// country is the ISO code the cities share: the provider's region.
	country string
}

// NewServiceArea checks every id against the reference data: an unknown
// city stops the server (fail-closed), and all must be of one country.
func NewServiceArea(places *Places, ids []uuid.UUID) (ServiceArea, error) {
	if len(ids) == 0 {
		return ServiceArea{}, errors.New("address: the service area needs at least one city")
	}
	a := ServiceArea{}
	for _, id := range ids {
		_, country, ok := places.CityName(id)
		if !ok {
			return ServiceArea{}, fmt.Errorf("address: the service area names %s, which is no city of the reference data", id)
		}
		if a.country != "" && a.country != country {
			return ServiceArea{}, errors.New("address: the service area's cities must be of one country")
		}
		a.country = country
		if !a.Has(id) {
			a.cities = append(a.cities, id)
		}
	}
	return a, nil
}

// Has reports whether the city is in the area.
func (a ServiceArea) Has(city uuid.UUID) bool {
	for _, c := range a.cities {
		if c == city {
			return true
		}
	}
	return false
}

// Cities is the area's cities in the order configured.
func (a ServiceArea) Cities() []uuid.UUID { return append([]uuid.UUID(nil), a.cities...) }

// Country is the ISO code of the area's country.
func (a ServiceArea) Country() string { return a.country }
