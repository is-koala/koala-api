package storage_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/fakes"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// fixedClock is the clock the tests wire, so the expiry a presign
// reports is a value and not a moving target.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

const testBucket = "koala-test"

var testNow = fixedClock{at: time.Date(2026, time.November, 8, 10, 0, 0, 0, time.UTC)}

// newStore starts a fake object store and a real client pointed at it.
func newStore(t *testing.T) (*storage.Store, *fakes.S3) {
	t.Helper()
	fake := fakes.NewS3(t, testBucket)
	store, err := storage.New(storage.Config{
		Endpoint:        fake.Endpoint(),
		Region:          "eu-central-1",
		Bucket:          testBucket,
		AccessKeyID:     "koala-test-access-key",
		SecretAccessKey: "koala-test-secret-key",
		PathStyle:       true,
		PublicBaseURL:   "https://cdn.example.com",
	}, testNow, fake.Client())
	if err != nil {
		t.Fatalf("storage.New() = %v", err)
	}
	return store, fake
}

func key(t *testing.T, prefix storage.Prefix, ext storage.Ext) storage.Key {
	t.Helper()
	id, err := storage.NewID()
	if err != nil {
		t.Fatalf("NewID() = %v", err)
	}
	k, err := storage.NewKey(prefix, id, ext)
	if err != nil {
		t.Fatalf("NewKey() = %v", err)
	}
	return k
}

func TestPutHeadGetDelete(t *testing.T) {
	t.Parallel()

	store, fake := newStore(t)
	ctx := context.Background()
	k := key(t, storage.PrefixImages, storage.ExtPNG)
	body := []byte("not really a png, but the store does not look")

	if err := store.Put(ctx, k, body, storage.PutOptions{ContentType: "image/png", CacheControl: "public, max-age=31536000"}); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if got := fake.Keys(); len(got) != 1 || got[0] != k.String() {
		t.Fatalf("the fake holds %v, want only %q", got, k.String())
	}
	stored, ok := fake.Object(k.String())
	if !ok {
		t.Fatal("the object is not in the fake")
	}
	if stored.ContentType != "image/png" {
		t.Errorf("stored Content-Type = %q, want the type the server detected", stored.ContentType)
	}
	if stored.CacheControl != "public, max-age=31536000" {
		t.Errorf("stored Cache-Control = %q", stored.CacheControl)
	}

	info, err := store.Head(ctx, k)
	if err != nil {
		t.Fatalf("Head() = %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Head().Size = %d, want %d", info.Size, len(body))
	}
	if info.ContentType != "image/png" {
		t.Errorf("Head().ContentType = %q", info.ContentType)
	}
	if info.ETag == "" || strings.Contains(info.ETag, `"`) {
		t.Errorf("Head().ETag = %q, want it unquoted and non-empty", info.ETag)
	}

	got, err := store.Get(ctx, k, 1<<20)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("Get() = %q, want %q", got, body)
	}

	if err := store.Delete(ctx, k); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if got := fake.Keys(); len(got) != 0 {
		t.Errorf("the fake still holds %v", got)
	}
	// A repeated delete is not an error: a retried cleanup must not fail.
	if err := store.Delete(ctx, k); err != nil {
		t.Errorf("Delete() of a missing object = %v, want nil", err)
	}
}

func TestMissingObject(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	k := key(t, storage.PrefixIncoming, storage.ExtJPEG)

	if _, err := store.Head(ctx, k); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Head() of a missing object = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, k, 1<<20); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get() of a missing object = %v, want ErrNotFound", err)
	}
	// The error names the prefix and not the key: a 404 on a read path
	// must not confirm which key was asked for (ADR-0029 Karar 6).
	_, err := store.Head(ctx, k)
	if err == nil || strings.Contains(err.Error(), k.ID().String()) {
		t.Errorf("Head() error = %v; it must not echo the key", err)
	}
}

// TestGetIsBounded covers the decompression and memory bound: Get never
// buffers more than the caller allowed, and it does not trust the length
// the store reports.
func TestGetIsBounded(t *testing.T) {
	t.Parallel()

	store, fake := newStore(t)
	ctx := context.Background()
	k := key(t, storage.PrefixFiles, storage.ExtPDF)
	fake.Seed(k.String(), fakes.S3Object{Body: bytes.Repeat([]byte("x"), 1000), ContentType: "application/pdf"})

	if _, err := store.Get(ctx, k, 999); !errors.Is(err, storage.ErrTooLarge) {
		t.Errorf("Get() with a bound of 999 = %v, want ErrTooLarge", err)
	}
	got, err := store.Get(ctx, k, 1000)
	if err != nil {
		t.Fatalf("Get() with a bound of exactly the size = %v", err)
	}
	if len(got) != 1000 {
		t.Errorf("len(Get()) = %d, want 1000", len(got))
	}
	if _, err := store.Get(ctx, k, 0); !errors.Is(err, storage.ErrConfig) {
		t.Errorf("Get() with a bound of 0 = %v, want ErrConfig", err)
	}
}

func TestMoveLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	store, fake := newStore(t)
	ctx := context.Background()
	from := key(t, storage.PrefixIncoming, storage.ExtJPEG)
	to, err := from.WithPrefix(storage.PrefixIdentityDocument)
	if err != nil {
		t.Fatalf("WithPrefix() = %v", err)
	}
	body := []byte("stripped bytes")
	fake.Seed(from.String(), fakes.S3Object{Body: body, ContentType: "image/jpeg"})

	if err := store.Move(ctx, from, to, storage.PutOptions{ContentType: "image/jpeg", Attachment: true}); err != nil {
		t.Fatalf("Move() = %v", err)
	}
	if got := fake.Keys(); len(got) != 1 || got[0] != to.String() {
		t.Fatalf("the fake holds %v, want only %q", got, to.String())
	}
	moved, ok := fake.Object(to.String())
	if !ok {
		t.Fatal("the destination is not in the fake")
	}
	if !bytes.Equal(moved.Body, body) {
		t.Errorf("the moved body changed: %q", moved.Body)
	}
	if moved.ContentDisposition != "attachment" {
		t.Errorf("Content-Disposition = %q, want attachment: an identity document is never rendered inline", moved.ContentDisposition)
	}

	// Moving an object that is not there fails and creates nothing.
	if err := store.Move(ctx, from, to, storage.PutOptions{ContentType: "image/jpeg"}); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Move() of a missing source = %v, want ErrNotFound", err)
	}
}

// TestPresignPutAddressesOneKey is the scope claim of ADR-0030 Karar 1:
// the write address names one key under the quarantine prefix, carries a
// lifetime, and a client can actually write to it.
func TestPresignPutAddressesOneKey(t *testing.T) {
	t.Parallel()

	store, fake := newStore(t)
	ctx := context.Background()
	k := key(t, storage.PrefixIncoming, storage.ExtJPEG)
	const ttl = 15 * time.Minute

	req, err := store.PresignPut(ctx, k, ttl)
	if err != nil {
		t.Fatalf("PresignPut() = %v", err)
	}
	if req.Method != http.MethodPut {
		t.Errorf("Method = %q, want PUT", req.Method)
	}
	if want := testNow.at.Add(ttl); !req.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", req.ExpiresAt, want)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) = %v", req.URL, err)
	}
	if want := "/" + testBucket + "/" + k.String(); u.Path != want {
		t.Errorf("path = %q, want %q", u.Path, want)
	}
	if got := u.Query().Get("X-Amz-Expires"); got != "900" {
		t.Errorf("X-Amz-Expires = %q, want 900", got)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Error("the address carries no signature")
	}

	// A client follows the address and writes.
	body := []byte("uploaded by the client, straight to the store")
	putReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext() = %v", err)
	}
	putReq.Header.Set("Content-Type", "image/jpeg")
	resp, err := fake.Client().Do(putReq)
	if err != nil {
		t.Fatalf("PUT to the presigned address = %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	stored, ok := fake.Object(k.String())
	if !ok {
		t.Fatal("the client's write did not land under the quarantine key")
	}
	if !bytes.Equal(stored.Body, body) {
		t.Errorf("stored body = %q, want %q", stored.Body, body)
	}

	// The signature the client sent is the presigned kind and carries
	// the lifetime. It covers the host and nothing that would bind the
	// type or the size, which is why the complete step has to check the
	// object itself (ADR-0030 Karar 1).
	sigs := fake.Signatures()
	if len(sigs) == 0 {
		t.Fatal("the fake recorded no signature")
	}
	last := sigs[len(sigs)-1]
	if !last.Presigned {
		t.Error("the client's request was not presigned")
	}
	if last.Expires != int(ttl.Seconds()) {
		t.Errorf("X-Amz-Expires = %d, want %d", last.Expires, int(ttl.Seconds()))
	}
	for _, header := range last.SignedHeaders {
		if header == "content-type" || header == "content-length" {
			t.Errorf("the presigned PUT signs %q; ADR-0030 Karar 1 says it cannot, so the decision has changed", header)
		}
	}
}

func TestPresignGetCarriesTheResponseRules(t *testing.T) {
	t.Parallel()

	store, fake := newStore(t)
	ctx := context.Background()
	k := key(t, storage.PrefixIdentityDocument, storage.ExtJPEG)
	fake.Seed(k.String(), fakes.S3Object{Body: []byte("document"), ContentType: "image/jpeg"})

	// ADR-0029 Karar 6: a staff download of an identity document is a
	// three minute address, no-store, an attachment.
	const ttl = 3 * time.Minute
	req, err := store.PresignGet(ctx, k, ttl, storage.Download{
		ContentType: "image/jpeg",
		Attachment:  true,
		NoStore:     true,
	})
	if err != nil {
		t.Fatalf("PresignGet() = %v", err)
	}
	if req.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", req.Method)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatalf("url.Parse() = %v", err)
	}
	for param, want := range map[string]string{
		"response-content-type":        "image/jpeg",
		"response-content-disposition": "attachment",
		"response-cache-control":       "no-store",
		"X-Amz-Expires":                "180",
	} {
		if got := u.Query().Get(param); got != want {
			t.Errorf("%s = %q, want %q", param, got, want)
		}
	}

	getReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequestWithContext() = %v", err)
	}
	resp, err := fake.Client().Do(getReq)
	if err != nil {
		t.Fatalf("GET from the presigned address = %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body = %v", err)
	}
	if string(body) != "document" {
		t.Errorf("body = %q", body)
	}
	for header, want := range map[string]string{
		"Content-Disposition": "attachment",
		"Cache-Control":       "no-store",
		"Content-Type":        "image/jpeg",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestPresignRefusesAnUnboundedLifetime(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	k := key(t, storage.PrefixImages, storage.ExtJPEG)

	for _, ttl := range []time.Duration{0, -time.Second, storage.MaxSignedTTL + time.Second, 7 * 24 * time.Hour} {
		if _, err := store.PresignPut(ctx, k, ttl); !errors.Is(err, storage.ErrConfig) {
			t.Errorf("PresignPut() with ttl %s = %v, want ErrConfig", ttl, err)
		}
		if _, err := store.PresignGet(ctx, k, ttl, storage.Download{}); !errors.Is(err, storage.ErrConfig) {
			t.Errorf("PresignGet() with ttl %s = %v, want ErrConfig", ttl, err)
		}
	}
}

// TestZeroKeyReachesNothing is the last line of defense: a Key that came
// from nowhere names no object, so no operation can be talked into
// addressing the bucket root.
func TestZeroKeyReachesNothing(t *testing.T) {
	t.Parallel()

	store, fake := newStore(t)
	ctx := context.Background()
	var zero storage.Key
	good := key(t, storage.PrefixImages, storage.ExtJPEG)

	checks := map[string]error{
		"put":         store.Put(ctx, zero, []byte("x"), storage.PutOptions{ContentType: "image/jpeg"}),
		"delete":      store.Delete(ctx, zero),
		"copy source": store.Copy(ctx, zero, good, storage.PutOptions{ContentType: "image/jpeg"}),
		"copy target": store.Copy(ctx, good, zero, storage.PutOptions{ContentType: "image/jpeg"}),
	}
	for name, err := range checks {
		if !errors.Is(err, storage.ErrKeyInvalid) {
			t.Errorf("%s with the zero key = %v, want ErrKeyInvalid", name, err)
		}
	}
	if _, err := store.Head(ctx, zero); !errors.Is(err, storage.ErrKeyInvalid) {
		t.Errorf("head with the zero key = %v, want ErrKeyInvalid", err)
	}
	if _, err := store.Get(ctx, zero, 10); !errors.Is(err, storage.ErrKeyInvalid) {
		t.Errorf("get with the zero key = %v, want ErrKeyInvalid", err)
	}
	if _, err := store.PresignPut(ctx, zero, time.Minute); !errors.Is(err, storage.ErrKeyInvalid) {
		t.Errorf("presign put with the zero key = %v, want ErrKeyInvalid", err)
	}
	if got := fake.Keys(); len(got) != 0 {
		t.Errorf("the fake holds %v after only zero key calls", got)
	}
}

// TestWriteNeedsADetectedType pins that an object is never written
// without the type the server detected (ADR-0029 Karar 5).
func TestWriteNeedsADetectedType(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	k := key(t, storage.PrefixImages, storage.ExtJPEG)

	if err := store.Put(ctx, k, []byte("x"), storage.PutOptions{}); !errors.Is(err, storage.ErrConfig) {
		t.Errorf("Put() without a content type = %v, want ErrConfig", err)
	}
	if err := store.Copy(ctx, k, k, storage.PutOptions{}); !errors.Is(err, storage.ErrConfig) {
		t.Errorf("Copy() without a content type = %v, want ErrConfig", err)
	}
}

func TestPublicURL(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)

	public := key(t, storage.PrefixImages, storage.ExtJPEG)
	got, ok := store.PublicURL(public)
	if !ok {
		t.Fatal("an images/ key has no public URL")
	}
	if want := "https://cdn.example.com/" + public.String(); got != want {
		t.Errorf("PublicURL() = %q, want %q", got, want)
	}

	// IDN-007: an identity document resolves to no URL at all, and
	// neither does anything else that is not publicly readable.
	for _, prefix := range []storage.Prefix{
		storage.PrefixIdentityDocument,
		storage.PrefixFiles,
		storage.PrefixDisputeEvidence,
		storage.PrefixIncoming,
	} {
		if address, ok := store.PublicURL(key(t, prefix, storage.ExtJPEG)); ok {
			t.Errorf("PublicURL() of a %s key = %q, want none", prefix, address)
		}
	}
	var zero storage.Key
	if _, ok := store.PublicURL(zero); ok {
		t.Error("the zero key has a public URL")
	}
}

func TestNewFailsClosed(t *testing.T) {
	t.Parallel()

	valid := storage.Config{
		Region:          "eu-central-1",
		Bucket:          testBucket,
		AccessKeyID:     "koala-test-access-key",
		SecretAccessKey: "koala-test-secret-key",
	}
	tests := []struct {
		name   string
		change func(c *storage.Config)
	}{
		{"no bucket", func(c *storage.Config) { c.Bucket = "" }},
		{"no region", func(c *storage.Config) { c.Region = "" }},
		{"no access key", func(c *storage.Config) { c.AccessKeyID = "" }},
		{"no secret", func(c *storage.Config) { c.SecretAccessKey = "" }},
		{"endpoint is not a URL", func(c *storage.Config) { c.Endpoint = "objects.example.com" }},
		{"endpoint with credentials", func(c *storage.Config) { c.Endpoint = "https://user:pw@objects.example.com" }},
		{"endpoint with a query", func(c *storage.Config) { c.Endpoint = "https://objects.example.com/?x=1" }},
		{"public base URL is not a URL", func(c *storage.Config) { c.PublicBaseURL = "cdn.example.com" }},
		{"public base URL with a fragment", func(c *storage.Config) { c.PublicBaseURL = "https://cdn.example.com/#f" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			tt.change(&cfg)
			if err := cfg.Validate(); !errors.Is(err, storage.ErrConfig) {
				t.Errorf("Validate() = %v, want ErrConfig", err)
			}
			store, err := storage.New(cfg, testNow, nil)
			if !errors.Is(err, storage.ErrConfig) {
				t.Errorf("New() = %v, want ErrConfig", err)
			}
			if store != nil {
				t.Error("New() returned a store on error")
			}
		})
	}

	if err := valid.Validate(); err != nil {
		t.Errorf("Validate() of the valid configuration = %v", err)
	}
	if _, err := storage.New(valid, nil, nil); !errors.Is(err, storage.ErrConfig) {
		t.Errorf("New() without a clock = %v, want ErrConfig", err)
	}
}

// TestSecretsNeverFormat keeps the credentials out of anything the
// process prints: the store has no accessor for them and they are held
// in a closure the SDK never formats. config.Secret does the same job on
// the configuration side.
func TestSecretsNeverFormat(t *testing.T) {
	t.Parallel()

	const secret = "koala-test-secret-key"
	store, _ := newStore(t)

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if rendered := fmt.Sprintf(verb, store); strings.Contains(rendered, secret) {
			t.Errorf("a Store formatted with %s holds the secret access key", verb)
		}
	}
	if got := store.Bucket(); got != testBucket {
		t.Errorf("Bucket() = %q, want %q", got, testBucket)
	}
}
