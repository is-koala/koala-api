package media_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// TestPublicReadableIsTheStoresRule binds the two places that answer "is
// this object public": a kind's PublicReadable and the address
// Store.PublicURL resolves for a key under the kind's prefix (ADR-0030,
// "İki liste, bir kural"; ADR-0035 Karar 5). Both read
// storage.Prefix.Public now, so they cannot drift; this test is what
// turns red if one of them stops reading it.
func TestPublicReadableIsTheStoresRule(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	id := uuid.Must(uuid.NewV7())
	public := 0
	for _, kind := range media.Kinds() {
		prefix, ok := kind.Prefix()
		if !ok {
			t.Fatalf("%s has no prefix", kind)
		}
		key, err := storage.NewKey(prefix, id, storage.ExtJPEG)
		if err != nil {
			t.Fatal(err)
		}
		_, resolves := h.store.PublicURL(key)
		if resolves != kind.PublicReadable() {
			t.Errorf("%s: PublicReadable() = %v, Store.PublicURL resolves %v", kind, kind.PublicReadable(), resolves)
		}
		if kind.PublicReadable() {
			public++
		}
	}
	// Both answers false for every kind would pass the loop above.
	if public == 0 || public == len(media.Kinds()) {
		t.Fatalf("%d of %d kinds are public; the comparison proves nothing", public, len(media.Kinds()))
	}
	for _, prefix := range storage.Prefixes() {
		key, err := storage.NewKey(prefix, id, storage.ExtPNG)
		if err != nil {
			t.Fatal(err)
		}
		if _, resolves := h.store.PublicURL(key); resolves != prefix.Public() {
			t.Errorf("%s: Store.PublicURL resolves %v, Prefix.Public() = %v", prefix, resolves, prefix.Public())
		}
	}
	if storage.PrefixIdentityDocument.Public() || storage.PrefixIncoming.Public() {
		t.Error("IDN-007: identity documents or the quarantine are public")
	}
}
