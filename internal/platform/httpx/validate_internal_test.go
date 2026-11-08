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
// every slot inlineSchema serializes. It holds today by two mechanisms,
// and the split is worth writing down because reading schemaUses2020
// alone suggests a hole where there is none:
//
//   - properties, allOf, anyOf, oneOf, not, items and
//     additionalProperties are **walked**;
//   - prefixItems, patternProperties, dependentSchemas, contains,
//     propertyNames, if, then, else, $defs, contentSchema and the two
//     unevaluated slots are **not** walked and do not need to be: each
//     is itself a 2020-only keyword, so a schema that has one is
//     reported by schemaUses2020Here before its contents are read.
//
// The two sets cover every slot with nothing left over, which is why the
// child list is deliberately not widened (ADR-0031 Karar 5). The cover
// is what this table keeps honest: narrowing schemaUses2020Here --
// dropping `contains` from it, say -- while leaving the walk alone opens
// the hole and turns a row red. The same argument covers
// uses2020UnderNotOrOneOf, which walks the same two sets.
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
	for name, schema := range slots {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var s openapi3.Schema
			if err := json.Unmarshal([]byte(schema), &s); err != nil {
				t.Fatal(err)
			}
			if !schemaUses2020(&s, map[*openapi3.Schema]bool{}) {
				t.Errorf("a 2020-only keyword under %s is not detected: the body would be checked by "+
					"the 3.0 validator alone, which ignores the keyword, and the rule would not be enforced", name)
			}
		})
	}

	// The negative case, so the table above cannot pass by answering
	// true to everything: a schema built only from keywords the 3.0
	// validator checks needs no second validator.
	t.Run("no 2020 keyword", func(t *testing.T) {
		t.Parallel()

		var s openapi3.Schema
		const plain = `{"type":"object","additionalProperties":false,"required":["a"],
			"properties":{"a":{"type":"string","maxLength":5},"b":{"type":"array","maxItems":2}}}`
		if err := json.Unmarshal([]byte(plain), &s); err != nil {
			t.Fatal(err)
		}
		if schemaUses2020(&s, map[*openapi3.Schema]bool{}) {
			t.Error("a schema with no 2020-only keyword was sent down the 2020 path")
		}
	})
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
