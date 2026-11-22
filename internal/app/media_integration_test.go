//go:build integration

package app

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/platform/fakes"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// mediaBody is the part of the contract's Media a test reads.
type mediaBody struct {
	ID            uuid.UUID `json:"id"`
	Status        string    `json:"status"`
	ContentType   *string   `json:"contentType"`
	URL           *string   `json:"url"`
	URLExpiresAt  *string   `json:"urlExpiresAt"`
	RejectionCode *string   `json:"rejectionCode"`
}

// startOverHTTP starts an upload of kind with key and returns the answer.
func (a *authApp) startOverHTTP(t *testing.T, bearer, kind, key string) *httptest.ResponseRecorder {
	t.Helper()
	return a.postKeyedAs(t, "/v1/me/media", key, `{"kind":"`+kind+`","sizeBytes":1024}`, bearer)
}

// pngOf is a small PNG.
func pngOf(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The four operations over the wired router (ADR-0035): a start answers
// 201 with the PENDING media, its Location and the write address; the
// completion answers 202 with the media as it stands and the job decides;
// a read renders the outcome; a deletion answers 204 and the media is then
// the same 404 as one that never existed, as is another user's media
// (ADR-0014).
func TestMediaOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	bearer := a.addSignedIn(t, "medya@example.com")

	rec := a.startOverHTTP(t, bearer, "PROFILE_PICTURE", "start-0001")
	wantStatus(t, rec, http.StatusCreated, "")
	var up struct {
		Media     mediaBody `json:"media"`
		UploadURL string    `json:"uploadUrl"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	id := up.Media.ID
	if rec.Header().Get("Location") != "/v1/me/media/"+id.String() || up.Media.Status != "PENDING" ||
		up.Media.URL != nil || up.Media.RejectionCode != nil || !strings.Contains(up.UploadURL, "/incoming/"+id.String()+".jpg") {
		t.Fatalf("MED-001: start answered %s %s", rec.Header().Get("Location"), rec.Body)
	}

	a.s3.Seed("incoming/"+id.String()+".jpg", fakes.S3Object{Body: pngOf(t), ContentType: "image/jpeg"})
	rec = a.call(t, http.MethodPost, "/v1/me/media/"+id.String()+"/complete", bearer, "")
	wantStatus(t, rec, http.StatusAccepted, "")
	if got := decodeBody[mediaBody](t, rec); got.Status != "PENDING" {
		t.Fatalf("complete answered %s, want the PENDING media", rec.Body)
	}
	if err := a.mods.media.Complete(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	rec = a.call(t, http.MethodGet, "/v1/me/media/"+id.String(), bearer, "")
	wantStatus(t, rec, http.StatusOK, "")
	got := decodeBody[mediaBody](t, rec)
	wantURL := testPublicBaseURL + "/images/" + id.String() + ".png"
	if got.Status != "READY" || got.ContentType == nil || *got.ContentType != "IMAGE_PNG" ||
		got.URL == nil || *got.URL != wantURL || got.URLExpiresAt != nil {
		t.Fatalf("read answered %s, want a READY PNG at %s", rec.Body, wantURL)
	}

	// Completing a READY media changes nothing and answers it as it is.
	rec = a.call(t, http.MethodPost, "/v1/me/media/"+id.String()+"/complete", bearer, "")
	wantStatus(t, rec, http.StatusAccepted, "")
	if got := decodeBody[mediaBody](t, rec); got.Status != "READY" {
		t.Fatalf("second complete answered %s", rec.Body)
	}

	// Another user's media and a media that never existed: one answer.
	stranger := a.addSignedIn(t, "yabanci@example.com")
	theirs := a.call(t, http.MethodGet, "/v1/me/media/"+id.String(), stranger, "")
	missing := a.call(t, http.MethodGet, "/v1/me/media/"+uuid.NewString(), stranger, "")
	wantStatus(t, theirs, http.StatusNotFound, string(media.CodeNotFound))
	if !bytes.Equal(theirs.Body.Bytes(), missing.Body.Bytes()) {
		t.Errorf("another user's media %s differs from a missing one %s", theirs.Body, missing.Body)
	}
	// A stranger cannot complete or delete it either, and learns nothing.
	wantStatus(t, a.call(t, http.MethodDelete, "/v1/me/media/"+id.String(), stranger, ""),
		http.StatusNotFound, string(media.CodeNotFound))
	wantStatus(t, a.call(t, http.MethodPost, "/v1/me/media/"+id.String()+"/complete", stranger, ""),
		http.StatusNotFound, string(media.CodeNotFound))

	rec = a.call(t, http.MethodDelete, "/v1/me/media/"+id.String(), bearer, "")
	wantStatus(t, rec, http.StatusNoContent, "")
	rec = a.call(t, http.MethodGet, "/v1/me/media/"+id.String(), bearer, "")
	wantStatus(t, rec, http.StatusNotFound, string(media.CodeNotFound))
}

// A rejection is not a problem: the completion answers 202 and the media
// renders the contract's code, with CORRUPT folded into
// MEDIA_TYPE_UNSUPPORTED (ADR-0035 Karar 3).
func TestRejectedMediaRendersItsCode(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	bearer := a.addSignedIn(t, "bozuk@example.com")
	rec := a.startOverHTTP(t, bearer, "COMPANY_LOGO", "start-corrupt")
	wantStatus(t, rec, http.StatusCreated, "")
	id := decodeBody[struct {
		Media mediaBody `json:"media"`
	}](t, rec).Media.ID
	a.s3.Seed("incoming/"+id.String()+".jpg", fakes.S3Object{Body: append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 64)...)})
	wantStatus(t, a.call(t, http.MethodPost, "/v1/me/media/"+id.String()+"/complete", bearer, ""), http.StatusAccepted, "")
	if err := a.mods.media.Complete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	rec = a.call(t, http.MethodGet, "/v1/me/media/"+id.String(), bearer, "")
	wantStatus(t, rec, http.StatusOK, "")
	got := decodeBody[mediaBody](t, rec)
	if got.Status != "REJECTED" || got.RejectionCode == nil || *got.RejectionCode != "MEDIA_TYPE_UNSUPPORTED" || got.URL != nil {
		t.Errorf("a corrupt upload reads %s, want REJECTED with MEDIA_TYPE_UNSUPPORTED", rec.Body)
	}
	var stored string
	if err := a.db.Pool().QueryRow(t.Context(), "SELECT rejection FROM active_media WHERE id = $1", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != string(media.RejectionCorrupt) {
		t.Errorf("the row stores %q, want the pipeline's own CORRUPT", stored)
	}
}

// The daily budget answers 429 with Retry-After (ADR-0035 Karar 6); the
// starts are deleted as they go, so the open-upload cap never binds and a
// deletion is seen not to refill the budget.
func TestDailyUploadLimitAnswersRetryAfter(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	bearer := a.addSignedIn(t, "cok@example.com")
	for i := range media.DailyUploads {
		rec := a.startOverHTTP(t, bearer, "COMPANY_IMAGE", "budget-"+strconv.Itoa(i)+"-key")
		wantStatus(t, rec, http.StatusCreated, "")
		id := decodeBody[struct {
			Media mediaBody `json:"media"`
		}](t, rec).Media.ID
		wantStatus(t, a.call(t, http.MethodDelete, "/v1/me/media/"+id.String(), bearer, ""), http.StatusNoContent, "")
	}
	rec := a.startOverHTTP(t, bearer, "COMPANY_IMAGE", "budget-over-key")
	wantStatus(t, rec, http.StatusTooManyRequests, string(media.CodeDailyLimit))
	if wait, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || wait <= 0 {
		t.Errorf("Retry-After = %q, want the seconds until the oldest start leaves the window", rec.Header().Get("Retry-After"))
	}
}

// MED-003, MED-004: no media path reaches outside its prefix. The id is a
// UUID path parameter, so a path that tries to climb out of its segment
// is refused before routing (PATH_INVALID: an encoded dot, slash,
// backslash or percent, double encoding included) or by validation
// (REQUEST_INVALID: not a UUID); none of them reaches a handler, let
// alone a key. The keys themselves are the server's and are read back
// through storage.ParseKey (FuzzStorageKey, TestParseKeyDecodesExactlyOnce).
func TestMediaPathsStayInTheirPrefix(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	bearer := a.addSignedIn(t, "yol@example.com")
	tests := []struct {
		path, code string
	}{
		{"/v1/me/media/..%2f..%2fidentity-documents%2f0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.jpg", httpx.CodePathInvalid},
		{"/v1/me/media/%2e%2e", httpx.CodePathInvalid},
		{"/v1/me/media/.%2E", httpx.CodePathInvalid},
		{"/v1/me/media/%252e%252e%252fimages", httpx.CodePathInvalid},
		{"/v1/me/media/a%5cb", httpx.CodePathInvalid},
		{"/v1/me/media//0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30", httpx.CodePathInvalid},
		{"/v1/me/media/identity-documents", httpx.CodeRequestInvalid},
		{"/v1/me/media/0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a30.jpg", httpx.CodeRequestInvalid},
	}
	for _, tt := range tests {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			rec := a.call(t, method, tt.path, bearer, "")
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+tt.code+`"`) {
				t.Errorf("MED-003/MED-004: %s %s = %d %s, want 400 %s", method, tt.path, rec.Code, rec.Body, tt.code)
			}
		}
	}
}
