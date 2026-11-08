package app

import (
	"context"
	"slices"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/media"
)

// The media kind and the media status exist in three places: the
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
