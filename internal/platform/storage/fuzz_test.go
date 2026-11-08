package storage_test

import (
	"path"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/storage"
)

// seeds are the input classes the Faz 2 exit criterion names for the path
// traversal test (ADR-0029 Karar 6, ADR-0030 Karar 2), plus the well
// formed keys that keep the target from only ever exercising the reject
// path. They are F.Add seeds and not a testdata corpus, so `go test`
// runs every one of them in the gate's verify job without -fuzz, and
// `go test -fuzz=FuzzStorageKey` starts from them.
func seeds() []string {
	const id = "0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30"
	return []string{
		// Well formed, one per prefix and per extension.
		"images/" + id + ".jpg",
		"public/" + id + ".png",
		"files/" + id + ".pdf",
		"files/dispute-evidence/" + id + ".png",
		"identity-documents/" + id + ".jpg",
		"incoming/" + id + ".jpg",
		// A UUIDv4 key, as Java wrote them: readable, not writable.
		"images/47f7b083-32c6-4e09-a851-1f90f1e66c34.png",
		// "..", "." and empty segments.
		"images/../" + id + ".jpg",
		"images/./" + id + ".jpg",
		"../images/" + id + ".jpg",
		"images//" + id + ".jpg",
		"images/..",
		"..",
		"../../etc/passwd",
		"/images/" + id + ".jpg",
		"images/" + id + ".jpg/",
		// Percent encoded "." and "/" (%2e, %2f) in both cases.
		"images/%2e%2e/" + id + ".jpg",
		"images/%2E%2E%2F" + id + ".jpg",
		"%2fimages/" + id + ".jpg",
		"images%2f" + id + ".jpg",
		"identity-documents%2F" + id + ".jpg",
		// Double and triple encoding.
		"images/%252e%252e/" + id + ".jpg",
		"images/%25252e%25252e/" + id + ".jpg",
		"images/%252f" + id + ".jpg",
		// Truncated and invalid escapes.
		"images/%2" + id + ".jpg",
		"images/%zz" + id + ".jpg",
		"images/%" + id + ".jpg",
		// Backslashes, Windows style and encoded.
		`images\` + id + ".jpg",
		`images\..\` + id + ".jpg",
		"images/%5c.." + id + ".jpg",
		`..\..\windows\system32`,
		// NUL, control characters and whitespace.
		"images/" + id + ".jpg\x00",
		"images/" + id + ".jpg\x00.txt",
		"images/\x00" + id + ".jpg",
		"images/%00" + id + ".jpg",
		"images/" + id + ".jpg\n",
		" images/" + id + ".jpg",
		"images/" + id + ".jpg ",
		"images/\t" + id + ".jpg",
		// Unicode normalization: a composed and a decomposed form, a
		// full width solidus and a Turkish dotless i. The allowlist
		// alphabet is ASCII, so each is refused by construction; the
		// target proves the claim instead of only asserting it.
		"ïmages/" + id + ".jpg",
		"i̇mages/" + id + ".jpg",
		"images／" + id + ".jpg",
		"ımages/" + id + ".jpg",
		"İMAGES/" + id + ".jpg",
		"images/" + id + ".jpg",
		// Over long names.
		"images/" + strings.Repeat("x", 300) + ".jpg",
		strings.Repeat("images/", 100) + id + ".jpg",
		"images/" + id + "." + strings.Repeat("jpg", 200),
		// Case: the allowlist is lower case hex and lower case prefixes.
		"IMAGES/" + id + ".jpg",
		"images/0192F3C4-7B1A-7CC0-9A51-3F2B8E4D6A30.jpg",
		"images/" + id + ".JPG",
		// Shapes that are close to valid but are not.
		"images/" + id,
		"images/" + id + ".svg",
		"images/" + id + ".jpg.svg",
		"images/" + id + ".php",
		"images/not-a-uuid.jpg",
		"images/" + id + "x.jpg",
		"files/dispute-evidence/sub/" + id + ".pdf",
		"unknown/" + id + ".jpg",
		"images",
		"images/",
		"",
		// Query and fragment smuggling, and a full URL.
		"images/" + id + ".jpg?x=1",
		"images/" + id + ".jpg#f",
		"https://objects.example.com/images/" + id + ".jpg",
		"//objects.example.com/images/" + id + ".jpg",
	}
}

// FuzzStorageKey is the path traversal fuzz target of the Faz 2 exit
// criterion (ADR-0029 Karar 6). It feeds the decode-and-validate chain
// arbitrary input and checks the two invariants the ADR states:
//
//  1. every accepted key matches the allowlist pattern, which it proves
//     by round tripping: parsing the key a Key prints must give the same
//     Key back, so the printed form is in the accepted language;
//  2. no accepted key reaches outside its own prefix.
//
// It also checks what a traversal attempt would need in order to work:
// an accepted key is its own cleaned path, has exactly the number of
// segments its prefix declares, and holds no percent sign, backslash,
// NUL or non-ASCII byte.
func FuzzStorageKey(f *testing.F) {
	for _, seed := range seeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		key, err := storage.ParseKey(raw)
		if err != nil {
			// A rejected input must leave nothing usable behind.
			if !key.Zero() {
				t.Fatalf("ParseKey(%q) failed but returned %q", raw, key.String())
			}
			return
		}

		got := key.String()

		// Invariant 1: the printed key is in the accepted language and
		// parses back to the same key.
		again, err := storage.ParseKey(got)
		if err != nil {
			t.Fatalf("ParseKey(%q) accepted %q, which does not parse: %v", raw, got, err)
		}
		if again != key {
			t.Fatalf("ParseKey(%q) is not idempotent: %q then %q", raw, got, again.String())
		}

		// Invariant 2: the key stays under its prefix.
		if key.EscapesPrefix() {
			t.Fatalf("ParseKey(%q) accepted %q, which escapes prefix %q", raw, got, key.Prefix())
		}
		prefix := string(key.Prefix())
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("ParseKey(%q) accepted %q, which is not under %q", raw, got, prefix)
		}
		if path.Clean("/"+got) != "/"+got {
			t.Fatalf("ParseKey(%q) accepted %q, which is not a cleaned path", raw, got)
		}
		if strings.Count(got, "/") != strings.Count(prefix, "/") {
			t.Fatalf("ParseKey(%q) accepted %q, which adds a path segment to %q", raw, got, prefix)
		}

		// Nothing that could survive another decoding round, be read as
		// a Windows path, truncate a C string or normalize into a
		// different key may be in an accepted key.
		for _, bad := range []string{"%", `\`, "\x00", "..", "./", "?", "#", ":", " "} {
			if strings.Contains(got, bad) {
				t.Fatalf("ParseKey(%q) accepted %q, which holds %q", raw, got, bad)
			}
		}
		for i := range len(got) {
			if got[i] < 0x21 || got[i] > 0x7e {
				t.Fatalf("ParseKey(%q) accepted %q, which holds byte %#x", raw, got, got[i])
			}
		}
		if len(got) > storage.MaxKeyLen {
			t.Fatalf("ParseKey(%q) accepted %q, longer than MaxKeyLen", raw, got)
		}
	})
}
