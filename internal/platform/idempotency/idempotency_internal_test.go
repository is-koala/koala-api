package idempotency

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func secret(b byte) []byte { return bytes.Repeat([]byte{b, b + 1, b + 2, b + 3}, 8) }

// decode reads a body the way kin-openapi does (numbers as json.Number).
func decode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Two spellings of one value hash alike; two values do not.
func TestCanonicalJSON(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"member order", `{"a":1,"b":[true,null]}`, `{"b":[true,null],"a":1}`, true},
		{"space", `{ "a" : 1 }`, `{"a":1}`, true},
		{"integer spellings", `{"n":1}`, `{"n":1.0}`, true},
		{"exponent", `{"n":100}`, `{"n":1e2}`, true},
		{"negative zero", `{"n":0}`, `{"n":-0}`, true},
		{"escapes", `{"s":"\u003c\u0026\u003e"}`, `{"s":"<&>"}`, true},
		{"another number", `{"n":1}`, `{"n":2}`, false},
		{"another string", `{"s":"a"}`, `{"s":"b"}`, false},
		{"array order matters", `[1,2]`, `[2,1]`, false},
		{"string is not number", `{"n":"1"}`, `{"n":1}`, false},
		{"null is not absent", `{"a":null}`, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := canonicalJSON(decode(t, tc.a))
			if err != nil {
				t.Fatal(err)
			}
			b, err := canonicalJSON(decode(t, tc.b))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(a, b) != tc.equal {
				t.Fatalf("canonical(%s) = %s, canonical(%s) = %s; equal want %v", tc.a, a, tc.b, b, tc.equal)
			}
		})
	}

	got, err := canonicalJSON(decode(t, `{"b":"<ç>","a":[1.5,1e21,{"z":false,"y":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":[1.5,1e+21,{"y":null,"z":false}],"b":"<ç>"}`; string(got) != want {
		t.Fatalf("canonical form = %s, want %s", got, want)
	}
	// The differences from RFC 8785 the ADR names (ADR-0034 Karar 2):
	// U+2028 is escaped; an integer past 2^53 is written by its digits, so
	// it and the same number with a fraction are two requests.
	got, err = canonicalJSON(decode(t, `["\u2028",9007199254740993,9007199254740993.0]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `["\u2028",9007199254740993,9007199254740992]`; string(got) != want {
		t.Fatalf("canonical form = %s, want %s", got, want)
	}
	if _, err := canonicalJSON(map[string]any{"x": struct{}{}}); err == nil {
		t.Fatal("a value that is not decoded JSON has a canonical form")
	}
}

// The response opens only with the request it was sealed for, on its own
// row and under the same server secret.
func TestSealOpensOnlyWithTheSameRequest(t *testing.T) {
	t.Parallel()

	k, err := deriveKeys(secret(1))
	if err != nil {
		t.Fatal(err)
	}
	req, row := []byte(`{"body":{"token":"t"}}`), rowID{"registration:x", "completeRegistration", "key-00000001"}.aad()
	plain := []byte(`{"status":201,"body":"c2VjcmV0"}`)
	sealed, err := k.seal(req, row, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) || bytes.Contains(sealed, []byte("c2VjcmV0")) {
		t.Fatal("the sealed response carries its plaintext")
	}
	if got, err := k.open(req, row, sealed); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open with the same request = %q, %v", got, err)
	}
	other, err := deriveKeys(secret(9))
	if err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func() ([]byte, error){
		"another request": func() ([]byte, error) { return k.open([]byte(`{"body":{"token":"u"}}`), row, sealed) },
		"another row": func() ([]byte, error) {
			return k.open(req, rowID{"registration:y", "completeRegistration", "key-00000001"}.aad(), sealed)
		},
		"another secret": func() ([]byte, error) { return other.open(req, row, sealed) },
		"truncated":      func() ([]byte, error) { return k.open(req, row, sealed[:10]) },
	} {
		if _, err := open(); !errors.Is(err, errUnsealable) {
			t.Errorf("%s: open = %v, want errUnsealable", name, err)
		}
	}
	if _, err := deriveKeys(make([]byte, 16)); err == nil {
		t.Error("a 16-byte secret is accepted")
	}
}

// The request hash is keyed: the same request under another secret hashes
// differently, so a leaked table confirms no guessed body.
func TestRequestHashIsKeyed(t *testing.T) {
	t.Parallel()

	a, _ := deriveKeys(secret(1))
	b, _ := deriveKeys(secret(9))
	req := []byte(`{"body":{"n":1}}`)
	if !bytes.Equal(a.hash(req), a.hash(req)) {
		t.Fatal("the hash is not stable")
	}
	if bytes.Equal(a.hash(req), b.hash(req)) || bytes.Equal(a.hash(req), a.hash([]byte(`{"body":{"n":2}}`))) {
		t.Fatal("the hash ignores the secret or the request")
	}
}

// Which statuses keep the key (ADR-0034 Karar 5).
func TestStatusesThatKeepTheKey(t *testing.T) {
	t.Parallel()

	for status, keep := range map[int]bool{
		http.StatusOK: true, http.StatusCreated: true, http.StatusNoContent: true,
		http.StatusBadRequest: true, http.StatusNotFound: true, http.StatusConflict: true, http.StatusUnprocessableEntity: true,
		http.StatusRequestTimeout: false, http.StatusTooEarly: false, http.StatusTooManyRequests: false,
		http.StatusInternalServerError: false, http.StatusBadGateway: false, http.StatusServiceUnavailable: false,
		http.StatusFound: false,
	} {
		if got := success(status) || permanent(status); got != keep {
			t.Errorf("status %d keeps the key: %v, want %v", status, got, keep)
		}
	}
}

// The recorder Commit renders into keeps the representation headers only.
func TestRecorderKeepsRepresentationHeaders(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.Header().Set("Location", "/v1/me")
	rec.Header().Set("Retry-After", "3")
	rec.Header().Set("X-Request-Id", "req-1")
	rec.WriteHeader(http.StatusCreated)
	_, _ = rec.Write([]byte(`{}`))
	got := rec.stored()
	if got.Status != http.StatusCreated || string(got.Body) != `{}` || len(got.Header) != 2 ||
		got.Header["Location"][0] != "/v1/me" || got.Header["Content-Type"][0] != "application/json" {
		t.Fatalf("stored = %+v", got)
	}
}

// The recorder keeps no more than maxStoredBody: past it Commit keeps
// nothing (ADR-0034 Karar 5).
func TestRecorderStopsAtTheStoredBodyBound(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	_, _ = rec.Write(bytes.Repeat([]byte("a"), maxStoredBody))
	if rec.overflow {
		t.Fatal("a body of exactly the bound overflowed")
	}
	_, _ = rec.Write([]byte("b"))
	if !rec.overflow || rec.body.Len() != 0 {
		t.Fatalf("overflow %v with %d bytes kept, want an overflow and nothing kept", rec.overflow, rec.body.Len())
	}
}
