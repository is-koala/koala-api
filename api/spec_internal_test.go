package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

func loadFile(t *testing.T, name string) *openapi3.T {
	t.Helper()
	doc, err := load(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func specFiles(t *testing.T) []string {
	t.Helper()
	components, err := fs.Glob(files, "components/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{"openapi.yaml"}, components...)
}

// The Problem schema and httpx.Problem describe the same wire format: the
// same member names, and a member is required exactly when the Go field
// is always written (no omitempty).
func TestProblemSchemaMatchesHTTPX(t *testing.T) {
	t.Parallel()

	schemas := loadFile(t, "components/problem.yaml").Components.Schemas
	for name, typ := range map[string]reflect.Type{
		"Problem":      reflect.TypeFor[httpx.Problem](),
		"ProblemField": reflect.TypeFor[httpx.ProblemField](),
	} {
		var members, required []string
		for i := range typ.NumField() {
			tag := typ.Field(i).Tag.Get("json")
			member, opts, _ := strings.Cut(tag, ",")
			members = append(members, member)
			if !strings.Contains(opts, "omitempty") {
				required = append(required, member)
			}
		}
		s := schemas[name].Value
		props := make([]string, 0, len(s.Properties))
		for p := range s.Properties {
			props = append(props, p)
		}
		slices.Sort(members)
		slices.Sort(props)
		slices.Sort(required)
		schemaRequired := slices.Sorted(slices.Values(s.Required))
		if !slices.Equal(members, props) {
			t.Errorf("%s: httpx members %v, schema properties %v", name, members, props)
		}
		if !slices.Equal(required, schemaRequired) {
			t.Errorf("%s: httpx always writes %v, schema requires %v", name, required, schemaRequired)
		}
	}
}

// A field error names either a body member (pointer) or a parameter
// (parameter + in), never both and never neither.
func TestProblemFieldIsPointerXorParameter(t *testing.T) {
	t.Parallel()

	field := loadFile(t, "components/problem.yaml").Components.Schemas["ProblemField"].Value
	tests := []struct {
		value map[string]any
		valid bool
	}{
		{map[string]any{"pointer": "/a", "code": "FIELD_REQUIRED"}, true},
		{map[string]any{"pointer": "", "code": "FIELD_REQUIRED"}, true},
		{map[string]any{"parameter": "limit", "in": "QUERY", "code": "FIELD_OUT_OF_RANGE"}, true},
		{map[string]any{"code": "FIELD_REQUIRED"}, false},
		{map[string]any{"parameter": "limit", "code": "FIELD_REQUIRED"}, false},
		{map[string]any{"in": "QUERY", "code": "FIELD_REQUIRED"}, false},
		{map[string]any{"pointer": "/a", "parameter": "limit", "in": "QUERY", "code": "FIELD_REQUIRED"}, false},
	}
	for _, tt := range tests {
		err := field.VisitJSON(tt.value, openapi3.EnableJSONSchema2020())
		if (err == nil) != tt.valid {
			t.Errorf("%v: valid = %v, want %v (%v)", tt.value, err == nil, tt.valid, err)
		}
	}
}

// MoneyInput is Money for requests: the same members and constraints, but
// closed to unknown members (money.yaml).
func TestMoneyVariantsMatch(t *testing.T) {
	t.Parallel()

	schemas := loadFile(t, "components/money.yaml").Components.Schemas
	money, input := schemas["Money"].Value, schemas["MoneyInput"].Value
	if !slices.Equal(money.Required, input.Required) || len(money.Properties) != len(input.Properties) {
		t.Fatalf("Money %v/%d vs MoneyInput %v/%d", money.Required, len(money.Properties), input.Required, len(input.Properties))
	}
	shape := func(ref *openapi3.SchemaRef) string {
		if ref.Ref != "" {
			return ref.Ref
		}
		v := *ref.Value
		v.Description, v.Examples = "", nil
		b, err := json.Marshal(&v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for name, p := range money.Properties {
		q, ok := input.Properties[name]
		if !ok || shape(p) != shape(q) {
			t.Errorf("member %s differs between Money and MoneyInput", name)
		}
	}
	if input.AdditionalProperties.Has == nil || *input.AdditionalProperties.Has {
		t.Error("MoneyInput must have additionalProperties: false")
	}
	if money.AdditionalProperties.Has != nil {
		t.Error("Money is a response schema; it must not close additionalProperties")
	}
}

// Every example in the contract is valid against its own schema.
func TestExamplesMatchTheirSchemas(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, name := range specFiles(t) {
		doc := loadFile(t, name)
		w := exampleWalker{t: t, file: name, seen: map[*openapi3.Schema]bool{}}
		c := doc.Components
		if c != nil {
			for n, s := range c.Schemas {
				w.schema("schemas/"+n, s)
			}
			for n, p := range c.Parameters {
				w.schema("parameters/"+n, p.Value.Schema)
			}
			for n, h := range c.Headers {
				w.schema("headers/"+n, h.Value.Schema)
			}
			for n, r := range c.Responses {
				w.content("responses/"+n, r.Value.Content)
			}
		}
		if doc.Paths != nil {
			for path, item := range doc.Paths.Map() {
				for method, op := range item.Operations() {
					where := method + " " + path
					for _, p := range op.Parameters {
						w.schema(where+" "+p.Value.Name, p.Value.Schema)
					}
					if op.RequestBody != nil {
						w.content(where+" body", op.RequestBody.Value.Content)
					}
					for code, r := range op.Responses.Map() {
						w.content(where+" "+code, r.Value.Content)
					}
				}
			}
		}
		checked += w.checked
	}
	if checked < 20 {
		t.Fatalf("only %d examples checked; the walker misses some", checked)
	}
}

type exampleWalker struct {
	t       *testing.T
	file    string
	seen    map[*openapi3.Schema]bool
	checked int
}

func (w *exampleWalker) check(where string, s *openapi3.Schema, example any) {
	w.t.Helper()
	w.checked++
	opts := []openapi3.SchemaValidationOption{
		openapi3.EnableJSONSchema2020(),
		openapi3.WithStringFormatValidator("uuid",
			openapi3.NewRegexpFormatValidator(openapi3.FormatOfStringForUUIDOfRFC9562)),
	}
	if err := s.VisitJSON(example, opts...); err != nil {
		w.t.Errorf("%s: %s: example %v does not match: %v", w.file, where, example, err)
	}
}

func (w *exampleWalker) content(where string, content openapi3.Content) {
	for mt, m := range content {
		if m.Schema == nil {
			continue
		}
		if m.Example != nil {
			w.check(where+" "+mt, m.Schema.Value, m.Example)
		}
		for n, ex := range m.Examples {
			w.check(where+" "+mt+" "+n, m.Schema.Value, ex.Value.Value)
		}
		w.schema(where+" "+mt, m.Schema)
	}
}

func (w *exampleWalker) schema(where string, ref *openapi3.SchemaRef) {
	if ref == nil || ref.Value == nil || w.seen[ref.Value] {
		return
	}
	s := ref.Value
	w.seen[s] = true
	for _, ex := range s.Examples {
		w.check(where, s, ex)
	}
	if s.Example != nil {
		w.check(where, s, s.Example)
	}
	for n, p := range s.Properties {
		w.schema(where+"."+n, p)
	}
	for _, c := range slices.Concat(s.AllOf, s.AnyOf, s.OneOf, openapi3.SchemaRefs{s.Items, s.AdditionalProperties.Schema}) {
		w.schema(where, c)
	}
}

// Timestamp is UTC: the response writes Z, and an offset is rejected.
func TestTimestampRequiresZ(t *testing.T) {
	t.Parallel()

	ts := loadFile(t, "components/datetime.yaml").Components.Schemas["Timestamp"].Value
	if err := ts.VisitJSON("2026-08-27T06:30:00Z", openapi3.EnableJSONSchema2020()); err != nil {
		t.Errorf("UTC rejected: %v", err)
	}
	for _, v := range []string{"2026-08-27T09:30:00+03:00", "2026-08-27T06:30:00+00:00"} {
		if err := ts.VisitJSON(v, openapi3.EnableJSONSchema2020()); err == nil {
			t.Errorf("%s accepted, want only Z", v)
		}
	}
}
