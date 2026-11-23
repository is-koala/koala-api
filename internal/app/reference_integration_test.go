//go:build integration

package app

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/reference"
)

type referenceItem struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type referenceList struct {
	Items []referenceItem `json:"items"`
}

// list reads a reference list without a bearer.
func (a *authApp) list(t *testing.T, path string) []referenceItem {
	t.Helper()
	rec := a.call(t, http.MethodGet, path, "", "")
	wantStatus(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), `"items":[`) {
		t.Fatalf("%s: body %s, want an items array", path, rec.Body)
	}
	return decodeBody[referenceList](t, rec).Items
}

// named finds the item called name.
func named(t *testing.T, items []referenceItem, name string) referenceItem {
	t.Helper()
	i := slices.IndexFunc(items, func(it referenceItem) bool { return it.Name == name })
	if i < 0 {
		t.Fatalf("%s is not in the list", name)
	}
	return items[i]
}

// position is the index of name in items, or -1.
func position(items []referenceItem, name string) int {
	return slices.IndexFunc(items, func(it referenceItem) bool { return it.Name == name })
}

// turkishRank orders the capital letters of the Turkish alphabet; the
// test's own oracle for names made of them, independent of ICU.
func turkishRank(r rune) int {
	return strings.IndexRune("ABCÇDEFGĞHIİJKLMNOÖPRSŞTUÜVYZ", r)
}

// inTurkishOrder reports the first pair of names out of Turkish
// alphabetical order, for names of capital letters only.
func inTurkishOrder(t *testing.T, items []referenceItem) {
	t.Helper()
	less := func(a, b string) bool {
		ra, rb := []rune(a), []rune(b)
		for i := range min(len(ra), len(rb)) {
			ka, kb := turkishRank(ra[i]), turkishRank(rb[i])
			if ka < 0 || kb < 0 {
				t.Fatalf("%q or %q has a character outside the alphabet", a, b)
			}
			if ka != kb {
				return ka < kb
			}
		}
		return len(ra) < len(rb)
	}
	for i := 1; i < len(items); i++ {
		if less(items[i].Name, items[i-1].Name) {
			t.Fatalf("%q comes after %q; want the Turkish alphabetical order", items[i].Name, items[i-1].Name)
		}
	}
}

// TestReferenceOverHTTP reads the reference lists as an anonymous caller
// (MED-012): country → city → district and university → faculty →
// department, in Turkish alphabetical order; an unknown parent is
// REFERENCE_ITEM_NOT_FOUND, a parent without children an empty list; a
// retired row leaves its list, and a retired parent's list is the unknown
// parent's answer.
func TestReferenceOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())

	// MED-012: the chain country → city → district, without a bearer.
	countries := a.list(t, "/v1/reference/countries")
	if len(countries) != 1 || countries[0] != (referenceItem{ID: uuid.MustParse("26a60ab5-db94-4167-8678-1bb6a85fce30"), Name: "TÜRKİYE"}) {
		t.Fatalf("MED-012: countries = %+v", countries)
	}
	cities := a.list(t, "/v1/reference/countries/"+countries[0].ID.String()+"/cities")
	if len(cities) != 81 {
		t.Fatalf("MED-012: %d cities, want 81", len(cities))
	}
	// The byte order puts Ç after Z and ISPARTA before IĞDIR; the root
	// collation puts İ with I. Turkish puts them where a reader looks.
	inTurkishOrder(t, cities)
	for _, w := range [][2]string{{"BURSA", "ÇANAKKALE"}, {"ÇORUM", "DENİZLİ"}, {"IĞDIR", "ISPARTA"}, {"ISPARTA", "İSTANBUL"}, {"OSMANİYE", "RİZE"}, {"ŞANLIURFA", "ŞIRNAK"}, {"ŞIRNAK", "TEKİRDAĞ"}} {
		if i, j := position(cities, w[0]), position(cities, w[1]); i < 0 || j < 0 || i >= j {
			t.Errorf("%s at %d, %s at %d; want the first before the second", w[0], i, w[1], j)
		}
	}
	ankara := named(t, cities, "ANKARA")
	districts := a.list(t, "/v1/reference/cities/"+ankara.ID.String()+"/districts")
	if len(districts) != 25 {
		t.Fatalf("MED-012: Ankara has %d districts, want 25", len(districts))
	}
	inTurkishOrder(t, districts)

	universities := a.list(t, "/v1/reference/universities")
	if len(universities) != 223 {
		t.Fatalf("%d universities, want 223", len(universities))
	}
	// Under the root collation IŞIK sorts before ISPARTA (Ş with S) and
	// İBN HALDUN first of all (İ with I); under the byte order IĞDIR
	// comes after ISPARTA and ÇUKUROVA after every Z.
	for _, w := range [][2]string{
		{"IĞDIR ÜNİVERSİTESİ", "ISPARTA UYGULAMALI BİLİMLER ÜNİVERSİTESİ"},
		{"ISPARTA UYGULAMALI BİLİMLER ÜNİVERSİTESİ", "IŞIK ÜNİVERSİTESİ"},
		{"IŞIK ÜNİVERSİTESİ", "İBN HALDUN ÜNİVERSİTESİ"},
		{"BURSA ULUDAĞ ÜNİVERSİTESİ", "ÇUKUROVA ÜNİVERSİTESİ"},
		{"ÇUKUROVA ÜNİVERSİTESİ", "DİCLE ÜNİVERSİTESİ"},
	} {
		if i, j := position(universities, w[0]), position(universities, w[1]); i < 0 || j < 0 || i >= j {
			t.Errorf("%s at %d, %s at %d; want the first before the second", w[0], i, w[1], j)
		}
	}
	odtu := named(t, universities, "ORTA DOĞU TEKNİK ÜNİVERSİTESİ")
	faculties := a.list(t, "/v1/reference/universities/"+odtu.ID.String()+"/faculties")
	if len(faculties) == 0 {
		t.Fatal("ODTÜ has no faculties")
	}
	departments := a.list(t, "/v1/reference/faculties/"+faculties[0].ID.String()+"/departments")
	if len(departments) == 0 {
		t.Fatalf("%s has no departments", faculties[0].Name)
	}

	// A faculty without departments is an empty list, not a 404.
	var empty uuid.UUID
	if err := a.dbt.Open(t, db.RoleOwner).Pool().QueryRow(t.Context(), `
		SELECT f.id FROM faculties f WHERE NOT EXISTS (SELECT FROM departments d WHERE d.faculty_id = f.id) LIMIT 1`).
		Scan(&empty); err != nil {
		t.Fatal(err)
	}
	if got := a.list(t, "/v1/reference/faculties/"+empty.String()+"/departments"); len(got) != 0 {
		t.Fatalf("a faculty without departments: %+v", got)
	}

	// An unknown parent.
	notFound := string(reference.CodeReferenceItemNotFound)
	for _, path := range []string{
		"/v1/reference/countries/%s/cities", "/v1/reference/cities/%s/districts",
		"/v1/reference/universities/%s/faculties", "/v1/reference/faculties/%s/departments",
	} {
		wantStatus(t, a.call(t, http.MethodGet, strings.Replace(path, "%s", uuid.NewString(), 1), "", ""), http.StatusNotFound, notFound)
	}

	// Retired rows leave their lists; a retired parent is unknown.
	owner := a.dbt.Open(t, db.RoleOwner)
	cankaya := named(t, districts, "ÇANKAYA")
	for _, stmt := range []struct {
		sql string
		id  uuid.UUID
	}{
		{"UPDATE districts SET retired_at = now() WHERE id = $1", cankaya.ID},
		{"UPDATE universities SET retired_at = now() WHERE id = $1", odtu.ID},
		{"UPDATE faculties SET retired_at = now() WHERE id = $1", faculties[0].ID},
		{"UPDATE cities SET retired_at = now() WHERE id = $1", ankara.ID},
	} {
		if _, err := owner.Pool().Exec(t.Context(), stmt.sql, stmt.id); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.list(t, "/v1/reference/universities"); len(got) != 222 || position(got, odtu.Name) >= 0 {
		t.Errorf("after retiring ODTÜ: %d universities, ODTÜ at %d", len(got), position(got, odtu.Name))
	}
	if got := a.list(t, "/v1/reference/countries/"+countries[0].ID.String()+"/cities"); len(got) != 80 || position(got, "ANKARA") >= 0 {
		t.Errorf("after retiring Ankara: %d cities", len(got))
	}
	for _, path := range []string{
		"/v1/reference/universities/" + odtu.ID.String() + "/faculties",
		"/v1/reference/faculties/" + faculties[0].ID.String() + "/departments",
		"/v1/reference/cities/" + ankara.ID.String() + "/districts",
	} {
		wantStatus(t, a.call(t, http.MethodGet, path, "", ""), http.StatusNotFound, notFound)
	}
	// The other faculty of a university in use keeps its departments; a
	// retired district leaves the list of a city in use.
	if _, err := owner.Pool().Exec(t.Context(), "UPDATE cities SET retired_at = NULL WHERE id = $1", ankara.ID); err != nil {
		t.Fatal(err)
	}
	if got := a.list(t, "/v1/reference/cities/"+ankara.ID.String()+"/districts"); len(got) != 24 || position(got, "ÇANKAYA") >= 0 {
		t.Errorf("after retiring Çankaya: %d districts, Çankaya at %d", len(got), position(got, "ÇANKAYA"))
	}
}

// TestEducationOnTheSeededReference: with the reference data loaded, an
// education record names real rows (ADR-0036 Karar 7 left
// createMyEducation answering REFERENCE_ITEM_NOT_FOUND to every id until
// this data came): the ids the lists give are accepted and the record
// shows their names.
func TestEducationOnTheSeededReference(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "odtu@example.com")

	odtu := named(t, a.list(t, "/v1/reference/universities"), "ORTA DOĞU TEKNİK ÜNİVERSİTESİ")
	faculty := a.list(t, "/v1/reference/universities/"+odtu.ID.String()+"/faculties")[0]
	department := a.list(t, "/v1/reference/faculties/"+faculty.ID.String()+"/departments")[0]

	rec := a.createEducation(t, tok, educationBody(odtu.ID, &faculty.ID, &department.ID, 2010, year(2014)))
	wantStatus(t, rec, http.StatusCreated, "")
	got := decodeBody[educationAnswer](t, rec)
	if got.University.ID != odtu.ID || got.University.Name != odtu.Name || got.Faculty == nil || got.Faculty.Name != faculty.Name ||
		got.Department == nil || got.Department.Name != department.Name {
		t.Fatalf("education = %s", rec.Body)
	}
}

// summaryOf parses the table `koala reference sync` prints: per table,
// inserted, updated, retired, unchanged.
func summaryOf(t *testing.T, stdout string) map[string][4]int {
	t.Helper()
	out := map[string][4]int{}
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !slices.Contains(referencedata.Tables(), f[0]) {
			continue
		}
		if len(f) != 5 {
			t.Fatalf("summary line %q: want a table and four counts", line)
		}
		var n [4]int
		for i := range n {
			v, err := strconv.Atoi(f[i+1])
			if err != nil {
				t.Fatalf("summary line %q: %v", line, err)
			}
			n[i] = v
		}
		out[f[0]] = n
	}
	return out
}

// TestReferenceSyncCommand drives `koala reference sync`: a dry run
// reports the drift and writes nothing, a real run repairs it, and a run
// after that has nothing to do. The drift is what a hand edit or an older
// release leaves: a renamed university and a retired department.
func TestReferenceSyncCommand(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	owner := d.Open(t, db.RoleOwner)
	environ := []string{"KOALA_ENV=test", "KOALA_OWNER_DATABASE_URL=" + d.URL(db.RoleOwner)}
	const (
		university = "a1c346d6-2872-4194-9b98-3f0ea5b838ec" // ADIYAMAN ÜNİVERSİTESİ
		department = "97d43fdf-0a52-4f95-9dab-8d5adbf2d37f" // Psikoloji (İngilizce)
	)
	for _, stmt := range []string{
		"UPDATE universities SET name = 'ELLE DEĞİŞTİRİLDİ' WHERE id = '" + university + "'",
		"UPDATE departments SET retired_at = now() WHERE id = '" + department + "'",
	} {
		if _, err := owner.Pool().Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	drift := func() (name string, retired bool) {
		t.Helper()
		err := owner.Pool().QueryRow(t.Context(), `
			SELECT (SELECT name FROM universities WHERE id = $1), (SELECT retired_at IS NOT NULL FROM departments WHERE id = $2)`,
			university, department).Scan(&name, &retired)
		if err != nil {
			t.Fatal(err)
		}
		return name, retired
	}
	syncCmd := func(args ...string) map[string][4]int {
		t.Helper()
		code, stdout, stderr := run(args, environ...)
		if code != ExitOK {
			t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
		}
		return summaryOf(t, stdout)
	}

	code, stdout, _ := run([]string{"reference", "sync", "--dry-run"}, environ...)
	if code != ExitOK || !strings.Contains(stdout, "dry run: nothing was written") {
		t.Fatalf("dry run: exit %d, stdout %s; want the dry run said", code, stdout)
	}
	s := summaryOf(t, stdout)
	if s["universities"] != [4]int{0, 1, 0, 222} || s["departments"] != [4]int{0, 1, 0, 11249} || s["districts"] != [4]int{0, 0, 0, 995} {
		t.Fatalf("dry run = %v", s)
	}
	if name, retired := drift(); name != "ELLE DEĞİŞTİRİLDİ" || !retired {
		t.Fatalf("the dry run wrote: name %q, retired %v", name, retired)
	}
	s = syncCmd("reference", "sync")
	if s["universities"] != [4]int{0, 1, 0, 222} || s["departments"] != [4]int{0, 1, 0, 11249} {
		t.Fatalf("sync = %v", s)
	}
	if name, retired := drift(); name != "ADIYAMAN ÜNİVERSİTESİ" || retired {
		t.Fatalf("after the sync: name %q, retired %v", name, retired)
	}
	s = syncCmd("reference", "sync")
	for table, n := range s {
		if n[0]+n[1]+n[2] != 0 {
			t.Errorf("third run: %s %v, want nothing to do", table, n)
		}
	}
	if len(s) != 6 {
		t.Errorf("summary has %d tables, want 6: %v", len(s), s)
	}
}
