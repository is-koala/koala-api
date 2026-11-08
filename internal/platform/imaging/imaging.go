// Package imaging detects the type of an uploaded file from its bytes and
// strips every piece of metadata from an image by re-encoding it
// (ADR-0029 Karar 5, ADR-0030 Karar 3 and 4).
//
// It is the only place in the repository that does either. The Java
// backend wrote the work twice — ImageMetadataStripper.strip and an
// inlined copy in IdentityVerificationManager — and the copy is what
// koala-backend issue #362 is about: a fix to one did not reach the
// other. Every Koala upload path calls Detect and Sanitize here, so
// there is one implementation to review and one to fix.
//
// Two rules hold throughout:
//
//   - The type comes from the content. The name the client sent and the
//     Content-Type header it declared are evidence at most, never the
//     decision (ADR-0029 Karar 5, a deliberate departure from Java,
//     which took the allowlist from the file name's extension).
//   - An image carries nothing but its pixel raster out of here. The
//     orientation tag is read first and baked into the pixels, then it
//     is dropped with everything else, so the photograph is stored
//     upright and no EXIF, XMP, IPTC or PNG text chunk survives — GPS
//     latitude and longitude included.
package imaging

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// Type is a content type imaging understands. The set is the allowlist:
// a file whose bytes match none of these is refused, and there is no
// "unknown, pass it through" branch.
type Type string

// The accepted types (ADR-0029 Karar 5 and the CV upload; the two Word
// types ADR-0031 Karar 2).
const (
	TypeJPEG Type = "image/jpeg"
	TypePNG  Type = "image/png"
	TypePDF  Type = "application/pdf"
	TypeDOC  Type = "application/msword"
	TypeDOCX Type = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// Types lists every accepted type.
func Types() []Type { return []Type{TypeJPEG, TypePNG, TypePDF, TypeDOC, TypeDOCX} }

// Image reports whether the type is a raster image, which is what
// Sanitize can re-encode. PDF and the two Word types are not: see
// Sanitize.
func (t Type) Image() bool { return t == TypeJPEG || t == TypePNG }

// Container reports whether the type is carried by a container file --
// an OLE compound file or a ZIP archive -- whose structure, and not its
// leading bytes, decides what it is (ADR-0031 Karar 2). Detect needs the
// whole object for such a type, never a head.
//
// The answer is read from the signature table, so a type whose second
// stage is added or removed cannot disagree with this method.
func (t Type) Container() bool {
	for _, m := range signatures() {
		if m.typ == t {
			return m.verify != nil
		}
	}
	return false
}

// Errors of this package.
var (
	// ErrUnsupportedType means the bytes match no accepted type. The file
	// name and the declared header are not consulted, so this is a
	// statement about the content.
	ErrUnsupportedType = errors.New("imaging: content type not accepted")
	// ErrTooLarge means the input is over the byte bound.
	ErrTooLarge = errors.New("imaging: file over the size limit")
	// ErrCanvasTooLarge means width times height is over the pixel bound:
	// a decompression bomb, where a small file expands into a raster that
	// does not fit in memory.
	ErrCanvasTooLarge = errors.New("imaging: image over the pixel limit")
	// ErrCorrupt means the bytes carry an accepted magic number but do
	// not decode.
	ErrCorrupt = errors.New("imaging: image does not decode")
)

// magic is one entry of the signature table.
type magic struct {
	typ    Type
	prefix []byte
	// at is the offset the prefix sits at.
	at int
	// verify is the second stage, for a signature that several formats
	// share: the prefix says which container, this says what is inside
	// it. nil means the prefix is the whole decision, which is the case
	// for JPEG, PNG and PDF.
	verify func([]byte) bool
}

// signatures is the magic byte table, in the order it is tried. It is a
// hand written table and not net/http.DetectContentType on purpose
// (ADR-0030 Karar 3): DetectContentType answers with any of about thirty
// types, among them text/html and image/svg+xml, so a caller would have
// to turn its answer into an allowlist anyway, and a type Koala does not
// accept would reach that conversion as a plausible looking value.
//
//   - JPEG: SOI (FFD8) followed by any marker start (FF). JFIF and Exif
//     variants differ after that, so only three bytes are fixed.
//   - PNG: the eight byte signature of the PNG specification.
//   - PDF: "%PDF-" at offset 0. The specification tolerates it within the
//     first 1024 bytes, which is how a polyglot hides a PDF behind
//     another format's header; Koala requires offset 0 (ADR-0030 Karar 3).
//   - DOCX: the local file header of a ZIP archive. Every OOXML file,
//     every ODF file, every jar and every apk starts with these four
//     bytes, so the prefix decides nothing on its own and a second stage
//     reads the archive's own table of contents.
//   - DOC: the eight byte signature of an OLE compound file, shared with
//     xls, ppt and msi for the same reason, with the same consequence.
func signatures() []magic {
	return []magic{
		{typ: TypeJPEG, prefix: []byte{0xFF, 0xD8, 0xFF}},
		{typ: TypePNG, prefix: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}},
		{typ: TypePDF, prefix: []byte("%PDF-")},
		{typ: TypeDOCX, prefix: []byte("PK\x03\x04"), verify: isWordprocessingML},
		{typ: TypeDOC, prefix: []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, verify: isWordBinary},
	}
}

// wordMainPart is the part every WordprocessingML document carries: the
// main document body. A spreadsheet carries xl/workbook.xml instead and a
// presentation ppt/presentation.xml, so this one name is what separates
// a .docx from the other members of the family.
const wordMainPart = "word/document.xml"

// isWordprocessingML reports whether the ZIP archive in b is a
// WordprocessingML document, by reading its central directory and
// looking for the main document part.
//
// Nothing is decompressed. Only the archive's table of contents is
// parsed, so a zip bomb is never inflated: its compressed entries are
// never opened. The work and the memory are bounded by len(b), which the
// caller has already bounded (media.Kind.Limits, 10 MiB) -- archive/zip
// refuses a record count larger than the file could hold, so the
// directory it builds is at most one entry per 46 bytes of input.
//
// An archive whose directory does not parse is refused, not passed
// through: this is an allowlist, and "it is some kind of zip" is not a
// membership proof.
func isWordprocessingML(b []byte) bool {
	r, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return false
	}
	for _, f := range r.File {
		if f.Name == wordMainPart {
			return true
		}
	}
	return false
}

// wordBinaryStream is the name of the stream every Word 97-2003 document
// carries, as it is spelled in an OLE directory entry: UTF-16, little
// endian, no terminator.
func wordBinaryStream() []byte {
	name := "WordDocument"
	out := make([]byte, 0, 2*len(name))
	for _, c := range []byte(name) {
		out = append(out, c, 0x00)
	}
	return out
}

// isWordBinary reports whether the OLE compound file in b is a Word
// 97-2003 document, by looking for the name of its main stream.
//
// This is a bounded search over bytes already in memory and not a parse
// of the compound file: the directory of a compound file is a chain of
// sectors that may sit anywhere in the file, so reaching it means
// following the FAT, and writing that walk would be a new parser over
// attacker supplied offsets for no gain here. Searching for the name
// costs one pass and cannot run off the end of anything.
//
// It is therefore a signal and not a proof, and the direction of its
// error matters: it accepts an xls or a ppt that **embeds** a Word
// object, because such a file does carry a WordDocument stream. The
// consequence is bounded -- the object is stored as an attachment, is
// never rendered by Koala and resolves to no public address -- and
// recorded in ADR-0031 Karar 2. It does not accept a file that carries
// no Word stream at all, which is what keeps the CV slot from becoming
// a general file host.
func isWordBinary(b []byte) bool { return bytes.Contains(b, wordBinaryStream()) }

// Detect returns the type the bytes are, and false when they are none of
// the accepted ones.
//
// It wants the whole object. For JPEG, PNG and PDF the signature is the
// whole decision and a head would do, but a container type is decided by
// its structure: a DOCX's table of contents sits at the **end** of the
// file, so a head of one is not a DOCX and is answered as such. There is
// no way to get the first stage's answer on its own, on purpose -- an
// "it is a zip" that escaped into a caller would be a type decision
// nobody verified (ADR-0031 Karar 2).
//
// A prefix that matches while the second stage refuses is refused
// outright. No other signature can match the same first bytes, so there
// is nothing further to try, and falling through to "unknown" is the
// fail-closed answer.
func Detect(b []byte) (Type, bool) {
	for _, m := range signatures() {
		end := m.at + len(m.prefix)
		if len(b) < end || !bytes.Equal(b[m.at:end], m.prefix) {
			continue
		}
		if m.verify != nil && !m.verify(b) {
			return "", false
		}
		return m.typ, true
	}
	return "", false
}

// Limits bound the work Sanitize will do. Zero fields fall back to the
// defaults below, so a caller that forgets one gets the safe value and
// not "no limit".
type Limits struct {
	// MaxBytes is the largest input accepted, in bytes.
	MaxBytes int64
	// MaxPixels is the largest canvas accepted: width times height. It is
	// checked from the header, before the raster is allocated.
	MaxPixels int64
	// JPEGQuality is the quality the re-encode writes with, 1 to 100.
	JPEGQuality int
}

// Defaults of Limits (ADR-0029 Karar 5 for the first two, which are
// Java's values; ADR-0030 Karar 5 for the quality).
const (
	// DefaultMaxBytes is 10 MiB per file.
	DefaultMaxBytes int64 = 10 << 20
	// DefaultMaxPixels is 50 megapixels of canvas.
	DefaultMaxPixels int64 = 50_000_000
	// DefaultJPEGQuality is the quality of the re-encode. Re-encoding is
	// lossy and there is no way around it: stripping metadata by keeping
	// the original entropy coded data would mean writing a JPEG parser,
	// and the stripping has to be exact. 85 is the usual choice for a
	// one-off transcode; Go's own default is 75, which is visibly worse
	// on a photograph of an identity document.
	DefaultJPEGQuality = 85
)

// withDefaults fills the zero fields.
func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	if l.MaxPixels <= 0 {
		l.MaxPixels = DefaultMaxPixels
	}
	if l.JPEGQuality <= 0 || l.JPEGQuality > 100 {
		l.JPEGQuality = DefaultJPEGQuality
	}
	return l
}

// Result is what Sanitize produced.
type Result struct {
	// Data is the re-encoded image, metadata free.
	Data []byte
	// Type is the type detected in the input, which is also the type of
	// Data: the re-encode keeps the format.
	Type Type
	// Width and Height are the dimensions after the orientation was
	// applied, so they are the dimensions a viewer sees.
	Width, Height int
	// Orientation is the EXIF orientation tag that was found and baked
	// into the pixels, 1 to 8. 1 means there was nothing to apply.
	Orientation int
}

// Sanitize detects the type of data, checks it against limits and
// returns the image with every piece of metadata removed.
//
// The order matters and each step is a gate for the next:
//
//  1. the byte bound, before anything is parsed;
//  2. the type, from the content;
//  3. the canvas bound, from the image header only, so a decompression
//     bomb is refused before its raster is allocated;
//  4. the orientation tag, read from the original bytes;
//  5. the full decode;
//  6. the orientation applied to the pixels;
//  7. the re-encode, which is what actually drops the metadata.
//
// Sanitize refuses a PDF and the two Word types with
// ErrUnsupportedType. None of them can be re-encoded without a library
// for its format, and there is no approved one (ADR-0003); the caller
// moves such an object as it is and relies on how it is served instead
// (ADR-0030 Karar 4, widened to the Word types by ADR-0031 Karar 2).
//
// The CPU work is bounded by MaxBytes and MaxPixels rather than by a
// timer, because a decode cannot be interrupted part way. ctx is checked
// between the steps, so a canceled job stops at the next boundary.
//
// Memory is bounded by MaxPixels, not by MaxBytes, and the bound is per
// call: the decoded raster is up to MaxPixels times four bytes, and an
// orientation other than 1 allocates a second raster of the same size.
// At the default 50 MP that is about 400 MB at peak for one upload, and
// nothing here limits how many run at once -- that is the caller's job
// (ADR-0030 Karar 5).
func Sanitize(ctx context.Context, data []byte, limits Limits) (Result, error) {
	limits = limits.withDefaults()

	if int64(len(data)) > limits.MaxBytes {
		return Result{}, fmt.Errorf("%w: %d bytes, limit is %d", ErrTooLarge, len(data), limits.MaxBytes)
	}
	typ, ok := Detect(data)
	if !ok {
		return Result{}, ErrUnsupportedType
	}
	if !typ.Image() {
		return Result{}, fmt.Errorf("%w: %s cannot be re-encoded", ErrUnsupportedType, typ)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("imaging: %w", err)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("%w: header: %w", ErrCorrupt, err)
	}
	if err := checkFormat(typ, format); err != nil {
		return Result{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Result{}, fmt.Errorf("%w: empty canvas", ErrCorrupt)
	}
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > limits.MaxPixels {
		return Result{}, fmt.Errorf("%w: %d pixels, limit is %d", ErrCanvasTooLarge, pixels, limits.MaxPixels)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("imaging: %w", err)
	}

	orientation := Orientation(data, typ)

	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("imaging: %w", err)
	}

	upright := applyOrientation(decoded, orientation)
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("imaging: %w", err)
	}

	out, err := encode(typ, upright, limits.JPEGQuality)
	if err != nil {
		return Result{}, err
	}
	// The output is bounded too, not only the input. A re-encode is not
	// guaranteed to shrink: a JPEG written at a low quality comes back
	// larger at quality 85, and a paletted PNG whose orientation tag is
	// not 1 is turned into an RGBA raster, which loses the palette and
	// grows. Without this gate the declared 10 MiB policy would not
	// hold, the size recorded for the media would be over its own
	// limit, and later code reading that object with the same bound
	// would get ErrTooLarge on an object the server itself wrote --
	// correct code invalidated by a mechanism added next to it.
	if int64(len(out)) > limits.MaxBytes {
		return Result{}, fmt.Errorf("%w: re-encoded to %d bytes, limit is %d", ErrTooLarge, len(out), limits.MaxBytes)
	}
	bounds := upright.Bounds()
	return Result{
		Data:        out,
		Type:        typ,
		Width:       bounds.Dx(),
		Height:      bounds.Dy(),
		Orientation: orientation,
	}, nil
}

// checkFormat refuses an image whose magic number and the decoder that
// claimed it disagree: a file Detect called a JPEG and the image
// registry decoded as something else would otherwise be re-encoded into
// the second format under the first format's extension.
//
// With today's two tables this cannot happen, and saying so is part of
// the check: Detect's JPEG prefix (FF D8 FF) is stricter than the two
// bytes image/jpeg registers, and the PNG signatures are identical, so
// every file Detect accepts sniffs as the same format. It is a guard
// against the tables drifting apart, not a live path. The polyglot that
// is a live case -- a header claiming a format the body is not -- is
// refused by the decode itself, as ErrCorrupt.
func checkFormat(typ Type, format string) error {
	want := typ.decoderName()
	if want == "" || format != want {
		return fmt.Errorf("%w: magic says %s, decoder says %q", ErrUnsupportedType, typ, format)
	}
	return nil
}

// decoderName is the name image.DecodeConfig reports for the type, or ""
// for a type the image registry has no decoder for.
func (t Type) decoderName() string {
	if t == TypeJPEG {
		return "jpeg"
	}
	if t == TypePNG {
		return "png"
	}
	return ""
}

// encode writes the image back out. The encoders are the standard
// library's, which write no metadata at all: that is the whole mechanism
// by which EXIF, XMP, IPTC and PNG text chunks disappear.
func encode(typ Type, img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if typ == TypeJPEG {
		// JPEG has no alpha channel. Go's encoder would turn a
		// transparent pixel black; compositing onto white keeps what a
		// viewer saw, which is Java's behavior and is kept.
		if err := jpeg.Encode(&buf, overWhite(img), &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("imaging: encode jpeg: %w", err)
		}
		return buf.Bytes(), nil
	}
	if typ == TypePNG {
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("imaging: encode png: %w", err)
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("%w: %s cannot be re-encoded", ErrUnsupportedType, typ)
}

// overWhite flattens an image with an alpha channel onto white. An image
// without one is returned unchanged, so the common path copies nothing.
func overWhite(img image.Image) image.Image {
	if opaque, ok := img.(interface{ Opaque() bool }); ok && opaque.Opaque() {
		return img
	}
	bounds := img.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, bounds.Min, draw.Over)
	return flat
}
