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

	"github.com/is-koala/koala-api/internal/media/mediadb"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// Attempts of the media jobs (ADR-0035 Karar 2). River waits attempt⁴
// seconds between attempts: eight attempts spend about two and a half
// hours, well inside the 24 hours after which the sweep takes the upload
// anyway; ten deletions spend about seven hours.
const (
	completeAttempts      = 8
	deleteObjectsAttempts = 10
)

// completeArgs is the completion of one upload: the media's id and
// nothing else, so the job reads the row as it stands when it runs.
type completeArgs struct {
	Media uuid.UUID `json:"media"`
}

// Kind is the job kind in River's tables.
func (completeArgs) Kind() string { return "media_complete" }

// InsertOpts are the job's River options. The job is unique by its
// arguments in River's default states (waiting, running, retrying and
// completed): a second request to complete queues nothing while one job
// waits or runs. A discarded or canceled job does not block a new one, so
// the owner can try again after River gave up.
func (completeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: completeAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// deleteObjectsArgs is the deletion of a deleted media's objects. It
// carries the kind and the quarantine key because the row is no longer
// readable through active_media when the job runs.
type deleteObjectsArgs struct {
	Media     uuid.UUID `json:"media"`
	MediaKind string    `json:"kind"`
	UploadKey string    `json:"upload_key"`
}

// Kind is the job kind in River's tables.
func (deleteObjectsArgs) Kind() string { return "media_delete_objects" }

// InsertOpts are the job's River options.
func (deleteObjectsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: deleteObjectsAttempts}
}

// SweepArgs is the periodic sweep of unfinished uploads and of the budget
// ledger (ADR-0035 Karar 4 and 6). internal/app schedules it.
type SweepArgs struct{}

// Kind is the job kind in River's tables.
func (SweepArgs) Kind() string { return "media_sweep" }

// AddWorkers registers the workers of the media jobs on workers.
func (s *Service) AddWorkers(workers *river.Workers) error {
	errs := []error{
		river.AddWorkerSafely(workers, &completeWorker{service: s}),
		river.AddWorkerSafely(workers, &deleteObjectsWorker{service: s}),
		river.AddWorkerSafely(workers, &sweepWorker{service: s}),
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("media: workers: %w", err)
	}
	return nil
}

type completeWorker struct {
	river.WorkerDefaults[completeArgs]

	service *Service
}

func (w *completeWorker) Work(ctx context.Context, job *river.Job[completeArgs]) error {
	return w.service.Complete(ctx, job.Args.Media)
}

type deleteObjectsWorker struct {
	river.WorkerDefaults[deleteObjectsArgs]

	service *Service
}

func (w *deleteObjectsWorker) Work(ctx context.Context, job *river.Job[deleteObjectsArgs]) error {
	return w.service.DeleteObjects(ctx, job.Args.Media, Kind(job.Args.MediaKind), job.Args.UploadKey)
}

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]

	service *Service
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	_, err := w.service.Sweep(ctx)
	return err
}

// Complete is the completion job (ADR-0029 Karar 6, ADR-0035 Karar 2):
// validate, strip and move the quarantine object, then record the
// outcome. The request that queued it answered 202; the owner reads the
// outcome from the media.
//
// The flow is "decide, call out, record", with no transaction open
// during the object store's calls (CLAUDE.md):
//
//  1. Under the row's lock the row must still be PENDING; otherwise the
//     job has nothing to do (the owner deleted it, the sweep took it, an
//     earlier run decided it). Pipeline.Complete is therefore only ever
//     called for a PENDING row, which is its precondition (ADR-0030).
//  2. Pipeline.Complete runs. A Rejection is a business outcome; an error
//     is the store failing and is retried.
//  3. Under the lock again the outcome is written, PENDING → READY or
//     REJECTED, if the row is still PENDING.
//
// Error classes:
//
//   - permanent: a row that does not read (fromRow) cannot be completed by
//     any retry; the job is canceled and the sweep takes the row;
//   - transient: the store or the database failed; River retries
//     (completeAttempts). An object the pipeline already wrote is kept
//     across the retry -- the next run finds it at its final key
//     (Pipeline.finishedEarlier) instead of answering NOT_UPLOADED for an
//     upload that happened.
//
// Objects (MED-006): when step 3 finds the row gone or decided, nothing
// will ever name the object this run wrote, and it is deleted after the
// commit (Discard, WriteFailed). When step 3's own transaction fails its
// outcome may be unknown (ADR-0017), and the object is kept
// (WriteIndeterminate); the retry records it or, after the last attempt,
// the sweep deletes it. A READY outcome also deletes the row's other final
// keys -- the same id under the extensions the kind accepts and the row
// does not name -- which is the orphan sweep of the final prefixes
// (ADR-0035 Karar 4).
func (s *Service) Complete(ctx context.Context, id uuid.UUID) error {
	if s.pipeline == nil {
		return errNoObjectStore
	}
	var m Media
	var pending bool
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		r, err := queries(q).LockMedia(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: lock media: %w", err)
		}
		if Status(r.Status) != StatusPending {
			return nil
		}
		m, err = fromRow(row(r))
		if err != nil {
			return fmt.Errorf("media: read media: %w", river.JobCancel(err))
		}
		pending = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("media: complete %s: %w", id, err)
	}
	if !pending {
		return nil
	}

	done, err := s.pipeline.Complete(ctx, m.Kind, m.UploadKey)
	if err != nil {
		return fmt.Errorf("media: complete %s: %w", id, err)
	}

	var recorded, orphaned bool
	err = s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		mq := queries(q)
		r, err := mq.LockMedia(ctx, id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			orphaned = true
			return nil
		case err != nil:
			return fmt.Errorf("media: lock media: %w", err)
		}
		from := Status(r.Status)
		if from != StatusPending {
			// Decided meanwhile: by the sweep (REJECTED) or, which the
			// unique job rules out, by another run. An object at the key
			// the row names is the row's and stays.
			orphaned = r.ObjectKey == nil || *r.ObjectKey != done.Key.String()
			return nil
		}
		now := s.clock.Now()
		if done.Rejected() {
			if !CanTransition(from, StatusRejected) {
				return fmt.Errorf("media: %s cannot become %s", from, StatusRejected)
			}
			_, err = mq.MarkRejected(ctx, mediadb.MarkRejectedParams{Now: now, Rejection: string(done.Rejection), ID: id})
		} else {
			if !CanTransition(from, StatusReady) {
				return fmt.Errorf("media: %s cannot become %s", from, StatusReady)
			}
			_, err = mq.MarkReady(ctx, mediadb.MarkReadyParams{
				Now: now, ObjectKey: done.Key.String(), ContentType: string(done.Type), SizeBytes: done.Size, ID: id,
			})
		}
		if err != nil {
			return fmt.Errorf("media: record the outcome: %w", err)
		}
		recorded = true
		return nil
	})
	if err != nil {
		if !done.Rejected() {
			s.pipeline.Discard(ctx, done.Key, WriteIndeterminate)
		}
		return fmt.Errorf("media: complete %s: %w", id, err)
	}
	switch {
	case done.Rejected():
		// The pipeline deleted the quarantine object; nothing was written.
	case orphaned:
		s.pipeline.Discard(ctx, done.Key, WriteFailed)
	case recorded:
		s.deleteSiblings(ctx, m, done.Key)
	}
	return nil
}

// candidates are every final key the media id of kind can have: the
// kind's prefix with each extension the kind accepts. Every object the
// pipeline writes for the media is one of them, because the final key is
// the quarantine key's id under the kind's prefix (ADR-0035 Karar 4).
func candidates(kind Kind, id uuid.UUID) ([]storage.Key, error) {
	prefix, ok := kind.Prefix()
	if !ok {
		return nil, fmt.Errorf("media: unknown kind %q", kind)
	}
	var out []storage.Key
	for _, typ := range kind.AllowedTypes() {
		ext, ok := ExtOf(typ)
		if !ok {
			return nil, fmt.Errorf("media: no key extension for %q", typ)
		}
		key, err := storage.NewKey(prefix, id, ext)
		if err != nil {
			return nil, fmt.Errorf("media: candidate key: %w", err)
		}
		out = append(out, key)
	}
	return out, nil
}

// deleteSiblings deletes the final keys of m that are not keep: objects
// written for this id under another extension, which no row names. It is
// best effort: a failure is logged and left (the object is unreachable
// from any row, and the next completion of this id cannot happen).
func (s *Service) deleteSiblings(ctx context.Context, m Media, keep storage.Key) {
	keys, err := candidates(m.Kind, m.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "media sibling keys", slog.Any("error", err))
		return
	}
	for _, key := range keys {
		if key == keep {
			continue
		}
		if err := s.store.Delete(ctx, key); err != nil {
			s.logger.ErrorContext(ctx, "object left in the store after a failed cleanup",
				slog.String("prefix", string(key.Prefix())), slog.String("id", key.ID().String()),
				slog.Any("error", err))
		}
	}
}

// deleteAll deletes every object the media id of kind can have: its
// candidate final keys and its quarantine key.
func (s *Service) deleteAll(ctx context.Context, kind Kind, id uuid.UUID, upload storage.Key) error {
	keys, err := candidates(kind, id)
	if err != nil {
		return err
	}
	if !upload.Zero() {
		keys = append(keys, upload)
	}
	var errs []error
	for _, key := range keys {
		if err := s.store.Delete(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("media: delete objects: %w", err)
	}
	return nil
}

// DeleteObjects is the job a deletion queues (ADR-0035 Karar 7): it
// deletes every object the media could have. A media that is active again
// when the job runs (restored by staff in between) keeps its objects.
func (s *Service) DeleteObjects(ctx context.Context, id uuid.UUID, kind Kind, uploadKey string) error {
	active, err := queries(s.db.Pool()).MediaExists(ctx, id)
	if err != nil {
		return fmt.Errorf("media: delete objects of %s: %w", id, err)
	}
	if active || s.store == nil {
		return nil
	}
	upload, err := storage.ParseKey(uploadKey)
	if err != nil {
		return fmt.Errorf("media: delete objects of %s: %w", id, river.JobCancel(err))
	}
	if _, ok := kind.Prefix(); !ok {
		return fmt.Errorf("media: delete objects of %s: %w", id, river.JobCancel(fmt.Errorf("unknown kind %q", kind)))
	}
	return s.deleteAll(ctx, kind, id, upload)
}

// Swept is what one sweep removed.
type Swept struct {
	// Expired counts the PENDING uploads the sweep rejected for want of a
	// completion.
	Expired int
	// Deleted counts the unfinished uploads soft-deleted, with their
	// objects.
	Deleted int
	// Kept counts the uploads whose objects could not be deleted; they
	// are taken again next run.
	Kept int
	// Starts counts the purged rows of the budget ledger.
	Starts int64
}

// Sweep removes what unfinished uploads leave (ADR-0035 Karar 4; the
// River side of ADR-0030's "incoming/ süpürme işi"):
//
//  1. Under row locks (SKIP LOCKED, so a running completion is not
//     waited for) uploads older than UploadRetention that are PENDING or
//     REJECTED are taken; the PENDING ones become REJECTED
//     (NOT_UPLOADED). From then on no completion job records them, and a
//     request to complete them queues nothing (the status gate).
//  2. Outside any transaction every object the media could have is
//     deleted: its candidate final keys and its quarantine key. The
//     quarantine also expires by the bucket's lifecycle rule (ADR-0030,
//     "Zorunlu ops adımı"); the final keys are what only this sweep
//     covers -- a completion that wrote its object and never recorded it.
//  3. The row is soft-deleted, as the system, for retention. A row whose
//     objects could not be deleted stays REJECTED and is taken again.
//
// The budget ledger's rows older than UploadBudgetWindow are purged too.
func (s *Service) Sweep(ctx context.Context) (Swept, error) {
	var out Swept
	now := s.clock.Now()
	n, err := queries(s.db.Pool()).PurgeUploadStarts(ctx, now.Add(-UploadBudgetWindow))
	if err != nil {
		return out, fmt.Errorf("media: purge upload starts: %w", err)
	}
	out.Starts = n
	seen := map[uuid.UUID]bool{}
	for range sweepBatches {
		taken, expired, err := s.takeUnfinished(ctx, now.Add(-UploadRetention), seen)
		if err != nil {
			return out, err
		}
		out.Expired += expired
		for _, m := range taken {
			seen[m.ID] = true
			if s.store != nil {
				if err := s.deleteAll(ctx, m.Kind, m.ID, m.UploadKey); err != nil {
					s.logger.WarnContext(ctx, "unfinished upload kept: its objects could not be deleted",
						slog.String("id", m.ID.String()), slog.Any("error", err))
					out.Kept++
					continue
				}
			}
			deleted, err := s.retire(ctx, m.ID, now)
			if err != nil {
				return out, err
			}
			if deleted {
				out.Deleted++
			}
		}
		if len(taken) < sweepBatch {
			break
		}
	}
	s.logger.InfoContext(ctx, "media sweep",
		slog.Int("expired", out.Expired), slog.Int("deleted", out.Deleted),
		slog.Int("kept", out.Kept), slog.Int64("upload_starts", out.Starts))
	return out, nil
}

// takeUnfinished is step 1 of Sweep: it rejects the expired PENDING rows
// of one batch and returns the batch, without the rows seen earlier in
// this run.
func (s *Service) takeUnfinished(ctx context.Context, before time.Time, seen map[uuid.UUID]bool) ([]Media, int, error) {
	var taken []Media
	expired := 0
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		mq := queries(q)
		rows, err := mq.LockUnfinished(ctx, mediadb.LockUnfinishedParams{Before: before, Batch: sweepBatch})
		if err != nil {
			return fmt.Errorf("media: lock unfinished uploads: %w", err)
		}
		now := s.clock.Now()
		for i := range rows {
			r := &rows[i]
			if seen[r.ID] {
				continue
			}
			m, err := fromRow(row(*r))
			if err != nil {
				// Unreachable under the table's CHECKs; a row that does not
				// read is left for a person rather than stopping the sweep.
				s.logger.ErrorContext(ctx, "unfinished upload skipped: the row does not read",
					slog.String("id", r.ID.String()), slog.Any("error", err))
				continue
			}
			if m.Status == StatusPending {
				if !CanTransition(m.Status, StatusRejected) {
					return fmt.Errorf("media: %s cannot become %s", m.Status, StatusRejected)
				}
				_, err := mq.MarkRejected(ctx, mediadb.MarkRejectedParams{
					Now: now, Rejection: string(RejectionNotUploaded), ID: m.ID,
				})
				if err != nil {
					return fmt.Errorf("media: expire upload: %w", err)
				}
				m.Status, m.Rejection = StatusRejected, RejectionNotUploaded
				expired++
			}
			taken = append(taken, m)
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("media: sweep: %w", err)
	}
	return taken, expired, nil
}

// retire is step 3 of Sweep: soft-delete the row if it is still a
// REJECTED one.
func (s *Service) retire(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	deleted := false
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		r, err := queries(q).LockMedia(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: lock media: %w", err)
		}
		if Status(r.Status) != StatusRejected {
			return nil
		}
		_, err = s.deleter.Delete(ctx, q, softdelete.DeleteRequest{
			Table: tableMedia, ID: id,
			Actor:  softdelete.Actor{Kind: softdelete.ActorSystem},
			Reason: softdelete.ReasonRetention, At: now,
		})
		if err != nil {
			return fmt.Errorf("media: retire upload: %w", err)
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("media: sweep: %w", err)
	}
	return deleted, nil
}
