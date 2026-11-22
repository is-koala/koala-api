package idempotency

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// canonicalRequest is the form of a request that its hash and its
// response key are taken from (stil rehberi §4, ADR-0034 Karar 2):
// everything a handler reads from it. That is the validated body (kin-
// openapi has written the schema defaults into it, so a request that
// leaves out a member with a default is the same request as one that
// sends the default), the path and query parameters and the client
// headers a session binds to. The headers the edge checks (version,
// request id) and the User-Agent are not part of it: they do not change
// what the operation does.
func canonicalRequest(ctx context.Context, r *http.Request, rctx *chi.Context, body any) ([]byte, error) {
	path := map[string]any{}
	if rctx != nil {
		for i, k := range rctx.URLParams.Keys {
			if k != "*" && i < len(rctx.URLParams.Values) {
				path[k] = rctx.URLParams.Values[i]
			}
		}
	}
	query := map[string]any{}
	for k, vs := range r.URL.Query() {
		values := make([]any, len(vs))
		for i, v := range vs {
			values[i] = v
		}
		query[k] = values
	}
	return canonicalJSON(map[string]any{
		"body":  body,
		"path":  path,
		"query": query,
		"client": map[string]any{
			"deviceId": httpx.DeviceIDFrom(ctx),
			"platform": string(httpx.ClientPlatformFrom(ctx)),
		},
	})
}

// validatedValue is the body httpx.ValidateRequests left on r, nil when
// the operation takes none.
func validatedValue(r *http.Request) any {
	if b, ok := r.Body.(interface{ ValidatedValue() any }); ok {
		return b.ValidatedValue()
	}
	return nil
}

// canonicalJSON writes a decoded JSON value in one form whatever the
// spelling it came in: object members sorted by key (UTF-8 byte order),
// no insignificant space, strings escaped by encoding/json without the
// HTML escapes, and numbers by value: an integer that fits in an int64
// as its decimal digits, any other number in ECMAScript's shortest form
// (encoding/json's float format). So 1, 1.0 and 1e0 are the same member.
// This is RFC 8785 (JCS) but for the key order, which RFC 8785 takes in
// UTF-16 code units; the two orders differ only between keys outside the
// Basic Multilingual Plane and keys above U+E000, and the hash only needs
// one form per value.
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		return writeString(b, x)
	case json.Number:
		return writeNumber(b, x)
	case float64:
		return writeFloat(b, x)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case map[string]any:
		names := make([]string, 0, len(x))
		for k := range x {
			names = append(names, k)
		}
		slices.Sort(names)
		b.WriteByte('{')
		for i, k := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		// A decoded JSON value holds none of these; failing keeps a new
		// kind of value from hashing as something else.
		return fmt.Errorf("idempotency: no canonical form for %T", v)
	}
	return nil
}

func writeString(b *bytes.Buffer, s string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("idempotency: encode a string: %w", err)
	}
	b.Write(bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}))
	return nil
}

func writeNumber(b *bytes.Buffer, n json.Number) error {
	if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		b.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return fmt.Errorf("idempotency: number %q: %w", n, err)
	}
	return writeFloat(b, f)
}

// maxExactInt is 2^53: below it every integer is exact in a float64.
const maxExactInt = 1 << 53

func writeFloat(b *bytes.Buffer, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("idempotency: number %v has no JSON form", f)
	}
	if f == math.Trunc(f) && math.Abs(f) < maxExactInt {
		b.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	out, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("idempotency: encode a number: %w", err)
	}
	b.Write(out)
	return nil
}
