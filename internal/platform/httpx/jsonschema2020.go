package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// compiled2020 holds an operation's request body schemas compiled once
// with the JSON Schema 2020-12 validator, by media type (ADR-0019 Karar 2).
type compiled2020 struct {
	content openapi3.Content
	schemas map[*openapi3.MediaType]*jsonschema.Schema
}

// compileBody2020 compiles the JSON request body schemas of op when one of
// them uses a keyword kin-openapi's OpenAPI 3.0 validator does not check.
// It returns nil when none does. Refused at start-up:
//   - a parameter schema with such a keyword: parameters are decoded
//     inside kin-openapi, where only its per-request-compiling 2020
//     validator could check them;
//   - such a keyword under `not` or `oneOf`: the 3.0 validator, which runs
//     first, ignores it, so `not` would reject every value and `oneOf`
//     would match several branches, a 400 the body does not deserve.
func compileBody2020(name string, item *openapi3.PathItem, op *openapi3.Operation) (*compiled2020, error) {
	for _, p := range slices.Concat(item.Parameters, op.Parameters) {
		if p == nil || p.Value == nil || p.Value.Schema == nil {
			continue
		}
		if anywhere, _ := uses2020(p.Value.Schema.Value); anywhere {
			return nil, fmt.Errorf("parameter %s uses a JSON Schema 2020-only keyword; parameters may not", p.Value.Name)
		}
	}
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil, nil
	}
	needed := false
	for media, mt := range op.RequestBody.Value.Content {
		if mt.Schema == nil {
			continue
		}
		anywhere, underNotOrOneOf := uses2020(mt.Schema.Value)
		// The refusal is checked before the decision to compile, so a
		// schema this package cannot honour never reaches the compiler.
		if underNotOrOneOf {
			return nil, fmt.Errorf("request body %s: a JSON Schema 2020-only keyword under not or oneOf is not supported", media)
		}
		if anywhere {
			needed = true
		}
	}
	if !needed {
		return nil, nil
	}
	out := &compiled2020{content: op.RequestBody.Value.Content, schemas: map[*openapi3.MediaType]*jsonschema.Schema{}}
	for media, mt := range op.RequestBody.Value.Content {
		if mt.Schema == nil {
			continue
		}
		if !isJSONMediaType(media) {
			return nil, fmt.Errorf("request body %s: JSON Schema 2020 keywords are only supported for JSON bodies", media)
		}
		doc, err := inlineSchema(mt.Schema, nil)
		if err != nil {
			return nil, fmt.Errorf("request body %s: %w", media, err)
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		url := "https://api.iskoala.com/schemas/" + strings.ReplaceAll(name+" "+media, " ", "_")
		if err := c.AddResource(url, doc); err != nil {
			return nil, fmt.Errorf("request body %s: %w", media, err)
		}
		sch, err := c.Compile(url)
		if err != nil {
			return nil, fmt.Errorf("request body %s: compile: %w", media, err)
		}
		out.schemas[mt] = sch
	}
	return out, nil
}

// uses2020 walks s and answers the two questions compileBody2020 asks of
// a schema: whether it uses a keyword only the JSON Schema 2020-12
// validator checks, and whether one of those keywords sits below a `not`
// or a `oneOf`.
//
// One walk answers both on purpose. They used to be two walks with two
// hand written child lists, and the lists drifted: the rejection gate
// reached seven of the nineteen slots a schema can nest another schema
// in, so `{contains: {not: {const: 1}}}` in a request body was neither
// refused nor validated -- the 3.0 validator ran alone, ignored the
// `not`, and every body on that operation would have been refused with
// 400. Nothing was red. One list cannot drift from itself, and a slot
// added to it is known to both answers at once (ADR-0031 Karar 5).
func uses2020(s *openapi3.Schema) (anywhere, underNotOrOneOf bool) {
	w := walker2020{seen: map[visit2020]bool{}}
	w.walk(s, false)
	return w.anywhere, w.under
}

// visit2020 is a schema looked at with a particular answer to "are we
// below a not or a oneOf".
type visit2020 struct {
	schema *openapi3.Schema
	under  bool
}

type walker2020 struct {
	seen     map[visit2020]bool
	anywhere bool
	under    bool
}

// walk visits s and everything nested in it. under is true once a `not`
// or a `oneOf` has been passed.
//
// seen is keyed by the pair and not by the schema alone, which is a fix
// and not a detail: a schema reached first through an ordinary member
// and later through a `oneOf` has to be looked at twice, because the
// second visit is the one that can refuse the spec. Keying on the schema
// alone let the first visit swallow the second, so a schema shared
// between an ordinary member and a `oneOf` -- which is what a `$ref`
// used twice is -- hid its keyword from the gate.
func (w *walker2020) walk(s *openapi3.Schema, under bool) {
	if s == nil || w.seen[visit2020{schema: s, under: under}] {
		return
	}
	w.seen[visit2020{schema: s, under: under}] = true
	if schemaUses2020Here(s) {
		w.anywhere = true
		if under {
			w.under = true
		}
	}
	// `not` and `oneOf` are the two the 3.0 validator misreads, so
	// everything below them is "under" whatever the caller said; every
	// other slot inherits.
	for _, ref := range slices.Concat(openapi3.SchemaRefs{s.Not}, s.OneOf) {
		if ref != nil {
			w.walk(ref.Value, true)
		}
	}
	for _, ref := range subschemas2020(s) {
		if ref != nil {
			w.walk(ref.Value, under)
		}
	}
}

// subschemas2020 is every slot a schema can hold another schema in,
// except `not` and `oneOf`, which walk handles itself. It is the same
// vocabulary inlineSchema serializes, and the two lists are the only
// places in this package that enumerate it.
func subschemas2020(s *openapi3.Schema) openapi3.SchemaRefs {
	out := slices.Concat(s.AllOf, s.AnyOf, s.PrefixItems, openapi3.SchemaRefs{
		s.Items, s.Contains, s.PropertyNames, s.If, s.Then, s.Else, s.ContentSchema,
		s.AdditionalProperties.Schema, s.UnevaluatedItems.Schema, s.UnevaluatedProperties.Schema,
	})
	for _, named := range []openapi3.Schemas{s.Properties, s.PatternProperties, s.DependentSchemas, s.Defs} {
		for _, ref := range named {
			out = append(out, ref)
		}
	}
	return out
}

func isJSONMediaType(media string) bool {
	media = strings.ToLower(media)
	return media == "application/json" || strings.HasSuffix(media, "+json")
}

// inlineSchema turns a resolved kin-openapi schema into a self-contained
// JSON Schema document: every $ref is replaced by the schema it resolves
// to, so the compiler never fetches anything. Formats are left to
// kin-openapi's validator, which runs first; here they stay annotations.
func inlineSchema(ref *openapi3.SchemaRef, stack []*openapi3.Schema) (any, error) {
	s := ref.Value
	if s == nil {
		return nil, errors.New("unresolved schema reference " + ref.Ref)
	}
	if slices.Contains(stack, s) {
		return nil, errors.New("recursive schema " + ref.Ref)
	}
	stack = append(stack, s)

	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	one := func(key string, child *openapi3.SchemaRef) error {
		if child == nil {
			return nil
		}
		v, err := inlineSchema(child, stack)
		if err == nil {
			m[key] = v
		}
		return err
	}
	list := func(key string, children openapi3.SchemaRefs) error {
		if len(children) == 0 {
			return nil
		}
		vs := make([]any, len(children))
		for i, c := range children {
			v, err := inlineSchema(c, stack)
			if err != nil {
				return err
			}
			vs[i] = v
		}
		m[key] = vs
		return nil
	}
	named := func(key string, children openapi3.Schemas) error {
		if len(children) == 0 {
			return nil
		}
		vs := make(map[string]any, len(children))
		for n, c := range children {
			v, err := inlineSchema(c, stack)
			if err != nil {
				return err
			}
			vs[n] = v
		}
		m[key] = vs
		return nil
	}
	return m, errors.Join(
		named("properties", s.Properties), named("patternProperties", s.PatternProperties),
		named("dependentSchemas", s.DependentSchemas), named("$defs", s.Defs),
		list("allOf", s.AllOf), list("anyOf", s.AnyOf), list("oneOf", s.OneOf), list("prefixItems", s.PrefixItems),
		one("not", s.Not), one("items", s.Items), one("contains", s.Contains), one("propertyNames", s.PropertyNames),
		one("if", s.If), one("then", s.Then), one("else", s.Else), one("contentSchema", s.ContentSchema),
		one("additionalProperties", s.AdditionalProperties.Schema),
		one("unevaluatedProperties", s.UnevaluatedProperties.Schema),
		one("unevaluatedItems", s.UnevaluatedItems.Schema),
	)
}

// validate checks the validated body value against the compiled schema for
// the request's media type, found the way kin-openapi found it (parameters
// and case ignored, wildcards allowed). A media type without a compiled
// schema is not one the operation accepts: 415.
func (c *compiled2020) validate(contentType string, value any) error {
	sch, ok := c.schemas[c.content.Get(contentType)]
	if !ok {
		return apperr.New(apperr.KindUnsupportedMediaType, CodeContentTypeUnsupported)
	}
	err := sch.Validate(value)
	var verr *jsonschema.ValidationError
	if err == nil || !errors.As(err, &verr) {
		if err != nil {
			return fmt.Errorf("validate request: JSON Schema 2020: %T", err)
		}
		return nil
	}
	fields := fields2020(verr, nil)
	if len(fields) == 0 {
		return fmt.Errorf("validate request: JSON Schema 2020 error without a field: %T", verr.ErrorKind)
	}
	return apperr.New(apperr.KindInvalid, CodeRequestInvalid).WithFields(sortFields(fields)...)
}

// fields2020 maps the leaves of a JSON Schema validation error to field
// errors. Only kinds and locations are read, never the messages, which
// contain request values.
func fields2020(verr *jsonschema.ValidationError, out []apperr.FieldError) []apperr.FieldError {
	if len(verr.Causes) > 0 {
		for _, c := range verr.Causes {
			out = fields2020(c, out)
		}
		return out
	}
	base := jsonPointer(verr.InstanceLocation)
	member := func(code apperr.Code, names []string) {
		for _, n := range names {
			out = append(out, apperr.FieldError{Pointer: base + "/" + escapePointer(n), Code: code})
		}
	}
	switch k := verr.ErrorKind.(type) {
	case *kind.Required:
		member(CodeFieldRequired, k.Missing)
	case *kind.AdditionalProperties:
		member(CodeFieldUnknown, k.Properties)
	case *kind.FalseSchema:
		// unevaluatedProperties: false fails a false schema at the member.
		out = append(out, apperr.FieldError{Pointer: base, Code: CodeFieldUnknown})
	case *kind.Type:
		out = append(out, apperr.FieldError{Pointer: base, Code: CodeFieldTypeInvalid})
	case *kind.Format:
		out = append(out, apperr.FieldError{Pointer: base, Code: CodeFieldFormatInvalid})
	case *kind.Minimum, *kind.Maximum, *kind.ExclusiveMinimum, *kind.ExclusiveMaximum,
		*kind.MinLength, *kind.MaxLength, *kind.MinItems, *kind.MaxItems,
		*kind.MinProperties, *kind.MaxProperties, *kind.MinContains, *kind.MaxContains:
		out = append(out, apperr.FieldError{Pointer: base, Code: CodeFieldOutOfRange})
	default:
		out = append(out, apperr.FieldError{Pointer: base, Code: CodeFieldValueInvalid})
	}
	return out
}
