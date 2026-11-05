package api

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/is-koala/koala-api/internal/realtime"
)

// openRequestObjects returns the JSON paths of the request body objects in
// doc that accept unknown members (stil rehberi §4). It complements the
// vacuum rule koala-closed-request-body, which cannot follow allOf: vacuum
// only reaches an object that has its own `properties`, so an object built
// from allOf members (or a property defined that way) escaped it
// (issue #7).
//
// The rule it applies, on resolved schemas:
//   - An object (type object, properties or allOf) is closed by
//     `additionalProperties: false` or `unevaluatedProperties: false`.
//   - An allOf member is not closed on its own (it would refuse the other
//     members' properties); the composite closes it with
//     `unevaluatedProperties: false`. The members' properties are checked
//     like any other.
//   - oneOf and anyOf branches, array items and map values are checked.
func openRequestObjects(doc *openapi3.T) []string {
	var open []string
	for _, path := range slices.Sorted(maps.Keys(doc.Paths.Map())) {
		item := doc.Paths.Value(path)
		for _, method := range slices.Sorted(maps.Keys(item.Operations())) {
			op := item.Operations()[method]
			if op.RequestBody == nil || op.RequestBody.Value == nil {
				continue
			}
			for _, ct := range []string{"application/json", "application/merge-patch+json"} {
				mt := op.RequestBody.Value.Content.Get(ct)
				if mt == nil || mt.Schema == nil {
					continue
				}
				w := closedWalker{seen: map[*openapi3.Schema]bool{}}
				w.walk(mt.Schema, method+" "+path+" "+ct, false)
				open = append(open, w.open...)
			}
		}
	}
	return open
}

type closedWalker struct {
	seen map[*openapi3.Schema]bool
	open []string
}

func (w *closedWalker) walk(ref *openapi3.SchemaRef, at string, allOfMember bool) {
	if ref == nil || ref.Value == nil {
		return
	}
	s := ref.Value
	objectLike := len(s.Properties) > 0 || len(s.AllOf) > 0 || (s.Type != nil && s.Type.Includes("object"))
	closed := isFalse(s.AdditionalProperties) || isFalse(s.UnevaluatedProperties)
	if objectLike && !closed && !allOfMember {
		w.open = append(w.open, at)
	}
	if w.seen[s] {
		return
	}
	w.seen[s] = true
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		w.walk(s.Properties[name], at+".properties."+name, false)
	}
	for i, m := range s.AllOf {
		w.walk(m, at+".allOf["+strconv.Itoa(i)+"]", true)
	}
	for i, m := range s.OneOf {
		w.walk(m, at+".oneOf["+strconv.Itoa(i)+"]", false)
	}
	for i, m := range s.AnyOf {
		w.walk(m, at+".anyOf["+strconv.Itoa(i)+"]", false)
	}
	w.walk(s.Items, at+".items", false)
	w.walk(s.AdditionalProperties.Schema, at+".additionalProperties", false)
}

func isFalse(b openapi3.BoolSchema) bool { return b.Has != nil && !*b.Has }

// The contract's request bodies are closed, allOf members included.
func TestRequestBodiesAreClosed(t *testing.T) {
	t.Parallel()

	doc, err := Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if open := openRequestObjects(doc); len(open) > 0 {
		t.Fatalf("request body objects accept unknown members:\n%s", strings.Join(open, "\n"))
	}
}

// The check catches what vacuum cannot reach and accepts the correct
// allOf form.
func TestOpenRequestObjectsFindsAllOfGaps(t *testing.T) {
	t.Parallel()

	doc, err := openapi3.NewLoader().LoadFromData([]byte(`openapi: 3.1.0
info: {title: closed-body probe, version: "1"}
paths:
  /closed:
    post:
      requestBody:
        content:
          application/json:
            schema:
              allOf:
                - $ref: "#/components/schemas/Base"
                - type: object
                  properties:
                    extra: {type: string}
              unevaluatedProperties: false
      responses: {default: {description: x}}
  /open-composite:
    post:
      requestBody:
        content:
          application/json:
            schema:
              allOf:
                - $ref: "#/components/schemas/Base"
      responses: {default: {description: x}}
  /open-allof-property:
    post:
      requestBody:
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              properties:
                child:
                  allOf:
                    - $ref: "#/components/schemas/Base"
      responses: {default: {description: x}}
  /open-member-property:
    post:
      requestBody:
        content:
          application/json:
            schema:
              allOf:
                - type: object
                  properties:
                    inner:
                      type: object
                      properties:
                        x: {type: string}
              unevaluatedProperties: false
      responses: {default: {description: x}}
components:
  schemas:
    Base:
      type: object
      properties:
        name: {type: string}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := openRequestObjects(doc)
	want := []string{
		"POST /open-allof-property application/json.properties.child",
		"POST /open-composite application/json",
		"POST /open-member-property application/json.allOf[0].properties.inner",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("open objects\n got: %q\nwant: %q", got, want)
	}
}

// RealtimeTicket in the contract and realtime.TicketResponse, which
// internal/realtime writes by hand (the realtime operations are not
// generated), have the same members, all required.
func TestRealtimeTicketMatchesContract(t *testing.T) {
	t.Parallel()

	doc, err := Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := doc.Components.Schemas["RealtimeTicket"].Value
	typ := reflect.TypeFor[realtime.TicketResponse]()
	var members []string
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		if strings.Contains(tag, "omitempty") {
			t.Errorf("TicketResponse.%s is omitempty; every member is required", typ.Field(i).Name)
		}
		members = append(members, strings.Split(tag, ",")[0])
	}
	slices.Sort(members)
	props := slices.Sorted(maps.Keys(s.Properties))
	if !slices.Equal(members, props) || !slices.Equal(props, slices.Sorted(slices.Values(s.Required))) {
		t.Fatalf("TicketResponse members %v, schema properties %v, required %v", members, props, s.Required)
	}
}
