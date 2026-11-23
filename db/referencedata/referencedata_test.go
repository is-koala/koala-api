package referencedata

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func load(t *testing.T) *Dataset {
	t.Helper()
	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestDatasetCounts binds the embedded files to their row counts after the
// cleanup (README.md): a file cut short, a lost line, a cleanup undone or
// a file that stopped being embedded is red here, before any database is
// involved.
func TestDatasetCounts(t *testing.T) {
	t.Parallel()

	d := load(t)
	for _, tc := range []struct {
		table string
		rows  []Row
		want  int
	}{
		{"countries", d.Countries, 1},
		{"cities", d.Cities, 81},
		{"districts", d.Districts, 973},
		{"universities", d.Universities, 223},
		{"faculties", d.Faculties, 2043},
		{"departments", d.Departments, 7567},
	} {
		if len(tc.rows) != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.table, len(tc.rows), tc.want)
		}
	}
	if d.Version != 1 {
		t.Errorf("Version = %d, want 1", d.Version)
	}
	if got, want := Tables(), []string{"countries", "cities", "districts", "universities", "faculties", "departments"}; !slices.Equal(got, want) {
		t.Errorf("Tables() = %v, want %v (parents first)", got, want)
	}
}

// TestDatasetKeepsJavaIDs spot-checks that the ids are Java's (ADR-0037):
// a record that names a Java id finds the same row here.
func TestDatasetKeepsJavaIDs(t *testing.T) {
	t.Parallel()

	d := load(t)
	for _, tc := range []struct {
		rows   []Row
		id     string
		parent string
		name   string
	}{
		{d.Countries, "26a60ab5-db94-4167-8678-1bb6a85fce30", "", "TÜRKİYE"},
		{d.Cities, "82ca8c6a-7a71-4288-9248-740256024599", "26a60ab5-db94-4167-8678-1bb6a85fce30", "ADANA"},
		{d.Districts, "a058819e-e2db-41d9-b7ae-245f1ec7b8b9", "", "FATİH"},
		{d.Universities, "a1c346d6-2872-4194-9b98-3f0ea5b838ec", "", "ADIYAMAN ÜNİVERSİTESİ"},
		{d.Faculties, "a8004de5-6f16-41ea-9841-42ed646684cf", "b8cf7211-a21b-4e1f-ad1a-7bacdd914fc3", "İnsan ve Toplum Bilimleri Fakültesi"},
		{d.Departments, "97d43fdf-0a52-4f95-9dab-8d5adbf2d37f", "a8004de5-6f16-41ea-9841-42ed646684cf", "Psikoloji (İngilizce)"},
		// İYTE: listed as a faculty of İzmir Katip Çelebi, a university of
		// its own under the same id, with its three faculties (the
		// cleanup, rule 4).
		{d.Universities, "f458e88f-1201-4231-b4f7-7ede110e9c05", "", "İZMİR YÜKSEK TEKNOLOJİ ENSTİTÜSÜ"},
		{d.Faculties, "8119fff5-7078-4943-979e-6aa702c2be72", "f458e88f-1201-4231-b4f7-7ede110e9c05", "Fen Fakültesi"},
		{d.Faculties, "b2903de4-251f-4475-b434-bc033f0c4bd4", "f458e88f-1201-4231-b4f7-7ede110e9c05", "Mimarlık Fakültesi"},
		{d.Faculties, "54fc7dd0-cad2-434b-ba49-7c896ffc3e54", "f458e88f-1201-4231-b4f7-7ede110e9c05", "Mühendislik Fakültesi"},
		// DERECİK (Hakkari), split from Şemdinli in 2018: the one row with
		// an id of its own (rule 2).
		{d.Districts, "01a2077d-d01b-7bb5-8d9b-9c94173f5c34", "3eaa6616-f66c-409c-a49b-4342d879fa2c", "DERECİK"},
	} {
		i := slices.IndexFunc(tc.rows, func(r Row) bool { return r.ID == uuid.MustParse(tc.id) })
		if i < 0 {
			t.Errorf("%s (%s) is missing", tc.id, tc.name)
			continue
		}
		r := tc.rows[i]
		if r.Name != tc.name || (tc.parent != "" && r.Parent != uuid.MustParse(tc.parent)) {
			t.Errorf("%s = %+v, want %q under %s", tc.id, r, tc.name, tc.parent)
		}
	}
	// Eminönü joined Fatih in 2008 and Java retired it (2026-09-05-04):
	// it is not in use, so the file does not list it.
	eminonu := uuid.MustParse("9b43a8a6-f165-460d-80b1-e711a2db5714")
	if slices.ContainsFunc(d.Districts, func(r Row) bool { return r.ID == eminonu }) {
		t.Error("Eminönü is listed; Java retired it")
	}
}

// TestCleanupHolds binds the cleanup of tools/clean.py to the files
// (ADR-0037 Karar 4a): a file written back from Java's snapshot without
// it is red here, rule by rule.
func TestCleanupHolds(t *testing.T) {
	t.Parallel()

	d := load(t)
	all := slices.Concat(d.Countries, d.Cities, d.Districts, d.Universities, d.Faculties, d.Departments)

	// 1. White space.
	for _, r := range all {
		if strings.Contains(r.Name, "  ") || strings.ContainsRune(r.Name, '\u00a0') {
			t.Errorf("%s %q: double space or NBSP", r.ID, r.Name)
		}
	}
	// 2. No MERKEZ in a metropolitan province; the 51 others keep theirs.
	metropolitan := map[string]bool{}
	for _, c := range []string{
		"ADANA", "ANKARA", "ANTALYA", "AYDIN", "BALIKESİR", "BURSA", "DENİZLİ", "DİYARBAKIR", "ERZURUM",
		"ESKİŞEHİR", "GAZİANTEP", "HATAY", "İSTANBUL", "İZMİR", "KAHRAMANMARAŞ", "KAYSERİ", "KOCAELİ", "KONYA",
		"MALATYA", "MANİSA", "MARDİN", "MERSİN", "MUĞLA", "ORDU", "SAKARYA", "SAMSUN", "ŞANLIURFA", "TEKİRDAĞ",
		"TRABZON", "VAN",
	} {
		metropolitan[c] = true
	}
	cityName := map[uuid.UUID]string{}
	cities := map[string]bool{}
	for _, c := range d.Cities {
		cityName[c.ID] = c.Name
		cities[c.Name] = true
	}
	found := 0
	for name := range metropolitan {
		if cities[name] {
			found++
		}
	}
	if found != 30 {
		t.Errorf("%d of the 30 metropolitan provinces are cities", found)
	}
	merkez := 0
	for _, r := range d.Districts {
		if r.Name == "MERKEZ" {
			merkez++
			if metropolitan[cityName[r.Parent]] {
				t.Errorf("MERKEZ %s in the metropolitan province %s", r.ID, cityName[r.Parent])
			}
		}
	}
	if merkez != 51 {
		t.Errorf("%d MERKEZ districts, want one per non-metropolitan province (51)", merkez)
	}
	// Every province has its official number of districts: Hakkari five
	// with DERECİK, 973 in all.
	hakkari := uuid.MustParse("3eaa6616-f66c-409c-a49b-4342d879fa2c")
	inHakkari := 0
	for _, r := range d.Districts {
		if r.Parent == hakkari {
			inHakkari++
		}
	}
	if inHakkari != 5 {
		t.Errorf("Hakkari has %d districts, want 5 (Çukurca, Derecik, Merkez, Şemdinli, Yüksekova)", inHakkari)
	}
	// 3 and 4. YÖK's note and İYTE are no faculties, and the SBÜ quota
	// note is no university.
	for _, id := range []string{
		"9c91f25a-d029-452b-8008-768f117bb9e4", "339d2654-b490-4863-8963-35f4b27980e8", "f458e88f-1201-4231-b4f7-7ede110e9c05",
	} {
		if slices.ContainsFunc(d.Faculties, func(r Row) bool { return r.ID == uuid.MustParse(id) }) {
			t.Errorf("faculty %s is listed", id)
		}
	}
	for _, r := range d.Universities {
		if strings.Contains(r.Name, "EĞİTİM ALACAKLAR") || r.ID == uuid.MustParse("147a9f46-e2e4-41df-9fd9-e11a086e224f") {
			t.Errorf("university %s %q: the SBÜ quota note", r.ID, r.Name)
		}
	}
	// İYTE has its three faculties, and İzmir Katip Çelebi no longer has
	// two departments of one name (İYTE's programs under it).
	iyte, ikcu := uuid.MustParse("f458e88f-1201-4231-b4f7-7ede110e9c05"), uuid.MustParse("2b04c0a9-d5da-457f-a1f6-975fe0545667")
	universityOf := map[uuid.UUID]uuid.UUID{}
	iyteFaculties := 0
	for _, f := range d.Faculties {
		universityOf[f.ID] = f.Parent
		if f.Parent == iyte {
			iyteFaculties++
		}
	}
	if iyteFaculties != 3 {
		t.Errorf("İYTE has %d faculties, want 3 (Fen, Mimarlık, Mühendislik)", iyteFaculties)
	}
	ikcuNames, iyteDepartments := map[string]bool{}, 0
	for _, r := range d.Departments {
		switch universityOf[r.Parent] {
		case ikcu:
			if ikcuNames[r.Name] {
				t.Errorf("İzmir Katip Çelebi has two departments named %q", r.Name)
			}
			ikcuNames[r.Name] = true
		case iyte:
			iyteDepartments++
		}
	}
	if iyteDepartments != 18 {
		t.Errorf("İYTE has %d departments, want 18", iyteDepartments)
	}
	// 5 to 7. One row per (parent, name), and no quota or fee suffix.
	quota := regexp.MustCompile(`\((Burslu|%\d+ İndirimli|Ücretli|İÖ|KKTC Uyruklu|M\.T\.O\.K\.|Erkek|Kız|Milli Savunma Bakanlığı Adına|İçişleri Bakanlığı Adına)\)`)
	for _, rows := range [][]Row{d.Faculties, d.Departments} {
		seen := map[[2]string]bool{}
		for _, r := range rows {
			k := [2]string{r.Parent.String(), r.Name}
			if seen[k] {
				t.Errorf("%s %q: a sibling has the same name", r.ID, r.Name)
			}
			seen[k] = true
			if quota.MatchString(r.Name) {
				t.Errorf("%s %q: a quota or fee suffix", r.ID, r.Name)
			}
		}
	}
	// What the cleanup keeps as programs of their own.
	for _, keep := range []string{"(İngilizce)", "(Uzaktan Öğretim)", "(Açıköğretim)", "(UOLP-"} {
		if !slices.ContainsFunc(d.Departments, func(r Row) bool { return strings.Contains(r.Name, keep) }) {
			t.Errorf("no department carries %s; the cleanup keeps it as a program", keep)
		}
	}
}

// small is a valid dataset with one row per table.
func small() *Dataset {
	country, city := uuid.New(), uuid.New()
	university, faculty := uuid.New(), uuid.New()
	return &Dataset{
		Version:      1,
		Countries:    []Row{{ID: country, Code: "TR", Name: "TÜRKİYE"}},
		Cities:       []Row{{ID: city, Parent: country, Name: "ADANA"}},
		Districts:    []Row{{ID: uuid.New(), Parent: city, Name: "SEYHAN"}},
		Universities: []Row{{ID: university, Name: "Örnek Üniversitesi"}},
		Faculties:    []Row{{ID: faculty, Parent: university, Name: "Mühendislik Fakültesi"}},
		Departments:  []Row{{ID: uuid.New(), Parent: faculty, Name: "Bilgisayar Mühendisliği"}},
	}
}

// TestValidate gives every rule of Validate its own case: each mutation
// breaks exactly one rule, so a rule that is removed turns its case red.
func TestValidate(t *testing.T) {
	t.Parallel()

	if err := small().Validate(); err != nil {
		t.Fatalf("the valid dataset: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Dataset)
		want   string
	}{
		"no version":           {func(d *Dataset) { d.Version = 0 }, "positive version"},
		"no id":                {func(d *Dataset) { d.Districts[0].ID = uuid.Nil }, "without an id"},
		"listed twice":         {func(d *Dataset) { d.Departments = append(d.Departments, d.Departments[0]) }, "listed twice"},
		"orphan":               {func(d *Dataset) { d.Departments[0].Parent = uuid.New() }, "is not a row of faculties"},
		"parent dropped":       {func(d *Dataset) { d.Universities = nil }, "is not a row of universities"},
		"root with a parent":   {func(d *Dataset) { d.Universities[0].Parent = uuid.New() }, "universities has no parent"},
		"empty name":           {func(d *Dataset) { d.Cities[0].Name = "" }, "0 characters"},
		"long name":            {func(d *Dataset) { d.Faculties[0].Name = strings.Repeat("ş", 201) }, "201 characters"},
		"name of 200":          {func(d *Dataset) { d.Faculties[0].Name = strings.Repeat("ş", 200) }, ""},
		"surrounding space":    {func(d *Dataset) { d.Cities[0].Name = "ADANA " }, "surrounding space"},
		"control character":    {func(d *Dataset) { d.Cities[0].Name = "ADA\tNA" }, "control character"},
		"zero width space":     {func(d *Dataset) { d.Cities[0].Name = "ADA\u200bNA" }, "invisible format character"},
		"right-to-left":        {func(d *Dataset) { d.Faculties[0].Name = "Fen \u202eFakültesi" }, "invisible format character"},
		"byte order mark":      {func(d *Dataset) { d.Departments[0].Name = "\ufeffFizik" }, "invisible format character"},
		"NBSP inside":          {func(d *Dataset) { d.Cities[0].Name = "ADA\u00a0NA" }, ""},
		"not UTF-8":            {func(d *Dataset) { d.Cities[0].Name = "AD\xffNA" }, "not UTF-8"},
		"bad code":             {func(d *Dataset) { d.Countries[0].Code = "tr" }, "not ISO 3166-1 alpha-2"},
		"code twice":           {func(d *Dataset) { d.Countries = append(d.Countries, Row{ID: uuid.New(), Code: "TR", Name: "X"}) }, "code TR listed twice"},
		"code outside country": {func(d *Dataset) { d.Cities[0].Code = "TR" }, "cities has no code"},
		"header only leaf":     {func(d *Dataset) { d.Departments = nil }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := small()
			tc.mutate(d)
			err := d.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Validate() = %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// TestParse refuses a file that does not have its table's shape.
func TestParse(t *testing.T) {
	t.Parallel()

	faculties := tables()[4]
	if faculties.name != "faculties" {
		t.Fatalf("tables()[4] = %s", faculties.name)
	}
	id, parent := uuid.New(), uuid.New()
	for name, tc := range map[string]struct {
		data string
		want string
	}{
		"valid":        {"id,university_id,name\n" + id.String() + "," + parent.String() + ",\"Fen, Edebiyat Fakültesi\"\n", ""},
		"header":       {"id,parent_id,name\n", "header"},
		"empty":        {"", "header"},
		"bad id":       {"id,university_id,name\nx," + parent.String() + ",A\n", `id "x"`},
		"bad parent":   {"id,university_id,name\n" + id.String() + ",y,A\n", `university_id "y"`},
		"extra column": {"id,university_id,name\n" + id.String() + "," + parent.String() + ",A,B\n", "wrong number of fields"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rows, err := parse(faculties, []byte(tc.data))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0] != (Row{ID: id, Parent: parent, Name: "Fen, Edebiyat Fakültesi"}) {
					t.Fatalf("rows = %+v", rows)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse() = %v, want an error with %q", err, tc.want)
			}
		})
	}
	// A header and nothing else is a valid file: it lists no row. Sync's
	// retirement limit, not Validate, stops it (TestSyncStopsAtTheRetireLimit).
	rows, err := parse(faculties, []byte("id,university_id,name\n"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("header only: %v, %v", rows, err)
	}
}

// TestParseVersion reads VERSION: one positive integer.
func TestParseVersion(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]int{"1\n": 1, "12": 12, "2147483647\n": 2147483647} {
		if got, err := parseVersion([]byte(raw)); err != nil || got != want {
			t.Errorf("parseVersion(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "0\n", "-1\n", "1.0\n", "v1\n", "1\n\n", "2147483648\n"} {
		if _, err := parseVersion([]byte(raw)); err == nil {
			t.Errorf("parseVersion(%q) succeeded", raw)
		}
	}
}

// TestParseAllowRetire reads --allow-retire and its variable: every entry
// names a table, a row count and the data version it is for.
func TestParseAllowRetire(t *testing.T) {
	t.Parallel()

	got, err := ParseAllowRetire("departments:120@2, faculties:0@3")
	if err != nil || len(got) != 2 || got["departments"] != (Allowance{Rows: 120, Version: 2}) || got["faculties"] != (Allowance{Rows: 0, Version: 3}) {
		t.Fatalf("ParseAllowRetire = %v, %v", got, err)
	}
	if got["departments"].String() != "120@2" {
		t.Errorf("String() = %s", got["departments"])
	}
	if got, err := ParseAllowRetire(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, s := range []string{
		"departments", "departments:120", "departments:120@", "departments:-1@2", "departments:x@2", "departments:1@0",
		"departments:1@x", "users:3@1", "departments:1@1,departments:2@1", ",",
	} {
		if _, err := ParseAllowRetire(s); err == nil {
			t.Errorf("ParseAllowRetire(%q) succeeded", s)
		}
	}
}

// TestRetireLimit is 5 % of the rows in use, at least 10.
func TestRetireLimit(t *testing.T) {
	t.Parallel()

	for active, want := range map[int]int{0: 10, 81: 10, 199: 10, 220: 11, 973: 48, 7567: 378} {
		if got := RetireLimit(active); got != want {
			t.Errorf("RetireLimit(%d) = %d, want %d", active, got, want)
		}
	}
}

// published maps every data version to the checksum of its files. A
// published version is never rewritten: a change of the files without a
// new VERSION changes the checksum of a version that databases already
// hold, and is red here. A new version appends its line; the old lines
// stay, so a rewritten one shows in review.
func published() map[int]string {
	return map[int]string{
		1: "ca194cde236561690ec3585df93bc42469169a4532037b646ffc26bcb874f289",
	}
}

// TestVersionNamesTheData binds VERSION to the files: the current files'
// checksum is the one published for the current version, and VERSION is
// the newest published version.
func TestVersionNamesTheData(t *testing.T) {
	t.Parallel()

	d := load(t)
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, e := range entries { // ReadDir sorts by name
		if !strings.HasSuffix(e.Name(), ".csv") {
			continue
		}
		data, err := FS.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(e.Name()))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	got := hex.EncodeToString(h.Sum(nil))
	versions := published()
	if want, ok := versions[d.Version]; !ok || got != want {
		t.Errorf("version %d: the files' checksum is %s, published %q; a change of the files needs a new VERSION and a new line in published()", d.Version, got, want)
	}
	if newest := slices.Max(slices.Collect(maps.Keys(versions))); newest != d.Version {
		t.Errorf("VERSION is %d, the newest published version %d", d.Version, newest)
	}
}
