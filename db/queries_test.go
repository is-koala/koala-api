package db_test

import (
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

func catalog(t *testing.T) *softdelete.Catalog {
	t.Helper()
	cat, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// TestQueriesJoinLint is the JOIN lint of ADR-0013 over every sqlc query:
// a non-LEFT join to the parent of a detach edge fails (CheckJoins; its
// own test proves which forms are caught). It reads the files on disk, so
// a file the embed would skip is linted too.
func TestQueriesJoinLint(t *testing.T) {
	t.Parallel()

	cat := catalog(t)
	queries, err := dbsrc.AllQueries(os.DirFS("queries"))
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) == 0 {
		t.Fatal("no queries under db/queries")
	}
	for _, q := range queries {
		for _, finding := range softdelete.CheckJoins(cat, q.SQL) {
			t.Errorf("db/queries/%s: %s: %s", q.File, q.Name, finding)
		}
	}
}

// TestEmbeddedQueriesMatchTheDirectory holds the two sources of the query
// files equal. The join lint reads the directory and the marker rule reads
// the embed (db.Queries(), ADR-0027 §2); the tie between them assumes the
// same queries. //go:embed skips a file whose name starts with "." or "_",
// so such a file would be linted and never checked against a JoinTest
// declaration.
func TestEmbeddedQueriesMatchTheDirectory(t *testing.T) {
	t.Parallel()

	onDisk, err := dbsrc.AllQueries(os.DirFS("queries"))
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := dbsrc.AllQueries(dbsrc.Queries())
	if err != nil {
		t.Fatal(err)
	}
	names := func(qs []dbsrc.Query) []string {
		out := make([]string, len(qs))
		for i, q := range qs {
			out[i] = q.File + ":" + q.Name
		}
		slices.Sort(out)
		return out
	}
	if d, e := names(onDisk), names(embedded); !slices.Equal(d, e) {
		t.Errorf("db/queries on disk = %v, embedded = %v; rename a file that //go:embed skips", d, e)
	}
}

type sqlcConfig struct {
	SQL []struct {
		Name    string   `yaml:"name"`
		Queries string   `yaml:"queries"`
		Rules   []string `yaml:"rules"`
		Gen     struct {
			Go struct {
				Package string `yaml:"package"`
				Out     string `yaml:"out"`
			} `yaml:"go"`
		} `yaml:"gen"`
	} `yaml:"sql"`
	Rules []struct {
		Name string `yaml:"name"`
		Rule string `yaml:"rule"`
	} `yaml:"rules"`
}

const (
	maintQueries = "db/queries/maint"
	maintOut     = "internal/platform/db/maintdb"
)

// TestSQLCConfig keeps the layout of sqlc.yaml (ADR-0020): every query
// directory is one sql entry; the koala_maint queries are one package,
// maintdb, without rules; every other entry has both rules; and the rules
// are the ones softdeletegen writes.
func TestSQLCConfig(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../sqlc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg sqlcConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	dirs, err := fs.Glob(os.DirFS("queries"), "*")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	sawMaint := false
	for _, s := range cfg.SQL {
		if listed[s.Queries] {
			t.Errorf("%s is listed by two sql entries", s.Queries)
		}
		listed[s.Queries] = true
		if s.Queries == maintQueries {
			sawMaint = true
			if s.Name != "maint" || s.Gen.Go.Out != maintOut || s.Gen.Go.Package != "maintdb" {
				t.Errorf("maint entry = %s -> %s (%s), want maint -> %s (maintdb)", s.Name, s.Gen.Go.Out, s.Gen.Go.Package, maintOut)
			}
			if len(s.Rules) > 0 {
				t.Errorf("maint entry has rules %v; koala_maint works on base tables", s.Rules)
			}
			continue
		}
		if !strings.HasPrefix(s.Queries, "db/queries/") || strings.Count(s.Queries, "/") != 2 {
			t.Errorf("sql entry %s: queries %s, want db/queries/<area>", s.Name, s.Queries)
		}
		if s.Gen.Go.Out == maintOut {
			t.Errorf("sql entry %s generates into %s; only db/queries/maint may", s.Name, maintOut)
		}
		for _, rule := range []string{softdelete.RuleNoBaseTable, softdelete.RuleNoSoftDeleteColumns} {
			if !slices.Contains(s.Rules, rule) {
				t.Errorf("sql entry %s (application queries) lacks the %s rule", s.Name, rule)
			}
		}
	}
	if !sawMaint {
		t.Errorf("no sql entry for %s", maintQueries)
	}
	for _, d := range dirs {
		if !listed["db/queries/"+d] {
			t.Errorf("db/queries/%s is in no sql entry of sqlc.yaml", d)
		}
	}

	want := map[string]string{
		softdelete.RuleNoBaseTable:         softdelete.NoBaseTableRule(catalog(t)),
		softdelete.RuleNoSoftDeleteColumns: softdelete.NoSoftDeleteColumnsRule(),
	}
	got := map[string]string{}
	for _, r := range cfg.Rules {
		got[r.Name] = r.Rule
	}
	for name, rule := range want {
		if got[name] != rule {
			t.Errorf("%s rule = %q, want %q (run make generate)", name, got[name], rule)
		}
	}
}
