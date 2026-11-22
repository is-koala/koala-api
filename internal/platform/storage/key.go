// Package storage is the object store client and the object key scheme
// (ADR-0019 §9, ADR-0029 Karar 6, ADR-0030). Uploads do not pass through
// the API: the client writes to a presigned URL, the server validates and
// transforms the object afterwards.
//
// A key is never built from client input. The server picks the prefix
// from the media kind, the identifier is a UUIDv7 it generates and the
// extension comes from the type it detected in the bytes, so the whole
// alphabet of a key is closed. The read path takes the opposite route:
// ParseKey percent-decodes once and matches an allowlist, and nothing
// else in the repository turns a string into a Key.
package storage

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Prefix is an object key prefix: the folder an object class lives in
// (ADR-0029 Karar 6). The set is closed; a prefix is not configurable,
// because the read allowlist and the public read rules are written
// against exactly these names.
type Prefix string

// The prefixes. Every one ends in "/" so that concatenation needs no
// separator and no prefix is a path segment prefix of another by accident
// ("files/" vs "files/dispute-evidence/" nest on purpose).
const (
	// PrefixImages holds publicly readable images (profile pictures,
	// company logos and gallery images).
	PrefixImages Prefix = "images/"
	// PrefixPublic holds publicly readable non-image files.
	PrefixPublic Prefix = "public/"
	// PrefixFiles holds private files reached through a signed URL (a CV).
	PrefixFiles Prefix = "files/"
	// PrefixDisputeEvidence holds dispute evidence. It nests under
	// PrefixFiles and is private like it, with its own access rules.
	PrefixDisputeEvidence Prefix = "files/dispute-evidence/"
	// PrefixIdentityDocument holds identity document photos. They resolve
	// to no public URL at all (ADR-0029 Karar 6, IDN-007).
	PrefixIdentityDocument Prefix = "identity-documents/"
	// PrefixIncoming is the upload quarantine. An object lands here, is
	// validated and stripped, and is then moved to its final prefix
	// (ADR-0019 §9, ADR-0029 Karar 6). An unfinished upload is swept
	// after 24 hours.
	PrefixIncoming Prefix = "incoming/"
)

// Prefixes lists every prefix, with a nested prefix ahead of the one it
// nests under, so that the prefix of a nested key is reported as
// "files/dispute-evidence/" and not "files/". The order is not by
// length; only that one pair has to be ordered, and a new nested prefix
// has to be placed the same way.
func Prefixes() []Prefix {
	return []Prefix{
		PrefixDisputeEvidence,
		PrefixIdentityDocument,
		PrefixImages,
		PrefixIncoming,
		PrefixPublic,
		PrefixFiles,
	}
}

// Ext is the extension of a key. It is derived from the content type the
// server detected in the bytes, never from the name the client sent, so
// the alphabet is these five values and nothing else (ADR-0029 Karar 6:
// a deliberate narrowing of Java, which took the extension from the file
// name and allowed up to ten alphanumeric characters; the two Word
// extensions ADR-0031 Karar 2).
type Ext string

// The extensions.
const (
	ExtJPEG Ext = "jpg"
	ExtPNG  Ext = "png"
	ExtPDF  Ext = "pdf"
	ExtDOC  Ext = "doc"
	ExtDOCX Ext = "docx"
)

// Exts lists every extension a key may carry.
func Exts() []Ext { return []Ext{ExtJPEG, ExtPNG, ExtPDF, ExtDOC, ExtDOCX} }

// keyPattern is the read allowlist of ADR-0029 Karar 6. It is an
// allowlist and not a denylist on purpose: it closes "..", ".", "%2e",
// "%2f", "%5c", double encoding, an empty segment, a trailing slash, a
// leading slash and an absolute path by construction, so no list of
// traversal spellings has to be kept complete. The hex is lower case, so
// a key differing only in case is a different key and is refused.
var keyPattern = regexp.MustCompile(
	`^(?:images|public|files/dispute-evidence|files|identity-documents|incoming)/` +
		`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}` +
		`\.(?:jpg|png|pdf|docx|doc)$`)

// MaxKeyLen bounds the raw input ParseKey accepts before it decodes
// anything. A well formed key is 46 to 64 bytes -- the shortest prefix
// with a three letter extension, the longest with "docx"; the bound is
// generous enough for a percent-encoded one and small enough that no
// pathological input reaches the decoder.
const MaxKeyLen = 256

// Errors of the key scheme.
var (
	// ErrKeyInvalid is returned for a string that is not a well formed
	// key. The caller maps it to 404 and never echoes the input
	// (ADR-0029 Karar 6).
	ErrKeyInvalid = errors.New("storage: invalid object key")
	// ErrKeyNotV7 is returned by NewKey for an identifier that is not a
	// UUIDv7. Keys the server writes are time ordered (plan §3.6-4).
	ErrKeyNotV7 = errors.New("storage: object key identifier is not a UUIDv7")
)

// Key is a validated object key. The zero Key is invalid; every Key in
// circulation came from NewKey or ParseKey, so code that holds one needs
// no further checks.
type Key struct {
	prefix Prefix
	id     uuid.UUID
	ext    Ext
}

// NewKey returns the key of a new object: <prefix><uuidv7>.<ext>. The
// identifier must be a UUIDv7 so that keys sort by creation time; the
// read path is laxer, because Java wrote UUIDv4 keys that still have to
// be readable.
func NewKey(prefix Prefix, id uuid.UUID, ext Ext) (Key, error) {
	if !validPrefix(prefix) {
		return Key{}, fmt.Errorf("%w: unknown prefix", ErrKeyInvalid)
	}
	if !validExt(ext) {
		return Key{}, fmt.Errorf("%w: unknown extension", ErrKeyInvalid)
	}
	if id.Version() != 7 {
		return Key{}, ErrKeyNotV7
	}
	return Key{prefix: prefix, id: id, ext: ext}, nil
}

// NewID returns the identifier of a new key: a UUIDv7, so that keys and
// the rows that name them sort by creation time (plan §3.6-4).
func NewID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("storage: new key id: %w", err)
	}
	return id, nil
}

// ParseKey validates raw and returns the key it names.
//
// The steps are exactly those of ADR-0029 Karar 6 and the order matters:
//  1. bound the length, before any decoding;
//  2. percent-decode once;
//  3. refuse a result that still holds a percent sign, which is the
//     signature of double encoding;
//  4. match the allowlist.
//
// Any failure is ErrKeyInvalid; the caller answers 404 and tells the two
// cases apart nowhere.
func ParseKey(raw string) (Key, error) {
	if raw == "" || len(raw) > MaxKeyLen {
		return Key{}, fmt.Errorf("%w: length", ErrKeyInvalid)
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return Key{}, fmt.Errorf("%w: percent encoding", ErrKeyInvalid)
	}
	if strings.Contains(decoded, "%") {
		return Key{}, fmt.Errorf("%w: double encoding", ErrKeyInvalid)
	}
	if !keyPattern.MatchString(decoded) {
		return Key{}, fmt.Errorf("%w: shape", ErrKeyInvalid)
	}
	prefix, ok := prefixOf(decoded)
	if !ok {
		// Unreachable while keyPattern and Prefixes() list the same
		// prefixes; TestEveryPrefixAndExtIsParsable proves they do.
		return Key{}, fmt.Errorf("%w: prefix", ErrKeyInvalid)
	}
	rest := decoded[len(prefix):]
	dot := strings.LastIndexByte(rest, '.')
	id, err := uuid.Parse(rest[:dot])
	if err != nil {
		return Key{}, fmt.Errorf("%w: identifier", ErrKeyInvalid)
	}
	// The extension is checked against the list as well, the way the
	// prefix is. keyPattern already allows only these five, so this is
	// the same redundancy as the prefix check above and exists for the
	// same reason: the pattern and the two lists are three descriptions
	// of one language, and a Key must not be constructible from a
	// disagreement between them.
	ext := Ext(rest[dot+1:])
	if !validExt(ext) {
		return Key{}, fmt.Errorf("%w: extension", ErrKeyInvalid)
	}
	return Key{prefix: prefix, id: id, ext: ext}, nil
}

// String returns the key as the object store sees it.
func (k Key) String() string {
	if k.prefix == "" {
		return ""
	}
	return string(k.prefix) + k.id.String() + "." + string(k.ext)
}

// Prefix returns the prefix the key lives under.
func (k Key) Prefix() Prefix { return k.prefix }

// ID returns the key's identifier.
func (k Key) ID() uuid.UUID { return k.id }

// Ext returns the key's extension.
func (k Key) Ext() Ext { return k.ext }

// Zero reports whether the key is the zero value, which names no object.
func (k Key) Zero() bool { return k.prefix == "" }

// WithPrefix returns the same object identity under another prefix. It is
// how the quarantine move names its destination: the identifier and the
// extension are carried over, so the final key of an upload is
// predictable and the move cannot land outside the prefix set.
func (k Key) WithPrefix(prefix Prefix) (Key, error) {
	if k.Zero() {
		return Key{}, fmt.Errorf("%w: zero key", ErrKeyInvalid)
	}
	if !validPrefix(prefix) {
		return Key{}, fmt.Errorf("%w: unknown prefix", ErrKeyInvalid)
	}
	return Key{prefix: prefix, id: k.id, ext: k.ext}, nil
}

// WithExt returns the same object identity with another extension. The
// quarantine object is named from the type the client declared; the final
// key is named from the type the server detected, and the two may differ.
func (k Key) WithExt(ext Ext) (Key, error) {
	if k.Zero() {
		return Key{}, fmt.Errorf("%w: zero key", ErrKeyInvalid)
	}
	if !validExt(ext) {
		return Key{}, fmt.Errorf("%w: unknown extension", ErrKeyInvalid)
	}
	return Key{prefix: k.prefix, id: k.id, ext: ext}, nil
}

// EscapesPrefix reports whether the key resolves outside its own prefix.
// Nothing that ParseKey or NewKey returns can, which is the second
// invariant FuzzStorageKey checks; the function exists so that the
// invariant is stated in code and not only in the test.
func (k Key) EscapesPrefix() bool {
	s := k.String()
	if s == "" {
		return true
	}
	if path.Clean("/"+s) != "/"+s {
		return true
	}
	return !strings.HasPrefix(s, string(k.prefix))
}

func validPrefix(p Prefix) bool { return slices.Contains(Prefixes(), p) }

func validExt(e Ext) bool { return slices.Contains(Exts(), e) }

// prefixOf returns the prefix of decoded: the first match in the order
// Prefixes() declares, which is why a nested prefix is listed ahead of
// the one it nests under. It is not a longest-match search, and reading
// it as one would make the order look incidental.
func prefixOf(decoded string) (Prefix, bool) {
	for _, p := range Prefixes() {
		if strings.HasPrefix(decoded, string(p)) {
			return p, true
		}
	}
	return "", false
}
