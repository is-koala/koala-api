package db_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"

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

var queryName = regexp.MustCompile(`(?m)^-- name: (\w+)`)

// TestQueriesJoinLint is the JOIN lint of ADR-0013 over every sqlc query:
// a non-LEFT join to the parent of a detach edge fails (CheckJoins; its
// own test proves which forms are caught).
func TestQueriesJoinLint(t *testing.T) {
	t.Parallel()

	cat := catalog(t)
	files, err := fs.Glob(os.DirFS("queries"), "*/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no query files under db/queries")
	}
	for _, f := range files {
		data, err := os.ReadFile(path.Join("queries", f))
		if err != nil {
			t.Fatal(err)
		}
		bounds := queryName.FindAllStringSubmatchIndex(string(data), -1)
		for i, b := range bounds {
			end := len(data)
			if i+1 < len(bounds) {
				end = bounds[i+1][0]
			}
			name := string(data[b[2]:b[3]])
			for _, finding := range softdelete.CheckJoins(cat, string(data[b[0]:end])) {
				t.Errorf("db/queries/%s: %s: %s", f, name, finding)
			}
		}
	}
}

type sqlcConfig struct {
	SQL []struct {
		Name    string   `json:"name"`
		Queries string   `json:"queries"`
		Rules   []string `json:"rules"`
		Gen     struct {
			Go struct {
				Package string `json:"package"`
				Out     string `json:"out"`
			} `json:"go"`
		} `json:"gen"`
	} `json:"sql"`
	Rules []struct {
		Name string `json:"name"`
		Rule string `json:"rule"`
	} `json:"rules"`
}

const (
	maintQueries = "db/queries/maint"
	maintOut     = "internal/platform/db/maintdb"
)

// TestSQLCConfig keeps the layout of sqlc.yaml (ADR-0020): every query
// directory is one sql entry; the koala_maint queries are one package,
// maintdb, without the no-base-table rule; every other entry has the rule;
// and the rule is the one the catalog generates.
func TestSQLCConfig(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../sqlc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v, err := openapi3filter.YamlBodyDecoder(bytes.NewReader(data), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var cfg sqlcConfig
	if err := json.Unmarshal(j, &cfg); err != nil {
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
			continue
		}
		if !strings.HasPrefix(s.Queries, "db/queries/") || strings.Count(s.Queries, "/") != 2 {
			t.Errorf("sql entry %s: queries %s, want db/queries/<area>", s.Name, s.Queries)
		}
		if s.Gen.Go.Out == maintOut {
			t.Errorf("sql entry %s generates into %s; only db/queries/maint may", s.Name, maintOut)
		}
		if !slices.Contains(s.Rules, "no-base-table") {
			t.Errorf("sql entry %s (application queries) lacks the no-base-table rule", s.Name)
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

	want := softdelete.NoBaseTableRule(catalog(t))
	found := false
	for _, r := range cfg.Rules {
		if r.Name == "no-base-table" {
			found = true
			if r.Rule != want {
				t.Errorf("no-base-table rule = %s, want %s (run make generate)", r.Rule, want)
			}
		}
	}
	if !found {
		t.Error("sqlc.yaml has no no-base-table rule")
	}
}
