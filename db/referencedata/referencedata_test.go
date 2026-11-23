package referencedata

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestDatasetCounts binds the embedded files to their row counts
// (README.md): a file cut short, a lost line or a file that stopped being
// embedded is red here, before any database is involved.
func TestDatasetCounts(t *testing.T) {
	t.Parallel()

	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		table string
		rows  []Row
		want  int
	}{
		{"countries", d.Countries, 1},
		{"cities", d.Cities, 81},
		{"districts", d.Districts, 995},
		{"universities", d.Universities, 223},
		{"faculties", d.Faculties, 2326},
		{"departments", d.Departments, 11250},
	} {
		if len(tc.rows) != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.table, len(tc.rows), tc.want)
		}
	}
	if got, want := Tables(), []string{"countries", "cities", "districts", "universities", "faculties", "departments"}; !slices.Equal(got, want) {
		t.Errorf("Tables() = %v, want %v (parents first)", got, want)
	}
}

// TestDatasetKeepsJavaIDs spot-checks that the ids are Java's (ADR-0037):
// a record that names a Java id finds the same row here.
func TestDatasetKeepsJavaIDs(t *testing.T) {
	t.Parallel()

	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
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

// TestDuplicateNamesAreAllowed records the source as it is: 5 faculties
// and 17 departments share their name with a sibling, and the files keep
// them (no unique name; ADR-0037). A validation rule that refused them
// would refuse the whole snapshot.
func TestDuplicateNamesAreAllowed(t *testing.T) {
	t.Parallel()

	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	dups := func(rows []Row) int {
		seen := map[[2]string]bool{}
		n := 0
		for _, r := range rows {
			k := [2]string{r.Parent.String(), r.Name}
			if seen[k] {
				n++
			}
			seen[k] = true
		}
		return n
	}
	if got := dups(d.Faculties); got != 5 {
		t.Errorf("faculties: %d duplicate names under one university, want 5", got)
	}
	if got := dups(d.Departments); got != 17 {
		t.Errorf("departments: %d duplicate names under one faculty, want 17", got)
	}
}

// small is a valid dataset with one row per table.
func small() *Dataset {
	country, city := uuid.New(), uuid.New()
	university, faculty := uuid.New(), uuid.New()
	return &Dataset{
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
		"not UTF-8":            {func(d *Dataset) { d.Cities[0].Name = "AD\xffNA" }, "not UTF-8"},
		"bad code":             {func(d *Dataset) { d.Countries[0].Code = "tr" }, "not ISO 3166-1 alpha-2"},
		"code twice":           {func(d *Dataset) { d.Countries = append(d.Countries, Row{ID: uuid.New(), Code: "TR", Name: "X"}) }, "code TR listed twice"},
		"code outside country": {func(d *Dataset) { d.Cities[0].Code = "TR" }, "cities has no code"},
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
}
