package httpx

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// What the validator cannot classify is a server error: 500, logged, and
// described without the request values its messages may carry.
func TestRequestValidationErrorUnknownIsServerError(t *testing.T) {
	t.Parallel()

	const secret = "TR330006100519786457841326"
	tests := map[string]error{
		"security": &openapi3filter.SecurityRequirementsError{Errors: []error{errors.New("token " + secret)}},
		"body error of an unknown type": &openapi3filter.RequestError{
			RequestBody: &openapi3.RequestBody{}, Err: errors.New("compile schema: " + secret),
		},
		"parameter error of an unknown type": &openapi3filter.RequestError{
			Parameter: &openapi3.Parameter{Name: "id", In: "path"}, Err: errors.New(secret),
		},
		"unsupported parameter format": &openapi3filter.RequestError{
			Parameter: &openapi3.Parameter{Name: "id", In: "path"},
			Err:       &openapi3filter.ParseError{Kind: openapi3filter.KindUnsupportedFormat, Reason: secret},
		},
	}
	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := requestValidationError(openapi3.MultiError{err})
			if e := apperr.From(got); e.Kind != apperr.KindInternal {
				t.Fatalf("kind = %s, want internal (500)", e.Kind)
			}
			if strings.Contains(got.Error(), secret) {
				t.Fatalf("server error carries a request value: %v", got)
			}
		})
	}
}

// Client errors carry no cause: the validator's message stays out of the
// error chain entirely.
func TestRequestValidationErrorClientErrorsHaveNoCause(t *testing.T) {
	t.Parallel()

	const secret = "TR330006100519786457841326"
	err := &openapi3filter.RequestError{
		RequestBody: &openapi3.RequestBody{},
		Err:         &openapi3filter.ParseError{Kind: openapi3filter.KindInvalidFormat, Reason: secret},
	}
	got := requestValidationError(openapi3.MultiError{err})
	if e := apperr.From(got); e.Kind != apperr.KindInvalid || e.Code != CodeBodyMalformed {
		t.Fatalf("got %s %s", e.Kind, e.Code)
	}
	if errors.Unwrap(got) != nil || strings.Contains(got.Error(), secret) {
		t.Fatalf("client error keeps the validator's message: %v", got)
	}
}

// TestSchemaUses2020FindsAKeywordAnywhere pins the property the whole
// 2020-12 path rests on: a keyword only that validator checks is
// detected wherever it sits in a request body schema, so the operation
// is compiled and the keyword is really enforced. A missed keyword is
// not a loose end -- it is a rule written in the contract and checked by
// nobody, because kin-openapi's 3.0 validator ignores it and nothing
// else would run.
//
// The table is every slot a schema can hold another schema in, which is
// the vocabulary subschemas2020 enumerates and the one inlineSchema
// serializes. Each row buries a keyword one level under one slot, so a
// slot dropped from the list is a red row (ADR-0031 Karar 5).
//
// What the table does **not** cover, and cannot: the three keywords in
// schemaUses2020Here that hold no schema at all -- dependentRequired,
// $dynamicRef and nullable. A slot row cannot reach them, so they get
// rows of their own below; dependentRequired is the one the contract
// actually uses.
func TestSchemaUses2020FindsAKeywordAnywhere(t *testing.T) {
	t.Parallel()

	// A 2020-only keyword (`const`, or `"a"` for propertyNames) buried
	// one level under each slot.
	slots := map[string]string{
		"properties":            `{"type":"object","properties":{"a":{"const":1}}}`,
		"allOf":                 `{"allOf":[{"const":1}]}`,
		"anyOf":                 `{"anyOf":[{"const":1}]}`,
		"oneOf":                 `{"oneOf":[{"const":1}]}`,
		"not":                   `{"not":{"const":1}}`,
		"items":                 `{"type":"array","items":{"const":1}}`,
		"additionalProperties":  `{"type":"object","additionalProperties":{"const":1}}`,
		"prefixItems":           `{"type":"array","prefixItems":[{"const":1}]}`,
		"patternProperties":     `{"type":"object","patternProperties":{"^a":{"const":1}}}`,
		"dependentSchemas":      `{"type":"object","dependentSchemas":{"a":{"const":1}}}`,
		"contains":              `{"type":"array","contains":{"const":1}}`,
		"propertyNames":         `{"type":"object","propertyNames":{"const":"a"}}`,
		"if":                    `{"if":{"const":1}}`,
		"then":                  `{"then":{"const":1}}`,
		"else":                  `{"else":{"const":1}}`,
		"$defs":                 `{"$defs":{"a":{"const":1}}}`,
		"contentSchema":         `{"type":"string","contentSchema":{"const":1}}`,
		"unevaluatedProperties": `{"type":"object","unevaluatedProperties":{"const":1}}`,
		"unevaluatedItems":      `{"type":"array","unevaluatedItems":{"const":1}}`,
	}
	// The keywords that hold no schema, so no slot row reaches them.
	// dependentRequired is live in the contract (components/problem.yaml,
	// in a response schema, so it is not compiled today); the other two
	// are not used anywhere and are here so that dropping them from
	// schemaUses2020Here is red rather than silent.
	bare := map[string]string{
		"dependentRequired": `{"type":"object","dependentRequired":{"a":["b"]}}`,
		"$dynamicRef":       `{"$dynamicRef":"#meta"}`,
		"nullable":          `{"type":"string","nullable":true}`,
	}
	for name, schema := range slots {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertUses2020(t, schema, "a 2020-only keyword under "+name)
		})
	}
	for name, schema := range bare {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertUses2020(t, schema, "the 2020-only keyword "+name)
		})
	}

	// The negative case, so the tables above cannot pass by answering
	// true to everything: a schema built only from keywords the 3.0
	// validator checks needs no second validator.
	t.Run("no 2020 keyword", func(t *testing.T) {
		t.Parallel()

		const plain = `{"type":"object","additionalProperties":false,"required":["a"],
			"properties":{"a":{"type":"string","maxLength":5},"b":{"type":"array","maxItems":2}}}`
		if anywhere, _ := uses2020(unmarshalSchema(t, plain)); anywhere {
			t.Error("a schema with no 2020-only keyword was sent down the 2020 path")
		}
	})
}

func assertUses2020(t *testing.T, schema, what string) {
	t.Helper()

	if anywhere, _ := uses2020(unmarshalSchema(t, schema)); !anywhere {
		t.Errorf("%s is not detected: the body would be checked by the 3.0 validator alone, "+
			"which ignores the keyword, and the rule would not be enforced", what)
	}
}

func unmarshalSchema(t *testing.T, schema string) *openapi3.Schema {
	t.Helper()

	var s openapi3.Schema
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

// TestUses2020SeesASharedSchemaTwice is the one walk's other half: the
// same schema object can be reached both through an ordinary member and
// through a `oneOf`, and only the second reading can refuse the spec.
// While `seen` was keyed on the schema alone the first reading swallowed
// the second, so a schema shared between the two -- which is what a
// `$ref` used twice is -- hid its keyword from the start-up gate.
//
// The two members are an allOf and therefore ordered, so the ordinary
// reading really does come first and the test cannot pass by luck the
// way a map's iteration order would allow.
func TestUses2020SeesASharedSchemaTwice(t *testing.T) {
	t.Parallel()

	shared := openapi3.NewSchemaRef("", &openapi3.Schema{Contains: openapi3.NewSchemaRef("", &openapi3.Schema{})})
	root := &openapi3.Schema{AllOf: openapi3.SchemaRefs{
		openapi3.NewSchemaRef("", &openapi3.Schema{Properties: openapi3.Schemas{"a": shared}}),
		openapi3.NewSchemaRef("", &openapi3.Schema{OneOf: openapi3.SchemaRefs{shared}}),
	}}
	anywhere, under := uses2020(root)
	if !anywhere {
		t.Error("the shared schema's keyword was not detected at all")
	}
	if !under {
		t.Error("the keyword below the oneOf was not seen, so the spec would be accepted and " +
			"every body on that operation refused by the 3.0 validator")
	}
}

// fields2020 maps each JSON Schema 2020 error kind to a field error at the
// member it is about. Formats are asserted here only to produce a Format
// error: in production kin-openapi checks formats before this validator.
//
// Every branch of fields2020 is exercised from here, so a kind that is
// moved into the default branch -- where it would become
// FIELD_VALUE_INVALID instead of the code the contract documents -- is
// red in this table.
func TestFields2020(t *testing.T) {
	t.Parallel()

	const schema = `{
		"type": "object",
		"required": ["a", "b"],
		"additionalProperties": false,
		"properties": {
			"a": {"type": "string"},
			"b": {"type": "integer", "minimum": 1, "maximum": 10},
			"s": {"type": "string", "maxLength": 2},
			"list": {"type": "array", "maxItems": 1},
			"at": {"type": "string", "format": "date-time"},
			"k": {"const": "K"},
			"covers": {
				"type": "array",
				"contains": {"type": "object", "required": ["cover"], "properties": {"cover": {"const": true}}},
				"minContains": 0,
				"maxContains": 1
			},
			"needsCover": {
				"type": "array",
				"contains": {"type": "object", "required": ["cover"], "properties": {"cover": {"const": true}}},
				"maxContains": 1
			},
			"atLeastTwo": {"type": "array", "contains": {"const": "x"}, "minContains": 2}
		}
	}`
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("https://api.iskoala.com/schemas/test", doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("https://api.iskoala.com/schemas/test")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		value string
		want  []apperr.FieldError
	}{
		{"required", `{}`, []apperr.FieldError{
			{Pointer: "/a", Code: CodeFieldRequired}, {Pointer: "/b", Code: CodeFieldRequired},
		}},
		{"additional properties", `{"a":"x","b":1,"x~y":1}`, []apperr.FieldError{
			{Pointer: "/x~0y", Code: CodeFieldUnknown},
		}},
		{"type", `{"a":1,"b":1}`, []apperr.FieldError{{Pointer: "/a", Code: CodeFieldTypeInvalid}}},
		{"format", `{"a":"x","b":1,"at":"yesterday"}`, []apperr.FieldError{{Pointer: "/at", Code: CodeFieldFormatInvalid}}},
		{"minimum", `{"a":"x","b":0}`, []apperr.FieldError{{Pointer: "/b", Code: CodeFieldOutOfRange}}},
		{"maximum", `{"a":"x","b":11}`, []apperr.FieldError{{Pointer: "/b", Code: CodeFieldOutOfRange}}},
		{"maxLength", `{"a":"x","b":1,"s":"abc"}`, []apperr.FieldError{{Pointer: "/s", Code: CodeFieldOutOfRange}}},
		{"maxItems", `{"a":"x","b":1,"list":[1,2]}`, []apperr.FieldError{{Pointer: "/list", Code: CodeFieldOutOfRange}}},
		{"const", `{"a":"x","b":1,"k":"L"}`, []apperr.FieldError{{Pointer: "/k", Code: CodeFieldValueInvalid}}},
		// maxContains is the "at most one cover" invariant of the gallery
		// (ADR-0031 Karar 1). The library reports it as a leaf error, so
		// it reaches this mapping and not the causes below it.
		{"maxContains", `{"a":"x","b":1,"covers":[{"cover":true},{"cover":true}]}`,
			[]apperr.FieldError{{Pointer: "/covers", Code: CodeFieldOutOfRange}}},
		{"minContains", `{"a":"x","b":1,"atLeastTwo":["x"]}`,
			[]apperr.FieldError{{Pointer: "/atLeastTwo", Code: CodeFieldOutOfRange}}},
		// The same schema without minContains: 0. JSON Schema 2020-12
		// defaults minContains to 1, so `contains` then demands a match
		// and a gallery with no cover marked is refused -- which is the
		// compatibility promise ADR-0031 Karar 1 rests on. The row is
		// here so that dropping `minContains: 0` from the contract has a
		// visible meaning and not only a comment.
		{"contains without minContains", `{"a":"x","b":1,"needsCover":[]}`,
			[]apperr.FieldError{{Pointer: "/needsCover", Code: CodeFieldValueInvalid}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := jsonschema.UnmarshalJSON(strings.NewReader(tt.value))
			if err != nil {
				t.Fatal(err)
			}
			var verr *jsonschema.ValidationError
			if !errors.As(sch.Validate(v), &verr) {
				t.Fatal("no validation error")
			}
			if got := sortFields(fields2020(verr, nil)); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
