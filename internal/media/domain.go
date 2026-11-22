// Package media is the upload pipeline: the quarantine flow of ADR-0019
// §9 and ADR-0029 Karar 6, the rules that say which prefix, which types
// and which limits a kind of upload gets, and the four operations of
// /v1/me/media (ADR-0035): start, complete, read and delete.
//
// This package carries the decisions; the object store client and the
// metadata stripper are platform (internal/platform/storage,
// internal/platform/imaging). The layers are the module's usual ones:
// domain.go (kinds, states, the state machine, the rejection codes),
// pipeline.go (the object flow), service.go and jobs.go (the media table
// and the River jobs that validate, delete and sweep), store.go (the
// sqlc wrapper) and http.go (the handlers).
package media

import (
	"slices"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// Error codes of this package (api/components/problem.yaml ErrorCode).
const (
	// CodeNotFound: no such media, or not the caller's; the two are one
	// answer (ADR-0014).
	CodeNotFound apperr.Code = "MEDIA_NOT_FOUND"
	// CodeTooLarge: the declared size is over the kind's limit at the
	// start, or the object is over it at the completion (rejectionCode).
	CodeTooLarge apperr.Code = "MEDIA_TOO_LARGE"
	// CodeTypeUnsupported is a rejection's code (rejectionCode only).
	CodeTypeUnsupported apperr.Code = "MEDIA_TYPE_UNSUPPORTED"
	// CodeUploadIncomplete is a rejection's code (rejectionCode only).
	CodeUploadIncomplete apperr.Code = "MEDIA_UPLOAD_INCOMPLETE"
	// CodePendingLimit: the caller holds MaxOpenUploads unfinished uploads.
	CodePendingLimit apperr.Code = "MEDIA_UPLOAD_PENDING_LIMIT"
	// CodeDailyLimit: the caller started DailyUploads uploads in the last
	// UploadBudgetWindow.
	CodeDailyLimit apperr.Code = "MEDIA_UPLOAD_DAILY_LIMIT"
)

// Codes lists the error codes of this package; the contract test holds it
// against api/components/problem.yaml (ADR-0028). Three of them never
// leave as a problem: they are the codes a rejected media row renders in
// rejectionCode (Rejection.Code).
func Codes() []apperr.Code {
	return []apperr.Code{
		CodeNotFound, CodeTooLarge, CodeTypeUnsupported, CodeUploadIncomplete,
		CodePendingLimit, CodeDailyLimit,
	}
}

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

// Statuses lists every status.
func Statuses() []Status { return []Status{StatusPending, StatusReady, StatusRejected} }

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
// Rejection.Code (ADR-0035 Karar 3). It is not one to one:
// MEDIA_UPLOAD_INCOMPLETE, MEDIA_TYPE_UNSUPPORTED and MEDIA_TOO_LARGE
// line up with three of the four rejections, and RejectionCorrupt folds
// into MEDIA_TYPE_UNSUPPORTED. The media row stores the Rejection, not
// the code, so the distinction the pipeline makes survives in the
// database; only the response narrows it.
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

// Code is the contract's code of a rejection, which Media.rejectionCode
// renders (ADR-0035 Karar 3). It is not one to one: CORRUPT folds into
// MEDIA_TYPE_UNSUPPORTED. Bytes that carry an accepted magic number and
// do not decode as that format are not a valid file of the type they
// claim, and the client's next step is the same as for an unaccepted
// type: send a valid JPEG or PNG. The fold is the industry's usual answer
// (an image API reports "invalid image" for both), and it costs the row
// nothing: the media row stores the Rejection itself, so the distinction
// the pipeline makes is kept where it is evidence, and only the response
// narrows it. The alternative, a fifth code, would tell a client apart
// two failures it cannot act on differently.
//
// RejectionNone has no code: the second result is false.
func (r Rejection) Code() (apperr.Code, bool) {
	switch r {
	case RejectionNotUploaded:
		return CodeUploadIncomplete, true
	case RejectionTypeUnsupported, RejectionCorrupt:
		return CodeTypeUnsupported, true
	case RejectionTooLarge:
		return CodeTooLarge, true
	case RejectionNone:
		return "", false
	default:
		return "", false
	}
}

// Rejections lists every rejection a row may record.
func Rejections() []Rejection {
	return []Rejection{RejectionNotUploaded, RejectionTypeUnsupported, RejectionTooLarge, RejectionCorrupt}
}

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
// content. Images are JPEG and PNG; a CV is a PDF, a DOC or a DOCX.
//
// The CV takes all three because that is what the industry takes and
// what Java takes today: LinkedIn, Indeed, Greenhouse, Lever and
// Kariyer.net all accept the three, and several of them parse .docx
// best. An earlier decision narrowed the Go rewrite to PDF alone;
// ADR-0031 Karar 2 reverses it.
func (k Kind) AllowedTypes() []imaging.Type {
	switch k {
	case KindProfilePicture, KindCompanyLogo, KindCompanyImage, KindIdentityDocument:
		return []imaging.Type{imaging.TypeJPEG, imaging.TypePNG}
	case KindCV:
		return []imaging.Type{imaging.TypePDF, imaging.TypeDOC, imaging.TypeDOCX}
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
func (Kind) Limits() imaging.Limits {
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
// none of its types can be re-encoded; ADR-0030 Karar 4, ADR-0031
// Karar 2). For a CV this is load bearing and not hygiene: it is one of
// the mitigations the accepted limit rests on.
func (k Kind) Attachment() bool {
	return k == KindIdentityDocument || k == KindCV
}

// PublicReadable reports whether a finished object of this kind gets a
// public address. An identity document never does, in any state
// (ADR-0029 Karar 6, IDN-007).
//
// The rule is the prefix's (storage.Prefix.Public), the same one
// Store.PublicURL resolves a key with: a kind is public exactly when its
// prefix is. Before ADR-0035 the two were written separately, this one
// as "the prefix is images/" and the store's as "images/ or public/",
// and agreed only because no kind went to public/ (ADR-0030, "İki liste,
// bir kural").
func (k Kind) PublicReadable() bool {
	prefix, ok := k.Prefix()
	return ok && prefix.Public()
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
	case imaging.TypeDOC:
		return storage.ExtDOC, true
	case imaging.TypeDOCX:
		return storage.ExtDOCX, true
	default:
		return "", false
	}
}
