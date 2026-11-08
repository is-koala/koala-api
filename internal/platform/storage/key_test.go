package storage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/storage"
)

// v7 is a fixed UUIDv7 and v4 a fixed UUIDv4, so the tests name keys
// without generating identifiers.
const (
	v7 = "0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30"
	v4 = "47f7b083-32c6-4e09-a851-1f90f1e66c34"
)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) = %v", s, err)
	}
	return id
}

func TestNewKeyBuildsThePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix storage.Prefix
		ext    storage.Ext
		want   string
	}{
		{"images jpeg", storage.PrefixImages, storage.ExtJPEG, "images/" + v7 + ".jpg"},
		{"public png", storage.PrefixPublic, storage.ExtPNG, "public/" + v7 + ".png"},
		{"files pdf", storage.PrefixFiles, storage.ExtPDF, "files/" + v7 + ".pdf"},
		{"dispute evidence", storage.PrefixDisputeEvidence, storage.ExtPNG, "files/dispute-evidence/" + v7 + ".png"},
		{"identity document", storage.PrefixIdentityDocument, storage.ExtJPEG, "identity-documents/" + v7 + ".jpg"},
		{"quarantine", storage.PrefixIncoming, storage.ExtJPEG, "incoming/" + v7 + ".jpg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key, err := storage.NewKey(tt.prefix, mustUUID(t, v7), tt.ext)
			if err != nil {
				t.Fatalf("NewKey() = %v", err)
			}
			if got := key.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if got := key.Prefix(); got != tt.prefix {
				t.Errorf("Prefix() = %q, want %q", got, tt.prefix)
			}
			if got := key.Ext(); got != tt.ext {
				t.Errorf("Ext() = %q, want %q", got, tt.ext)
			}
			parsed, err := storage.ParseKey(tt.want)
			if err != nil {
				t.Fatalf("ParseKey(%q) = %v", tt.want, err)
			}
			if parsed != key {
				t.Errorf("ParseKey(%q) = %q, want the key NewKey built", tt.want, parsed.String())
			}
		})
	}
}

// TestNewKeyRefusesWhatItMustNotWrite covers the write side: the server
// picks the prefix and the extension from a closed set, and the
// identifier is a UUIDv7 so that keys sort by creation time (ADR-0029
// Karar 6).
func TestNewKeyRefusesWhatItMustNotWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prefix  storage.Prefix
		id      string
		ext     storage.Ext
		wantErr error
	}{
		{"unknown prefix", storage.Prefix("avatars/"), v7, storage.ExtJPEG, storage.ErrKeyInvalid},
		{"prefix without slash", storage.Prefix("images"), v7, storage.ExtJPEG, storage.ErrKeyInvalid},
		{"empty prefix", storage.Prefix(""), v7, storage.ExtJPEG, storage.ErrKeyInvalid},
		{"traversal as prefix", storage.Prefix("../"), v7, storage.ExtJPEG, storage.ErrKeyInvalid},
		{"unknown extension", storage.PrefixImages, v7, storage.Ext("svg"), storage.ErrKeyInvalid},
		{"extension with a dot", storage.PrefixImages, v7, storage.Ext(".jpg"), storage.ErrKeyInvalid},
		{"empty extension", storage.PrefixImages, v7, storage.Ext(""), storage.ErrKeyInvalid},
		{"uuid v4", storage.PrefixImages, v4, storage.ExtJPEG, storage.ErrKeyNotV7},
		{"nil uuid", storage.PrefixImages, uuid.Nil.String(), storage.ExtJPEG, storage.ErrKeyNotV7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key, err := storage.NewKey(tt.prefix, mustUUID(t, tt.id), tt.ext)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewKey() error = %v, want %v", err, tt.wantErr)
			}
			if !key.Zero() {
				t.Errorf("NewKey() returned %q on error", key.String())
			}
		})
	}
}

// TestParseKeyReadsLegacyV4 pins the asymmetry: the read path accepts the
// UUIDv4 keys Java wrote, the write path does not produce them.
func TestParseKeyReadsLegacyV4(t *testing.T) {
	t.Parallel()

	raw := "images/" + v4 + ".png"
	key, err := storage.ParseKey(raw)
	if err != nil {
		t.Fatalf("ParseKey(%q) = %v", raw, err)
	}
	if key.ID().Version() != 4 {
		t.Errorf("ID().Version() = %d, want 4", key.ID().Version())
	}
	if _, err := storage.NewKey(storage.PrefixImages, key.ID(), storage.ExtPNG); !errors.Is(err, storage.ErrKeyNotV7) {
		t.Errorf("NewKey() with a v4 id = %v, want ErrKeyNotV7", err)
	}
}

// TestParseKeyRejects is the table of the traversal and injection classes
// the exit criterion names. FuzzStorageKey covers the same classes
// generatively; this table names them, so a regression says which class
// broke.
func TestParseKeyRejects(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, raw string }{
		{"empty", ""},
		{"prefix only", "images/"},
		{"dot dot segment", "images/../" + v7 + ".jpg"},
		{"dot segment", "images/./" + v7 + ".jpg"},
		{"leading dot dot", "../images/" + v7 + ".jpg"},
		{"empty segment", "images//" + v7 + ".jpg"},
		{"absolute", "/images/" + v7 + ".jpg"},
		{"trailing slash", "images/" + v7 + ".jpg/"},
		{"encoded dot dot", "images/%2e%2e/" + v7 + ".jpg"},
		{"encoded dot dot upper", "images/%2E%2E%2F" + v7 + ".jpg"},
		{"double encoded dot dot", "images/%252e%252e/" + v7 + ".jpg"},
		{"double encoded slash", "images/%252f" + v7 + ".jpg"},
		{"truncated escape", "images/%2" + v7 + ".jpg"},
		{"invalid escape", "images/%zz" + v7 + ".jpg"},
		{"bare percent", "images/%" + v7 + ".jpg"},
		{"backslash", `images\` + v7 + ".jpg"},
		{"encoded backslash", "images/%5c" + v7 + ".jpg"},
		{"nul byte", "images/" + v7 + ".jpg\x00"},
		{"nul byte before extension", "images/" + v7 + "\x00.jpg"},
		{"encoded nul", "images/%00" + v7 + ".jpg"},
		{"newline", "images/" + v7 + ".jpg\n"},
		{"leading space", " images/" + v7 + ".jpg"},
		{"trailing space", "images/" + v7 + ".jpg "},
		{"non ascii prefix", "ïmages/" + v7 + ".jpg"},
		{"combining mark", "i̇mages/" + v7 + ".jpg"},
		{"fullwidth solidus", "images／" + v7 + ".jpg"},
		{"dotless i", "ımages/" + v7 + ".jpg"},
		{"upper case prefix", "IMAGES/" + v7 + ".jpg"},
		{"upper case identifier", "images/" + strings.ToUpper(v7) + ".jpg"},
		{"upper case extension", "images/" + v7 + ".JPG"},
		{"unknown prefix", "avatars/" + v7 + ".jpg"},
		{"extra segment", "files/dispute-evidence/sub/" + v7 + ".pdf"},
		{"no extension", "images/" + v7},
		{"unsupported extension", "images/" + v7 + ".svg"},
		{"double extension", "images/" + v7 + ".jpg.svg"},
		{"not a uuid", "images/not-a-uuid.jpg"},
		{"long identifier", "images/" + v7 + "x.jpg"},
		{"query string", "images/" + v7 + ".jpg?x=1"},
		{"fragment", "images/" + v7 + ".jpg#f"},
		{"absolute url", "https://objects.example.com/images/" + v7 + ".jpg"},
		{"protocol relative", "//objects.example.com/images/" + v7 + ".jpg"},
		{"over the length bound", "images/" + strings.Repeat("x", storage.MaxKeyLen) + ".jpg"},
		{"repeated prefix", strings.Repeat("images/", 100) + v7 + ".jpg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key, err := storage.ParseKey(tt.raw)
			if !errors.Is(err, storage.ErrKeyInvalid) {
				t.Fatalf("ParseKey(%q) error = %v, want ErrKeyInvalid", tt.raw, err)
			}
			if !key.Zero() {
				t.Errorf("ParseKey(%q) returned %q on error", tt.raw, key.String())
			}
		})
	}
}

// TestParseKeyDecodesExactlyOnce pins the rule of ADR-0029 Karar 6: one
// percent-decoding round, and a result that still holds a percent sign is
// refused.
//
// One consequence is worth stating, because it reads like a hole and is
// not: the singly encoded form of a well formed key is accepted,
// including one whose separator was encoded ("images%2f<uuid>.jpg"). That
// is safe for the reason the allowlist exists -- after the one decoding
// round the result has to match the pattern exactly, so it resolves to
// "images/<uuid>.jpg", inside its own prefix, which is what
// FuzzStorageKey checks for every accepted key. The rule closes the
// second round instead: "%252f" decodes to "%2f", still holds a percent
// sign, and is refused before any pattern match, so no caller is handed a
// string that a later decoder would turn into a different key.
func TestParseKeyDecodesExactlyOnce(t *testing.T) {
	t.Parallel()

	want := "images/" + v7 + ".jpg"
	// "%69" is "i" and "%2f" is "/": one decoding round turns each of
	// these into the key above.
	for _, once := range []string{
		"%69mages/" + v7 + ".jpg",
		"images%2f" + v7 + ".jpg",
		"images%2F" + v7 + ".jpg",
		"%69mages%2f" + v7 + ".jpg",
	} {
		key, err := storage.ParseKey(once)
		if err != nil {
			t.Fatalf("ParseKey(%q) = %v", once, err)
		}
		if got := key.String(); got != want {
			t.Errorf("ParseKey(%q) = %q, want %q", once, got, want)
		}
		if key.EscapesPrefix() {
			t.Errorf("ParseKey(%q) = %q, which escapes its prefix", once, key.String())
		}
	}

	// A second encoding round is where the line is: the result still
	// holds a percent sign, so it never reaches the pattern.
	for _, twice := range []string{
		"%2569mages/" + v7 + ".jpg",
		"images%252f" + v7 + ".jpg",
		"images/%252e%252e/" + v7 + ".jpg",
	} {
		if _, err := storage.ParseKey(twice); !errors.Is(err, storage.ErrKeyInvalid) {
			t.Errorf("ParseKey(%q) error = %v, want ErrKeyInvalid", twice, err)
		}
	}
}

func TestWithPrefixAndWithExtKeepIdentity(t *testing.T) {
	t.Parallel()

	quarantined, err := storage.NewKey(storage.PrefixIncoming, mustUUID(t, v7), storage.ExtJPEG)
	if err != nil {
		t.Fatalf("NewKey() = %v", err)
	}

	final, err := quarantined.WithPrefix(storage.PrefixIdentityDocument)
	if err != nil {
		t.Fatalf("WithPrefix() = %v", err)
	}
	if want := "identity-documents/" + v7 + ".jpg"; final.String() != want {
		t.Errorf("WithPrefix() = %q, want %q", final.String(), want)
	}
	if final.ID() != quarantined.ID() {
		t.Error("WithPrefix() changed the identifier")
	}

	retyped, err := final.WithExt(storage.ExtPNG)
	if err != nil {
		t.Fatalf("WithExt() = %v", err)
	}
	if want := "identity-documents/" + v7 + ".png"; retyped.String() != want {
		t.Errorf("WithExt() = %q, want %q", retyped.String(), want)
	}

	if _, err := quarantined.WithPrefix(storage.Prefix("../")); !errors.Is(err, storage.ErrKeyInvalid) {
		t.Errorf("WithPrefix(\"../\") error = %v, want ErrKeyInvalid", err)
	}
	if _, err := quarantined.WithExt(storage.Ext("svg")); !errors.Is(err, storage.ErrKeyInvalid) {
		t.Errorf("WithExt(\"svg\") error = %v, want ErrKeyInvalid", err)
	}
	var zero storage.Key
	if _, err := zero.WithPrefix(storage.PrefixImages); !errors.Is(err, storage.ErrKeyInvalid) {
		t.Errorf("zero.WithPrefix() error = %v, want ErrKeyInvalid", err)
	}
	if _, err := zero.WithExt(storage.ExtJPEG); !errors.Is(err, storage.ErrKeyInvalid) {
		t.Errorf("zero.WithExt() error = %v, want ErrKeyInvalid", err)
	}
	if !zero.EscapesPrefix() {
		t.Error("the zero key must count as escaping: it names no object")
	}
	if zero.String() != "" {
		t.Errorf("zero.String() = %q, want empty", zero.String())
	}
}

// TestEveryPrefixAndExtIsParsable keeps the three lists that describe the
// key language in step: the prefix list, the extension list and the
// allowlist pattern inside ParseKey. A prefix added to one and not the
// others is red here, which is the failure mode of a key scheme spread
// over three places.
func TestEveryPrefixAndExtIsParsable(t *testing.T) {
	t.Parallel()

	for _, prefix := range storage.Prefixes() {
		for _, ext := range storage.Exts() {
			raw := string(prefix) + v7 + "." + string(ext)
			key, err := storage.ParseKey(raw)
			if err != nil {
				t.Errorf("ParseKey(%q) = %v; the pattern does not know prefix %q or extension %q", raw, err, prefix, ext)
				continue
			}
			if key.Prefix() != prefix {
				t.Errorf("ParseKey(%q).Prefix() = %q, want %q", raw, key.Prefix(), prefix)
			}
			if key.Ext() != ext {
				t.Errorf("ParseKey(%q).Ext() = %q, want %q", raw, key.Ext(), ext)
			}
			if _, err := storage.NewKey(prefix, mustUUID(t, v7), ext); err != nil {
				t.Errorf("NewKey(%q, %q) = %v", prefix, ext, err)
			}
		}
	}
}

func TestNewIDIsAV7(t *testing.T) {
	t.Parallel()

	first, err := storage.NewID()
	if err != nil {
		t.Fatalf("NewID() = %v", err)
	}
	if first.Version() != 7 {
		t.Fatalf("NewID().Version() = %d, want 7", first.Version())
	}
	second, err := storage.NewID()
	if err != nil {
		t.Fatalf("NewID() = %v", err)
	}
	if first == second {
		t.Error("NewID() returned the same identifier twice")
	}
	if _, err := storage.NewKey(storage.PrefixIncoming, first, storage.ExtJPEG); err != nil {
		t.Errorf("NewKey() with NewID() = %v", err)
	}
}
