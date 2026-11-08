package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
)

// Orientation returns the EXIF orientation tag (TIFF 0x0112) carried by
// data, or 1 when there is none, when it is unreadable or when it is out
// of range.
//
// It is the one tag Koala reads before stripping, because it is not
// metadata about the photograph but part of how the photograph is meant
// to be displayed: a phone writes the sensor's raster and says "turn
// this". Dropping it without applying it stores every portrait photo
// sideways, which is why keeping it and baking it in is the industry
// practice (ADR-0029 Karar 5, ADR-0030 Karar 4). Nothing else is read
// and nothing at all is written back.
//
// Both image formats are handled, which is a correction of Java: Java
// read the tag from JPEG only, so a PNG with an eXIf chunk lost its
// orientation when the chunk was dropped.
func Orientation(data []byte, typ Type) int {
	switch {
	case typ == TypeJPEG:
		return tiffOrientation(jpegExifPayload(data))
	case typ == TypePNG:
		return tiffOrientation(pngExifChunk(data))
	default:
		return OrientationNone
	}
}

// OrientationNone is the orientation of an image that needs no turning:
// EXIF value 1, which is also what a missing tag means.
const OrientationNone = 1

// maxOrientation is the largest EXIF orientation value (8).
const maxOrientation = 8

// exifHeader prefixes the TIFF block inside a JPEG APP1 segment.
var exifHeader = []byte("Exif\x00\x00") //nolint:gochecknoglobals // read-only constant bytes

// jpegExifPayload walks the JPEG marker segments and returns the TIFF
// block of the first APP1 segment that carries an EXIF header, or nil.
//
// The walk is bounded by the input and stops at the start of scan: the
// entropy coded data after SOS is not a marker stream, and continuing
// into it would read image content as segment lengths.
func jpegExifPayload(data []byte) []byte {
	const (
		markerPrefix = 0xFF
		soi          = 0xD8
		eoi          = 0xD9
		sos          = 0xDA
		app1         = 0xE1
		tem          = 0x01
		rst0         = 0xD0
		rst7         = 0xD7
		lengthBytes  = 2
	)
	if len(data) < 4 || data[0] != markerPrefix || data[1] != soi {
		return nil
	}
	i := 2
	for i+lengthBytes <= len(data) {
		if data[i] != markerPrefix {
			i++
			continue
		}
		// Fill bytes: any number of 0xFF may precede a marker.
		for i < len(data) && data[i] == markerPrefix {
			i++
		}
		if i >= len(data) {
			return nil
		}
		marker := data[i]
		i++
		// Standalone markers carry no length.
		if marker == soi || marker == eoi || marker == tem || (marker >= rst0 && marker <= rst7) {
			continue
		}
		if i+lengthBytes > len(data) {
			return nil
		}
		length := int(binary.BigEndian.Uint16(data[i : i+lengthBytes]))
		if length < lengthBytes || i+length > len(data) {
			return nil
		}
		if marker == app1 {
			payload := data[i+lengthBytes : i+length]
			if after, ok := bytes.CutPrefix(payload, exifHeader); ok {
				return after
			}
		}
		if marker == sos {
			return nil
		}
		i += length
	}
	return nil
}

// pngExifChunk returns the contents of the first eXIf chunk, or nil. The
// chunk was added to the PNG specification in 2017 and holds the same
// TIFF block a JPEG APP1 segment does, with or without the Exif header.
func pngExifChunk(data []byte) []byte {
	const (
		signatureLen = 8
		lengthLen    = 4
		typeLen      = 4
		crcLen       = 4
		// maxChunk bounds a chunk length; the PNG specification allows up
		// to 2^31-1, which would overflow an int on a 32 bit build.
		maxChunk = 1 << 24
	)
	if len(data) < signatureLen {
		return nil
	}
	i := signatureLen
	for i+lengthLen+typeLen+crcLen <= len(data) {
		length := int(binary.BigEndian.Uint32(data[i : i+lengthLen]))
		if length > maxChunk {
			return nil
		}
		name := data[i+lengthLen : i+lengthLen+typeLen]
		start := i + lengthLen + typeLen
		if start+length+crcLen > len(data) {
			return nil
		}
		if string(name) == "eXIf" {
			payload := data[start : start+length]
			if after, ok := bytes.CutPrefix(payload, exifHeader); ok {
				return after
			}
			return payload
		}
		if string(name) == "IDAT" || string(name) == "IEND" {
			// The specification puts eXIf before IDAT; stopping here
			// keeps the walk off the compressed image data.
			return nil
		}
		i = start + length + crcLen
	}
	return nil
}

// tiffOrientation reads tag 0x0112 out of a TIFF block. It reads the
// first IFD only: that is where the orientation of the main image lives,
// and following offsets into sub-IFDs would mean walking attacker
// controlled pointers for a value Koala is about to throw away.
func tiffOrientation(tiff []byte) int {
	const (
		headerLen    = 8
		entryLen     = 12
		tagCount     = 2
		tagOrient    = 0x0112
		typeShort    = 3
		typeLong     = 4
		tiffMagic    = 42
		minIFDOffset = 8
	)
	if len(tiff) < headerLen {
		return OrientationNone
	}
	var order binary.ByteOrder
	switch string(tiff[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return OrientationNone
	}
	if order.Uint16(tiff[2:4]) != tiffMagic {
		return OrientationNone
	}
	offset := int64(order.Uint32(tiff[4:8]))
	if offset < minIFDOffset || offset+tagCount > int64(len(tiff)) {
		return OrientationNone
	}
	ifd := int(offset)
	entries := int(order.Uint16(tiff[ifd : ifd+tagCount]))
	for n := range entries {
		entry := ifd + tagCount + n*entryLen
		if entry+entryLen > len(tiff) {
			return OrientationNone
		}
		if order.Uint16(tiff[entry:entry+2]) != tagOrient {
			continue
		}
		if order.Uint32(tiff[entry+4:entry+8]) != 1 {
			return OrientationNone // a count other than one is not an orientation
		}
		var value int
		switch order.Uint16(tiff[entry+2 : entry+4]) {
		case typeShort:
			value = int(order.Uint16(tiff[entry+8 : entry+10]))
		case typeLong:
			value = int(order.Uint32(tiff[entry+8 : entry+12]))
		default:
			return OrientationNone
		}
		if value < OrientationNone || value > maxOrientation {
			return OrientationNone
		}
		return value
	}
	return OrientationNone
}

// applyOrientation returns the image as it is meant to be seen. The
// eight EXIF values are the eight ways a rectangle can be mapped onto
// itself: four rotations, each with or without a mirror.
//
// Orientation 1 returns the image unchanged, so the common case copies
// nothing; every other value allocates one raster of the same pixel
// count and fills it once.
func applyOrientation(src image.Image, orientation int) image.Image {
	if orientation <= OrientationNone || orientation > maxOrientation {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	swapped := orientation >= 5
	outW, outH := w, h
	if swapped {
		outW, outH = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	for y := range h {
		for x := range w {
			dx, dy := orientedPoint(orientation, x, y, w, h)
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// orientedPoint maps a source pixel to its place in the upright image.
// The cases are the EXIF orientation table: 2 mirrors horizontally, 3
// rotates 180, 4 mirrors vertically, 5 transposes, 6 rotates 90
// clockwise, 7 transposes the other diagonal and 8 rotates 90
// counter-clockwise.
func orientedPoint(orientation, x, y, w, h int) (int, int) {
	switch orientation {
	case 2:
		return w - 1 - x, y
	case 3:
		return w - 1 - x, h - 1 - y
	case 4:
		return x, h - 1 - y
	case 5:
		return y, x
	case 6:
		return h - 1 - y, x
	case 7:
		return h - 1 - y, w - 1 - x
	case 8:
		return y, w - 1 - x
	default:
		return x, y
	}
}
