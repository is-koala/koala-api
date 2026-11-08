package media_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/platform/fakes"
	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

const testBucket = "koala-media-test"

// clock is the pipeline's view of time; the store needs it for the
// expiry of a write address.
type clock struct{ at time.Time }

func (c clock) Now() time.Time { return c.at }

// harness is a real object store client over a fake store, a real
// pipeline and the log the pipeline writes to.
type harness struct {
	pipeline *media.Pipeline
	store    *storage.Store
	fake     *fakes.S3
	logs     *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := fakes.NewS3(t, testBucket)
	store, err := storage.New(storage.Config{
		Endpoint:        fake.Endpoint(),
		Region:          "eu-central-1",
		Bucket:          testBucket,
		AccessKeyID:     "media-test-access-key",
		SecretAccessKey: "media-test-secret",
		PathStyle:       true,
		PublicBaseURL:   "https://cdn.example.com",
	}, clock{at: time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)}, fake.Client())
	if err != nil {
		t.Fatalf("storage.New() = %v", err)
	}
	logs := &bytes.Buffer{}
	pipeline, err := media.NewPipeline(store, log.New(logs, slog.LevelDebug))
	if err != nil {
		t.Fatalf("media.NewPipeline() = %v", err)
	}
	return &harness{pipeline: pipeline, store: store, fake: fake, logs: logs}
}

// objectPath is the request path of a key in the fake, for Enqueue.
func objectPath(key storage.Key) string { return "/" + testBucket + "/" + key.String() }

// failedDeletes are enough 500s to outlast the SDK's retries, so a test
// that wants a delete to fail gets a delete that fails.
func failedDeletes() []fakes.Response {
	out := make([]fakes.Response, 8)
	for i := range out {
		out[i] = fakes.Response{Status: http.StatusInternalServerError}
	}
	return out
}

func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 0x80, A: 0xFF})
		}
	}
	return img
}

// jpegWithGPS is a JPEG carrying an APP1 EXIF segment with a GPS marker
// and an orientation tag, which is what a phone uploads.
func jpegWithGPS(t *testing.T, orientation uint16) ([]byte, []byte) {
	t.Helper()
	marker := []byte("KOALA-GPS-41.0082-28.9784")

	var tiff bytes.Buffer
	tiff.WriteString("II")
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(42))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))
	for _, entry := range []struct{ tag, value uint16 }{{0x0112, orientation}, {0x8825, 0x1234}} {
		_ = binary.Write(&tiff, binary.LittleEndian, entry.tag)
		_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
		_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
		_ = binary.Write(&tiff, binary.LittleEndian, entry.value)
		_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))
	}
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	payload = append(payload, marker...)

	var body bytes.Buffer
	if err := jpeg.Encode(&body, gradient(20, 12), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode() = %v", err)
	}
	raw := body.Bytes()
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	out = append(out, payload...)
	out = append(out, raw[2:]...)
	return out, marker
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, gradient(w, h)); err != nil {
		t.Fatalf("png.Encode() = %v", err)
	}
	return buf.Bytes()
}

// upload runs steps one and two: reserve a key and write the object the
// way a client would, straight into the quarantine.
func (h *harness) upload(t *testing.T, kind media.Kind, declared imaging.Type, body []byte) media.Upload {
	t.Helper()
	up, err := h.pipeline.Start(context.Background(), kind, declared)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	h.fake.Seed(up.Key.String(), fakes.S3Object{Body: body, ContentType: string(declared)})
	return up
}

func TestStartReservesAQuarantineKey(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	up, err := h.pipeline.Start(context.Background(), media.KindProfilePicture, imaging.TypePNG)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if up.Key.Prefix() != storage.PrefixIncoming {
		t.Errorf("Key.Prefix() = %q, want the quarantine", up.Key.Prefix())
	}
	if up.Key.Ext() != storage.ExtPNG {
		t.Errorf("Key.Ext() = %q, want the declared type's extension", up.Key.Ext())
	}
	if up.Key.ID().Version() != 7 {
		t.Errorf("Key.ID().Version() = %d, want 7", up.Key.ID().Version())
	}
	if up.Method != http.MethodPut {
		t.Errorf("Method = %q, want PUT", up.Method)
	}
	if !strings.Contains(up.URL, up.Key.String()) {
		t.Errorf("URL %q does not address the reserved key", up.URL)
	}
	if up.ExpiresAt.IsZero() {
		t.Error("ExpiresAt is zero")
	}

	// Two uploads never share a key, so one client cannot overwrite
	// another's quarantine object.
	other, err := h.pipeline.Start(context.Background(), media.KindProfilePicture, imaging.TypePNG)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if other.Key == up.Key {
		t.Error("two uploads were given the same key")
	}
}

func TestStartRefusesWhatTheKindDoesNotTake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		kind     media.Kind
		declared imaging.Type
	}{
		{"unknown kind", media.Kind("COMPANY_COVER"), imaging.TypePNG},
		{"empty kind", media.Kind(""), imaging.TypePNG},
		{"pdf as a profile picture", media.KindProfilePicture, imaging.TypePDF},
		{"image as a CV", media.KindCV, imaging.TypePNG},
		{"type outside the allowlist", media.KindProfilePicture, imaging.Type("image/svg+xml")},
		{"empty type", media.KindProfilePicture, imaging.Type("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			if _, err := h.pipeline.Start(context.Background(), tt.kind, tt.declared); err == nil {
				t.Fatal("Start() accepted it")
			}
			if got := h.fake.Keys(); len(got) != 0 {
				t.Errorf("the store holds %v after a refused start", got)
			}
		})
	}
}

// TestCompleteStripsAndMoves is the whole flow of ADR-0029 Karar 6 for
// an image: the GPS marker is gone, the orientation is baked in, the
// final key is under the kind's prefix and the quarantine is empty.
func TestCompleteStripsAndMoves(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body, marker := jpegWithGPS(t, 6)
	up := h.upload(t, media.KindProfilePicture, imaging.TypeJPEG, body)

	done, err := h.pipeline.Complete(context.Background(), media.KindProfilePicture, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if done.Rejected() {
		t.Fatalf("Complete() rejected with %q", done.Rejection)
	}
	if done.Key.Prefix() != storage.PrefixImages {
		t.Errorf("Key.Prefix() = %q, want images/", done.Key.Prefix())
	}
	if done.Key.ID() != up.Key.ID() {
		t.Error("the final key does not carry the quarantine key's identifier")
	}
	if done.Type != imaging.TypeJPEG {
		t.Errorf("Type = %q", done.Type)
	}
	if done.Orientation != 6 {
		t.Errorf("Orientation = %d, want the tag that was applied", done.Orientation)
	}
	// Orientation 6 is a quarter turn, so the stored image is 12x20.
	if done.Width != 12 || done.Height != 20 {
		t.Errorf("size = %dx%d, want 12x20 after the turn", done.Width, done.Height)
	}
	if want := "https://cdn.example.com/" + done.Key.String(); done.PublicURL != want {
		t.Errorf("PublicURL = %q, want %q", done.PublicURL, want)
	}

	if got := h.fake.Keys(); len(got) != 1 || got[0] != done.Key.String() {
		t.Fatalf("the store holds %v, want only the final key", got)
	}
	stored, ok := h.fake.Object(done.Key.String())
	if !ok {
		t.Fatal("the final object is not in the store")
	}
	if bytes.Contains(stored.Body, marker) {
		t.Error("the GPS marker survived into the stored object")
	}
	if int64(len(stored.Body)) != done.Size {
		t.Errorf("Size = %d, stored %d bytes", done.Size, len(stored.Body))
	}
	if stored.ContentType != string(imaging.TypeJPEG) {
		t.Errorf("stored Content-Type = %q, want the detected type", stored.ContentType)
	}
	if stored.ContentDisposition != "" {
		t.Errorf("stored Content-Disposition = %q; a profile picture is rendered inline", stored.ContentDisposition)
	}
	if !strings.Contains(stored.CacheControl, "immutable") {
		t.Errorf("stored Cache-Control = %q", stored.CacheControl)
	}
	if imaging.Orientation(stored.Body, imaging.TypeJPEG) != imaging.OrientationNone {
		t.Error("the stored object still carries an orientation tag")
	}
}

// TestCompleteIdentityDocument covers the strictest kind: its own
// prefix, an attachment, no caching and no public address at all
// (ADR-0029 Karar 6, IDN-007).
func TestCompleteIdentityDocument(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body, marker := jpegWithGPS(t, 1)
	up := h.upload(t, media.KindIdentityDocument, imaging.TypeJPEG, body)

	done, err := h.pipeline.Complete(context.Background(), media.KindIdentityDocument, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if done.Rejected() {
		t.Fatalf("Complete() rejected with %q", done.Rejection)
	}
	if done.Key.Prefix() != storage.PrefixIdentityDocument {
		t.Errorf("Key.Prefix() = %q", done.Key.Prefix())
	}
	if done.PublicURL != "" {
		t.Errorf("PublicURL = %q; an identity document resolves to no URL", done.PublicURL)
	}
	stored, ok := h.fake.Object(done.Key.String())
	if !ok {
		t.Fatal("the final object is not in the store")
	}
	if stored.ContentDisposition != "attachment" {
		t.Errorf("stored Content-Disposition = %q, want attachment", stored.ContentDisposition)
	}
	if stored.CacheControl != "no-store" {
		t.Errorf("stored Cache-Control = %q, want no-store", stored.CacheControl)
	}
	if bytes.Contains(stored.Body, marker) {
		t.Error("the GPS marker of an identity photo survived; location is never stored for these")
	}
}

// TestCompleteCVMovesThePDFAsItIs covers the one type that is not
// re-encoded (ADR-0030 Karar 4): the bytes are carried over unchanged,
// the stored type is the detected one and the disposition is attachment.
func TestCompleteCVMovesThePDFAsItIs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body := []byte("%PDF-1.7\n1 0 obj\n<< /Author (Yusuf) >>\nendobj\n%%EOF\n")
	up := h.upload(t, media.KindCV, imaging.TypePDF, body)

	done, err := h.pipeline.Complete(context.Background(), media.KindCV, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if done.Rejected() {
		t.Fatalf("Complete() rejected with %q", done.Rejection)
	}
	if done.Key.Prefix() != storage.PrefixFiles || done.Key.Ext() != storage.ExtPDF {
		t.Errorf("Key = %q, want a files/ PDF", done.Key.String())
	}
	if done.Width != 0 || done.Height != 0 || done.Orientation != 0 {
		t.Errorf("a PDF reported dimensions: %+v", done)
	}
	stored, ok := h.fake.Object(done.Key.String())
	if !ok {
		t.Fatal("the final object is not in the store")
	}
	if !bytes.Equal(stored.Body, body) {
		t.Error("the PDF bytes changed; a PDF is moved, not re-encoded")
	}
	// The document's own metadata is still in there. That is the known
	// limitation ADR-0030 Karar 4 records, and it is why a CV is served
	// only as an attachment through a short lived address.
	if !bytes.Contains(stored.Body, []byte("Yusuf")) {
		t.Error("the fixture no longer proves the limitation it was written for")
	}
	if stored.ContentDisposition != "attachment" || stored.CacheControl != "no-store" {
		t.Errorf("stored as %+v, want an uncached attachment", stored)
	}
	if got := h.fake.Keys(); len(got) != 1 {
		t.Errorf("the store holds %v, want only the final key", got)
	}
}

// TestCompleteNamesTheKeyFromTheDetectedType is the departure from Java
// in ADR-0029 Karar 5: the client declared one type and uploaded another,
// and the server believes the bytes.
func TestCompleteNamesTheKeyFromTheDetectedType(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body, _ := jpegWithGPS(t, 1)
	// Declared PNG, so the quarantine key is named .png; the content is
	// a JPEG.
	up := h.upload(t, media.KindProfilePicture, imaging.TypePNG, body)
	if up.Key.Ext() != storage.ExtPNG {
		t.Fatalf("the fixture does not set up the case: quarantine ext is %q", up.Key.Ext())
	}

	done, err := h.pipeline.Complete(context.Background(), media.KindProfilePicture, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if done.Rejected() {
		t.Fatalf("Complete() rejected with %q", done.Rejection)
	}
	if done.Key.Ext() != storage.ExtJPEG {
		t.Errorf("final Key.Ext() = %q, want jpg: the extension follows the detected type", done.Key.Ext())
	}
	if done.Type != imaging.TypeJPEG {
		t.Errorf("Type = %q, want the detected type", done.Type)
	}
}

func TestCompleteRejections(t *testing.T) {
	t.Parallel()

	jpegMagicOnly := append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 64)...)

	tests := []struct {
		name     string
		kind     media.Kind
		declared imaging.Type
		// seed is nil when the client never wrote to the address.
		seed []byte
		want media.Rejection
	}{
		{"never uploaded", media.KindProfilePicture, imaging.TypeJPEG, nil, media.RejectionNotUploaded},
		{"a PDF where an image belongs", media.KindProfilePicture, imaging.TypeJPEG, []byte("%PDF-1.7\n"), media.RejectionTypeUnsupported},
		{"an image where a PDF belongs", media.KindCV, imaging.TypePDF, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, media.RejectionTypeUnsupported},
		{"plain text", media.KindProfilePicture, imaging.TypeJPEG, []byte("bu bir metin dosyası"), media.RejectionTypeUnsupported},
		{"an SVG", media.KindProfilePicture, imaging.TypeJPEG, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`), media.RejectionTypeUnsupported},
		{"empty object", media.KindProfilePicture, imaging.TypeJPEG, []byte{}, media.RejectionTypeUnsupported},
		{"JPEG magic, no image", media.KindProfilePicture, imaging.TypeJPEG, jpegMagicOnly, media.RejectionCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			up, err := h.pipeline.Start(context.Background(), tt.kind, tt.declared)
			if err != nil {
				t.Fatalf("Start() = %v", err)
			}
			if tt.seed != nil {
				h.fake.Seed(up.Key.String(), fakes.S3Object{Body: tt.seed, ContentType: string(tt.declared)})
			}

			done, err := h.pipeline.Complete(context.Background(), tt.kind, up.Key)
			if err != nil {
				t.Fatalf("Complete() = %v, want a rejection and no error", err)
			}
			if done.Rejection != tt.want {
				t.Errorf("Rejection = %q, want %q", done.Rejection, tt.want)
			}
			if done.PublicURL != "" || !done.Key.Zero() {
				t.Errorf("a rejected completion carries %+v", done)
			}
			// A rejected upload leaves nothing in the store: the
			// quarantine object is deleted on the way out.
			if got := h.fake.Keys(); len(got) != 0 {
				t.Errorf("the store holds %v after a rejection", got)
			}
		})
	}
}

// TestCompleteRejectsAnOversizeObject exercises the head gate, which
// refuses before a byte is read.
func TestCompleteRejectsAnOversizeObject(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	up, err := h.pipeline.Start(context.Background(), media.KindProfilePicture, imaging.TypeJPEG)
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	// The fake reports the length it was seeded with, so a body one byte
	// over the kind's bound is enough; nothing of it is read.
	h.fake.Seed(up.Key.String(), fakes.S3Object{
		Body:        make([]byte, media.KindProfilePicture.Limits().MaxBytes+1),
		ContentType: "image/jpeg",
	})

	done, err := h.pipeline.Complete(context.Background(), media.KindProfilePicture, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if done.Rejection != media.RejectionTooLarge {
		t.Errorf("Rejection = %q, want TOO_LARGE", done.Rejection)
	}
	if got := h.fake.Keys(); len(got) != 0 {
		t.Errorf("the store holds %v after a rejection", got)
	}
}

// TestCompleteRejectsADecompressionBomb covers the pixel bound: the
// canvas is read from the image header, so a small file that expands
// into an enormous raster is refused before the raster exists.
func TestCompleteRejectsADecompressionBomb(t *testing.T) {
	t.Parallel()

	// A PNG whose IHDR declares a 900 megapixel canvas, carrying the
	// image data of a 4x4 one. The header is internally consistent --
	// the CRC is recomputed -- so image.DecodeConfig reads it happily
	// and the pixel bound is what refuses the file, before a raster of
	// 900 million pixels is allocated.
	bomb := bytes.Clone(pngBytes(t, 4, 4))
	// Signature 8 + length 4 + type 4 = 16: width, then height, then the
	// rest of IHDR, then the CRC over type and data at 29.
	binary.BigEndian.PutUint32(bomb[16:20], 30_000)
	binary.BigEndian.PutUint32(bomb[20:24], 30_000)
	binary.BigEndian.PutUint32(bomb[29:33], crc32.ChecksumIEEE(bomb[12:29]))

	h := newHarness(t)
	up := h.upload(t, media.KindProfilePicture, imaging.TypePNG, bomb)

	done, err := h.pipeline.Complete(context.Background(), media.KindProfilePicture, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if done.Rejection != media.RejectionTooLarge {
		t.Fatalf("Rejection = %q, want TOO_LARGE: the 900 megapixel canvas must be refused from the header", done.Rejection)
	}
	if got := h.fake.Keys(); len(got) != 0 {
		t.Errorf("the store holds %v after a rejection", got)
	}
}

func TestCompleteRefusesAKeyOutsideTheQuarantine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	for _, prefix := range []storage.Prefix{
		storage.PrefixImages,
		storage.PrefixIdentityDocument,
		storage.PrefixFiles,
		storage.PrefixPublic,
	} {
		id, err := storage.NewID()
		if err != nil {
			t.Fatal(err)
		}
		key, err := storage.NewKey(prefix, id, storage.ExtJPEG)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.pipeline.Complete(ctx, media.KindProfilePicture, key); err == nil {
			t.Errorf("Complete() accepted a %s key", prefix)
		}
	}
	var zero storage.Key
	if _, err := h.pipeline.Complete(ctx, media.KindProfilePicture, zero); err == nil {
		t.Error("Complete() accepted the zero key")
	}
	id, err := storage.NewID()
	if err != nil {
		t.Fatal(err)
	}
	key, err := storage.NewKey(storage.PrefixIncoming, id, storage.ExtJPEG)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pipeline.Complete(ctx, media.Kind("COMPANY_COVER"), key); err == nil {
		t.Error("Complete() accepted an unknown kind")
	}
}

// TestCompleteSurvivesAFailedQuarantineDelete is the half finished move:
// the final object is written, the quarantine delete fails. The upload
// succeeds, because the object the client asked for exists and is
// correct, and the leftover is logged at ERROR for the sweep.
func TestCompleteSurvivesAFailedQuarantineDelete(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body, _ := jpegWithGPS(t, 1)
	up := h.upload(t, media.KindProfilePicture, imaging.TypeJPEG, body)
	// The SDK retries a 5xx, so the queue has to outlast the retries.
	h.fake.Enqueue(http.MethodDelete, objectPath(up.Key), failedDeletes()...)

	done, err := h.pipeline.Complete(context.Background(), media.KindProfilePicture, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v, want the upload to succeed", err)
	}
	if done.Rejected() {
		t.Fatalf("Complete() rejected with %q", done.Rejection)
	}
	if _, ok := h.fake.Object(done.Key.String()); !ok {
		t.Error("the final object was not written")
	}
	if _, ok := h.fake.Object(up.Key.String()); !ok {
		t.Error("the fixture did not leave the quarantine object behind")
	}
	logged := h.logs.String()
	if !strings.Contains(logged, "object left in the store after a failed cleanup") {
		t.Errorf("the leftover was not logged: %s", logged)
	}
	if !strings.Contains(logged, `"level":"ERROR"`) {
		t.Errorf("the leftover was not logged at ERROR: %s", logged)
	}
	if strings.Contains(logged, "media-test-secret") {
		t.Error("the log holds the secret access key")
	}
}

// TestDiscardKeepsAnObjectAfterAnIndeterminateWrite is the rollback
// invariant of ADR-0029 Karar 6: a failed write deletes the object, an
// indeterminate one keeps it, because a row pointing at a deleted object
// is worse than an object no row points at.
func TestDiscardKeepsAnObjectAfterAnIndeterminateWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome media.Outcome
		kept    bool
	}{
		{"the write definitely failed", media.WriteFailed, false},
		{"the write outcome is unknown", media.WriteIndeterminate, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			body, _ := jpegWithGPS(t, 1)
			up := h.upload(t, media.KindProfilePicture, imaging.TypeJPEG, body)
			done, err := h.pipeline.Complete(context.Background(), media.KindProfilePicture, up.Key)
			if err != nil {
				t.Fatalf("Complete() = %v", err)
			}

			h.pipeline.Discard(context.Background(), done.Key, tt.outcome)
			_, ok := h.fake.Object(done.Key.String())
			if ok != tt.kept {
				t.Errorf("the object is there: %v, want %v", ok, tt.kept)
			}
		})
	}
}

// TestDiscardDoesNotMaskTheOriginalError: a cleanup that fails is logged
// and nothing else, so the caller still answers with why it was rolling
// back.
func TestDiscardDoesNotMaskTheOriginalError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body, _ := jpegWithGPS(t, 1)
	up := h.upload(t, media.KindProfilePicture, imaging.TypeJPEG, body)
	done, err := h.pipeline.Complete(context.Background(), media.KindProfilePicture, up.Key)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	h.fake.Enqueue(http.MethodDelete, objectPath(done.Key), failedDeletes()...)

	h.pipeline.Discard(context.Background(), done.Key, media.WriteFailed)
	if !strings.Contains(h.logs.String(), "object left in the store after a failed cleanup") {
		t.Errorf("the failed cleanup was not logged: %s", h.logs.String())
	}
}

func TestNewPipelineNeedsItsDependencies(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, err := media.NewPipeline(nil, log.New(h.logs, slog.LevelError)); err == nil {
		t.Error("NewPipeline() without a store succeeded")
	}
	if _, err := media.NewPipeline(h.store, nil); err == nil {
		t.Error("NewPipeline() without a logger succeeded")
	}
}
