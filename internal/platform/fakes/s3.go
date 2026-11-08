package fakes

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// S3 is a fake S3 compatible object store: the HTTP API the real client
// in internal/platform/storage talks to, started with httptest and
// addressed path style.
//
// It is a fake and not a MinIO container on purpose (ADR-0030 Karar 6).
// The repository had no object store fake and no compose file, and the
// gate's integration job runs with one Postgres service container; a
// second service container would change the gate for every PR. The
// object store surface Koala uses is six operations, so an in-process
// fake covers it, runs in the unit test job and needs no Docker.
//
// What the fake does not do is verify a signature. It checks that a
// request arrived signed — either SigV4 in the Authorization header, the
// way the SDK calls, or the X-Amz-Signature query parameter, the way a
// client follows a presigned URL — and it records the parameters so a
// test can assert on the lifetime and the signed header list. Proving
// that the signature is correct would mean reimplementing SigV4, which
// would test the fake and not Koala.
//
// Operations: PUT (write and copy), GET, HEAD, DELETE on an object, with
// the GET response overrides S3 supports. Anything else is an unexpected
// request and fails the test, like every other fake here.
type S3 struct {
	*Server
	bucket string

	mu      sync.Mutex
	objects map[string]S3Object
	signed  []S3Signature
}

// S3Object is a stored object and the metadata it was stored with.
type S3Object struct {
	Body               []byte
	ContentType        string
	ContentDisposition string
	CacheControl       string
}

// S3Signature is how one request was signed.
type S3Signature struct {
	// Method and Key are the request's.
	Method, Key string
	// Presigned is true when the signature came from the query string,
	// which is what a client following an upload or download address
	// sends.
	Presigned bool
	// Expires is the X-Amz-Expires value of a presigned request, in
	// seconds; 0 for a header signed one.
	Expires int
	// SignedHeaders is the X-Amz-SignedHeaders list, lower case, in the
	// order the signature covers them.
	SignedHeaders []string
}

// NewS3 starts a fake object store serving bucket; it stops at cleanup.
func NewS3(tb testing.TB, bucket string) *S3 {
	tb.Helper()
	f := &S3{bucket: bucket, objects: map[string]S3Object{}}
	f.Server = newServer(tb, "s3", f.check, f.fallback)
	return f
}

// Endpoint is the base endpoint to configure in place of the store's.
func (f *S3) Endpoint() string { return f.URL() }

// Seed stores an object without going through the API, so a test can set
// up the state an upload would have left behind.
func (f *S3) Seed(key string, obj S3Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = obj
}

// Object returns the stored object at key.
func (f *S3) Object(key string) (S3Object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[key]
	return obj, ok
}

// Keys returns every stored key, sorted.
func (f *S3) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.objects))
}

// Signatures returns how each request so far was signed, in arrival
// order.
func (f *S3) Signatures() []S3Signature {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.signed)
}

// check refuses an unsigned request and records how a signed one was
// signed. It declines to answer, so the scenario and the default
// behavior still apply.
func (f *S3) check(req Request) (Response, bool) {
	key, ok := f.keyOf(req.Path)
	if !ok {
		return s3Error(http.StatusNotFound, "NoSuchBucket", "the fake serves one bucket"), true
	}
	sig := S3Signature{Method: req.Method, Key: key}
	switch {
	case req.Query.Get("X-Amz-Signature") != "":
		sig.Presigned = true
		sig.Expires, _ = strconv.Atoi(req.Query.Get("X-Amz-Expires"))
		sig.SignedHeaders = splitSignedHeaders(req.Query.Get("X-Amz-SignedHeaders"))
	case strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 "):
		sig.SignedHeaders = splitSignedHeaders(signedHeadersOf(req.Header.Get("Authorization")))
	default:
		return s3Error(http.StatusForbidden, "AccessDenied", "the request carried no signature"), true
	}
	f.mu.Lock()
	f.signed = append(f.signed, sig)
	f.mu.Unlock()
	return Response{}, false
}

// fallback is the store's behavior.
func (f *S3) fallback(req Request) (Response, bool) {
	key, ok := f.keyOf(req.Path)
	if !ok {
		return Response{}, false
	}
	switch req.Method {
	case http.MethodPut:
		if source := req.Header.Get("X-Amz-Copy-Source"); source != "" {
			return f.copyObject(source, key, req), true
		}
		return f.putObject(key, req), true
	case http.MethodGet:
		return f.getObject(key, req, true), true
	case http.MethodHead:
		return f.getObject(key, req, false), true
	case http.MethodDelete:
		return f.deleteObject(key), true
	default:
		return Response{}, false
	}
}

func (f *S3) putObject(key string, req Request) Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = S3Object{
		Body:               slices.Clone(req.Body),
		ContentType:        req.Header.Get("Content-Type"),
		ContentDisposition: req.Header.Get("Content-Disposition"),
		CacheControl:       req.Header.Get("Cache-Control"),
	}
	return Response{
		Status: http.StatusOK,
		Header: http.Header{"ETag": []string{`"` + etagOf(req.Body) + `"`}},
	}
}

// copyObject implements the PUT with X-Amz-Copy-Source that CopyObject
// sends. Only the REPLACE directive is modeled, which is the only one
// Koala uses.
func (f *S3) copyObject(source, key string, req Request) Response {
	from, ok := f.keyOf("/" + strings.TrimPrefix(source, "/"))
	if !ok {
		return s3Error(http.StatusNotFound, "NoSuchBucket", "the copy source names another bucket")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, found := f.objects[from]
	if !found {
		return s3Error(http.StatusNotFound, "NoSuchKey", "the copy source does not exist")
	}
	if directive := req.Header.Get("X-Amz-Metadata-Directive"); directive != "REPLACE" {
		return s3Error(http.StatusBadRequest, "InvalidRequest",
			fmt.Sprintf("the fake models the REPLACE directive only, got %q", directive))
	}
	obj.ContentType = req.Header.Get("Content-Type")
	obj.ContentDisposition = req.Header.Get("Content-Disposition")
	obj.CacheControl = req.Header.Get("Cache-Control")
	f.objects[key] = obj
	return Response{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/xml"}},
		Body: []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<CopyObjectResult><ETag>&quot;` + etagOf(obj.Body) + `&quot;</ETag></CopyObjectResult>`),
	}
}

// getObject answers GET and HEAD. withBody is false for HEAD, which
// carries the same headers and no body.
func (f *S3) getObject(key string, req Request, withBody bool) Response {
	f.mu.Lock()
	obj, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		if !withBody {
			// A HEAD has no body to put an error document in, which is
			// why the real client falls back to the status code.
			return Response{Status: http.StatusNotFound}
		}
		return s3Error(http.StatusNotFound, "NoSuchKey", "the key does not exist")
	}
	header := http.Header{
		"Content-Length": []string{strconv.Itoa(len(obj.Body))},
		"ETag":           []string{`"` + etagOf(obj.Body) + `"`},
	}
	// The response overrides of a presigned GET win over what the object
	// was stored with, as they do in S3.
	setIfAny(header, "Content-Type", req.Query.Get("response-content-type"), obj.ContentType)
	setIfAny(header, "Content-Disposition", req.Query.Get("response-content-disposition"), obj.ContentDisposition)
	setIfAny(header, "Cache-Control", req.Query.Get("response-cache-control"), obj.CacheControl)

	resp := Response{Status: http.StatusOK, Header: header}
	if withBody {
		resp.Body = slices.Clone(obj.Body)
	}
	return resp
}

func (f *S3) deleteObject(key string) Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; !ok {
		// S3 answers 204 for a key that is not there: a retried delete
		// must not fail.
		return Response{Status: http.StatusNoContent}
	}
	delete(f.objects, key)
	return Response{Status: http.StatusNoContent}
}

// keyOf splits a path style request path into the bucket and the key,
// and reports false when the bucket is not the fake's.
func (f *S3) keyOf(path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	bucket, key, found := strings.Cut(trimmed, "/")
	if !found || bucket != f.bucket || key == "" {
		return "", false
	}
	// net/http has already percent-decoded the path, and the fake does
	// not decode it a second time. A second round would make the fake
	// accept keys the real store would refuse, and the difference
	// between the fake's decoding and the real client's is exactly what
	// this file exists not to introduce.
	return key, true
}

// setIfAny writes the first non-empty value to the header.
func setIfAny(header http.Header, name string, values ...string) {
	for _, v := range values {
		if v != "" {
			header.Set(name, v)
			return
		}
	}
}

// splitSignedHeaders turns "host;x-amz-date" into a list.
func splitSignedHeaders(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ";")
}

// signedHeadersOf pulls the SignedHeaders list out of an Authorization
// header: "AWS4-HMAC-SHA256 Credential=…, SignedHeaders=a;b, Signature=…".
func signedHeadersOf(authorization string) string {
	for _, part := range strings.Split(authorization, ",") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(part), "SignedHeaders="); ok {
			return after
		}
	}
	return ""
}

// etagOf is a stable stand-in for S3's entity tag. It is not MD5: the
// client only compares it with itself, and an MD5 here would invite a
// reader to think the fake models S3's checksums.
func etagOf(body []byte) string {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	hash := offset64
	for _, b := range body {
		hash ^= uint64(b)
		hash *= prime64
	}
	return strconv.FormatUint(hash, 16)
}

// s3Error builds the XML error document S3 answers with, which is what
// the SDK's deserializer reads the code out of.
func s3Error(status int, code, message string) Response {
	return Response{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/xml"}},
		Body: []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<Error><Code>` + code + `</Code><Message>` + message + `</Message></Error>`),
	}
}
