package imaging_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/imaging"
)

// The EXIF and PNG chunk walkers in exif.go index into attacker supplied
// bytes by hand: a JPEG segment length, a PNG chunk length, a TIFF IFD
// offset and an entry count all come out of the file and are used as
// indices. Bounds checks are written for each of them and the table
// tests cover the cases those checks were written for; these targets
// cover the cases nobody thought of, which is the whole point. A panic
// here is a denial of service on an endpoint whose input comes straight
// from a user.
//
// Seeds are F.Add, so the gate's verify job runs every one of them with
// plain `go test`; `go test -fuzz=FuzzOrientation ./internal/platform/imaging`
// starts from them and mutates.

// pngIHDRChecksum recomputes the CRC32 of a PNG's IHDR chunk. image/png
// verifies it before it reads the dimensions, so a fixture that edits
// IHDR has to repair it or it never reaches the pixel bound.
func pngIHDRChecksum(data []byte) uint32 {
	const (
		typeStart = 8 + 4   // signature, then the length field
		crcStart  = 12 + 17 // chunk type (4) plus IHDR data (13)
	)
	return crc32.ChecksumIEEE(data[typeStart:crcStart])
}

// malformed are byte sequences aimed at the middle of each walker's
// arithmetic: a length that runs past the end, a length too small to
// contain itself, a length of zero that could stop the walk advancing,
// an IFD offset inside its own header or past the end, an entry count
// far larger than the data, and the markers that carry no length at all.
func malformed() [][]byte {
	withSOI := func(rest ...byte) []byte {
		return append([]byte{0xFF, 0xD8}, rest...)
	}
	withPNGSignature := func(rest ...byte) []byte {
		return append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, rest...)
	}
	// withEXIF wraps a TIFF block in the APP1 segment a camera writes.
	withEXIF := func(block ...byte) []byte {
		header := []byte{0xFF, 0xE1, 0x00, byte(len(block) + 8), 'E', 'x', 'i', 'f', 0x00, 0x00}
		return withSOI(append(header, block...)...)
	}
	// zeroChunks is a chain of zero length PNG chunks: if the walk does
	// not advance past a zero length chunk it never terminates.
	zeroChunks := bytes.Repeat([]byte{0x00, 0x00, 0x00, 0x00, 'a', 'b', 'c', 'd', 0x00, 0x00, 0x00, 0x00}, 60)

	return [][]byte{
		nil,
		{},
		{0xFF},
		{0xFF, 0xD8},
		{0xFF, 0xD8, 0xFF},
		{0xFF, 0xD8, 0xFF, 0xE1},
		// APP1 lengths: past the end, smaller than the length field,
		// exactly the Exif header and nothing more.
		withSOI(0xFF, 0xE1, 0xFF, 0xFF),
		withSOI(0xFF, 0xE1, 0x00, 0x00),
		withSOI(0xFF, 0xE1, 0x00, 0x01),
		withSOI(0xFF, 0xE1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0x00, 0x00),
		// An APP1 that is not EXIF at all, and one whose header is cut.
		withSOI(0xFF, 0xE1, 0x00, 0x08, 'J', 'F', 'I', 'F', 0x00, 0x01),
		withSOI(0xFF, 0xE1, 0x00, 0x05, 'E', 'x', 'i'),
		// Fill bytes, then nothing. Then start of scan straight away:
		// the walk must stop there instead of reading entropy coded
		// data as segment lengths.
		withSOI(0xFF, 0xFF, 0xFF, 0xFF),
		withSOI(0xFF, 0xDA, 0x00, 0x02, 0xFF, 0xE1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0x00, 0x00),
		// Markers that carry no length: restart, TEM, a second SOI.
		withSOI(0xFF, 0xD0, 0xFF, 0xD7, 0xFF, 0x01, 0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x02),
		// TIFF headers: byte order only, no magic, wrong magic, an
		// unknown byte order.
		withEXIF('I', 'I'),
		withEXIF('I', 'I', 0x2A, 0x00),
		withEXIF('M', 'M', 0x00, 0x2A),
		withEXIF('X', 'X', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00),
		withEXIF('I', 'I', 0x00, 0x00, 0x08, 0x00, 0x00, 0x00),
		// IFD offsets: zero, below the eight byte header, past the end.
		withEXIF('I', 'I', 0x2A, 0x00, 0x00, 0x00, 0x00, 0x00),
		withEXIF('I', 'I', 0x2A, 0x00, 0x07, 0x00, 0x00, 0x00),
		withEXIF('I', 'I', 0x2A, 0x00, 0xFF, 0xFF, 0xFF, 0xFF),
		withEXIF('I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00),
		// An entry count far larger than the entries that follow, and
		// one entry truncated part way through.
		withEXIF('I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0xFF, 0xFF),
		withEXIF('I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00, 0x12, 0x01),
		// The orientation tag with a count other than one, with a type
		// the specification does not use for it, and out of range.
		withEXIF('I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00,
			0x12, 0x01, 0x03, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0x06, 0x00, 0x00, 0x00),
		withEXIF('I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00,
			0x12, 0x01, 0x09, 0x00, 0x01, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00),
		withEXIF('I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00,
			0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00),
		// A LONG typed orientation, which is legal and takes the other
		// branch of the reader.
		withEXIF('I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00,
			0x12, 0x01, 0x04, 0x00, 0x01, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00),
		// PNG chunk walking: nothing after the signature, a length past
		// the end, the largest length the specification allows, a zero
		// length eXIf chunk, IDAT and IEND first, and a long chain of
		// zero length chunks.
		withPNGSignature(),
		withPNGSignature(0x00, 0x00, 0x00, 0x00),
		withPNGSignature(0xFF, 0xFF, 0xFF, 0xFF, 'e', 'X', 'I', 'f', 0x00, 0x00, 0x00, 0x00),
		withPNGSignature(0x7F, 0xFF, 0xFF, 0xFF, 'e', 'X', 'I', 'f', 0x00, 0x00, 0x00, 0x00),
		withPNGSignature(0x00, 0x00, 0x00, 0x00, 'e', 'X', 'I', 'f', 0x00, 0x00, 0x00, 0x00),
		withPNGSignature(0x00, 0x00, 0x00, 0x00, 'I', 'D', 'A', 'T', 0x00, 0x00, 0x00, 0x00),
		withPNGSignature(0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0x00, 0x00, 0x00, 0x00),
		withPNGSignature(zeroChunks...),
		// An eXIf chunk whose payload is a TIFF block with no Exif
		// header, which the PNG reader also has to accept.
		withPNGSignature(0x00, 0x00, 0x00, 0x08, 'e', 'X', 'I', 'f',
			'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00),
		// Other formats, and data that is not a format at all.
		[]byte("%PDF-1.7\n1 0 obj"),
		[]byte("GIF89a\x01\x00"),
		[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`),
		[]byte(strings.Repeat("A", 1000)),
		bytes.Repeat([]byte{0xFF}, 500),
	}
}

// addSeeds adds the malformed sequences and real images, with and
// without metadata, so a target exercises the accepting path too and not
// only the rejecting one.
func addSeeds(f *testing.F) {
	f.Helper()
	for _, seed := range malformed() {
		f.Add(seed)
	}
	block := tiffBlock(map[uint16]uint16{0x0112: 6, 0x8825: 0x1234})
	f.Add(encodeJPEG(f, gradient(6, 4)))
	f.Add(encodePNG(f, gradient(6, 4)))
	f.Add(withJPEGAPP1(f, encodeJPEG(f, gradient(6, 4)), block))
	f.Add(withPNGeXIf(f, encodePNG(f, gradient(6, 4)), block))
	// A PNG whose IHDR lies about its canvas, with the CRC repaired, so
	// the pixel bound is the only thing between it and a huge raster.
	bomb := bytes.Clone(encodePNG(f, gradient(4, 4)))
	binary.BigEndian.PutUint32(bomb[16:20], 40_000)
	binary.BigEndian.PutUint32(bomb[20:24], 40_000)
	binary.BigEndian.PutUint32(bomb[29:33], pngIHDRChecksum(bomb))
	f.Add(bomb)
}

// orientationTypes are the types Orientation is asked about: the three
// it knows, plus one outside the allowlist and the empty one.
func orientationTypes() []imaging.Type {
	return append(slices.Clone(imaging.Types()), imaging.Type("image/webp"), imaging.Type(""))
}

// FuzzOrientation drives the JPEG segment walk, the PNG chunk walk and
// the TIFF IFD reader. It checks the two things the reader promises for
// any input at all: it does not panic, and it answers inside the EXIF
// range. A value outside 1 to 8 would reach applyOrientation, where it
// would index outside the destination raster.
func FuzzOrientation(f *testing.F) {
	addSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, typ := range orientationTypes() {
			got := imaging.Orientation(data, typ)
			if got < imaging.OrientationNone || got > 8 {
				t.Fatalf("Orientation(%d bytes, %q) = %d, outside the EXIF range", len(data), typ, got)
			}
			if !typ.Image() && got != imaging.OrientationNone {
				t.Fatalf("Orientation() of a %q = %d; only images carry the tag", typ, got)
			}
			// The reader is pure: asking twice gives the same answer, so
			// nothing in the walk mutates the caller's bytes.
			if again := imaging.Orientation(data, typ); again != got {
				t.Fatalf("Orientation() = %d then %d for the same input", got, again)
			}
		}
	})
}

// FuzzDetect checks that the signature table answers only with a type
// from the allowlist, and never panics on a short or empty head.
func FuzzDetect(f *testing.F) {
	addSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		typ, ok := imaging.Detect(data)
		if !ok {
			if typ != "" {
				t.Fatalf("Detect() refused but returned %q", typ)
			}
			return
		}
		if !slices.Contains(imaging.Types(), typ) {
			t.Fatalf("Detect() = %q, which is not in the allowlist", typ)
		}
		// Detect reads a prefix only, so appending to a valid header
		// cannot change the type: a payload hidden after the header is
		// still the format the header says.
		longer, longerOK := imaging.Detect(append(slices.Clone(data), 0x00, 0x01, 0x02))
		if !longerOK || longer != typ {
			t.Fatalf("Detect() = %q, but %q with three bytes appended", typ, longer)
		}
	})
}

// FuzzSanitize is the strip pipeline end to end. It does not assert that
// any particular input is accepted -- almost none is -- but that an
// accepted one comes out as an image of the same format, within the
// bounds it was given, with no metadata and no orientation tag left.
func FuzzSanitize(f *testing.F) {
	addSeeds(f)

	limits := imaging.Limits{MaxBytes: 1 << 20, MaxPixels: 1 << 16, JPEGQuality: 80}

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := imaging.Sanitize(context.Background(), data, limits)
		if err != nil {
			if got.Data != nil {
				t.Fatalf("Sanitize() failed but returned %d bytes", len(got.Data))
			}
			return
		}
		if len(got.Data) == 0 {
			t.Fatal("Sanitize() succeeded with no output")
		}
		// The byte bound holds for the output and not only the input:
		// the re-encode is not guaranteed to shrink, and an object over
		// the limit would be one the same limit later refuses to read.
		if int64(len(got.Data)) > limits.MaxBytes {
			t.Fatalf("the output is %d bytes, over the bound %d", len(got.Data), limits.MaxBytes)
		}
		if !got.Type.Image() {
			t.Fatalf("Type = %q, which Sanitize cannot produce", got.Type)
		}
		if got.Width <= 0 || got.Height <= 0 {
			t.Fatalf("size = %dx%d", got.Width, got.Height)
		}
		if int64(got.Width)*int64(got.Height) > limits.MaxPixels {
			t.Fatalf("%dx%d is over the pixel bound %d", got.Width, got.Height, limits.MaxPixels)
		}
		if got.Orientation < imaging.OrientationNone || got.Orientation > 8 {
			t.Fatalf("Orientation = %d, outside the EXIF range", got.Orientation)
		}
		// The output is the same format and carries no orientation tag:
		// the strip is complete, and running it again changes nothing,
		// so it is idempotent.
		//
		// The check is structural, not a substring search for "Exif" and
		// the PNG text chunk names. On a fixed fixture that search is
		// exact, and TestSanitizeDropsEveryMetadataSegment does it
		// there; here the output's compressed image data is arbitrary
		// bytes, so a four byte sequence could appear in it by chance
		// and fail the gate for no reason. A flaky gate test is worse
		// than one fewer assertion, and reading the tag back is the
		// assertion that matters.
		typ, ok := imaging.Detect(got.Data)
		if !ok || typ != got.Type {
			t.Fatalf("Detect(output) = %q, %v; want %q", typ, ok, got.Type)
		}
		if orientation := imaging.Orientation(got.Data, got.Type); orientation != imaging.OrientationNone {
			t.Fatalf("the output still carries orientation %d", orientation)
		}
		again, err := imaging.Sanitize(context.Background(), got.Data, limits)
		if err != nil {
			t.Fatalf("Sanitize() of its own output = %v", err)
		}
		if again.Width != got.Width || again.Height != got.Height {
			t.Fatalf("a second pass turned %dx%d into %dx%d", got.Width, got.Height, again.Width, again.Height)
		}
		if again.Orientation != imaging.OrientationNone {
			t.Fatalf("a second pass found orientation %d", again.Orientation)
		}
	})
}
