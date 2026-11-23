package app

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/db/referencedata"
)

// TestReferenceHasNoWriteOperation holds the contract to MED-012: every
// operation of the reference tag reads (GET), is public (security: []),
// has a handler and names one of the reference tables in x-resource; no
// path under /v1/reference takes another method. The rows change only
// through db/referencedata and `koala migrate up` (ADR-0037).
func TestReferenceHasNoWriteOperation(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tables := referencedata.Tables()
	read := map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for method, op := range spec.Paths.Value(path).Operations() {
			tagged := slices.Contains(op.Tags, "reference")
			under := strings.HasPrefix(path, "/v1/reference/")
			if !tagged && !under {
				continue
			}
			where := method + " " + path
			if !tagged || !under || method != http.MethodGet {
				t.Errorf("MED-012: %s: the reference data has only GET operations under /v1/reference, tagged reference", where)
				continue
			}
			if op.Security == nil || len(*op.Security) != 0 {
				t.Errorf("MED-012: %s: a reference list is public (security: [])", where)
			}
			if _, frozen := op.Extensions[extNotImplemented]; frozen {
				t.Errorf("%s: still x-not-implemented", where)
			}
			table, _ := op.Extensions["x-resource"].(string)
			if !slices.Contains(tables, table) {
				t.Errorf("%s: x-resource %q is not a reference table %v", where, table, tables)
			}
			read[table] = true
		}
	}
	// The departments' source codes have no list: they match next year's
	// YÖK list to the rows and are not served (ADR-0038).
	listed := slices.DeleteFunc(slices.Clone(tables), func(table string) bool { return table == "department_source_codes" })
	if len(listed) != len(tables)-1 || !slices.Equal(slices.Sorted(maps.Keys(read)), slices.Sorted(slices.Values(listed))) {
		t.Errorf("the reference operations read %v; want one list per table %v", slices.Sorted(maps.Keys(read)), listed)
	}
}
