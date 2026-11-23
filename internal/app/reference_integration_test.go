//go:build integration

package app

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
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

// conditional sends a GET without a bearer, with one If-None-Match field
// line per value.
func (a *authApp) conditional(t *testing.T, path string, ifNoneMatch ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
	req.RemoteAddr = "192.0.2.1:40000"
	req.Header.Set(httpx.HeaderRequestID, "req-reference")
	setClient(req.Header)
	for _, v := range ifNoneMatch {
		req.Header.Add("If-None-Match", v)
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

// before fails unless first comes before second in items.
func before(t *testing.T, items []referenceItem, first, second string) {
	t.Helper()
	if i, j := position(items, first), position(items, second); i < 0 || j < 0 || i >= j {
		t.Errorf("%q at %d, %q at %d; want the first before the second", first, i, second, j)
	}
}

// TestReferenceOverHTTP reads the reference lists as an anonymous caller
// (MED-012): country → city → district and university → faculty →
// department, in Turkish alphabetical order; an unknown parent is
// REFERENCE_ITEM_NOT_FOUND, a parent without children an empty list.
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
		before(t, cities, w[0], w[1])
	}
	ankara := named(t, cities, "ANKARA")
	districts := a.list(t, "/v1/reference/cities/"+ankara.ID.String()+"/districts")
	if len(districts) != 25 {
		t.Fatalf("MED-012: Ankara has %d districts, want 25", len(districts))
	}
	inTurkishOrder(t, districts)

	universities := a.list(t, "/v1/reference/universities")
	if len(universities) != 231 {
		t.Fatalf("%d universities, want 231", len(universities))
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
		before(t, universities, w[0], w[1])
	}

	// Mixed-case names: Giresun's faculties put "Tıp" before "Tirebolu"
	// (ı before i; the byte order and the root collation both put i
	// first), Ankara's DTCF puts "Coğrafya" before "Çağdaş …" (the root
	// collation reads Ç as C and puts "Çağ" first) and "Çağdaş …" before
	// "Dilbilimi" (the byte order puts Ç after every ASCII letter).
	giresun := named(t, universities, "GİRESUN ÜNİVERSİTESİ")
	faculties := a.list(t, "/v1/reference/universities/"+giresun.ID.String()+"/faculties")
	before(t, faculties, "Tıp Fakültesi", "Tirebolu İletişim Fakültesi")
	dtcf := a.list(t, "/v1/reference/faculties/14b340a3-2419-40fe-8208-e4a7b57c768d/departments")
	before(t, dtcf, "Coğrafya", "Çağdaş Türk Lehçeleri ve Edebiyatları")
	before(t, dtcf, "Çağdaş Türk Lehçeleri ve Edebiyatları", "Dilbilimi")

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
}

// TestReferenceListsHideRetiredRows binds each retired_at filter of the
// list queries (db/queries/reference/reference.sql) on its own: a retired
// row leaves its list while its parent stays in use, and a retired
// parent's list is the unknown parent's 404. Each step retires one row.
func TestReferenceListsHideRetiredRows(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	owner := a.dbt.Open(t, db.RoleOwner)
	retire := func(table string, id uuid.UUID) {
		t.Helper()
		if _, err := owner.Pool().Exec(t.Context(), "UPDATE "+table+" SET retired_at = now() WHERE id = $1", id); err != nil {
			t.Fatal(err)
		}
	}
	notFound := string(reference.CodeReferenceItemNotFound)
	gone := func(path string) {
		t.Helper()
		wantStatus(t, a.call(t, http.MethodGet, path, "", ""), http.StatusNotFound, notFound)
	}
	turkey := "/v1/reference/countries/26a60ab5-db94-4167-8678-1bb6a85fce30/cities"

	// A city of a country in use (ListCities' join filter).
	adana := named(t, a.list(t, turkey), "ADANA")
	retire("cities", adana.ID)
	if got := a.list(t, turkey); len(got) != 80 || position(got, "ADANA") >= 0 {
		t.Errorf("after retiring Adana: %d cities", len(got))
	}
	gone("/v1/reference/cities/" + adana.ID.String() + "/districts")

	// A district of a city in use (ListDistricts' join filter).
	ankara := named(t, a.list(t, turkey), "ANKARA")
	districts := "/v1/reference/cities/" + ankara.ID.String() + "/districts"
	retire("districts", named(t, a.list(t, districts), "ÇANKAYA").ID)
	if got := a.list(t, districts); len(got) != 24 || position(got, "ÇANKAYA") >= 0 {
		t.Errorf("after retiring Çankaya: %d districts", len(got))
	}

	// A university (ListUniversities' filter) and its faculties (404).
	universities := a.list(t, "/v1/reference/universities")
	odtu := named(t, universities, "ORTA DOĞU TEKNİK ÜNİVERSİTESİ")
	retire("universities", odtu.ID)
	if got := a.list(t, "/v1/reference/universities"); len(got) != 230 || position(got, odtu.Name) >= 0 {
		t.Errorf("after retiring ODTÜ: %d universities", len(got))
	}
	gone("/v1/reference/universities/" + odtu.ID.String() + "/faculties")

	// One faculty of a university in use (ListFaculties' join filter),
	// and its departments (404).
	giresun := "/v1/reference/universities/" + named(t, universities, "GİRESUN ÜNİVERSİTESİ").ID.String() + "/faculties"
	faculties := a.list(t, giresun)
	tip := named(t, faculties, "Tıp Fakültesi")
	retire("faculties", tip.ID)
	if got := a.list(t, giresun); len(got) != len(faculties)-1 || position(got, tip.Name) >= 0 {
		t.Errorf("after retiring Giresun's Tıp Fakültesi: %d of %d faculties", len(got), len(faculties))
	}
	gone("/v1/reference/faculties/" + tip.ID.String() + "/departments")

	// One department of a faculty in use (ListDepartments' join filter).
	dtcf := "/v1/reference/faculties/14b340a3-2419-40fe-8208-e4a7b57c768d/departments"
	departments := a.list(t, dtcf)
	geo := named(t, departments, "Coğrafya")
	retire("departments", geo.ID)
	if got := a.list(t, dtcf); len(got) != len(departments)-1 || position(got, geo.Name) >= 0 {
		t.Errorf("after retiring Coğrafya: %d of %d departments", len(got), len(departments))
	}

	// The country last (ListCountries' filter): its list is empty and its
	// cities' list is the unknown country's.
	retire("countries", uuid.MustParse("26a60ab5-db94-4167-8678-1bb6a85fce30"))
	if got := a.list(t, "/v1/reference/countries"); len(got) != 0 {
		t.Errorf("after retiring Türkiye: countries %+v", got)
	}
	gone(turkey)
}

// TestReferenceOrderBreaksTiesByID: two siblings of one name come in id
// order, whatever order they were written in, so the order is total.
func TestReferenceOrderBreaksTiesByID(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	owner := a.dbt.Open(t, db.RoleOwner)
	faculty := uuid.New()
	low, high := uuid.MustParse("00000000-0000-4000-8000-000000000001"), uuid.MustParse("ffffffff-ffff-4fff-bfff-ffffffffffff")
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO faculties (id, university_id, name) VALUES ($1, 'a1c346d6-2872-4194-9b98-3f0ea5b838ec', 'Sıra Fakültesi')", []any{faculty}},
		// The larger id first: a heap scan without the id tie-break would
		// return it first.
		{"INSERT INTO departments (id, faculty_id, degree, name) VALUES ($1, $2, 'BACHELOR', 'Aynı Ad')", []any{high, faculty}},
		{"INSERT INTO departments (id, faculty_id, degree, name) VALUES ($1, $2, 'BACHELOR', 'Aynı Ad')", []any{low, faculty}},
		{"INSERT INTO departments (id, faculty_id, degree, name) VALUES ($1, $2, 'BACHELOR', 'Ayna')", []any{uuid.New(), faculty}},
	} {
		if _, err := owner.Pool().Exec(t.Context(), stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	got := a.list(t, "/v1/reference/faculties/"+faculty.String()+"/departments")
	if len(got) != 3 || got[0].Name != "Ayna" || got[1].ID != low || got[2].ID != high {
		t.Fatalf("departments = %+v, want Ayna, then the two of one name in id order", got)
	}
}

// TestReferenceListsAreCacheable: every list is public for a day and
// carries the ETag of the data version and its revision; a matching
// If-None-Match (exact, weak, in a list, on several field lines, or *) is
// 304 without a body, another tag or none is 200, and a missing parent
// stays 404 whatever the client sends. A repair under the same version
// and a new version each change the tag, and the old one gets the new
// list.
func TestReferenceListsAreCacheable(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	paths := []string{
		"/v1/reference/countries",
		"/v1/reference/countries/26a60ab5-db94-4167-8678-1bb6a85fce30/cities",
		"/v1/reference/cities/82ca8c6a-7a71-4288-9248-740256024599/districts",
		"/v1/reference/universities",
		"/v1/reference/universities/b8cf7211-a21b-4e1f-ad1a-7bacdd914fc3/faculties",
		"/v1/reference/faculties/a8004de5-6f16-41ea-9841-42ed646684cf/departments",
	}
	const tag = `"reference-v2.1"`
	for _, path := range paths {
		rec := a.conditional(t, path)
		wantStatus(t, rec, http.StatusOK, "")
		if rec.Header().Get("ETag") != tag || rec.Header().Get("Cache-Control") != "public, max-age=86400" {
			t.Errorf("%s: ETag %q, Cache-Control %q", path, rec.Header().Get("ETag"), rec.Header().Get("Cache-Control"))
		}
		for _, inm := range [][]string{{tag}, {"W/" + tag}, {`"other", ` + tag}, {"*"}, {`"other"`, tag}} {
			rec := a.conditional(t, path, inm...)
			if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != tag ||
				rec.Header().Get("Cache-Control") != "public, max-age=86400" {
				t.Errorf("%s with If-None-Match %q: %d %q (ETag %q)", path, inm, rec.Code, rec.Body, rec.Header().Get("ETag"))
			}
		}
		if rec := a.conditional(t, path, `"reference-v0.0"`); rec.Code != http.StatusOK {
			t.Errorf("%s with another tag: %d", path, rec.Code)
		}
	}
	missing := a.conditional(t, "/v1/reference/faculties/"+uuid.NewString()+"/departments", "*")
	wantStatus(t, missing, http.StatusNotFound, string(reference.CodeReferenceItemNotFound))

	// A repair under the same version: the sync writes the row and moves
	// the revision; the client's 304 tag now gets the repaired list.
	owner := a.dbt.Open(t, db.RoleOwner)
	if _, err := owner.Pool().Exec(t.Context(), "UPDATE universities SET name = 'BOZUK AD' WHERE id = 'a1c346d6-2872-4194-9b98-3f0ea5b838ec'"); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := run([]string{"reference", "sync"}, "KOALA_ENV=test", "KOALA_OWNER_DATABASE_URL="+a.dbt.URL(db.RoleOwner)); code != ExitOK {
		t.Fatalf("reference sync: exit %d\n%s\n%s", code, stdout, stderr)
	}
	rec := a.conditional(t, "/v1/reference/universities", tag)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"reference-v2.2"` ||
		position(decodeBody[referenceList](t, rec).Items, "ADIYAMAN ÜNİVERSİTESİ") < 0 {
		t.Fatalf("after the repair with the old tag: %d, ETag %q", rec.Code, rec.Header().Get("ETag"))
	}

	if _, err := owner.Pool().Exec(t.Context(), "UPDATE reference_data_version SET version = 3"); err != nil {
		t.Fatal(err)
	}
	rec = a.conditional(t, paths[0], `"reference-v2.2"`)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"reference-v3.2"` {
		t.Fatalf("after version 3 with the old tag: %d, ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
	// A filtered list is the same representation's selection: it carries
	// the same tag and answers the same If-None-Match.
	filtered := a.conditional(t, "/v1/reference/universities?degree=ASSOCIATE", `"reference-v3.2"`)
	if filtered.Code != http.StatusNotModified || filtered.Header().Get("ETag") != `"reference-v3.2"` {
		t.Fatalf("a filtered list with the current tag: %d, ETag %q", filtered.Code, filtered.Header().Get("ETag"))
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

	odtu := named(t, a.list(t, "/v1/reference/universities?degree=BACHELOR"), "ORTA DOĞU TEKNİK ÜNİVERSİTESİ")
	faculty := a.list(t, "/v1/reference/universities/"+odtu.ID.String()+"/faculties?degree=BACHELOR")[0]
	department := a.list(t, "/v1/reference/faculties/"+faculty.ID.String()+"/departments?degree=BACHELOR")[0]

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
// reports the drift row by row and writes nothing, a real run repairs it,
// and a run after that has nothing to do. The drift is what a hand edit or
// an older release leaves: a renamed university, a retired department and
// a department the files do not list.
func TestReferenceSyncCommand(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	owner := d.Open(t, db.RoleOwner)
	environ := []string{"KOALA_ENV=test", "KOALA_OWNER_DATABASE_URL=" + d.URL(db.RoleOwner)}
	const (
		university = "a1c346d6-2872-4194-9b98-3f0ea5b838ec" // ADIYAMAN ÜNİVERSİTESİ
		department = "97d43fdf-0a52-4f95-9dab-8d5adbf2d37f" // Psikoloji (İngilizce)
		faculty    = "a8004de5-6f16-41ea-9841-42ed646684cf" // its faculty
	)
	extra := uuid.New()
	for _, stmt := range []string{
		"UPDATE universities SET name = 'ELLE DEĞİŞTİRİLDİ' WHERE id = '" + university + "'",
		"UPDATE departments SET retired_at = now() WHERE id = '" + department + "'",
		"INSERT INTO departments (id, faculty_id, degree, name) VALUES ('" + extra.String() + "', '" + faculty + "', 'BACHELOR', 'Fazladan Bölüm')",
	} {
		if _, err := owner.Pool().Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	drift := func() (name string, retired, extraRetired bool) {
		t.Helper()
		err := owner.Pool().QueryRow(t.Context(), `
			SELECT (SELECT name FROM universities WHERE id = $1), (SELECT retired_at IS NOT NULL FROM departments WHERE id = $2),
			       (SELECT retired_at IS NOT NULL FROM departments WHERE id = $3)`,
			university, department, extra).Scan(&name, &retired, &extraRetired)
		if err != nil {
			t.Fatal(err)
		}
		return name, retired, extraRetired
	}
	syncCmd := func(args ...string) (map[string][4]int, string) {
		t.Helper()
		code, stdout, stderr := run(args, environ...)
		if code != ExitOK {
			t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
		}
		return summaryOf(t, stdout), stdout
	}
	wantSummary := map[string][4]int{
		"countries": {0, 0, 0, 1}, "cities": {0, 0, 0, 81}, "districts": {0, 0, 0, 973},
		"universities": {0, 1, 0, 230}, "faculties": {0, 0, 0, 3022}, "departments": {0, 1, 1, 14710},
		"department_source_codes": {0, 0, 0, 9254},
	}
	// The rows, each with its id and name: the rename with both names, the
	// department back in use, the one retired.
	wantRows := []string{
		`~ universities ` + university + ` "ELLE DEĞİŞTİRİLDİ" -> "ADIYAMAN ÜNİVERSİTESİ"`,
		`^ departments ` + department + ` "Psikoloji (İngilizce)"`,
		`- departments ` + extra.String() + ` "Fazladan Bölüm"`,
	}

	s, stdout := syncCmd("reference", "sync", "--dry-run")
	if !strings.Contains(stdout, "dry run: nothing was written") || !strings.Contains(stdout, "data version 2, database version 2") {
		t.Fatalf("dry run: stdout %s; want the dry run and the versions said", stdout)
	}
	if !maps.Equal(s, wantSummary) {
		t.Fatalf("dry run = %v, want %v", s, wantSummary)
	}
	for _, row := range wantRows {
		if !strings.Contains(stdout, row+"\n") {
			t.Errorf("dry run lacks the row %s:\n%s", row, stdout)
		}
	}
	if name, retired, extraRetired := drift(); name != "ELLE DEĞİŞTİRİLDİ" || !retired || extraRetired {
		t.Fatalf("the dry run wrote: name %q, retired %v, extra retired %v", name, retired, extraRetired)
	}
	s, stdout = syncCmd("reference", "sync")
	if !maps.Equal(s, wantSummary) || strings.Contains(stdout, "dry run") {
		t.Fatalf("sync = %v\n%s", s, stdout)
	}
	for _, row := range wantRows {
		if !strings.Contains(stdout, row+"\n") {
			t.Errorf("sync lacks the row %s", row)
		}
	}
	if name, retired, extraRetired := drift(); name != "ADIYAMAN ÜNİVERSİTESİ" || retired || !extraRetired {
		t.Fatalf("after the sync: name %q, retired %v, extra retired %v", name, retired, extraRetired)
	}
	s, stdout = syncCmd("reference", "sync")
	for table, n := range s {
		if n[0]+n[1]+n[2] != 0 {
			t.Errorf("third run: %s %v, want nothing to do", table, n)
		}
	}
	if len(s) != 7 || strings.Contains(stdout, "\n+ ") || strings.Contains(stdout, "\n- ") {
		t.Errorf("third run: %v\n%s", s, stdout)
	}
}

// TestMigrateUpHoldsTheRetireLimit: `koala migrate up` stops when its
// sync would retire more than a table's limit, names the table and the
// variable that allows it, and writes nothing; with
// KOALA_REFERENCE_ALLOW_RETIRE it goes through (ADR-0037).
func TestMigrateUpHoldsTheRetireLimit(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	owner := d.Open(t, db.RoleOwner)
	environ := []string{"KOALA_ENV=test", "KOALA_OWNER_DATABASE_URL=" + d.URL(db.RoleOwner)}
	// 800 departments the files do not list: with 15,511 rows in use the
	// limit is 775 (5 %), so retiring them all is above it.
	if _, err := owner.Pool().Exec(t.Context(), `
		INSERT INTO departments (id, faculty_id, degree, name)
		SELECT gen_random_uuid(), 'a8004de5-6f16-41ea-9841-42ed646684cf', 'BACHELOR', 'Fazla ' || n FROM generate_series(1, 800) AS n`); err != nil {
		t.Fatal(err)
	}
	inUse := func() int {
		t.Helper()
		var n int
		if err := owner.Pool().QueryRow(t.Context(), "SELECT count(*) FROM departments WHERE retired_at IS NULL").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	code, stdout, _ := run([]string{"migrate", "up"}, environ...)
	if code != ExitError || !strings.Contains(stdout, `"table":"departments"`) || !strings.Contains(stdout, "KOALA_REFERENCE_ALLOW_RETIRE=departments:800@2") {
		t.Fatalf("migrate up over the limit: exit %d\n%s", code, stdout)
	}
	if n := inUse(); n != 15511 {
		t.Fatalf("after the refusal %d departments in use, want 15511", n)
	}
	// An allowance left over from another data version lifts nothing and
	// says so.
	code, stdout, _ = run([]string{"migrate", "up"}, append(environ, "KOALA_REFERENCE_ALLOW_RETIRE=departments:800@3")...)
	if code != ExitError || !strings.Contains(stdout, "another data version and was ignored") {
		t.Fatalf("an allowance for version 3: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = run([]string{"migrate", "up"}, append(environ, "KOALA_REFERENCE_ALLOW_RETIRE=departments:800@2")...)
	if code != ExitOK || !strings.Contains(stdout, `"retired":800`) {
		t.Fatalf("migrate up allowed: exit %d\n%s", code, stdout)
	}
	if n := inUse(); n != 14711 {
		t.Fatalf("after the allowed sync %d departments in use, want 14711", n)
	}
	// The allowance stays set in the next release: nothing to retire, it
	// lifts nothing and the up succeeds.
	code, stdout, _ = run([]string{"migrate", "up"}, append(environ, "KOALA_REFERENCE_ALLOW_RETIRE=departments:800@3")...)
	if code != ExitOK || !strings.Contains(stdout, "reference allowance ignored") {
		t.Fatalf("a stale allowance with nothing to retire: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = run([]string{"migrate", "up"}, append(environ, "KOALA_REFERENCE_ALLOW_RETIRE=users:1@1")...)
	if code != ExitError || !strings.Contains(stdout, "KOALA_REFERENCE_ALLOW_RETIRE") {
		t.Fatalf("an allowance for another table: exit %d\n%s", code, stdout)
	}
}

// TestReferenceListsFilterByDegree: the academic lists take the degree of
// an education record (ADR-0038). ASSOCIATE lists the associate degree
// programs and the faculties and universities that have one in use; every
// other degree lists the bachelor's programs (MASTER as BACHELOR); without
// one the lists are whole. A parent whose children are all of the other
// degree is an empty list, not a 404, and a degree outside the contract is
// refused before the handler. Each retired_at filter inside the degree
// filters is bound by a step of its own.
func TestReferenceListsFilterByDegree(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	owner := a.dbt.Open(t, db.RoleOwner)
	const (
		thk     = "/v1/reference/universities/20b200c7-d3d3-4e77-ba67-2b01c96b92a8/faculties"
		myo     = "01a207cf-0348-7890-bf14-c44956448609" // Ankara Havacılık MYO
		aof     = "/v1/reference/faculties/75739eb2-88b3-403d-a2d4-5ea29f72db64/departments"
		konakID = "01a207cf-033b-777f-9064-34e3659cbf35" // İZMİR KONAK MESLEK YÜKSEKOKULU
	)
	ids := func(items []referenceItem) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(items))
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}

	// Universities: 197 with an associate program, 223 with a bachelor's.
	all := a.list(t, "/v1/reference/universities")
	associate := a.list(t, "/v1/reference/universities?degree=ASSOCIATE")
	bachelor := a.list(t, "/v1/reference/universities?degree=BACHELOR")
	if len(all) != 231 || len(associate) != 197 || len(bachelor) != 223 {
		t.Fatalf("universities: %d, %d with an associate program, %d with a bachelor's; want 231, 197, 223",
			len(all), len(associate), len(bachelor))
	}
	if position(associate, "İZMİR KONAK MESLEK YÜKSEKOKULU") < 0 || position(bachelor, "İZMİR KONAK MESLEK YÜKSEKOKULU") >= 0 ||
		position(associate, "ORTA DOĞU TEKNİK ÜNİVERSİTESİ") >= 0 || position(bachelor, "ORTA DOĞU TEKNİK ÜNİVERSİTESİ") < 0 {
		t.Errorf("İzmir Konak MYO teaches associate programs only, ODTÜ bachelor's only")
	}
	for _, degree := range []string{"MASTER", "DOCTORATE"} {
		if got := a.list(t, "/v1/reference/universities?degree="+degree); !slices.Equal(ids(got), ids(bachelor)) {
			t.Errorf("%s: %d universities, want the bachelor's list", degree, len(got))
		}
	}

	// Faculties: Türk Hava Kurumu's two vocational schools and its four
	// faculties.
	faculties := a.list(t, thk)
	thkAssociate := a.list(t, thk+"?degree=ASSOCIATE")
	thkBachelor := a.list(t, thk+"?degree=BACHELOR")
	if len(faculties) != 6 || len(thkAssociate) != 2 || len(thkBachelor) != 4 ||
		position(thkAssociate, "Ankara Havacılık Meslek Yüksekokulu") < 0 || position(thkBachelor, "Mühendislik Fakültesi") < 0 {
		t.Fatalf("THK faculties: %+v; associate %+v; bachelor %+v", faculties, thkAssociate, thkBachelor)
	}

	// Departments: Atatürk's Açıköğretim Fakültesi teaches both degrees.
	departments := a.list(t, aof)
	aofAssociate := a.list(t, aof+"?degree=ASSOCIATE")
	aofBachelor := a.list(t, aof+"?degree=BACHELOR")
	if len(aofAssociate) == 0 || len(aofBachelor) == 0 || len(aofAssociate)+len(aofBachelor) != len(departments) {
		t.Fatalf("Açıköğretim: %d departments, %d associate, %d bachelor's", len(departments), len(aofAssociate), len(aofBachelor))
	}
	if position(aofAssociate, "Halkla İlişkiler ve Tanıtım (Önlisans) (Açıköğretim)") < 0 ||
		position(aofBachelor, "Halkla İlişkiler ve Tanıtım (Açıköğretim)") < 0 ||
		position(aofAssociate, "Halkla İlişkiler ve Tanıtım (Açıköğretim)") >= 0 {
		t.Errorf("Açıköğretim's two Halkla İlişkiler programs are not where their degrees say")
	}
	// A vocational school has no bachelor's program: an empty list.
	if got := a.list(t, "/v1/reference/faculties/"+myo+"/departments?degree=BACHELOR"); len(got) != 0 {
		t.Errorf("Ankara Havacılık MYO's bachelor's programs: %+v", got)
	}
	if got := a.list(t, "/v1/reference/universities/"+konakID+"/faculties?degree=BACHELOR"); len(got) != 0 {
		t.Errorf("İzmir Konak MYO's faculties with a bachelor's program: %+v", got)
	}
	for _, path := range []string{"/v1/reference/universities?degree=PHD", thk + "?degree=associate", aof + "?degree=LISANS"} {
		wantStatus(t, a.call(t, http.MethodGet, path, "", ""), http.StatusBadRequest, "REQUEST_INVALID")
	}

	// The retired_at filters inside the degree filters, one at a time.
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Pool().Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// A retired associate program leaves the list of its degree.
	retired := named(t, aofAssociate, "Halkla İlişkiler ve Tanıtım (Önlisans) (Açıköğretim)")
	exec("UPDATE departments SET retired_at = now() WHERE id = $1", retired.ID)
	if got := a.list(t, aof+"?degree=ASSOCIATE"); len(got) != len(aofAssociate)-1 || position(got, retired.Name) >= 0 {
		t.Errorf("after retiring %q: %d associate programs", retired.Name, len(got))
	}
	// A faculty whose associate programs are all retired leaves the
	// faculties with one (ListFaculties' EXISTS).
	exec("UPDATE departments SET retired_at = now() WHERE faculty_id = $1", myo)
	if got := a.list(t, thk+"?degree=ASSOCIATE"); len(got) != 1 || position(got, "Ankara Havacılık Meslek Yüksekokulu") >= 0 {
		t.Errorf("after retiring Ankara Havacılık MYO's programs: %+v", got)
	}
	// A university whose only school is retired, with its programs in
	// use, leaves the universities with one (ListUniversities' faculty
	// filter).
	exec("UPDATE faculties SET retired_at = now() WHERE university_id = $1", konakID)
	if got := a.list(t, "/v1/reference/universities?degree=ASSOCIATE"); len(got) != 196 || position(got, "İZMİR KONAK MESLEK YÜKSEKOKULU") >= 0 {
		t.Errorf("after retiring İzmir Konak's school: %d universities", len(got))
	}
	// A university whose faculties are in use but whose associate programs
	// are all retired leaves it too (ListUniversities' department filter):
	// Türk Hava Kurumu, once İzmir Havacılık MYO's programs follow.
	exec(`UPDATE departments SET retired_at = now() WHERE faculty_id IN (
		SELECT id FROM faculties WHERE university_id = '20b200c7-d3d3-4e77-ba67-2b01c96b92a8')`)
	if got := a.list(t, "/v1/reference/universities?degree=ASSOCIATE"); len(got) != 195 || position(got, "TÜRK HAVA KURUMU ÜNİVERSİTESİ") >= 0 {
		t.Errorf("after retiring THK's programs: %d universities", len(got))
	}
}
