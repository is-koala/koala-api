package imaging_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/imaging"
)

// gradient builds an image whose every pixel is distinguishable, so a
// transform that mirrors or rotates the wrong way is visible in a pixel
// comparison and not only in the dimensions.
func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 0x40, A: 0xFF})
		}
	}
	return img
}

func encodeJPEG(tb testing.TB, img image.Image) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		tb.Fatalf("jpeg.Encode() = %v", err)
	}
	return buf.Bytes()
}

func encodePNG(tb testing.TB, img image.Image) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatalf("png.Encode() = %v", err)
	}
	return buf.Bytes()
}

// tiffBlock builds a little-endian TIFF block with one IFD carrying the
// tags given, each as a SHORT with count 1.
func tiffBlock(tags map[uint16]uint16) []byte {
	var b bytes.Buffer
	b.WriteString("II")
	_ = binary.Write(&b, binary.LittleEndian, uint16(42))
	_ = binary.Write(&b, binary.LittleEndian, uint32(8))
	_ = binary.Write(&b, binary.LittleEndian, uint16(len(tags)))
	// A map iterates in random order; the IFD does not have to be sorted
	// for the reader, and shuffling it between runs is free coverage.
	for tag, value := range tags {
		_ = binary.Write(&b, binary.LittleEndian, tag)
		_ = binary.Write(&b, binary.LittleEndian, uint16(3)) // SHORT
		_ = binary.Write(&b, binary.LittleEndian, uint32(1)) // count
		_ = binary.Write(&b, binary.LittleEndian, value)
		_ = binary.Write(&b, binary.LittleEndian, uint16(0)) // padding of the 4 byte value field
	}
	_ = binary.Write(&b, binary.LittleEndian, uint32(0)) // no next IFD
	return b.Bytes()
}

// withJPEGAPP1 splices an APP1 segment holding payload in right after
// the SOI marker, which is where a camera writes EXIF.
func withJPEGAPP1(tb testing.TB, jpegBytes, payload []byte) []byte {
	tb.Helper()
	if len(jpegBytes) < 2 || jpegBytes[0] != 0xFF || jpegBytes[1] != 0xD8 {
		tb.Fatal("not a JPEG")
	}
	segment := append([]byte("Exif\x00\x00"), payload...)
	out := make([]byte, 0, len(jpegBytes)+len(segment)+4)
	out = append(out, 0xFF, 0xD8, 0xFF, 0xE1)
	out = binary.BigEndian.AppendUint16(out, uint16(len(segment)+2))
	out = append(out, segment...)
	return append(out, jpegBytes[2:]...)
}

// withPNGeXIf splices an eXIf chunk in front of the first chunk after the
// signature, which is where the PNG specification puts it.
func withPNGeXIf(tb testing.TB, pngBytes, payload []byte) []byte {
	tb.Helper()
	const signatureLen = 8
	if len(pngBytes) < signatureLen {
		tb.Fatal("not a PNG")
	}
	// The IHDR chunk has to stay first, so the eXIf chunk goes after it:
	// 8 signature + 4 length + 4 type + 13 data + 4 crc.
	const afterIHDR = signatureLen + 25
	chunk := make([]byte, 0, len(payload)+12)
	chunk = binary.BigEndian.AppendUint32(chunk, uint32(len(payload)))
	body := append([]byte("eXIf"), payload...)
	chunk = append(chunk, body...)
	// image/png verifies every chunk's CRC, including the ones it
	// ignores, so the fixture has to carry a correct one.
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(body))
	out := make([]byte, 0, len(pngBytes)+len(chunk))
	out = append(out, pngBytes[:afterIHDR]...)
	out = append(out, chunk...)
	return append(out, pngBytes[afterIHDR:]...)
}

func TestDetectReadsTheContent(t *testing.T) {
	t.Parallel()

	jpegBytes := encodeJPEG(t, gradient(8, 8))
	pngBytes := encodePNG(t, gradient(8, 8))

	tests := []struct {
		name string
		in   []byte
		want imaging.Type
		ok   bool
	}{
		{"jpeg", jpegBytes, imaging.TypeJPEG, true},
		{"png", pngBytes, imaging.TypePNG, true},
		{"pdf", []byte("%PDF-1.7\n1 0 obj\n"), imaging.TypePDF, true},
		{"jpeg head only", jpegBytes[:3], imaging.TypeJPEG, true},
		{"empty", nil, "", false},
		{"text", []byte("merhaba, bu bir metin dosyası"), "", false},
		{"html", []byte("<!DOCTYPE html><html><body>x"), "", false},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`), "", false},
		{"gif", []byte("GIF89a\x01\x00"), "", false},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "", false},
		{"zip", []byte("PK\x03\x04"), "", false},
		{"elf", []byte("\x7fELF\x02\x01\x01\x00"), "", false},
		{"short jpeg", []byte{0xFF, 0xD8}, "", false},
		{"pdf not at offset zero", append([]byte("GIF89a"), "%PDF-1.7"...), "", false},
		{"png signature off by one byte", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0B}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := imaging.Detect(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("Detect() = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestSanitizeDropsEveryMetadataSegment is the EXIF claim of ADR-0029
// Karar 5 as a test: a GPS tag goes in, nothing of it comes out.
func TestSanitizeDropsEveryMetadataSegment(t *testing.T) {
	t.Parallel()

	const (
		tagOrientation = 0x0112
		tagGPSIFD      = 0x8825
		tagMake        = 0x010F
	)
	// A marker that would survive any copy that does not re-encode.
	marker := []byte("KOALA-GPS-41.0082-28.9784")
	exif := tiffBlock(map[uint16]uint16{
		tagOrientation: 1,
		tagGPSIFD:      0x1234,
		tagMake:        0x4242,
	})
	exif = append(exif, marker...)

	cases := []struct {
		name string
		in   []byte
		typ  imaging.Type
	}{
		{"jpeg app1", withJPEGAPP1(t, encodeJPEG(t, gradient(16, 16)), exif), imaging.TypeJPEG},
		{"png exif chunk", withPNGeXIf(t, encodePNG(t, gradient(16, 16)), exif), imaging.TypePNG},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !bytes.Contains(tc.in, marker) {
				t.Fatal("the fixture does not carry the marker; the test would pass vacuously")
			}
			got, err := imaging.Sanitize(context.Background(), tc.in, imaging.Limits{})
			if err != nil {
				t.Fatalf("Sanitize() = %v", err)
			}
			if got.Type != tc.typ {
				t.Errorf("Type = %q, want %q", got.Type, tc.typ)
			}
			if bytes.Contains(got.Data, marker) {
				t.Error("the metadata marker survived the strip")
			}
			if bytes.Contains(got.Data, []byte("Exif")) {
				t.Error("an Exif header survived the strip")
			}
			if bytes.Contains(got.Data, []byte("eXIf")) {
				t.Error("a PNG eXIf chunk survived the strip")
			}
			// Nothing of the input is carried over verbatim, and the
			// output still decodes.
			if _, _, err := image.Decode(bytes.NewReader(got.Data)); err != nil {
				t.Errorf("the stripped image does not decode: %v", err)
			}
			again, err := imaging.Sanitize(context.Background(), got.Data, imaging.Limits{})
			if err != nil {
				t.Fatalf("Sanitize() of the output = %v", err)
			}
			if again.Orientation != imaging.OrientationNone {
				t.Errorf("the output still carries orientation %d", again.Orientation)
			}
		})
	}
}

// TestSanitizeBakesTheOrientationIn covers the one tag that is read
// before it is dropped: the image comes out upright, so the dimensions
// swap for the four transposing values, and the tag is gone afterwards.
func TestSanitizeBakesTheOrientationIn(t *testing.T) {
	t.Parallel()

	const (
		w = 12
		h = 8
	)
	tests := []struct {
		orientation           int
		wantWidth, wantHeight int
	}{
		{1, w, h},
		{2, w, h},
		{3, w, h},
		{4, w, h},
		{5, h, w},
		{6, h, w},
		{7, h, w},
		{8, h, w},
	}
	for _, tt := range tests {
		t.Run(string(rune('0'+tt.orientation)), func(t *testing.T) {
			t.Parallel()

			exif := tiffBlock(map[uint16]uint16{0x0112: uint16(tt.orientation)})
			in := withJPEGAPP1(t, encodeJPEG(t, gradient(w, h)), exif)

			got, err := imaging.Sanitize(context.Background(), in, imaging.Limits{})
			if err != nil {
				t.Fatalf("Sanitize() = %v", err)
			}
			if got.Orientation != tt.orientation {
				t.Errorf("Orientation = %d, want %d", got.Orientation, tt.orientation)
			}
			if got.Width != tt.wantWidth || got.Height != tt.wantHeight {
				t.Errorf("size = %dx%d, want %dx%d", got.Width, got.Height, tt.wantWidth, tt.wantHeight)
			}
			// The stored image is upright: a second pass finds no tag,
			// so applying it twice is impossible.
			if orientation := imaging.Orientation(got.Data, imaging.TypeJPEG); orientation != imaging.OrientationNone {
				t.Errorf("Orientation(output) = %d, want %d", orientation, imaging.OrientationNone)
			}
		})
	}
}

func TestOrientationReadsBothFormatsAndIgnoresTheRest(t *testing.T) {
	t.Parallel()

	exif := tiffBlock(map[uint16]uint16{0x0112: 6})
	bigEndian := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 8, 0, 0, 0, 0, 0, 0}

	tests := []struct {
		name string
		in   []byte
		typ  imaging.Type
		want int
	}{
		{"jpeg app1", withJPEGAPP1(t, encodeJPEG(t, gradient(4, 4)), exif), imaging.TypeJPEG, 6},
		{"png exif chunk", withPNGeXIf(t, encodePNG(t, gradient(4, 4)), exif), imaging.TypePNG, 6},
		{"big endian tiff", withJPEGAPP1(t, encodeJPEG(t, gradient(4, 4)), bigEndian), imaging.TypeJPEG, 8},
		{"no exif at all", encodeJPEG(t, gradient(4, 4)), imaging.TypeJPEG, 1},
		{"png without a chunk", encodePNG(t, gradient(4, 4)), imaging.TypePNG, 1},
		{"pdf", []byte("%PDF-1.7"), imaging.TypePDF, 1},
		{"empty", nil, imaging.TypeJPEG, 1},
		{"truncated jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE1}, imaging.TypeJPEG, 1},
		{"app1 with an empty exif block", withJPEGAPP1(t, encodeJPEG(t, gradient(4, 4)), nil), imaging.TypeJPEG, 1},
		{"out of range value", withJPEGAPP1(t, encodeJPEG(t, gradient(4, 4)), tiffBlock(map[uint16]uint16{0x0112: 99})), imaging.TypeJPEG, 1},
		{"zero value", withJPEGAPP1(t, encodeJPEG(t, gradient(4, 4)), tiffBlock(map[uint16]uint16{0x0112: 0})), imaging.TypeJPEG, 1},
		{"other tags only", withJPEGAPP1(t, encodeJPEG(t, gradient(4, 4)), tiffBlock(map[uint16]uint16{0x010F: 1})), imaging.TypeJPEG, 1},
		{"bad byte order", withJPEGAPP1(t, encodeJPEG(t, gradient(4, 4)), []byte("XX\x00\x2a\x00\x00\x00\x08\x00\x00")), imaging.TypeJPEG, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := imaging.Orientation(tt.in, tt.typ); got != tt.want {
				t.Errorf("Orientation() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSanitizeRefusesWhatItMustNot(t *testing.T) {
	t.Parallel()

	jpegBytes := encodeJPEG(t, gradient(8, 8))
	pngBytes := encodePNG(t, gradient(8, 8))

	tests := []struct {
		name    string
		in      []byte
		limits  imaging.Limits
		wantErr error
	}{
		{"pdf", []byte("%PDF-1.7\n1 0 obj"), imaging.Limits{}, imaging.ErrUnsupportedType},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), imaging.Limits{}, imaging.ErrUnsupportedType},
		{"text named as an image", []byte("bu aslında bir metin"), imaging.Limits{}, imaging.ErrUnsupportedType},
		{"empty", nil, imaging.Limits{}, imaging.ErrUnsupportedType},
		{"over the byte bound", jpegBytes, imaging.Limits{MaxBytes: 10}, imaging.ErrTooLarge},
		{"over the pixel bound", pngBytes, imaging.Limits{MaxPixels: 4}, imaging.ErrCanvasTooLarge},
		{"jpeg magic, truncated body", jpegBytes[:12], imaging.Limits{}, imaging.ErrCorrupt},
		{"png magic, truncated body", pngBytes[:12], imaging.Limits{}, imaging.ErrCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := imaging.Sanitize(context.Background(), tt.in, tt.limits)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Sanitize() error = %v, want %v", err, tt.wantErr)
			}
			if got.Data != nil {
				t.Errorf("Sanitize() returned %d bytes on error", len(got.Data))
			}
		})
	}
}

// TestSanitizeRefusesAPolyglot covers the polyglot the re-encode cannot
// handle by itself: a file whose header claims one format and whose body
// is another. Nothing is written, so there is no object with a JPEG
// extension holding a PNG -- or anything else.
func TestSanitizeRefusesAPolyglot(t *testing.T) {
	t.Parallel()

	// JPEG SOI followed by a whole PNG. Detect says JPEG, because that is
	// what the first three bytes say; the JPEG decoder then refuses the
	// body. checkFormat would catch the other shape of this, where the
	// registry picks a different decoder than Detect named.
	pngBytes := encodePNG(t, gradient(8, 8))
	polyglot := append([]byte{0xFF, 0xD8, 0xFF}, pngBytes...)

	if typ, ok := imaging.Detect(polyglot); !ok || typ != imaging.TypeJPEG {
		t.Fatalf("Detect() = %q, %v; the fixture is not the case under test", typ, ok)
	}
	if _, err := imaging.Sanitize(context.Background(), polyglot, imaging.Limits{}); err == nil {
		t.Fatal("Sanitize() accepted a polyglot")
	}
}

// TestSanitizeDropsAppendedData is the other half of the polyglot story:
// a valid JPEG with an HTML payload appended after the end-of-image
// marker is accepted, because it is a valid JPEG, and the payload does
// not survive the re-encode.
func TestSanitizeDropsAppendedData(t *testing.T) {
	t.Parallel()

	payload := []byte("<script>alert(document.domain)</script>")
	in := append(encodeJPEG(t, gradient(8, 8)), payload...)

	got, err := imaging.Sanitize(context.Background(), in, imaging.Limits{})
	if err != nil {
		t.Fatalf("Sanitize() = %v", err)
	}
	if bytes.Contains(got.Data, payload) {
		t.Error("the appended payload survived the re-encode")
	}
	if typ, ok := imaging.Detect(got.Data); !ok || typ != imaging.TypeJPEG {
		t.Errorf("Detect(output) = %q, %v; want a JPEG", typ, ok)
	}
}

// TestSanitizeStopsOnACanceledContext proves the cancellation boundary:
// the decode itself cannot be interrupted, but a canceled job stops at
// the next step instead of finishing the whole pipeline.
func TestSanitizeStopsOnACanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := imaging.Sanitize(ctx, encodeJPEG(t, gradient(8, 8)), imaging.Limits{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sanitize() error = %v, want context.Canceled", err)
	}
}

// TestLimitsFallBackToTheDefaults pins that a zero field means the
// default and never "no limit", which is the failure mode of a struct of
// optional bounds.
func TestLimitsFallBackToTheDefaults(t *testing.T) {
	t.Parallel()

	// 10 MiB plus one byte of a valid JPEG would need a real 10 MiB
	// image; a run of bytes with the JPEG magic is enough, because the
	// byte bound is checked before anything is parsed.
	oversize := append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, imaging.DefaultMaxBytes)...)
	if _, err := imaging.Sanitize(context.Background(), oversize, imaging.Limits{}); !errors.Is(err, imaging.ErrTooLarge) {
		t.Errorf("Sanitize() with zero limits error = %v, want ErrTooLarge", err)
	}
	for _, l := range []imaging.Limits{
		{MaxBytes: -1},
		{MaxPixels: -1},
		{JPEGQuality: 0},
		{JPEGQuality: 1000},
	} {
		if _, err := imaging.Sanitize(context.Background(), encodeJPEG(t, gradient(8, 8)), l); err != nil {
			t.Errorf("Sanitize() with %+v = %v", l, err)
		}
	}
}

func TestTypesAndImage(t *testing.T) {
	t.Parallel()

	if got := len(imaging.Types()); got != 3 {
		t.Errorf("len(Types()) = %d, want 3", got)
	}
	for _, typ := range imaging.Types() {
		if !strings.Contains(string(typ), "/") {
			t.Errorf("Type %q is not a media type", typ)
		}
	}
	if !imaging.TypeJPEG.Image() || !imaging.TypePNG.Image() {
		t.Error("JPEG and PNG must count as images")
	}
	if imaging.TypePDF.Image() {
		t.Error("PDF must not count as an image: it cannot be re-encoded")
	}
}

// corners are four opaque, distinguishable colors for the four corners
// of a test image. The PNG path is lossless, so a decoded corner is
// byte-for-byte the color that was written.
func corners() [4]color.RGBA {
	return [4]color.RGBA{
		{R: 0xFF, G: 0x00, B: 0x00, A: 0xFF}, // top left
		{R: 0x00, G: 0xFF, B: 0x00, A: 0xFF}, // top right
		{R: 0x00, G: 0x00, B: 0xFF, A: 0xFF}, // bottom left
		{R: 0xFF, G: 0xFF, B: 0x00, A: 0xFF}, // bottom right
	}
}

// Indices into corners(), used as names in the expectation table.
const (
	tl = 0
	tr = 1
	bl = 2
	br = 3
)

// cornered returns a w by h image whose four corners carry corners() and
// whose middle is grey, so a transform that mirrors or rotates the wrong
// way moves a known color to a place the table does not expect.
func cornered(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xFF})
		}
	}
	c := corners()
	img.Set(0, 0, c[tl])
	img.Set(w-1, 0, c[tr])
	img.Set(0, h-1, c[bl])
	img.Set(w-1, h-1, c[br])
	return img
}

func sameColor(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br2, bg, bb, ba := b.RGBA()
	return ar == br2 && ag == bg && ab == bb && aa == ba
}

// TestSanitizeMovesThePixelsOfEveryOrientation is the pixel level check
// the dimension check cannot make: breaking one case of orientedPoint,
// say swapping the mirror of 2 for the rotation of 3, leaves every
// dimension assertion green because both keep the same width and height.
//
// The expectation table says which source corner has to end up at each
// of the four destination corners. The top-left column is the standard
// EXIF table ("which corner of the stored raster is the top left of the
// displayed image"): 1 and 5 top left, 2 and 8 top right, 4 and 6 bottom
// left, 3 and 7 bottom right. The other three columns follow from the
// same mapping.
//
// It runs on the PNG path on purpose: PNG is lossless, so the comparison
// is exact. The JPEG path shares applyOrientation, so the mapping is the
// same code; only the comparison would be approximate there.
func TestSanitizeMovesThePixelsOfEveryOrientation(t *testing.T) {
	t.Parallel()

	const (
		w = 9
		h = 5
	)
	tests := []struct {
		orientation int
		// want is the source corner expected at destination
		// (0,0), (W-1,0), (0,H-1) and (W-1,H-1).
		want [4]int
	}{
		{1, [4]int{tl, tr, bl, br}},
		{2, [4]int{tr, tl, br, bl}},
		{3, [4]int{br, bl, tr, tl}},
		{4, [4]int{bl, br, tl, tr}},
		{5, [4]int{tl, bl, tr, br}},
		{6, [4]int{bl, tl, br, tr}},
		{7, [4]int{br, tr, bl, tl}},
		{8, [4]int{tr, br, tl, bl}},
	}
	for _, tt := range tests {
		t.Run(string(rune('0'+tt.orientation)), func(t *testing.T) {
			t.Parallel()

			exif := tiffBlock(map[uint16]uint16{0x0112: uint16(tt.orientation)})
			in := withPNGeXIf(t, encodePNG(t, cornered(w, h)), exif)

			got, err := imaging.Sanitize(context.Background(), in, imaging.Limits{})
			if err != nil {
				t.Fatalf("Sanitize() = %v", err)
			}
			out, _, err := image.Decode(bytes.NewReader(got.Data))
			if err != nil {
				t.Fatalf("the output does not decode: %v", err)
			}
			b := out.Bounds()
			if b.Dx() != got.Width || b.Dy() != got.Height {
				t.Fatalf("Result says %dx%d, the image is %dx%d", got.Width, got.Height, b.Dx(), b.Dy())
			}

			c := corners()
			at := [4]image.Point{
				{X: b.Min.X, Y: b.Min.Y},
				{X: b.Max.X - 1, Y: b.Min.Y},
				{X: b.Min.X, Y: b.Max.Y - 1},
				{X: b.Max.X - 1, Y: b.Max.Y - 1},
			}
			names := [4]string{"top left", "top right", "bottom left", "bottom right"}
			for i, p := range at {
				if !sameColor(out.At(p.X, p.Y), c[tt.want[i]]) {
					r, g, bb, _ := out.At(p.X, p.Y).RGBA()
					wr, wg, wb, _ := c[tt.want[i]].RGBA()
					t.Errorf("orientation %d: the %s pixel is (%d,%d,%d), want source corner %s (%d,%d,%d)",
						tt.orientation, names[i], r>>8, g>>8, bb>>8, names[tt.want[i]], wr>>8, wg>>8, wb>>8)
				}
			}
		})
	}
}

// noisy builds an image whose pixels do not compress: a JPEG of it at a
// low quality is small and the same image at a high quality is several
// times larger, which is what makes the output bound reachable.
func noisy(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// A fixed linear congruential sequence, so the fixture is the same
	// on every run and in every order.
	state := uint32(1)
	next := func() uint8 {
		state = state*1664525 + 1013904223
		return uint8(state >> 24)
	}
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: next(), G: next(), B: next(), A: 0xFF})
		}
	}
	return img
}

// TestSanitizeBoundsItsOutput covers the gate on the re-encode's result
// and not only on the input. Without it the declared limit would hold
// for what a client sends and not for what the server stores, so the
// server could write an object that the same limit later refuses to
// read.
//
// The case is built the way it happens in practice: a photograph saved
// at a low JPEG quality is small, and re-encoding it at the quality this
// package uses makes it bigger. The input fits the bound, the output
// does not.
func TestSanitizeBoundsItsOutput(t *testing.T) {
	t.Parallel()

	var low bytes.Buffer
	if err := jpeg.Encode(&low, noisy(160, 160), &jpeg.Options{Quality: 1}); err != nil {
		t.Fatalf("jpeg.Encode() = %v", err)
	}
	in := low.Bytes()

	// Generous limits: the re-encode succeeds and grows.
	wide := imaging.Limits{MaxBytes: imaging.DefaultMaxBytes, MaxPixels: imaging.DefaultMaxPixels, JPEGQuality: 95}
	grown, err := imaging.Sanitize(context.Background(), in, wide)
	if err != nil {
		t.Fatalf("Sanitize() = %v", err)
	}
	if len(grown.Data) <= len(in) {
		t.Skipf("the fixture does not set up the case: %d bytes in, %d out", len(in), len(grown.Data))
	}

	// A bound that admits the input and refuses the output.
	narrow := imaging.Limits{MaxBytes: int64(len(in)) + 1, MaxPixels: imaging.DefaultMaxPixels, JPEGQuality: 95}
	if _, err := imaging.Sanitize(context.Background(), in, narrow); !errors.Is(err, imaging.ErrTooLarge) {
		t.Fatalf("Sanitize() = %v, want ErrTooLarge: %d bytes in, %d out, bound %d",
			err, len(in), len(grown.Data), narrow.MaxBytes)
	}

	// The bound is on the output as well, so anything Sanitize returns
	// fits it. FuzzSanitize checks the same invariant generatively.
	if int64(len(grown.Data)) > wide.MaxBytes {
		t.Errorf("the output is %d bytes, over the bound %d", len(grown.Data), wide.MaxBytes)
	}
}
