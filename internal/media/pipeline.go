package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// UploadTTL is how long a write address lives. Fifteen minutes is long
// enough for a slow mobile connection to push 10 MiB and short enough
// that an address found in a log or a crash report is already dead.
const UploadTTL = 15 * time.Minute

// Pipeline is the quarantine flow: start an upload, then complete it.
//
// It takes the object store concretely and not behind an interface. The
// store is the external integration, so the fake that stands in for it
// is fakes.S3 and the code under test is the real client (ADR-0030
// Karar 6; CLAUDE.md, "no in-memory store").
type Pipeline struct {
	store  *storage.Store
	logger *slog.Logger
}

// NewPipeline returns the pipeline. Both dependencies are required.
func NewPipeline(store *storage.Store, logger *slog.Logger) (*Pipeline, error) {
	if store == nil || logger == nil {
		return nil, errors.New("media: the pipeline needs an object store and a logger")
	}
	return &Pipeline{store: store, logger: logger}, nil
}

// Start reserves a quarantine key for kind and returns the address the
// client writes to (ADR-0029 Karar 6, step one of three).
//
// The key is the server's: the identifier is a UUIDv7 generated here and
// the extension comes from declared, the type the client says it is
// going to send. That declaration decides nothing but the name of the
// quarantine object; the type that ends up on the final object and in
// the row is the one Complete detects in the bytes.
func (p *Pipeline) Start(ctx context.Context, kind Kind, declared imaging.Type) (Upload, error) {
	if _, ok := kind.Prefix(); !ok {
		return Upload{}, fmt.Errorf("media: unknown kind %q", kind)
	}
	if !kind.Accepts(declared) {
		return Upload{}, fmt.Errorf("media: kind %q does not accept %q", kind, declared)
	}
	ext, ok := ExtOf(declared)
	if !ok {
		return Upload{}, fmt.Errorf("media: no key extension for %q", declared)
	}
	id, err := storage.NewID()
	if err != nil {
		return Upload{}, fmt.Errorf("media: start upload: %w", err)
	}
	key, err := storage.NewKey(storage.PrefixIncoming, id, ext)
	if err != nil {
		return Upload{}, fmt.Errorf("media: start upload: %w", err)
	}
	signed, err := p.store.PresignPut(ctx, key, UploadTTL)
	if err != nil {
		return Upload{}, fmt.Errorf("media: start upload: %w", err)
	}
	return Upload{Key: key, URL: signed.URL, Method: signed.Method, ExpiresAt: signed.ExpiresAt}, nil
}

// Upload is a reserved quarantine key and the address to write it at.
type Upload struct {
	// Key is the quarantine key. The row records it, so Complete needs
	// nothing from the client but the media's identifier.
	Key storage.Key
	// URL and Method are what the client sends.
	URL, Method string
	// ExpiresAt is when the address stops working.
	ExpiresAt time.Time
}

// Completion is the result of completing an upload.
type Completion struct {
	// Rejection is why the object failed validation, or RejectionNone.
	// When it is set the other fields are zero and the quarantine object
	// has been deleted.
	Rejection Rejection
	// AlreadyComplete means an earlier call had already validated,
	// stripped and moved the object, and this call found it at its final
	// key instead of doing the work again. Width, Height and Orientation
	// are zero on this path: they describe work this call did not do and
	// cannot recover without downloading the object, and the contract
	// does not carry them.
	AlreadyComplete bool
	// Key is the final key, under the kind's prefix.
	Key storage.Key
	// Type is the type detected in the bytes, which is also the type the
	// object is stored with.
	Type imaging.Type
	// Size is the stored length after the strip, which is not the length
	// the client uploaded: the re-encode changes it.
	Size int64
	// Width and Height are the dimensions after the orientation was
	// applied; zero for a PDF.
	Width, Height int
	// PublicURL is the object's public address, empty when the kind has
	// none.
	PublicURL string
	// Orientation is the EXIF tag that was found and baked in, 1 when
	// there was nothing to apply; zero for a PDF.
	Orientation int
}

// Rejected reports whether the upload failed validation.
func (c Completion) Rejected() bool { return c.Rejection != RejectionNone }

// Complete is step three: validate the quarantine object, strip it and
// move it to its final key (ADR-0029 Karar 6).
//
// The order is a sequence of gates, each cheaper than the next:
//
//  1. head the object — nothing there means the client never wrote;
//  2. the size, from the head, before a byte is read;
//  3. read, bounded by the same limit, because the head is the store's
//     word and not a fact;
//  4. the type, from the content, against what the kind accepts;
//  5. the canvas and the strip (internal/platform/imaging);
//  6. write the final object, then delete the quarantine one.
//
// A failure of 1 to 5 is a Rejection and not an error: it is the
// client's object that is wrong, the row records why, and the quarantine
// object is deleted on the way out. An error means the store or the
// process failed and the caller may retry.
//
// Precondition: the caller's row must still be PENDING. Complete does
// not read the row and cannot check this, and calling it on a READY row
// is how an orphan is made: the upload address lives for UploadTTL and
// its signature covers the key, not the content, so a client that writes
// a second object of a different type to the same address and calls
// Complete again gets the full flow -- the quarantine is occupied, so
// the recovery below never runs -- and a second final object is written
// under a second extension. The row already points at the first one and
// the state machine refuses the transition, so the second object is
// referenced by nothing, covered by no sweep (the lifecycle rule is on
// incoming/ only) and, for a publicly readable kind, publicly served.
// The status gate belongs to the handler, with the row locked; ADR-0030
// carries it as an obligation.
//
// A retry after a successful run is safe, but not because the flow runs
// again: a successful run deletes the quarantine object, so running it
// again would find nothing. "Nothing in the quarantine" has two causes
// that look identical from here -- the client never wrote, and an
// earlier call finished the work -- and they are told apart by looking
// for the object at its final key. That matters because the row is
// written by the caller, after this returns: if that write fails and the
// client retries, answering RejectionNotUploaded would reject a resource
// whose object is correct and sitting under a prefix no sweep covers,
// losing it for good.
//
// A PDF is not re-encoded: it is copied as it is, with the detected type
// and an attachment disposition replacing whatever metadata it carried
// (ADR-0030 Karar 4).
func (p *Pipeline) Complete(ctx context.Context, kind Kind, quarantined storage.Key) (Completion, error) {
	prefix, ok := kind.Prefix()
	if !ok {
		return Completion{}, fmt.Errorf("media: unknown kind %q", kind)
	}
	if quarantined.Prefix() != storage.PrefixIncoming {
		return Completion{}, fmt.Errorf("media: %q is not a quarantine key", quarantined.Prefix())
	}
	limits := kind.Limits()

	info, err := p.store.Head(ctx, quarantined)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return p.finishedEarlier(ctx, kind, quarantined, prefix)
	case err != nil:
		return Completion{}, fmt.Errorf("media: complete upload: %w", err)
	}
	if info.Size > limits.MaxBytes {
		return p.reject(ctx, quarantined, RejectionTooLarge), nil
	}

	body, err := p.store.Get(ctx, quarantined, limits.MaxBytes)
	switch {
	case errors.Is(err, storage.ErrTooLarge):
		return p.reject(ctx, quarantined, RejectionTooLarge), nil
	case errors.Is(err, storage.ErrNotFound):
		// The sweep, a concurrent delete or a concurrent completion got
		// there between the head and the read.
		return p.finishedEarlier(ctx, kind, quarantined, prefix)
	case err != nil:
		return Completion{}, fmt.Errorf("media: complete upload: %w", err)
	}

	typ, ok := imaging.Detect(body)
	if !ok || !kind.Accepts(typ) {
		return p.reject(ctx, quarantined, RejectionTypeUnsupported), nil
	}
	ext, ok := ExtOf(typ)
	if !ok {
		return Completion{}, fmt.Errorf("media: no key extension for %q", typ)
	}
	final, err := quarantined.WithPrefix(prefix)
	if err != nil {
		return Completion{}, fmt.Errorf("media: complete upload: %w", err)
	}
	// The extension follows the detected type, so a client that declared
	// a PNG and uploaded a JPEG gets a final key named jpg.
	final, err = final.WithExt(ext)
	if err != nil {
		return Completion{}, fmt.Errorf("media: complete upload: %w", err)
	}

	put := storage.PutOptions{
		ContentType:  string(typ),
		Attachment:   kind.Attachment(),
		CacheControl: kind.CacheControl(),
	}
	done := Completion{Key: final, Type: typ}

	if typ.Image() {
		stripped, err := imaging.Sanitize(ctx, body, limits)
		switch {
		case errors.Is(err, imaging.ErrCanvasTooLarge), errors.Is(err, imaging.ErrTooLarge):
			return p.reject(ctx, quarantined, RejectionTooLarge), nil
		case errors.Is(err, imaging.ErrUnsupportedType):
			return p.reject(ctx, quarantined, RejectionTypeUnsupported), nil
		case errors.Is(err, imaging.ErrCorrupt):
			return p.reject(ctx, quarantined, RejectionCorrupt), nil
		case err != nil:
			return Completion{}, fmt.Errorf("media: strip: %w", err)
		}
		if err := p.store.Put(ctx, final, stripped.Data, put); err != nil {
			return Completion{}, fmt.Errorf("media: complete upload: %w", err)
		}
		// The quarantine object has served its purpose. A failure here
		// leaves a leftover for the sweep and does not fail the upload:
		// the final object is written and correct.
		p.sweepLater(ctx, quarantined, p.store.Delete(ctx, quarantined))
		done.Size = int64(len(stripped.Data))
		done.Width, done.Height = stripped.Width, stripped.Height
		done.Orientation = stripped.Orientation
	} else {
		// A PDF moves as it is. ErrSourceKept means the copy landed and
		// the delete did not, which is the same leftover as above.
		err := p.store.Move(ctx, quarantined, final, put)
		switch {
		case errors.Is(err, storage.ErrSourceKept):
			p.sweepLater(ctx, quarantined, err)
		case err != nil:
			return Completion{}, fmt.Errorf("media: complete upload: %w", err)
		}
		done.Size = info.Size
	}

	if url, ok := p.store.PublicURL(done.Key); ok && kind.PublicReadable() {
		done.PublicURL = url
	}
	return done, nil
}

// finishedEarlier answers the case where the quarantine object is gone.
// It looks for the object at its final key and reports the upload as
// complete when it is there; only when it is not is the upload one the
// client never wrote.
//
// The final extension follows the type detected on the first run, which
// is not knowable here any more, so every extension the kind accepts is
// tried: two heads for an image kind, one for a CV, and only on this
// path. The object itself is the durable record this layer has, because
// the row may be exactly what failed.
func (p *Pipeline) finishedEarlier(ctx context.Context, kind Kind, quarantined storage.Key, prefix storage.Prefix) (Completion, error) {
	for _, typ := range kind.AllowedTypes() {
		ext, ok := ExtOf(typ)
		if !ok {
			continue
		}
		final, err := quarantined.WithPrefix(prefix)
		if err != nil {
			return Completion{}, fmt.Errorf("media: complete upload: %w", err)
		}
		final, err = final.WithExt(ext)
		if err != nil {
			return Completion{}, fmt.Errorf("media: complete upload: %w", err)
		}
		info, err := p.store.Head(ctx, final)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			continue
		case err != nil:
			return Completion{}, fmt.Errorf("media: complete upload: %w", err)
		}
		done := Completion{AlreadyComplete: true, Key: final, Type: typ, Size: info.Size}
		if url, ok := p.store.PublicURL(final); ok && kind.PublicReadable() {
			done.PublicURL = url
		}
		return done, nil
	}
	return Completion{Rejection: RejectionNotUploaded}, nil
}

// reject deletes the quarantine object and returns the rejection. The
// delete is best effort: a rejection that could not clean up is still a
// rejection, and the sweep removes what is left.
func (p *Pipeline) reject(ctx context.Context, quarantined storage.Key, why Rejection) Completion {
	p.sweepLater(ctx, quarantined, p.store.Delete(ctx, quarantined))
	return Completion{Rejection: why}
}

// Outcome is how a caller's own write ended, for Discard.
type Outcome int

// The outcomes (ADR-0029 Karar 6, the rollback invariant).
const (
	// WriteFailed means the caller's write definitely did not happen, so
	// nothing refers to the object and it is deleted.
	WriteFailed Outcome = iota
	// WriteIndeterminate means the caller does not know whether its write
	// happened — a commit that returned an error may still have
	// committed (ADR-0017). The object is kept, because a row that
	// points at a deleted object is worse than an object no row points
	// at: the first is a broken resource, the second is a leftover the
	// sweep removes.
	WriteIndeterminate
)

// Discard implements the rollback invariant Java established and
// ADR-0029 Karar 6 generalizes: when the database write that was going
// to name an object fails, the object is deleted; when the transaction's
// outcome is unknown, the object is kept. A cleanup failure never masks
// the original error: it is logged at ERROR and Discard reports nothing,
// so the caller still answers with why it was rolling back.
func (p *Pipeline) Discard(ctx context.Context, key storage.Key, outcome Outcome) {
	if outcome == WriteIndeterminate {
		p.logger.WarnContext(ctx, "object kept after an indeterminate write",
			slog.String("prefix", string(key.Prefix())))
		return
	}
	p.sweepLater(ctx, key, p.store.Delete(ctx, key))
}

// sweepLater logs a failed cleanup and does nothing else. It is one
// function so that every "the object is a leftover now" path says the
// same thing with the same keys, and so that none of them turns a
// cleanup failure into a request failure.
func (p *Pipeline) sweepLater(ctx context.Context, key storage.Key, err error) {
	if err == nil {
		return
	}
	p.logger.ErrorContext(ctx, "object left in the store after a failed cleanup",
		slog.String("prefix", string(key.Prefix())),
		slog.String("id", key.ID().String()),
		slog.Any("error", err))
}
