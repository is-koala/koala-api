package referencedata

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
// involved. Version 2 added YÖK's associate degree programs (ADR-0038):
// the bachelor's programs of version 1 are all still there.
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
		{"universities", d.Universities, 231},
		{"faculties", d.Faculties, 3022},
		{"departments", d.Departments, 14711},
	} {
		if len(tc.rows) != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.table, len(tc.rows), tc.want)
		}
	}
	if n := len(d.DepartmentSourceCodes); n != 9254 {
		t.Errorf("department_source_codes: %d rows, want 9254", n)
	}
	degrees := map[string]int{}
	for _, r := range d.Departments {
		degrees[r.Degree]++
	}
	if degrees[DegreeBachelor] != 7567 || degrees[DegreeAssociate] != 7144 || len(degrees) != 2 {
		t.Errorf("departments by degree: %v, want 7567 BACHELOR and 7144 ASSOCIATE", degrees)
	}
	if d.Version != 2 {
		t.Errorf("Version = %d, want 2", d.Version)
	}
	want := []string{"countries", "cities", "districts", "universities", "faculties", "departments", "department_source_codes"}
	if got := Tables(); !slices.Equal(got, want) {
		t.Errorf("Tables() = %v, want %v (parents first)", got, want)
	}
}

// ledgerRow is a row of tools/id_ledger.csv, the append-only id ledger of
// tools/from_yok.py (ADR-0038 Karar 6).
type ledgerRow struct {
	key     string
	id      uuid.UUID
	parent  string
	name    string
	version int
	origin  string
}

var ledgerKey = regexp.MustCompile(`^(yok:university:\d+|yok:unit:\d+|osym:\d{9})$`)

// readLedger reads the ledger and its bytes; the test runs in this
// package's directory.
func readLedger(t *testing.T) ([]ledgerRow, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("tools", "id_ledger.csv"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 || !slices.Equal(records[0], []string{"key", "id", "parent", "name", "version", "origin"}) {
		t.Fatalf("id_ledger.csv: header %v", records[:min(1, len(records))])
	}
	rows := make([]ledgerRow, 0, len(records)-1)
	for _, rec := range records[1:] {
		id, err := uuid.Parse(rec[1])
		if err != nil {
			t.Fatalf("id_ledger.csv %v: %v", rec, err)
		}
		v, err := strconv.Atoi(rec[4])
		if err != nil {
			t.Fatalf("id_ledger.csv %v: %v", rec, err)
		}
		rows = append(rows, ledgerRow{key: rec[0], id: id, parent: rec[2], name: rec[3], version: v, origin: rec[5]})
	}
	return rows, raw
}

// publishedLedger maps every data version to the ledger it published: the
// number of rows and the checksum of the file's header and those rows. A
// published row is never changed or removed: a later version appends its
// rows after them and adds its line here; the old lines stay.
func publishedLedger() map[int]struct {
	rows int
	sum  string
} {
	return map[int]struct {
		rows int
		sum  string
	}{
		2: {10438, "82b6edfffb93766e6c09b2987def6cd14141131f9c6dd392ab2ab10bec85fbcf"},
	}
}

// TestIDLedgerIsAppendOnly binds the ledger to its published versions: the
// file starts with every published version's rows, byte for byte, and the
// current version has published every row it holds. A row rewritten or
// removed, or a row added without a new VERSION, is red here.
func TestIDLedgerIsAppendOnly(t *testing.T) {
	t.Parallel()

	d := load(t)
	rows, raw := readLedger(t)
	lines := bytes.SplitAfter(raw, []byte("\n"))
	for v, p := range publishedLedger() {
		if p.rows > len(rows) {
			t.Errorf("version %d published %d ledger rows; the file has %d", v, p.rows, len(rows))
			continue
		}
		sum := sha256.Sum256(bytes.Join(lines[:p.rows+1], nil))
		if got := hex.EncodeToString(sum[:]); got != p.sum {
			t.Errorf("version %d: the first %d ledger rows sum to %s, published %s; the ledger is appended to, never rewritten",
				v, p.rows, got, p.sum)
		}
		for _, r := range rows[:p.rows] {
			if r.version > v {
				t.Errorf("%s: added in version %d, among the rows version %d published", r.key, r.version, v)
			}
		}
	}
	if p, ok := publishedLedger()[d.Version]; !ok || p.rows != len(rows) {
		t.Errorf("VERSION %d published %d ledger rows, the file has %d; a new row needs a new VERSION and a line in publishedLedger()",
			d.Version, p.rows, len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].version < rows[i-1].version {
			t.Errorf("%s (version %d) after %s (version %d): rows are appended in version order",
				rows[i].key, rows[i].version, rows[i-1].key, rows[i-1].version)
		}
	}
}

// TestIDLedger binds the files to the ledger (ADR-0038 Karar 6): every key
// is well formed; a minted id is a UUIDv7, never derived from an outside
// code; every associate degree program has a code and every code's latest
// ledger row names its department under its faculty, so the next year's
// list finds the program by any of its codes, and losing the one it was
// first seen under loses nothing.
func TestIDLedger(t *testing.T) {
	t.Parallel()

	d := load(t)
	rows, _ := readLedger(t)
	latest := map[string]ledgerRow{}
	for _, r := range rows {
		if !ledgerKey.MatchString(r.key) || (r.origin != "minted" && r.origin != "matched") || r.version < 1 || r.version > d.Version {
			t.Errorf("ledger row %+v: malformed", r)
		}
		if r.origin == "minted" && r.id.Version() != 7 {
			t.Errorf("ledger row %s: a minted id %s is not a UUIDv7", r.key, r.id)
		}
		latest[r.key] = r
	}
	parent := map[uuid.UUID]uuid.UUID{}
	degree := map[uuid.UUID]string{}
	for _, r := range d.Departments {
		parent[r.ID], degree[r.ID] = r.Parent, r.Degree
	}
	codes := map[uuid.UUID]int{}
	for _, c := range d.DepartmentSourceCodes {
		codes[c.Department]++
		r, ok := latest["osym:"+c.Code]
		switch {
		case !ok:
			t.Errorf("code %s is not in the ledger", c.Code)
		case r.id != c.Department || r.parent != parent[c.Department].String():
			t.Errorf("code %s names %s under %s; the ledger %s under %s", c.Code, c.Department, parent[c.Department], r.id, r.parent)
		case degree[c.Department] != DegreeAssociate:
			t.Errorf("code %s names %s, a %s department", c.Code, c.Department, degree[c.Department])
		}
	}
	merged := 0
	for _, r := range d.Departments {
		switch {
		case r.Degree == DegreeAssociate && codes[r.ID] == 0:
			t.Errorf("associate program %s %q has no source code", r.ID, r.Name)
		case codes[r.ID] > 1:
			merged++
		}
	}
	if merged == 0 {
		t.Error("no associate program has more than one code: the quota variants were not merged")
	}
	universities := map[uuid.UUID]bool{}
	for _, u := range d.Universities {
		universities[u.ID] = true
	}
	faculties := map[uuid.UUID]uuid.UUID{}
	for _, f := range d.Faculties {
		faculties[f.ID] = f.Parent
	}
	for key, r := range latest {
		switch {
		case strings.HasPrefix(key, "yok:university:") && !universities[r.id]:
			t.Errorf("%s: %s is not a university of the files", key, r.id)
		case strings.HasPrefix(key, "yok:unit:") && faculties[r.id].String() != r.parent:
			t.Errorf("%s: %s is not a faculty of %s in the files", key, r.id, r.parent)
		}
	}
}

// TestFromYokSelfTest runs tools/from_yok.py --self-test (ADR-0038 Karar
// 6): on made-up data, a code ÖSYM reuses for another program, or one
// that moves to another unit, stops the run until it is decided; "new"
// gives a new id, "same" keeps it and cannot keep a moved one; a program
// that loses its first code keeps its id; a second run changes nothing;
// the ledger is only appended to. The verify job has python3 (make
// generate runs pairs.py with it).
func TestFromYokSelfTest(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is needed to run tools/from_yok.py --self-test: %v", err)
	}
	out, err := exec.CommandContext(t.Context(), python, "-I", filepath.Join("tools", "from_yok.py"), "--self-test").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "self-test: ok") {
		t.Fatalf("from_yok.py --self-test: %v\n%s", err, out)
	}
}

// TestAssociateWitnesses spot-checks known vocational schools and
// programs: a school of a foundation university, one of a state university
// with its quota variants merged and its campus kept, a foundation
// vocational school that belongs to no university (a university row of
// its own), a unit that had bachelor's programs already, and the name an
// associate program takes beside a bachelor's program of the same name.
func TestAssociateWitnesses(t *testing.T) {
	t.Parallel()

	d := load(t)
	byID := map[uuid.UUID]Row{}
	for _, rows := range [][]Row{d.Universities, d.Faculties, d.Departments} {
		for _, r := range rows {
			byID[r.ID] = r
		}
	}
	codeOf := map[string]uuid.UUID{}
	for _, c := range d.DepartmentSourceCodes {
		codeOf[c.Code] = c.Department
	}
	for _, tc := range []struct {
		id, parent, name, degree string
	}{
		// Türk Hava Kurumu Üniversitesi, Ankara Havacılık MYO.
		{"01a207cf-0348-7890-bf14-c44956448609", "20b200c7-d3d3-4e77-ba67-2b01c96b92a8", "Ankara Havacılık Meslek Yüksekokulu", ""},
		{"01a207cf-03cc-7a3c-9df7-92d5f5825c58", "01a207cf-0348-7890-bf14-c44956448609", "Uçak Teknolojisi", DegreeAssociate},
		// Sağlık Bilimleri Üniversitesi, Gülhane SMYO: "(Ankara) (Erkek)
		// (Milli Savunma Bakanlığı Adına)" and its other variant are one
		// program; the campus stays.
		{"01a207cf-0355-7356-a698-0e102b1a814d", "7ae61a3a-d91a-4155-9319-bec58eba1c33", "Gülhane Sağlık Meslek Yüksekokulu (Ankara)", ""},
		{"01a207cf-050b-738c-a8ff-e13d44acdd15", "01a207cf-0355-7356-a698-0e102b1a814d", "Tıbbi Görüntüleme Teknikleri (Ankara)", DegreeAssociate},
		// İzmir Konak MYO: a foundation vocational school of its own.
		{"01a207cf-033b-777f-9064-34e3659cbf35", "", "İZMİR KONAK MESLEK YÜKSEKOKULU", ""},
		{"01a207cf-0346-7b3b-b802-c401a8d4b895", "01a207cf-033b-777f-9064-34e3659cbf35", "İzmir Konak Meslek Yüksekokulu", ""},
		// Atatürk Üniversitesi's Açıköğretim Fakültesi (Java's id) teaches
		// both degrees; the associate program of a bachelor's program's
		// name is told apart as YÖK does.
		{"bf9ed268-ffb0-4eae-9892-dc91dd6d9bd5", "75739eb2-88b3-403d-a2d4-5ea29f72db64", "Halkla İlişkiler ve Tanıtım (Açıköğretim)", DegreeBachelor},
		{"01a207cf-06d2-7ce9-8325-f1f68dc55b80", "75739eb2-88b3-403d-a2d4-5ea29f72db64", "Halkla İlişkiler ve Tanıtım (Önlisans) (Açıköğretim)", DegreeAssociate},
	} {
		r, ok := byID[uuid.MustParse(tc.id)]
		if !ok {
			t.Errorf("%s (%s) is missing", tc.id, tc.name)
			continue
		}
		if r.Name != tc.name || r.Degree != tc.degree || (tc.parent != "" && r.Parent != uuid.MustParse(tc.parent)) {
			t.Errorf("%s = %+v, want %q under %s, degree %q", tc.id, r, tc.name, tc.parent, tc.degree)
		}
	}
	for code, dept := range map[string]string{
		"205750094": "01a207cf-03cc-7a3c-9df7-92d5f5825c58", "205750101": "01a207cf-03cc-7a3c-9df7-92d5f5825c58",
		"111050204": "01a207cf-050b-738c-a8ff-e13d44acdd15", "111091387": "01a207cf-050b-738c-a8ff-e13d44acdd15",
	} {
		if codeOf[code] != uuid.MustParse(dept) {
			t.Errorf("code %s names %s, want %s", code, codeOf[code], dept)
		}
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
	university, faculty, department := uuid.New(), uuid.New(), uuid.New()
	return &Dataset{
		Version:               1,
		Countries:             []Row{{ID: country, Code: "TR", Name: "TÜRKİYE"}},
		Cities:                []Row{{ID: city, Parent: country, Name: "ADANA"}},
		Districts:             []Row{{ID: uuid.New(), Parent: city, Name: "SEYHAN"}},
		Universities:          []Row{{ID: university, Name: "Örnek Üniversitesi"}},
		Faculties:             []Row{{ID: faculty, Parent: university, Name: "Mühendislik Fakültesi"}},
		Departments:           []Row{{ID: department, Parent: faculty, Degree: DegreeBachelor, Name: "Bilgisayar Mühendisliği"}},
		DepartmentSourceCodes: []SourceCode{{Code: "102710001", Department: department}},
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
		"line separator":       {func(d *Dataset) { d.Cities[0].Name = "ADA\u2028NA" }, "line or paragraph separator"},
		"paragraph separator":  {func(d *Dataset) { d.Faculties[0].Name = "Fen\u2029Fakültesi" }, "line or paragraph separator"},
		"private use":          {func(d *Dataset) { d.Departments[0].Name = "Fizik\ue000" }, "private use character"},
		"right-to-left":        {func(d *Dataset) { d.Faculties[0].Name = "Fen \u202eFakültesi" }, "invisible format character"},
		"byte order mark":      {func(d *Dataset) { d.Departments[0].Name = "\ufeffFizik" }, "invisible format character"},
		"NBSP inside":          {func(d *Dataset) { d.Cities[0].Name = "ADA\u00a0NA" }, ""},
		"not UTF-8":            {func(d *Dataset) { d.Cities[0].Name = "AD\xffNA" }, "not UTF-8"},
		"bad code":             {func(d *Dataset) { d.Countries[0].Code = "tr" }, "not ISO 3166-1 alpha-2"},
		"code twice":           {func(d *Dataset) { d.Countries = append(d.Countries, Row{ID: uuid.New(), Code: "TR", Name: "X"}) }, "code TR listed twice"},
		"code outside country": {func(d *Dataset) { d.Cities[0].Code = "TR" }, "cities has no code"},
		"header only leaf":     {func(d *Dataset) { d.Departments, d.DepartmentSourceCodes = nil, nil }, ""},
		"associate":            {func(d *Dataset) { d.Departments[0].Degree = DegreeAssociate }, ""},
		"no degree":            {func(d *Dataset) { d.Departments[0].Degree = "" }, `degree ""`},
		"graduate degree":      {func(d *Dataset) { d.Departments[0].Degree = "MASTER" }, `degree "MASTER"`},
		"degree outside":       {func(d *Dataset) { d.Faculties[0].Degree = DegreeBachelor }, "faculties has no degree"},
		"short code":           {func(d *Dataset) { d.DepartmentSourceCodes[0].Code = "10271000" }, "not an ÖSYM program code"},
		"code with a letter":   {func(d *Dataset) { d.DepartmentSourceCodes[0].Code = "10271000x" }, "not an ÖSYM program code"},
		"source code twice": {func(d *Dataset) {
			d.DepartmentSourceCodes = append(d.DepartmentSourceCodes, d.DepartmentSourceCodes[0])
		}, "listed twice"},
		"code of no department": {func(d *Dataset) { d.DepartmentSourceCodes[0].Department = uuid.New() }, "is not a row of departments"},
		"code of a dropped department": {func(d *Dataset) {
			d.Departments = nil
		}, "is not a row of departments"},
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

	faculties, departments := tables()[4], tables()[5]
	if faculties.name != "faculties" || departments.name != "departments" {
		t.Fatalf("tables()[4:6] = %s, %s", faculties.name, departments.name)
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
	// A department carries its degree between its faculty and its name.
	rows, err := parse(departments, []byte("id,faculty_id,degree,name\n"+id.String()+","+parent.String()+",ASSOCIATE,Anestezi\n"))
	if err != nil || len(rows) != 1 || rows[0] != (Row{ID: id, Parent: parent, Degree: DegreeAssociate, Name: "Anestezi"}) {
		t.Fatalf("departments: %+v, %v", rows, err)
	}
	if _, err := parse(departments, []byte("id,faculty_id,name\n")); err == nil || !strings.Contains(err.Error(), "header") {
		t.Fatalf("departments without the degree column: %v, want the header refused", err)
	}
	// The source codes: a code and its department.
	codes, err := parseSourceCodes([]byte("code,department_id\n205750101," + id.String() + "\n"))
	if err != nil || len(codes) != 1 || codes[0] != (SourceCode{Code: "205750101", Department: id}) {
		t.Fatalf("source codes: %+v, %v", codes, err)
	}
	for name, data := range map[string]string{
		"header":       "department_id,code\n",
		"empty":        "",
		"bad id":       "code,department_id\n205750101,x\n",
		"extra column": "code,department_id\n205750101," + id.String() + ",x\n",
	} {
		if _, err := parseSourceCodes([]byte(data)); err == nil {
			t.Errorf("source codes, %s: parsed", name)
		}
	}
	// A header and nothing else is a valid file: it lists no row. Sync's
	// retirement limit, not Validate, stops it (TestSyncStopsAtTheRetireLimit).
	rows, err = parse(faculties, []byte("id,university_id,name\n"))
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

	for active, want := range map[int]int{0: 10, 81: 10, 199: 10, 220: 11, 973: 48, 3022: 151, 7567: 378, 9254: 462, 14711: 735} {
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
		// YÖK's associate degree programs, the departments' degree and
		// their source codes (ADR-0038).
		2: "ebd3dced24348f16920ce196cfd04f60fe34141fe5a84cb798d9211840cd01b9",
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
