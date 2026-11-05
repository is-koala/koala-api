package httpx

import (
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"
)

// CodePathInvalid answers a request whose path is not in canonical form
// (ADR-0021).
const CodePathInvalid = "PATH_INVALID"

// CanonicalPath refuses, with 400 PATH_INVALID, a request whose path is not
// canonical. It runs before routing, so neither the router nor a handler
// that builds a storage key from a path parameter ever sees:
//
//   - a path that does not start with "/" (the "*" of OPTIONS *),
//   - an empty segment ("//"); a trailing slash is left to routing (404),
//   - a "." or ".." segment, also percent-encoded (%2e, .%2E, %2e%2e),
//   - an encoded slash or backslash (%2f, %5c), which would turn one
//     segment into two after decoding, or an encoded percent (%25), which
//     would decode to an encoding a later layer might decode again,
//   - a backslash, a control character or invalid UTF-8 after decoding.
//
// Such paths are refused rather than rewritten: no Koala client produces
// them, and a rewrite would change which route and which authorization
// rule applies after the client chose the path. Spring's StrictHttpFirewall
// refuses the same set; Java's Koala relied on it for /uploads/** (N01 §8.2).
func CanonicalPath(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !canonicalPath(r.URL.Path, r.URL.RawPath) {
				WriteProblem(w, logger, r, NewProblem(r, http.StatusBadRequest, CodePathInvalid))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// canonicalPath reports whether the decoded path and its raw form (empty
// when the raw form is the default encoding of path) are canonical.
func canonicalPath(path, raw string) bool {
	if !strings.HasPrefix(path, "/") || !utf8.ValidString(path) {
		return false
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%25") {
		return false
	}
	for i := range len(path) {
		if c := path[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return false
		}
	}
	segments := strings.Split(path[1:], "/")
	for i, s := range segments {
		switch s {
		case ".", "..":
			return false
		case "":
			// "/" itself and a trailing slash have one empty last segment.
			if i != len(segments)-1 {
				return false
			}
		}
	}
	return true
}
