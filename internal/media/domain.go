// Package media is the upload pipeline: the quarantine flow of ADR-0019
// §9 and ADR-0029 Karar 6, and the rules that say which prefix, which
// types and which limits a kind of upload gets.
//
// This package carries the decisions; the object store client and the
// metadata stripper are platform (internal/platform/storage,
// internal/platform/imaging). There are no HTTP handlers here and no
// database access yet: the media table, its soft-delete classification
// and its queries come with Faz 2's data PR, and the three endpoints of
// the frozen contract (ADR-0029 Karar 6) get their handlers after it.
package media

import (
	"slices"

	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// Kind is the kind of an upload: the contract's MediaKind. It decides
// the storage prefix, the accepted types and the limits, which is why
// the client sends it when it starts an upload and never sends a file
// name or a type.
type Kind string

// The kinds (ADR-0029, api/openapi.yaml MediaKind).
const (
	KindProfilePicture   Kind = "PROFILE_PICTURE"
	KindCompanyLogo      Kind = "COMPANY_LOGO"
	KindCompanyImage     Kind = "COMPANY_IMAGE"
	KindIdentityDocument Kind = "IDENTITY_DOCUMENT"
	KindCV               Kind = "CV"
)

// Kinds lists every kind.
func Kinds() []Kind {
	return []Kind{KindProfilePicture, KindCompanyLogo, KindCompanyImage, KindIdentityDocument, KindCV}
}

// Status is the state of a media row: the contract's MediaStatus.
// PENDING means the upload has not been completed, READY means the
// object was validated, stripped and moved to its final key, REJECTED
// means it failed validation.
type Status string

// The statuses.
const (
	StatusPending  Status = "PENDING"
	StatusReady    Status = "READY"
	StatusRejected Status = "REJECTED"
)

// transitions is the state machine. PENDING is the only state with a way
// out, and both exits are terminal: a completed upload is never
// re-validated and a rejected one is never revived, so a client that
// completes twice reads the same row the second time.
func transitions() map[Status][]Status {
	return map[Status][]Status{
		StatusPending:  {StatusReady, StatusRejected},
		StatusReady:    nil,
		StatusRejected: nil,
	}
}

// CanTransition reports whether from may become to. A state may not
// transition to itself: completing a READY media changes nothing, which
// the caller answers from the row instead of writing it again.
func CanTransition(from, to Status) bool {
	return slices.Contains(transitions()[from], to)
}

// Rejection is why an upload failed validation. It is a value and not a
// sentinel error, because the outcome is business data: the contract
// carries it on the media resource as rejectionCode, so the row records
// why and the client reads it instead of getting it only once from a
// response (ADR-0030 Karar 7).
//
// The mapping from a Rejection to the contract's stable error code is
// the handler's, in the PR that implements the endpoints: those codes
// are frozen in ADR-0029's PR and are not in this branch's contract yet.
type Rejection string

// The rejections.
const (
	// RejectionNone means the upload passed.
	RejectionNone Rejection = ""
	// RejectionNotUploaded means there is no object in the quarantine:
	// the client never wrote to the address it was given.
	RejectionNotUploaded Rejection = "NOT_UPLOADED"
	// RejectionTypeUnsupported means the type detected in the bytes is
	// not one this kind accepts. The client's declaration and the file
	// name played no part (ADR-0029 Karar 5).
	RejectionTypeUnsupported Rejection = "TYPE_UNSUPPORTED"
	// RejectionTooLarge means the object is over the byte or the pixel
	// limit of its kind.
	RejectionTooLarge Rejection = "TOO_LARGE"
	// RejectionCorrupt means the bytes carry an accepted magic number but
	// do not decode as that format.
	RejectionCorrupt Rejection = "CORRUPT"
)

// Prefix is where a finished object of this kind lives (ADR-0029 Karar
// 6). An unknown kind gets no prefix, so it can reach no object.
func (k Kind) Prefix() (storage.Prefix, bool) {
	switch k {
	case KindProfilePicture, KindCompanyLogo, KindCompanyImage:
		return storage.PrefixImages, true
	case KindIdentityDocument:
		return storage.PrefixIdentityDocument, true
	case KindCV:
		return storage.PrefixFiles, true
	default:
		return "", false
	}
}

// AllowedTypes are the types this kind accepts, detected from the
// content. Images are JPEG and PNG; a CV is a PDF and nothing else.
func (k Kind) AllowedTypes() []imaging.Type {
	switch k {
	case KindProfilePicture, KindCompanyLogo, KindCompanyImage, KindIdentityDocument:
		return []imaging.Type{imaging.TypeJPEG, imaging.TypePNG}
	case KindCV:
		return []imaging.Type{imaging.TypePDF}
	default:
		return nil
	}
}

// Accepts reports whether this kind accepts the type.
func (k Kind) Accepts(typ imaging.Type) bool {
	return slices.Contains(k.AllowedTypes(), typ)
}

// Limits are the byte and pixel bounds of this kind. Every kind uses the
// defaults of ADR-0029 Karar 5 today — 10 MiB per file, 50 megapixels of
// canvas, which are Java's values. The method exists so that a kind
// which needs its own bound later gets it here and not at a call site.
func (k Kind) Limits() imaging.Limits {
	return imaging.Limits{
		MaxBytes:    imaging.DefaultMaxBytes,
		MaxPixels:   imaging.DefaultMaxPixels,
		JPEGQuality: imaging.DefaultJPEGQuality,
	}
}

// Attachment reports whether the object is stored and served as an
// attachment rather than rendered in place. Identity documents and CVs
// are: a document that a browser renders is a document a browser can be
// talked into executing, and neither is ever meant to be displayed
// inline (Java's rule for identity documents, widened to the CV because
// a PDF cannot be re-encoded; ADR-0030 Karar 4).
func (k Kind) Attachment() bool {
	return k == KindIdentityDocument || k == KindCV
}

// PublicReadable reports whether a finished object of this kind gets a
// public address. An identity document never does, in any state
// (ADR-0029 Karar 6, IDN-007).
func (k Kind) PublicReadable() bool {
	prefix, ok := k.Prefix()
	return ok && prefix == storage.PrefixImages
}

// CacheControl is what a finished object is stored with. A public image
// is immutable, because its key holds a UUID that is never reused; a
// private object is never cached.
func (k Kind) CacheControl() string {
	if k.PublicReadable() {
		return "public, max-age=31536000, immutable"
	}
	return "no-store"
}

// ExtOf is the key extension of a detected type (ADR-0029 Karar 6: the
// extension comes from the type, never from the file name).
func ExtOf(typ imaging.Type) (storage.Ext, bool) {
	switch typ {
	case imaging.TypeJPEG:
		return storage.ExtJPEG, true
	case imaging.TypePNG:
		return storage.ExtPNG, true
	case imaging.TypePDF:
		return storage.ExtPDF, true
	default:
		return "", false
	}
}
