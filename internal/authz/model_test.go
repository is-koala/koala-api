package authz_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/openfga/language/pkg/go/transformer"

	"github.com/is-koala/koala-api/internal/authz"
)

var update = flag.Bool("update", false, "rewrite model.json from model.fga")

// officialModelJSON parses model.fga with OpenFGA's own parser
// (openfga/language, test-only) and returns the JSON OpenFGA accepts.
func officialModelJSON(t *testing.T) []byte {
	t.Helper()
	dsl, err := os.ReadFile("model.fga")
	if err != nil {
		t.Fatal(err)
	}
	out, err := transformer.TransformDSLToJSON(string(dsl))
	if err != nil {
		t.Fatalf("model.fga does not parse with openfga/language: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(out), "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	return pretty.Bytes()
}

// TestModelJSONMatchesDSL fails when model.json, the file the service
// embeds and the conformance runner uploads, is not what OpenFGA's parser
// makes of model.fga. Run with -update to rewrite it.
func TestModelJSONMatchesDSL(t *testing.T) {
	t.Parallel()
	official := officialModelJSON(t)
	if *update {
		if err := os.WriteFile("model.json", official, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile("model.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := authz.LoadModel(official)
	if err != nil {
		t.Fatalf("the official parse of model.fga is outside the subset: %v", err)
	}
	got, err := authz.LoadModel(committed)
	if err != nil {
		t.Fatalf("model.json: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("model.json is stale: run go test ./internal/authz -run TestModelJSONMatchesDSL -update\nmodel.fga parses to:\n%s", official)
	}
}

// intersectionOrExclusion lists the relations allowed to use "and" or
// "but not" (ADR-0024: limited, they make list queries expensive on an
// external engine). A new one is a reviewed change to this list.
var intersectionOrExclusion = []string{
	"job#can_apply",      // applicant_pool but not editor (C21)
	"message#can_report", // can_read but not sender (C39)
}

// TestModelSubset checks, on OpenFGA's own parse of model.fga, that the
// model stays in the ADR-0024 subset. LoadModel rejects conditions,
// modules, other schema versions and nested "and" / "but not"; this test
// adds the explicit list of relations that use them.
func TestModelSubset(t *testing.T) {
	t.Parallel()
	m, err := authz.LoadModel(officialModelJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, typ := range m.Types() {
		for _, rel := range m.Relations(typ) {
			if m.UsesIntersectionOrExclusion(typ, rel) {
				found = append(found, typ+"#"+rel)
			}
		}
	}
	want := slices.Sorted(slices.Values(intersectionOrExclusion))
	if !slices.Equal(found, want) {
		t.Fatalf("relations using and / but not: %v, want %v", found, want)
	}
}

// TestModelDecisions pins the Faz 0 product decisions in the model, so a
// later edit of model.fga cannot quietly undo them.
func TestModelDecisions(t *testing.T) {
	t.Parallel()
	dsl, err := os.ReadFile("model.fga")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"define employer: individual_owner or member from owner_company", // D1
		"define editor: employer",                         // D1: creator grants nothing
		"define public_viewer: [user:*]",                  // D3
		"define can_apply: applicant_pool but not editor", // D3
		"define can_read_exact_address: individual_owner or can_manage_jobs from owner_company or active_worker or public_address_viewer", // C15
		"define can_download: owner or reviewer", // D5: no marketplace admin
	} {
		if !bytes.Contains(dsl, []byte(want)) {
			t.Errorf("model.fga lost %q", want)
		}
	}
	if bytes.Contains(dsl, []byte("or creator")) || bytes.Contains(dsl, []byte("creator or")) {
		t.Errorf("model.fga grants through job#creator (D1)")
	}
}

func TestConcealPolicyNamesModelRelations(t *testing.T) {
	t.Parallel()
	m := loadModel(t)
	for key, visibility := range authz.ConcealPolicy() {
		typ, rel, _ := strings.Cut(key, "#")
		if !slices.Contains(m.Relations(typ), rel) {
			t.Errorf("conceal policy names undefined relation %s", key)
		}
		if !slices.Contains(m.Relations(typ), string(visibility)) {
			t.Errorf("conceal policy %s names undefined visibility relation %s#%s", key, typ, visibility)
		}
	}
}

func TestLoadModelRejects(t *testing.T) {
	t.Parallel()
	const user = `{"type":"user"}`
	doc := func(types ...string) string {
		return `{"schema_version":"1.1","type_definitions":[` + strings.Join(types, ",") + `]}`
	}
	direct := `"metadata":{"relations":{"owner":{"directly_related_user_types":[{"type":"user"}]}}}`
	tests := []struct {
		name  string
		model string
	}{
		{"not json", `{`},
		{"unknown field", `{"schema_version":"1.1","type_definitions":[],"extra":1}`},
		{"schema 1.2", `{"schema_version":"1.2","type_definitions":[` + user + `]}`},
		{"condition", `{"schema_version":"1.1","type_definitions":[` + user + `],"conditions":{"c":{}}}`},
		{"duplicate type", doc(user, user)},
		{"module", doc(user, `{"type":"doc","metadata":{"module":"m"}}`)},
		{"direct without restriction", doc(user, `{"type":"doc","relations":{"owner":{"this":{}}}}`)},
		{"restriction without direct", doc(user, `{"type":"doc","relations":{"owner":{"computedUserset":{"relation":"owner"}}},`+direct+`}`)},
		{"undefined type", doc(`{"type":"doc","relations":{"owner":{"this":{}}},` + direct + `}`)},
		{"undefined computed relation", doc(user, `{"type":"doc","relations":{"owner":{"this":{}},"v":{"computedUserset":{"relation":"nope"}}},`+direct+`}`)},
		{"empty userset", doc(user, `{"type":"doc","relations":{"owner":{}}}`)},
		{"two rewrites", doc(user, `{"type":"doc","relations":{"owner":{"this":{},"computedUserset":{"relation":"owner"}}},`+direct+`}`)},
		{"conditioned restriction", doc(user, `{"type":"doc","relations":{"owner":{"this":{}}},"metadata":{"relations":{"owner":{"directly_related_user_types":[{"type":"user","condition":"c"}]}}}}`)},
		{"nested but not", doc(user, `{"type":"doc","relations":{"owner":{"this":{}},"a":{"union":{"child":[{"computedUserset":{"relation":"owner"}},{"difference":{"base":{"computedUserset":{"relation":"owner"}},"subtract":{"computedUserset":{"relation":"owner"}}}}]}}},`+direct+`}`)},
		{"set operation under and", doc(user, `{"type":"doc","relations":{"owner":{"this":{}},"a":{"intersection":{"child":[{"computedUserset":{"relation":"owner"}},{"union":{"child":[{"computedUserset":{"relation":"owner"}},{"computedUserset":{"relation":"owner"}}]}}]}}},`+direct+`}`)},
		{"tupleset with wildcard", doc(user, `{"type":"doc","relations":{"parent":{"this":{}},"v":{"tupleToUserset":{"tupleset":{"relation":"parent"},"computedUserset":{"relation":"parent"}}}},"metadata":{"relations":{"parent":{"directly_related_user_types":[{"type":"doc","wildcard":{}}]}}}}`)},
		{"tupleset parent lacks relation", doc(user, `{"type":"doc","relations":{"parent":{"this":{}},"v":{"tupleToUserset":{"tupleset":{"relation":"parent"},"computedUserset":{"relation":"nope"}}}},"metadata":{"relations":{"parent":{"directly_related_user_types":[{"type":"user"}]}}}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := authz.LoadModel([]byte(tt.model))
			if !errors.Is(err, authz.ErrModel) {
				t.Fatalf("LoadModel: %v, want ErrModel", err)
			}
		})
	}
}
