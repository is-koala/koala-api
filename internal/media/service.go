package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/is-koala/koala-api/internal/media/mediadb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// The upload budget (ADR-0035 Karar 6). The values are the product's
// largest legitimate use with room for retries, not a measurement:
//
//   - MaxOpenUploads: a company gallery holds ten images (MED-011) and a
//     client uploads a full gallery in parallel; an identity review takes
//     four photos. Ten open uploads cover the largest batch the product
//     has. Each open upload is a live write address, and a write address
//     is the one thing in the flow whose size the store does not bound
//     (ADR-0030 Karar 1), so this cap bounds how many of them one user
//     holds at once.
//   - DailyUploads: a user who sets up everything in one day -- a profile
//     picture, a logo, ten gallery images, four identity photos and a CV,
//     seventeen uploads -- and redoes all of it twice stays under fifty.
//     It bounds how many write addresses one user can obtain in a day,
//     which the open cap alone does not: deleting a PENDING upload frees
//     its place in the open cap, so start-delete-start would mint
//     addresses without end. The budget counts its own ledger, which a
//     deletion does not touch.
const (
	MaxOpenUploads     = 10
	DailyUploads       = 50
	UploadBudgetWindow = 24 * time.Hour
)

// UploadRetention is how long an upload that did not end READY is kept:
// the quarantine's 24 hours (M2, ADR-0029 Karar 6). After it the sweep
// deletes the row and every object it could have left.
const UploadRetention = 24 * time.Hour

// DownloadTTL is how long the signed download address of a private media
// lives (ADR-0035 Karar 5). The owner's client follows it at once; five
// minutes covers a slow start of the download and leaves an address that
// leaked into a log or a screenshot dead soon after. The staff download of
// identity documents is three minutes (ADR-0029 Karar 6); the owner's own
// CV is less sensitive and is fetched from a phone.
const DownloadTTL = 5 * time.Minute

// sweepBatch is how many unfinished uploads one sweep transaction takes.
// sweepBatches bounds a run, so a store that keeps failing cannot hold
// the job forever; what is left is the next run's.
const (
	sweepBatch   = 100
	sweepBatches = 20
)

// uploaderLockClass is the first key of the two-key advisory lock that
// serializes one user's starts (LockUploader): "MU".
const uploaderLockClass = 0x4d55

// errNoPrincipal means a handler ran without the authentication
// middleware's principal: a wiring bug, answered 500.
var errNoPrincipal = errors.New("media: handler reached without an authenticated principal")

// errNoObjectStore means the service runs without an object store, which
// config.ServeStorageErrors allows in local and test only.
var errNoObjectStore = errors.New("media: no object store is configured (KOALA_OBJECT_*)")

// jobQueue inserts a River job in the caller's transaction (ADR-0010):
// the completion and the deletion of objects are guaranteed side effects,
// so they commit with the row or not at all.
type jobQueue interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Service runs the media operations on the media table, the object store
// and the job queue (ADR-0035).
type Service struct {
	db    *db.DB
	queue jobQueue
	// store and pipeline are nil only in local and test without an object
	// store (config.ServeStorageErrors refuses that in staging and
	// production). Without a store no upload can start, so no row exists
	// whose object would need one; a read answers without an address.
	store    *storage.Store
	pipeline *Pipeline
	deleter  *softdelete.Deleter
	clock    clock.Clock
	logger   *slog.Logger
}

// NewService returns the service. store may be nil (see Service); every
// other dependency is required.
func NewService(database *db.DB, queue jobQueue, store *storage.Store, deleter *softdelete.Deleter,
	clk clock.Clock, logger *slog.Logger,
) (*Service, error) {
	if database == nil || queue == nil || deleter == nil || clk == nil || logger == nil {
		return nil, errors.New("media: the service needs a database, a job queue, a deleter, a clock and a logger")
	}
	s := &Service{db: database, queue: queue, store: store, deleter: deleter, clock: clk, logger: logger}
	if store != nil {
		p, err := NewPipeline(store, logger)
		if err != nil {
			return nil, err
		}
		s.pipeline = p
	}
	return s, nil
}

// Started is a started upload: the PENDING row and its write address.
type Started struct {
	Media     Media
	UploadURL string
	ExpiresAt time.Time
}

// Start starts an upload of kind for owner (ADR-0029 Karar 6, step one).
//
// The write address is signed first, outside the transaction: signing is
// local (SigV4 makes no request), and an address whose row then fails to
// commit was never handed out. Then, in one transaction under the
// owner's lock, the two budgets are checked and spent and the row is
// written. commit runs last in that transaction: the handler stores its
// answer for the Idempotency-Key there (ADR-0034 Karar 4).
//
// The row's id is the identifier of the quarantine key, so the final key
// the pipeline derives from it carries the row's id too (ADR-0035 Karar
// 4; the database's CHECKs hold it).
func (s *Service) Start(ctx context.Context, owner uuid.UUID, kind Kind, declaredSize int64,
	commit func(ctx context.Context, q db.Querier, started Started) error,
) (Started, error) {
	if s.pipeline == nil {
		return Started{}, errNoObjectStore
	}
	// The schema bounds the declared size at the default limit; a kind
	// with a smaller limit of its own answers here (Kind.Limits).
	if declaredSize > kind.Limits().MaxBytes {
		return Started{}, apperr.New(apperr.KindInvalid, CodeTooLarge)
	}
	types := kind.AllowedTypes()
	if len(types) == 0 {
		return Started{}, fmt.Errorf("media: kind %q accepts no type", kind)
	}
	// The request carries no type, and Pipeline.Start wants one: it names
	// the quarantine key's extension. AllowedTypes()[0] is passed, and the
	// choice is cosmetic (ADR-0031 Karar 3). The final key's extension
	// comes from the type Complete detects in the bytes (final.WithExt),
	// so a wrong guess here has no observable effect: a client that
	// uploads a PNG to incoming/<id>.jpg ends up with images/<id>.png
	// (TestCompleteNamesTheKeyFromTheDetectedType). The list is ordered
	// and its first entry is the most common type of the kind (JPEG for an
	// image, PDF for a CV), which TestKindRules pins with slices.Equal.
	up, err := s.pipeline.Start(ctx, kind, types[0])
	if err != nil {
		return Started{}, err
	}
	now := s.clock.Now()
	var started Started
	err = s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		mq := queries(q)
		if err := mq.LockUploader(ctx, mediadb.LockUploaderParams{LockClass: uploaderLockClass, OwnerID: owner}); err != nil {
			return fmt.Errorf("media: lock the uploader: %w", err)
		}
		// The caller is the parent (cascade edge): a concurrent deletion
		// of the account either waits for this transaction and takes the
		// new row with it, or has already marked the user. The second case
		// is a deleted caller that authentication let through moments
		// before; there is no code for it and it is answered 500.
		if err := softdelete.LockActiveParent(ctx, q, "users", owner); err != nil {
			return fmt.Errorf("media: lock the owner: %w", err)
		}
		open, err := mq.CountOpenUploads(ctx, owner)
		if err != nil {
			return fmt.Errorf("media: count open uploads: %w", err)
		}
		if open >= MaxOpenUploads {
			return apperr.New(apperr.KindConflict, CodePendingLimit)
		}
		since := now.Add(-UploadBudgetWindow)
		n, err := mq.CountUploadStarts(ctx, mediadb.CountUploadStartsParams{UserID: owner, Since: since})
		if err != nil {
			return fmt.Errorf("media: count upload starts: %w", err)
		}
		if n >= DailyUploads {
			oldest, err := mq.OldestUploadStart(ctx, mediadb.OldestUploadStartParams{UserID: owner, Since: since})
			if err != nil {
				return fmt.Errorf("media: oldest upload start: %w", err)
			}
			return &budgetError{
				err:  apperr.New(apperr.KindRateLimited, CodeDailyLimit),
				wait: oldest.Add(UploadBudgetWindow).Sub(now),
			}
		}
		id := up.Key.ID()
		r, err := mq.InsertMedia(ctx, mediadb.InsertMediaParams{
			ID: id, OwnerID: owner, Kind: string(kind), UploadKey: up.Key.String(),
			DeclaredSize: declaredSize, CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("media: insert media: %w", err)
		}
		if err := mq.InsertUploadStart(ctx, mediadb.InsertUploadStartParams{ID: id, UserID: owner, CreatedAt: now}); err != nil {
			return fmt.Errorf("media: record the upload start: %w", err)
		}
		m, err := fromRow(row(r))
		if err != nil {
			return err
		}
		started = Started{Media: m, UploadURL: up.URL, ExpiresAt: up.ExpiresAt}
		return commit(ctx, q, started)
	})
	if err != nil {
		return Started{}, fmt.Errorf("media: start upload: %w", err)
	}
	return started, nil
}

// budgetError is MEDIA_UPLOAD_DAILY_LIMIT with the wait its answer's
// Retry-After carries.
type budgetError struct {
	err  *apperr.Error
	wait time.Duration
}

func (e *budgetError) Error() string { return e.err.Error() }

func (e *budgetError) Unwrap() error { return e.err }

// RetryAfter returns the wait an error asks the client to keep before it
// tries again, if it carries one.
func RetryAfter(err error) (time.Duration, bool) {
	var b *budgetError
	if errors.As(err, &b) {
		return b.wait, true
	}
	return 0, false
}

// Get returns one of owner's media. Another user's media is the same
// MEDIA_NOT_FOUND as one that never existed (ADR-0014).
func (s *Service) Get(ctx context.Context, owner, id uuid.UUID) (Media, error) {
	r, err := queries(s.db.Pool()).GetOwnMedia(ctx, mediadb.GetOwnMediaParams{ID: id, OwnerID: owner})
	if errors.Is(err, pgx.ErrNoRows) {
		return Media{}, apperr.New(apperr.KindNotFound, CodeNotFound)
	}
	if err != nil {
		return Media{}, fmt.Errorf("media: get: %w", err)
	}
	return fromRow(r)
}

// RequestCompletion queues the completion of one of owner's uploads
// (ADR-0029 Karar 6, step three; ADR-0035 Karar 2) and returns the row as
// it stands.
//
// This is the status gate ADR-0030 left to the handler: under the row's
// lock, only a PENDING row gets a job. A READY or REJECTED row is answered
// as it is and Pipeline.Complete is never called for it. Without the gate
// a client could write a second object of another type to the still valid
// write address of a READY upload and complete again: the pipeline would
// write a second final object under a second extension that no row names
// and, for a public kind, anyone could read.
//
// The job is unique by its arguments, so completing a PENDING upload
// again while its job waits or runs queues nothing new; a job River gave
// up on (discarded) can be queued again by the next request.
func (s *Service) RequestCompletion(ctx context.Context, owner, id uuid.UUID) (Media, error) {
	var m Media
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		mq := queries(q)
		r, err := mq.LockOwnMedia(ctx, mediadb.LockOwnMediaParams{ID: id, OwnerID: owner})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.New(apperr.KindNotFound, CodeNotFound)
		}
		if err != nil {
			return fmt.Errorf("media: lock media: %w", err)
		}
		if Status(r.Status) != StatusPending {
			m, err = fromRow(row(r))
			return err
		}
		requested, err := mq.RequestCompletion(ctx, mediadb.RequestCompletionParams{Now: s.clock.Now(), ID: id})
		if err != nil {
			return fmt.Errorf("media: request completion: %w", err)
		}
		if m, err = fromRow(row(requested)); err != nil {
			return err
		}
		if _, err := s.queue.InsertTx(ctx, q.PgxTx(), completeArgs{Media: id}, nil); err != nil {
			return fmt.Errorf("media: queue the completion: %w", err)
		}
		return nil
	})
	if err != nil {
		return Media{}, fmt.Errorf("media: complete upload: %w", err)
	}
	return m, nil
}

// Delete soft-deletes one of owner's media and queues the deletion of its
// objects (ADR-0035 Karar 7). A media that a consuming table points at on
// a restrict edge is refused with SOFT_DELETE_RESTRICTED by the platform.
func (s *Service) Delete(ctx context.Context, owner, id uuid.UUID) error {
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		r, err := queries(q).LockOwnMedia(ctx, mediadb.LockOwnMediaParams{ID: id, OwnerID: owner})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.New(apperr.KindNotFound, CodeNotFound)
		}
		if err != nil {
			return fmt.Errorf("media: lock media: %w", err)
		}
		if _, err := s.deleter.Delete(ctx, q, softdelete.DeleteRequest{
			Table: tableMedia, ID: id,
			Actor:  softdelete.Actor{Kind: softdelete.ActorUser, ID: owner},
			Reason: softdelete.ReasonUserRequest, At: s.clock.Now(),
		}); err != nil {
			return fmt.Errorf("media: soft-delete: %w", err)
		}
		args := deleteObjectsArgs{Media: id, Kind: r.Kind, UploadKey: r.UploadKey}
		if _, err := s.queue.InsertTx(ctx, q.PgxTx(), args, nil); err != nil {
			return fmt.Errorf("media: queue the deletion of the objects: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("media: delete media: %w", err)
	}
	return nil
}

// tableMedia is the media table's name in the soft-delete catalog.
const tableMedia = "media"

// Address is how a media is reached: a URL and, for a signed one, its
// expiry.
type Address struct {
	URL       string
	ExpiresAt *time.Time
}

// Address returns how m is reached, by kind (ADR-0035 Karar 5):
//
//   - a publicly readable kind (profile picture, logo, gallery image) has
//     its permanent public URL;
//   - a CV has a short lived signed download address, and only when
//     download is true: getMedia asks for one, the answers of start and
//     complete do not, so no stored Idempotency-Key answer ever carries an
//     address that is dead by the time it is replayed;
//   - an identity document has none, in any state and for any caller
//     (IDN-007). Staff read it through the review's own download, which
//     the PII gate and the audit trail guard (ADR-0026).
//
// Only a READY media has one. The quarantine is never addressed:
// PresignGet refuses incoming/ in any case (ADR-0030 Karar 4).
func (s *Service) Address(ctx context.Context, m Media, download bool) (Address, error) {
	if m.Status != StatusReady || s.store == nil || m.ObjectKey.Zero() {
		return Address{}, nil
	}
	switch {
	case m.Kind.PublicReadable():
		url, ok := s.store.PublicURL(m.ObjectKey)
		if !ok {
			return Address{}, nil
		}
		return Address{URL: url}, nil
	case m.Kind == KindCV && download:
		signed, err := s.store.PresignGet(ctx, m.ObjectKey, DownloadTTL, storage.Download{
			ContentType: string(m.ContentType), Attachment: true, NoStore: true,
		})
		if err != nil {
			return Address{}, fmt.Errorf("media: sign the download: %w", err)
		}
		expires := signed.ExpiresAt
		return Address{URL: signed.URL, ExpiresAt: &expires}, nil
	default:
		return Address{}, nil
	}
}
