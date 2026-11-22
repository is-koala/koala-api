package media_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// TestKindRules is the table of ADR-0029 Karar 5 and 6: which prefix,
// which types, which disposition and which cache rule each kind of
// upload gets.
func TestKindRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind       media.Kind
		prefix     storage.Prefix
		types      []imaging.Type
		attachment bool
		public     bool
	}{
		{media.KindProfilePicture, storage.PrefixImages, []imaging.Type{imaging.TypeJPEG, imaging.TypePNG}, false, true},
		{media.KindCompanyLogo, storage.PrefixImages, []imaging.Type{imaging.TypeJPEG, imaging.TypePNG}, false, true},
		{media.KindCompanyImage, storage.PrefixImages, []imaging.Type{imaging.TypeJPEG, imaging.TypePNG}, false, true},
		{media.KindIdentityDocument, storage.PrefixIdentityDocument, []imaging.Type{imaging.TypeJPEG, imaging.TypePNG}, true, false},
		{media.KindCV, storage.PrefixFiles, []imaging.Type{imaging.TypePDF, imaging.TypeDOC, imaging.TypeDOCX}, true, false},
	}
	if len(tests) != len(media.Kinds()) {
		t.Fatalf("the table covers %d kinds, Kinds() lists %d; a kind added to the contract needs a row here",
			len(tests), len(media.Kinds()))
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			t.Parallel()

			prefix, ok := tt.kind.Prefix()
			if !ok || prefix != tt.prefix {
				t.Errorf("Prefix() = %q, %v; want %q", prefix, ok, tt.prefix)
			}
			for _, typ := range imaging.Types() {
				want := false
				for _, allowed := range tt.types {
					if allowed == typ {
						want = true
					}
				}
				if got := tt.kind.Accepts(typ); got != want {
					t.Errorf("Accepts(%q) = %v, want %v", typ, got, want)
				}
			}
			// The order is asserted and not only the membership. It
			// became load bearing with ADR-0031 Karar 3: the handler
			// hands Pipeline.Start the kind's AllowedTypes()[0], so a
			// reordering -- alphabetizing the slice, say -- would change
			// which type a quarantine key is named after without any
			// other test noticing. For the CV the first element is the
			// PDF, which is both the decision and the common case.
			if got := tt.kind.AllowedTypes(); !slices.Equal(got, tt.types) {
				t.Errorf("AllowedTypes() = %v, want %v in this order", got, tt.types)
			}
			if got := tt.kind.Attachment(); got != tt.attachment {
				t.Errorf("Attachment() = %v, want %v", got, tt.attachment)
			}
			if got := tt.kind.PublicReadable(); got != tt.public {
				t.Errorf("PublicReadable() = %v, want %v", got, tt.public)
			}
			cache := tt.kind.CacheControl()
			if tt.public && !strings.Contains(cache, "max-age") {
				t.Errorf("CacheControl() = %q; a public object is immutable", cache)
			}
			if !tt.public && cache != "no-store" {
				t.Errorf("CacheControl() = %q; a private object is never cached", cache)
			}
			limits := tt.kind.Limits()
			if limits.MaxBytes != imaging.DefaultMaxBytes || limits.MaxPixels != imaging.DefaultMaxPixels {
				t.Errorf("Limits() = %+v, want the defaults of ADR-0029 Karar 5", limits)
			}
		})
	}
}

// TestUnknownKindReachesNothing is the fail-closed rule for a kind the
// contract grows and this package has not learned yet: it gets no prefix,
// accepts no type and is never publicly readable, so the pipeline refuses
// it instead of picking a default bucket folder.
func TestUnknownKindReachesNothing(t *testing.T) {
	t.Parallel()

	for _, kind := range []media.Kind{"", "COMPANY_COVER", "profile_picture", "../images"} {
		if prefix, ok := kind.Prefix(); ok {
			t.Errorf("Prefix() of %q = %q, want none", kind, prefix)
		}
		if got := kind.AllowedTypes(); got != nil {
			t.Errorf("AllowedTypes() of %q = %v, want none", kind, got)
		}
		for _, typ := range imaging.Types() {
			if kind.Accepts(typ) {
				t.Errorf("%q accepts %q", kind, typ)
			}
		}
		if kind.PublicReadable() {
			t.Errorf("%q is publicly readable", kind)
		}
		if got := kind.CacheControl(); got != "no-store" {
			t.Errorf("CacheControl() of %q = %q, want no-store", kind, got)
		}
	}
}

func TestExtOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ  imaging.Type
		want storage.Ext
		ok   bool
	}{
		{imaging.TypeJPEG, storage.ExtJPEG, true},
		{imaging.TypePNG, storage.ExtPNG, true},
		{imaging.TypePDF, storage.ExtPDF, true},
		{imaging.TypeDOC, storage.ExtDOC, true},
		{imaging.TypeDOCX, storage.ExtDOCX, true},
		{imaging.Type("image/svg+xml"), "", false},
		{imaging.Type("image/webp"), "", false},
		{imaging.Type(""), "", false},
	}
	for _, tt := range tests {
		got, ok := media.ExtOf(tt.typ)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ExtOf(%q) = %q, %v; want %q, %v", tt.typ, got, ok, tt.want, tt.ok)
		}
	}
	// Every type the allowlist accepts has a key extension, so no
	// accepted upload can fail at the naming step.
	for _, typ := range imaging.Types() {
		if _, ok := media.ExtOf(typ); !ok {
			t.Errorf("ExtOf(%q) has no extension; the two lists are out of step", typ)
		}
	}
}

// TestStatusTransitions pins the state machine: PENDING is the only
// state with a way out and both exits are terminal, so completing twice
// reads the row and does not rewrite it.
func TestStatusTransitions(t *testing.T) {
	t.Parallel()

	all := []media.Status{media.StatusPending, media.StatusReady, media.StatusRejected}
	allowed := map[media.Status]map[media.Status]bool{
		media.StatusPending: {media.StatusReady: true, media.StatusRejected: true},
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[from][to]
			if got := media.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
		if media.CanTransition(from, from) {
			t.Errorf("CanTransition(%q, %q) = true; a state must not transition to itself", from, from)
		}
	}
	if media.CanTransition("WHATEVER", media.StatusReady) {
		t.Error("an unknown status may transition")
	}
}

func TestRejectionIsAValueNotAnError(t *testing.T) {
	t.Parallel()

	if media.RejectionNone != "" {
		t.Errorf("RejectionNone = %q, want the zero value so a Completion is accepted by default being absent", media.RejectionNone)
	}
	seen := map[media.Rejection]bool{}
	for _, r := range []media.Rejection{
		media.RejectionNotUploaded,
		media.RejectionTypeUnsupported,
		media.RejectionTooLarge,
		media.RejectionCorrupt,
	} {
		if r == media.RejectionNone {
			t.Error("a rejection reason equals RejectionNone")
		}
		if seen[r] {
			t.Errorf("rejection %q is declared twice", r)
		}
		seen[r] = true
	}
}
