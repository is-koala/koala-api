package app

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/platform/imaging"
)

// The media kind, the media status and the detected content type exist
// in three places: the
// contract's enum (api/openapi.yaml), the generated constants
// (internal/apiv1) and the domain type that decides a prefix, a type
// allowlist and a cache rule from them (internal/media). Nothing bound
// any pair of the three, so the only thing keeping them in step was
// someone remembering to look -- and media.Kind's own doc says it
// mirrors the contract by hand.
//
// What a drift costs is specific. A kind the contract grows and
// internal/media has not learned resolves to no prefix, so the pipeline
// refuses every upload of it; that fails closed, but it fails at
// runtime, on a kind the client was told it may send. A kind
// internal/media carries and the contract does not can never arrive, so
// its prefix, limits and cache rule are dead code that reads as live.
//
// These tests bind both directions, the way
// TestPlatformCodesAreInTheContract binds the error codes.
//
// The content type was the pair this file first missed. Its drift costs
// the same in both directions and one of them is visible to clients: a
// type internal/platform/imaging accepts and the contract's enum does
// not means Complete records -- and a response carries -- a
// MediaContentType the contract never declared, which nothing catches
// because responses are not validated. The other direction is a dead
// enum member the server can never produce (ADR-0031 Karar 2).

func TestMediaKindsMatchTheContract(t *testing.T) {
	t.Parallel()

	kinds := media.Kinds()
	if len(kinds) == 0 {
		t.Fatal("media.Kinds() is empty; the test would pass vacuously")
	}
	for _, kind := range kinds {
		if !apiv1.MediaKind(kind).Valid() {
			t.Errorf("media.Kind %q is not a member of the contract's MediaKind enum", kind)
		}
	}

	for _, value := range enumOf(t, "MediaKind") {
		if !slices.Contains(kinds, media.Kind(value)) {
			t.Errorf("the contract's MediaKind has %q, which internal/media does not carry; "+
				"an upload of that kind would resolve to no prefix and be refused", value)
		}
	}
}

func TestMediaStatusesMatchTheContract(t *testing.T) {
	t.Parallel()

	statuses := []media.Status{media.StatusPending, media.StatusReady, media.StatusRejected}
	for _, status := range statuses {
		if !apiv1.MediaStatus(status).Valid() {
			t.Errorf("media.Status %q is not a member of the contract's MediaStatus enum", status)
		}
	}

	values := enumOf(t, "MediaStatus")
	if len(values) != len(statuses) {
		t.Errorf("the contract's MediaStatus has %d members, internal/media has %d; "+
			"the state machine in media/domain.go covers the ones it knows", len(values), len(statuses))
	}
	for _, value := range values {
		if !slices.Contains(statuses, media.Status(value)) {
			t.Errorf("the contract's MediaStatus has %q, which internal/media does not carry", value)
		}
	}
}

// TestMediaContentTypesMatchTheContract binds the detected type to the
// contract's enum through the naming rule itself, in both directions.
func TestMediaContentTypesMatchTheContract(t *testing.T) {
	t.Parallel()

	types := imaging.Types()
	if len(types) == 0 {
		t.Fatal("imaging.Types() is empty; the test would pass vacuously")
	}
	values := enumOf(t, "MediaContentType")
	if len(values) != len(types) {
		t.Errorf("the contract's MediaContentType has %d members, imaging.Types() has %d", len(values), len(types))
	}

	spelled := make(map[string]imaging.Type, len(types))
	for _, typ := range types {
		value := contentTypeEnumValue(typ)
		spelled[value] = typ
		if !apiv1.MediaContentType(value).Valid() {
			t.Errorf("imaging.Type %q spells %q, which is not a member of the contract's MediaContentType enum; "+
				"a completed upload would record a type the contract does not declare", typ, value)
		}
	}
	for _, value := range values {
		if _, ok := spelled[value]; !ok {
			t.Errorf("the contract's MediaContentType has %q, which no imaging.Type spells; "+
				"the server can never produce it", value)
		}
	}
}

// contentTypeEnumValue is the contract's spelling of a media type: upper
// case, with every separator turned into an underscore (stil rehberi §1,
// "enum değerleri UPPER_SNAKE'tir").
//
// Writing the rule as code is the point. ADR-0031 Karar 2 refused to
// hand shorten the long WordprocessingML value precisely because the
// mapping has to stay mechanical in both directions; an enum value that
// no rule produces would make this function -- and therefore the binding
// above -- impossible to write.
func contentTypeEnumValue(typ imaging.Type) string {
	value := strings.ToUpper(string(typ))
	for _, separator := range []string{"/", ".", "-", "+"} {
		value = strings.ReplaceAll(value, separator, "_")
	}
	return value
}

// enumOf returns the string members of a named schema's enum in the
// contract. It fails when the schema is missing or carries no enum, so a
// renamed or reshaped schema is red rather than an empty loop.
func enumOf(t *testing.T, schema string) []string {
	t.Helper()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := spec.Components.Schemas[schema]
	if !ok || ref.Value == nil {
		t.Fatalf("the contract has no %s schema", schema)
	}
	if len(ref.Value.Enum) == 0 {
		t.Fatalf("the contract's %s schema carries no enum", schema)
	}
	out := make([]string, 0, len(ref.Value.Enum))
	for _, raw := range ref.Value.Enum {
		value, isString := raw.(string)
		if !isString {
			t.Fatalf("the contract's %s enum has a non-string member %#v", schema, raw)
		}
		out = append(out, value)
	}
	return out
}
