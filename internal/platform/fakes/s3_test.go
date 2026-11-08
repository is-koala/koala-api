package fakes_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/fakes"
)

// signedHeader is a SigV4 Authorization header of the shape the SDK
// sends. The fake does not verify the signature, only that one is there
// and which headers it claims to cover.
const signedHeader = "AWS4-HMAC-SHA256 Credential=k/20261108/eu-central-1/s3/aws4_request, " +
	"SignedHeaders=content-type;host;x-amz-date, Signature=deadbeef"

func s3Header(values map[string]string) http.Header {
	header := http.Header{"Authorization": []string{signedHeader}}
	for k, v := range values {
		header.Set(k, v)
	}
	return header
}

func TestS3StoresAndServesAnObject(t *testing.T) {
	t.Parallel()

	f := fakes.NewS3(t, "koala-test")
	const target = "/koala-test/images/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.png"

	status, _ := call(t, f.Client(), http.MethodPut, f.URL()+target,
		s3Header(map[string]string{"Content-Type": "image/png", "Cache-Control": "public"}), "pixels")
	if status != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", status)
	}
	if got := f.Keys(); len(got) != 1 || got[0] != "images/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.png" {
		t.Fatalf("Keys() = %v", got)
	}
	obj, ok := f.Object("images/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.png")
	if !ok || string(obj.Body) != "pixels" || obj.ContentType != "image/png" || obj.CacheControl != "public" {
		t.Fatalf("Object() = %+v, %v", obj, ok)
	}

	status, body := call(t, f.Client(), http.MethodGet, f.URL()+target, s3Header(nil), "")
	if status != http.StatusOK || string(body) != "pixels" {
		t.Errorf("GET = %d, %q", status, body)
	}

	status, _ = call(t, f.Client(), http.MethodDelete, f.URL()+target, s3Header(nil), "")
	if status != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", status)
	}
	// A repeated delete still succeeds, as S3 does.
	status, _ = call(t, f.Client(), http.MethodDelete, f.URL()+target, s3Header(nil), "")
	if status != http.StatusNoContent {
		t.Errorf("repeated DELETE status = %d, want 204", status)
	}
}

func TestS3AnswersAMissingKey(t *testing.T) {
	t.Parallel()

	f := fakes.NewS3(t, "koala-test")
	const target = "/koala-test/incoming/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a31.jpg"

	status, body := call(t, f.Client(), http.MethodGet, f.URL()+target, s3Header(nil), "")
	if status != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", status)
	}
	if !strings.Contains(string(body), "<Code>NoSuchKey</Code>") {
		t.Errorf("GET body = %q, want an S3 error document", body)
	}
	// A HEAD has no body to carry the error code; the status is all the
	// client gets, which is why storage.classify falls back to it.
	status, body = call(t, f.Client(), http.MethodHead, f.URL()+target, s3Header(nil), "")
	if status != http.StatusNotFound || len(body) != 0 {
		t.Errorf("HEAD = %d, %q", status, body)
	}
}

func TestS3RefusesAnUnsignedRequest(t *testing.T) {
	t.Parallel()

	f := fakes.NewS3(t, "koala-test")
	const target = "/koala-test/images/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.png"

	status, body := call(t, f.Client(), http.MethodGet, f.URL()+target, nil, "")
	if status != http.StatusForbidden {
		t.Errorf("unsigned GET status = %d, want 403", status)
	}
	if !strings.Contains(string(body), "AccessDenied") {
		t.Errorf("unsigned GET body = %q", body)
	}
	if got := f.Signatures(); len(got) != 0 {
		t.Errorf("Signatures() = %+v, want none recorded for an unsigned request", got)
	}
}

func TestS3RecordsHowARequestWasSigned(t *testing.T) {
	t.Parallel()

	f := fakes.NewS3(t, "koala-test")
	const key = "incoming/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.jpg"
	presigned := f.URL() + "/koala-test/" + key +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=deadbeef"

	if status, _ := call(t, f.Client(), http.MethodPut, presigned, nil, "bytes"); status != http.StatusOK {
		t.Fatalf("presigned PUT status = %d, want 200", status)
	}
	if status, _ := call(t, f.Client(), http.MethodGet, f.URL()+"/koala-test/"+key, s3Header(nil), ""); status != http.StatusOK {
		t.Fatalf("header signed GET did not succeed")
	}

	sigs := f.Signatures()
	if len(sigs) != 2 {
		t.Fatalf("Signatures() = %+v, want two", sigs)
	}
	if !sigs[0].Presigned || sigs[0].Expires != 900 || sigs[0].Key != key {
		t.Errorf("the presigned request was recorded as %+v", sigs[0])
	}
	if sigs[1].Presigned || sigs[1].Expires != 0 {
		t.Errorf("the header signed request was recorded as %+v", sigs[1])
	}
	if want := []string{"content-type", "host", "x-amz-date"}; strings.Join(sigs[1].SignedHeaders, ";") != strings.Join(want, ";") {
		t.Errorf("SignedHeaders = %v, want %v", sigs[1].SignedHeaders, want)
	}
}

func TestS3AppliesTheGetResponseOverrides(t *testing.T) {
	t.Parallel()

	f := fakes.NewS3(t, "koala-test")
	const key = "identity-documents/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.jpg"
	f.Seed(key, fakes.S3Object{Body: []byte("document"), ContentType: "image/jpeg", CacheControl: "public"})

	target := f.URL() + "/koala-test/" + key +
		"?X-Amz-Signature=deadbeef&X-Amz-Expires=180" +
		"&response-content-disposition=attachment&response-cache-control=no-store"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Client().Do(req)
	if err != nil {
		t.Fatalf("GET = %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if got := resp.Header.Get("Content-Disposition"); got != "attachment" {
		t.Errorf("Content-Disposition = %q, want attachment", got)
	}
	// The override wins over the stored value, as it does in S3.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestS3CopyNeedsTheReplaceDirective(t *testing.T) {
	t.Parallel()

	f := fakes.NewS3(t, "koala-test")
	const (
		from = "incoming/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.pdf"
		to   = "files/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.pdf"
	)
	f.Seed(from, fakes.S3Object{Body: []byte("%PDF-1.7"), ContentType: "application/pdf"})

	target := f.URL() + "/koala-test/" + to
	status, _ := call(t, f.Client(), http.MethodPut, target, s3Header(map[string]string{
		"X-Amz-Copy-Source":        "koala-test/" + from,
		"X-Amz-Metadata-Directive": "REPLACE",
		"Content-Type":             "application/pdf",
		"Content-Disposition":      "attachment",
	}), "")
	if status != http.StatusOK {
		t.Fatalf("copy status = %d, want 200", status)
	}
	copied, ok := f.Object(to)
	if !ok || string(copied.Body) != "%PDF-1.7" || copied.ContentDisposition != "attachment" {
		t.Fatalf("Object(%q) = %+v, %v", to, copied, ok)
	}

	// COPY keeps the source: it is Move that deletes it.
	if _, ok := f.Object(from); !ok {
		t.Error("the copy removed the source")
	}

	status, _ = call(t, f.Client(), http.MethodPut, f.URL()+"/koala-test/"+to, s3Header(map[string]string{
		"X-Amz-Copy-Source": "koala-test/" + from,
	}), "")
	if status != http.StatusBadRequest {
		t.Errorf("copy without the REPLACE directive status = %d, want 400", status)
	}

	status, _ = call(t, f.Client(), http.MethodPut, f.URL()+"/koala-test/"+to, s3Header(map[string]string{
		"X-Amz-Copy-Source":        "koala-test/incoming/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a99.pdf",
		"X-Amz-Metadata-Directive": "REPLACE",
	}), "")
	if status != http.StatusNotFound {
		t.Errorf("copy of a missing source status = %d, want 404", status)
	}
}

func TestS3ServesOneBucket(t *testing.T) {
	t.Parallel()

	f := fakes.NewS3(t, "koala-test")
	for _, target := range []string{
		"/other-bucket/images/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.png",
		"/koala-test/",
		"/koala-test",
		"/",
	} {
		status, _ := call(t, f.Client(), http.MethodGet, f.URL()+target, s3Header(nil), "")
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", target, status)
		}
	}
}
