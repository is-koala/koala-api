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
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// Queue is the River queue of the completion job (ADR-0035 Karar 2).
// It is a queue of its own, apart from the default queue's mail and
// verification code jobs: decoding a 50 megapixel image holds a CPU for
// seconds and a raster of up to 200 MiB (RGBA), twice when an orientation
// is applied, and a code job queued behind it would wait out a code's ten
// minutes. QueueWorkers is its worker count per instance.
const Queue = "media"

// QueueWorkers is one per instance. The worst case of one completion is
// about 400 MiB of rasters (the decoded image and its rotated copy at the
// canvas limit), so one worker bounds the memory a burst of uploads can
// take; a typical phone photo (12 megapixels) completes in well under a
// second, so one worker still clears dozens a minute, and instances scale
// it out. Raise it with the instance's memory, not before.
const QueueWorkers = 1

// Attempts of the media jobs (ADR-0035 Karar 2). River waits attempt⁴
// seconds between attempts: eight attempts spend about two and a half
// hours, well inside the 24 hours after which the sweep takes the upload
// anyway; ten deletions spend about seven hours.
const (
	completeAttempts      = 8
	deleteObjectsAttempts = 10
)

// SweepGrace keeps the sweep away from an upload whose completion was asked
// for recently: its job may be writing the object right now. Past it the
// job has either recorded the outcome or is retrying, and a retry that
// finds its row gone deletes what it wrote (Complete).
const SweepGrace = 15 * time.Minute

// UnboundRetention is how long a READY media nothing bound is kept
// (ADR-0035 Karar 8): a week for the client to attach what it uploaded.
// Without it the bucket is free image hosting, and an identity photo
// whose review was never submitted is kept for good.
const UnboundRetention = 7 * 24 * time.Hour

// completeArgs is the completion of one upload. The job reads the row as
// it stands when it runs; the kind and the quarantine key travel along so
// that a run that finds its row gone can still delete what an earlier run
// of it wrote. It is unique by the media's id alone.
type completeArgs struct {
	Media     uuid.UUID `json:"media" river:"unique"`
	MediaKind string    `json:"kind"`
	UploadKey string    `json:"upload_key"`
}

// Kind is the job kind in River's tables.
func (completeArgs) Kind() string { return "media_complete" }

// InsertOpts are the job's River options. The job runs on Queue. It is
// unique by the media's id in River's default states (waiting, running,
// retrying and completed): a second request to complete queues nothing
// while one job waits or runs. A discarded or canceled job does not block
// a new one, so the owner can try again after River gave up.
func (completeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: Queue, MaxAttempts: completeAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// deleteObjectsArgs is the deletion of the objects a media id may have
// left. It carries the kind and the quarantine key because the row may no
// longer be readable when the job runs. Keep, when set, is a final key
// the job never deletes: the object a completion recorded, which a row
// that disappears before the job runs (an account deletion's cascade) may
// get back with a restore (ADR-0035 Karar 7).
type deleteObjectsArgs struct {
	Media     uuid.UUID `json:"media"`
	MediaKind string    `json:"kind"`
	UploadKey string    `json:"upload_key"`
	Keep      string    `json:"keep,omitempty"`
}

// Kind is the job kind in River's tables.
func (deleteObjectsArgs) Kind() string { return "media_delete_objects" }

// InsertOpts are the job's River options.
func (deleteObjectsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: deleteObjectsAttempts}
}

// SweepArgs is the periodic sweep of unfinished and unbound media and of
// the budget ledger (ADR-0035 Karar 4, 6 and 8). internal/app schedules
// it.
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
	a := job.Args
	return w.service.Complete(ctx, a.Media, Kind(a.MediaKind), a.UploadKey)
}

type deleteObjectsWorker struct {
	river.WorkerDefaults[deleteObjectsArgs]

	service *Service
}

func (w *deleteObjectsWorker) Work(ctx context.Context, job *river.Job[deleteObjectsArgs]) error {
	a := job.Args
	return w.service.DeleteObjects(ctx, a.Media, Kind(a.MediaKind), a.UploadKey, a.Keep)
}

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]

	service *Service
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	_, err := w.service.Sweep(ctx)
	return err
}

// queueDeleteObjects queues the deletion of the objects of the media id
// in q's transaction: a guaranteed side effect, committed with the
// decision that makes the objects leftovers (CLAUDE.md, ADR-0010). keep is
// a final key the job must not delete, or "".
func (s *Service) queueDeleteObjects(ctx context.Context, q db.Querier, id uuid.UUID, kind, uploadKey, keep string) error {
	args := deleteObjectsArgs{Media: id, MediaKind: kind, UploadKey: uploadKey, Keep: keep}
	if _, err := s.queue.InsertTx(ctx, q.PgxTx(), args, nil); err != nil {
		return fmt.Errorf("media: queue the deletion of the objects: %w", err)
	}
	return nil
}

// Complete is the completion job (ADR-0029 Karar 6, ADR-0035 Karar 2):
// validate, strip and write the quarantine object to its final key, then
// record the outcome. The request that queued it answered 202; the owner
// reads the outcome from the media.
//
// The flow is "decide, call out, record", with no transaction open
// during the object store's calls (CLAUDE.md):
//
//  1. Under the row's lock the row must still be PENDING; otherwise the
//     job has nothing to complete (an earlier run decided it). Pipeline.
//     Complete is therefore only ever called for a PENDING row, which is
//     its precondition (ADR-0030). A row that is gone (the owner or the
//     sweep deleted it) may still have an object an earlier run of this
//     job wrote before its recording failed; the run deletes every object
//     of the id, as its own retried work.
//  2. Pipeline.Complete runs. A Rejection is a business outcome; an error
//     is the store failing and is retried.
//  3. Under the lock again the outcome is written, PENDING → READY or
//     REJECTED, if the row is still PENDING, and in the same transaction
//     the deletion of every object of the id that the row does not name
//     is queued (the quarantine key, the id's other extensions, or, when
//     the row is gone or was decided meanwhile, the object this run
//     wrote): a guaranteed side effect, never a best-effort call.
//
// Error classes:
//
//   - permanent: a row that does not read (fromRow) cannot be completed by
//     any retry; the job is canceled. The row stays PENDING until the
//     sweep takes it after UploadRetention -- the sweep reads only the
//     id, kind and keys as strings and the object job cancels on a key
//     that does not parse, so such a row ends deleted with its objects
//     left to the lifecycle rule and the log;
//   - transient: the store or the database failed; River retries
//     (completeAttempts). An object the pipeline already wrote is kept
//     across the retry -- the next run finds it at its final key
//     (Pipeline.finishedEarlier) instead of answering NOT_UPLOADED for an
//     upload that happened.
//
// When step 3's own transaction fails its outcome may be unknown
// (ADR-0017), so the object is kept (WriteIndeterminate): the retry
// either records it or, finding the row gone, deletes it (step 1).
//
// kind and uploadKey are the job's arguments, the row's at the time the
// completion was asked for.
func (s *Service) Complete(ctx context.Context, id uuid.UUID, kind Kind, uploadKey string) error {
	if s.pipeline == nil {
		return errNoObjectStore
	}
	var m Media
	var pending, gone bool
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		r, err := queries(q).LockMedia(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			gone = true
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
	if gone {
		return s.DeleteObjects(ctx, id, kind, uploadKey, "")
	}
	if !pending {
		return nil
	}

	done, err := s.pipeline.Complete(ctx, m.Kind, m.UploadKey)
	if err != nil {
		return fmt.Errorf("media: complete %s: %w", id, err)
	}

	err = s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		mq := queries(q)
		r, err := mq.LockMedia(ctx, id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Deleted meanwhile; the deletion's own job may have run before
			// the object was written.
			if done.Rejected() {
				return nil
			}
			return s.queueDeleteObjects(ctx, q, id, string(m.Kind), m.UploadKey.String(), "")
		case err != nil:
			return fmt.Errorf("media: lock media: %w", err)
		}
		from := Status(r.Status)
		if from != StatusPending {
			// Decided meanwhile: the unique job rules out another run, so
			// this is a row the sweep or a person took. Whatever this run
			// wrote that the row does not name goes (DeleteObjects keeps
			// the row's own object_key).
			if done.Rejected() {
				return nil
			}
			keep := ""
			if r.ObjectKey != nil {
				keep = *r.ObjectKey
			}
			return s.queueDeleteObjects(ctx, q, id, r.Kind, r.UploadKey, keep)
		}
		now := s.clock.Now()
		if done.Rejected() {
			if !CanTransition(from, StatusRejected) {
				return fmt.Errorf("media: %s cannot become %s", from, StatusRejected)
			}
			_, err = mq.MarkRejected(ctx, mediadb.MarkRejectedParams{Now: now, Rejection: string(done.Rejection), ID: id})
			if err != nil {
				return fmt.Errorf("media: record the outcome: %w", err)
			}
			return nil
		}
		if !CanTransition(from, StatusReady) {
			return fmt.Errorf("media: %s cannot become %s", from, StatusReady)
		}
		_, err = mq.MarkReady(ctx, mediadb.MarkReadyParams{
			Now: now, ObjectKey: done.Key.String(), ContentType: string(done.Type), SizeBytes: done.Size, ID: id,
		})
		if err != nil {
			return fmt.Errorf("media: record the outcome: %w", err)
		}
		// The id's other extensions and the quarantine key are leftovers
		// now. The object just recorded is named in the job: a job that
		// read only the active row would delete it when an account
		// deletion cascades to the row before the job runs, and the
		// account's restore would bring back a READY row with no object.
		return s.queueDeleteObjects(ctx, q, id, r.Kind, r.UploadKey, done.Key.String())
	})
	if err != nil {
		if !done.Rejected() {
			s.pipeline.Discard(ctx, done.Key, WriteIndeterminate)
		}
		return fmt.Errorf("media: complete %s: %w", id, err)
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

// DeleteObjects deletes every object the media id of kind may have that
// nothing keeps (ADR-0035 Karar 4 and 7): its candidate final keys and its
// quarantine key. keep, the job's own argument, is never deleted, whatever
// the row looks like when the job runs; a recorded completion names its
// object there. Beyond it, what stays depends on the active row with the
// id, read when the job runs:
//
//   - none (deleted by its owner, by the sweep, with its account): the
//     rest goes;
//   - READY: its object_key stays too;
//   - REJECTED: the rest goes;
//   - PENDING: nothing is touched -- the upload is still the completion's
//     (a restored row, or a job queued for a row that came back).
//
// An unfinished upload does not survive its account's deletion: a
// completion run that finds its row gone deletes the id's objects, and a
// restored PENDING row is answered NOT_UPLOADED by its next completion and
// swept. Only a recorded object is kept for a restore (keep).
//
// It is the job a deletion, a recorded completion and the sweep queue in
// their transactions, and what a completion run does itself when it finds
// its row gone. A failure is returned and River retries it.
func (s *Service) DeleteObjects(ctx context.Context, id uuid.UUID, kind Kind, uploadKey, keep string) error {
	if s.store == nil {
		return nil
	}
	var named string
	active, err := queries(s.db.Pool()).ActiveObject(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("media: delete objects of %s: %w", id, err)
	case Status(active.Status) == StatusPending:
		return nil
	case active.ObjectKey != nil:
		named = *active.ObjectKey
	}
	upload, err := storage.ParseKey(uploadKey)
	if err != nil {
		return fmt.Errorf("media: delete objects of %s: %w", id, river.JobCancel(err))
	}
	keys, err := candidates(kind, id)
	if err != nil {
		return fmt.Errorf("media: delete objects of %s: %w", id, river.JobCancel(err))
	}
	keys = append(keys, upload)
	var errs []error
	for _, key := range keys {
		if k := key.String(); k == keep || k == named {
			continue
		}
		if err := s.store.Delete(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("media: delete objects of %s: %w", id, err)
	}
	return nil
}

// Swept is what one sweep removed.
type Swept struct {
	// Expired counts the PENDING uploads the sweep rejected for want of a
	// completion.
	Expired int
	// Deleted counts the media soft-deleted, unfinished and unbound
	// together; each has its object deletion queued.
	Deleted int
	// Unbound counts the READY media among Deleted that nothing bound.
	Unbound int
	// Adopted counts the READY media the sweep found bound without a
	// bound_at -- a consuming row on a restrict edge points at them, so a
	// consumer forgot Bind. The sweep binds them (BindMedia), which takes
	// them out of every later listing, and the metric's alert names the
	// bug.
	Adopted int
	// Kept counts the media whose deletion failed otherwise; they are
	// logged and left, and the sweep pages past them.
	Kept int
	// Starts counts the purged rows of the budget ledger.
	Starts int64
}

// sweepBounds are the moments a sweep compares against.
type sweepBounds struct {
	unfinished, requested, unbound time.Time
}

// Sweep removes the media nobody will complete or use (ADR-0035 Karar 4
// and 8; the River side of ADR-0030's "incoming/ süpürme işi"):
//
//   - an upload that did not end READY, older than UploadRetention, whose
//     completion was not asked for within SweepGrace;
//   - a READY media nothing bound, decided more than UnboundRetention ago.
//
// It pages through them by (created_at, id) and retires each in a
// transaction of its own: the row is locked again (SKIP LOCKED, so a
// running completion is not waited for), a PENDING one becomes REJECTED
// (NOT_UPLOADED) so that no completion records it, the row is soft-deleted
// as the system for retention, and the deletion of every object of its id
// is queued in the same transaction. A media whose retirement fails (a
// consuming row on a restrict edge, say) is logged and left, and the cursor
// moves past it, so a run never stalls on it. The objects' deletion is a
// retried job of its own and cannot stall the sweep either.
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
	b := sweepBounds{
		unfinished: now.Add(-UploadRetention),
		requested:  now.Add(-SweepGrace),
		unbound:    now.Add(-UnboundRetention),
	}
	var afterAt time.Time
	var afterID uuid.UUID
	for range sweepBatches {
		rows, err := queries(s.db.Pool()).ListSweepable(ctx, mediadb.ListSweepableParams{
			AfterCreatedAt: afterAt, AfterID: afterID,
			UnfinishedBefore: b.unfinished, RequestedBefore: b.requested, UnboundBefore: b.unbound,
			Batch: sweepBatch,
		})
		if err != nil {
			return out, fmt.Errorf("media: list sweepable media: %w", err)
		}
		for i := range rows {
			r := &rows[i]
			afterAt, afterID = r.CreatedAt, r.ID
			status, err := s.retire(ctx, r, b, now)
			if err != nil {
				adopted, adoptErr := s.adopt(ctx, r, err, now)
				if adopted {
					s.logger.ErrorContext(ctx, "media bound by the sweep: a consumer points at it without media.Bind",
						slog.String("id", r.ID.String()))
					out.Adopted++
					s.metrics.swept(sweptAdopted)
					continue
				}
				s.logger.WarnContext(ctx, "media kept by the sweep: its retirement failed",
					slog.String("id", r.ID.String()), slog.Any("error", errors.Join(err, adoptErr)))
				out.Kept++
				s.metrics.swept(sweptKept)
				continue
			}
			if status != "" {
				s.metrics.swept(sweptDeleted)
			}
			switch status {
			case "":
			case StatusPending:
				out.Expired++
				out.Deleted++
			case StatusReady:
				out.Unbound++
				out.Deleted++
			case StatusRejected:
				out.Deleted++
			}
		}
		if len(rows) < sweepBatch {
			break
		}
	}
	s.logger.InfoContext(ctx, "media sweep",
		slog.Int("expired", out.Expired), slog.Int("deleted", out.Deleted), slog.Int("unbound", out.Unbound),
		slog.Int("adopted", out.Adopted), slog.Int("kept", out.Kept), slog.Int64("upload_starts", out.Starts))
	return out, nil
}

// adopt answers a retirement that failed on a restrict edge
// (SOFT_DELETE_RESTRICTED) for a READY media: an active consuming row
// points at it, so it is bound in fact, and the consumer did not call
// Bind. Leaving it unbound would list it, fail it and count it on every
// run, and enough of them would spend each run's pages before the sweep
// reached anything it can remove. It is bound through BindMedia, the one
// writer of bound_at, so no later listing returns it (ADR-0035 Karar 8).
// A media whose retirement failed otherwise, or that is not READY, is not
// adopted.
//
// The listing does not exclude such media itself: the consuming tables do
// not exist yet and their restrict edges are the catalog's, so a static
// query cannot name them; the restrict check is the platform's
// (softdelete.Delete), and this is where its answer arrives.
func (s *Service) adopt(ctx context.Context, r *mediadb.ListSweepableRow, cause error, now time.Time) (bool, error) {
	var ae *apperr.Error
	if !errors.As(cause, &ae) || ae.Code != softdelete.CodeRestricted {
		return false, nil
	}
	_, err := queries(s.db.Pool()).BindMedia(ctx, mediadb.BindMediaParams{Now: now, ID: r.ID, OwnerID: r.OwnerID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("media: bind restricted media: %w", err)
	}
	return true, nil
}

// retire retires one media of the sweep and returns the status it had,
// or "" when it no longer qualifies (completed, bound, deleted or held by
// a completion meanwhile).
func (s *Service) retire(ctx context.Context, r *mediadb.ListSweepableRow, b sweepBounds, now time.Time) (Status, error) {
	var was Status
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		mq := queries(q)
		status, err := mq.LockSweepable(ctx, mediadb.LockSweepableParams{
			ID: r.ID, UnfinishedBefore: b.unfinished, RequestedBefore: b.requested, UnboundBefore: b.unbound,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: lock sweepable media: %w", err)
		}
		was = Status(status)
		if was == StatusPending {
			if !CanTransition(was, StatusRejected) {
				return fmt.Errorf("media: %s cannot become %s", was, StatusRejected)
			}
			if _, err := mq.MarkRejected(ctx, mediadb.MarkRejectedParams{
				Now: now, Rejection: string(RejectionNotUploaded), ID: r.ID,
			}); err != nil {
				return fmt.Errorf("media: expire upload: %w", err)
			}
		}
		if _, err := s.deleter.Delete(ctx, q, softdelete.DeleteRequest{
			Table: tableMedia, ID: r.ID,
			Actor:  softdelete.Actor{Kind: softdelete.ActorSystem},
			Reason: softdelete.ReasonRetention, At: now,
		}); err != nil {
			return fmt.Errorf("media: retire media: %w", err)
		}
		return s.queueDeleteObjects(ctx, q, r.ID, r.Kind, r.UploadKey, "")
	})
	if err != nil {
		return "", fmt.Errorf("media: sweep %s: %w", r.ID, err)
	}
	return was, nil
}
