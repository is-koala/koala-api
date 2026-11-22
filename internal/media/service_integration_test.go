//go:build integration

package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/media/mediadb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	fakeclock "github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/fakes"
	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// app is the media service on a real database, a fake object store and a
// fake clock. Jobs are queued in river_job by an insert-only client and
// run by calling the service's job methods, so a test decides when.
type app struct {
	svc     *media.Service
	db      *db.DB
	dbt     dbtest.DB
	store   *storage.Store
	fake    *fakes.S3
	clock   *fakeclock.Fake
	catalog *softdelete.Catalog
}

func newApp(t *testing.T) *app {
	t.Helper()
	return newAppWith(t, dbtest.SoftDeleteCatalog(t))
}

// newAppWith is newApp with the soft-delete catalog the deletions follow.
func newAppWith(t *testing.T, catalog *softdelete.Catalog) *app {
	t.Helper()
	d := dbtest.New(t)
	clk := fakeclock.NewFake(time.Date(2026, time.November, 22, 9, 0, 0, 0, time.UTC))
	fake := fakes.NewS3(t, testBucket)
	store, err := storage.New(storage.Config{
		Endpoint: fake.Endpoint(), Region: "eu-central-1", Bucket: testBucket,
		AccessKeyID: "media-test-access-key", SecretAccessKey: "media-test-secret",
		PathStyle: true, PublicBaseURL: "https://cdn.example.com",
	}, clk, fake.Client())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{dbt: d, store: store, fake: fake, clock: clk, catalog: catalog}
	a.reopen(t)
	return a
}

// reopen opens a new koala_app pool and a service on it, for a test that
// closed the old pool to make a transaction fail.
func (a *app) reopen(t *testing.T) {
	t.Helper()
	database := a.dbt.Open(t, db.RoleApp)
	logger := log.New(&bytes.Buffer{}, slog.LevelDebug)
	queue, err := jobs.NewClient(database.Pool(), jobs.Config{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := media.NewService(database, queue, a.store, softdelete.New(a.catalog), a.clock, logger)
	if err != nil {
		t.Fatal(err)
	}
	a.db, a.svc = database, svc
}

// user writes an active user and returns its id.
func (a *app) user(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	err := authndb.New(a.db.Pool()).InsertUser(t.Context(), authndb.InsertUserParams{
		ID: id, Email: "medya-" + id.String() + "@example.com", Status: authn.UserActive, CreatedAt: a.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// noCommit is the commit of a start that stores no Idempotency-Key answer.
func noCommit(context.Context, db.Querier, media.Started) error { return nil }

// start starts an upload of kind for owner.
func (a *app) start(t *testing.T, owner uuid.UUID, kind media.Kind) media.Started {
	t.Helper()
	s, err := a.svc.Start(t.Context(), owner, kind, 524288, noCommit)
	if err != nil {
		t.Fatalf("Start(%s) = %v", kind, err)
	}
	return s
}

// put writes body to the upload's quarantine key, as the client does
// through the write address.
func (a *app) put(s media.Started, body []byte) {
	a.fake.Seed(s.Media.UploadKey.String(), fakes.S3Object{Body: body, ContentType: "application/octet-stream"})
}

// run runs the completion job of m with its arguments.
func (a *app) run(ctx context.Context, m media.Media) error {
	return a.svc.Complete(ctx, m.ID, m.Kind, m.UploadKey.String())
}

// complete requests the completion, runs its job and the object deletions
// it queued, and reads the media back.
func (a *app) complete(t *testing.T, owner, id uuid.UUID) media.Media {
	t.Helper()
	m, err := a.svc.RequestCompletion(t.Context(), owner, id)
	if err != nil {
		t.Fatalf("RequestCompletion() = %v", err)
	}
	if err := a.run(t.Context(), m); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	a.deleteObjects(t)
	m, err = a.svc.Get(t.Context(), owner, id)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	return m
}

// queued counts the jobs of kind for the media id.
func (a *app) queued(t *testing.T, kind string, id uuid.UUID) int {
	t.Helper()
	var n int
	err := a.db.Pool().QueryRow(t.Context(),
		"SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'media' = $2", kind, id.String()).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// deleteObjects runs every waiting object deletion job, as River would,
// and marks it completed.
func (a *app) deleteObjects(t *testing.T) {
	t.Helper()
	if err := a.runDeleteJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// runDeleteJobs is deleteObjects for a caller without a *testing.T (a
// fake's hook).
func (a *app) runDeleteJobs(ctx context.Context) error {
	rows, err := a.db.Pool().Query(ctx,
		"SELECT id, args FROM river_job WHERE kind = 'media_delete_objects' AND state = 'available' ORDER BY id")
	if err != nil {
		return err
	}
	type job struct {
		id   int64
		args []byte
	}
	var queue []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.args); err != nil {
			rows.Close()
			return err
		}
		queue = append(queue, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, j := range queue {
		var args struct {
			Media     uuid.UUID `json:"media"`
			Kind      string    `json:"kind"`
			UploadKey string    `json:"upload_key"`
		}
		if err := json.Unmarshal(j.args, &args); err != nil {
			return err
		}
		if err := a.svc.DeleteObjects(ctx, args.Media, media.Kind(args.Kind), args.UploadKey); err != nil {
			return err
		}
		if _, err := a.db.Pool().Exec(ctx, "UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1", j.id); err != nil {
			return err
		}
	}
	return nil
}

// finalKey is the final key of the image id with ext.
func finalKey(t *testing.T, id uuid.UUID, ext storage.Ext) storage.Key {
	t.Helper()
	k, err := storage.NewKey(storage.PrefixImages, id, ext)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func wantCode(t *testing.T, err error, code apperr.Code) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

// MED-001, MED-005: an upload is started (a PENDING row and a short lived
// write address), written, completed (validated, stripped of its GPS
// marker and written to its final key) and read; an upload that is never
// completed is gone after its retention.
func TestUploadIsStartedCompletedAndSwept(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)

	s := a.start(t, owner, media.KindProfilePicture)
	if s.Media.Status != media.StatusPending {
		t.Fatalf("MED-001: started media is %s, want PENDING", s.Media.Status)
	}
	if want := a.clock.Now().Add(media.UploadTTL); !s.ExpiresAt.Equal(want) {
		t.Errorf("MED-001: write address expires %v, want %v", s.ExpiresAt, want)
	}
	if !strings.Contains(s.UploadURL, "/incoming/"+s.Media.ID.String()+".jpg") {
		t.Errorf("MED-001: write address %s is not the quarantine key's", s.UploadURL)
	}

	body, marker := jpegWithGPS(t, 6)
	a.put(s, body)
	m := a.complete(t, owner, s.Media.ID)
	if m.Status != media.StatusReady || m.ContentType != imaging.TypeJPEG {
		t.Fatalf("MED-001: completed media = %+v, want a READY JPEG", m)
	}
	want := finalKey(t, s.Media.ID, storage.ExtJPEG)
	if m.ObjectKey != want {
		t.Errorf("MED-001: object key %s, want %s", m.ObjectKey, want)
	}
	stored, ok := a.fake.Object(want.String())
	if !ok {
		t.Fatal("MED-001: the final object is not in the store")
	}
	if bytes.Contains(stored.Body, marker) {
		t.Error("MED-005: the stored image still carries its GPS marker")
	}
	if _, ok := a.fake.Object(s.Media.UploadKey.String()); ok {
		t.Error("MED-001: the quarantine object is still there")
	}
	addr, err := a.svc.Address(t.Context(), m, true)
	if err != nil || addr.URL != "https://cdn.example.com/"+want.String() || addr.ExpiresAt != nil {
		t.Errorf("Address() = %+v, %v, want the permanent public URL", addr, err)
	}
	// Bound, so the unbound sweep leaves it alone below.
	if err := a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		return media.Bind(ctx, q, a.clock.Now(), owner, m.ID)
	}); err != nil {
		t.Fatal(err)
	}

	// Another upload is started and never completed.
	abandoned := a.start(t, owner, media.KindProfilePicture)
	a.clock.Advance(media.UploadRetention + time.Minute)
	swept, err := a.svc.Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if swept.Expired != 1 || swept.Deleted != 1 {
		t.Errorf("MED-001: sweep = %+v, want the abandoned upload expired and deleted", swept)
	}
	_, err = a.svc.Get(t.Context(), owner, abandoned.Media.ID)
	wantCode(t, err, media.CodeNotFound)
	if n := a.queued(t, "media_delete_objects", abandoned.Media.ID); n != 1 {
		t.Errorf("the sweep queued %d object deletions for the abandoned upload, want 1", n)
	}
	if got, err := a.svc.Get(t.Context(), owner, s.Media.ID); err != nil || got.Status != media.StatusReady {
		t.Errorf("the sweep touched a READY media: %+v, %v", got, err)
	}
	a.deleteObjects(t)
	if _, ok := a.fake.Object(want.String()); !ok {
		t.Error("the sweep deleted a READY media's object")
	}
}

// MED-002: the keys are the server's. The row's id is the identifier of
// both keys, the quarantine extension is the kind's first type, and the
// final extension is the type detected in the bytes: a PNG written to
// the .jpg address becomes images/<id>.png.
func TestKeysAreTheServersAndCarryTheRowsID(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)

	s := a.start(t, owner, media.KindProfilePicture)
	if s.Media.UploadKey.ID() != s.Media.ID || s.Media.UploadKey.Prefix() != storage.PrefixIncoming ||
		s.Media.UploadKey.Ext() != storage.ExtJPEG {
		t.Fatalf("MED-002: quarantine key %s, want incoming/<row id>.jpg", s.Media.UploadKey)
	}
	a.put(s, pngBytes(t, 8, 8))
	m := a.complete(t, owner, s.Media.ID)
	if want := finalKey(t, s.Media.ID, storage.ExtPNG); m.ObjectKey != want {
		t.Errorf("MED-002: final key %s, want %s (the detected type names it)", m.ObjectKey, want)
	}

	cv := a.start(t, owner, media.KindCV)
	if cv.Media.UploadKey.Ext() != storage.ExtPDF {
		t.Errorf("a CV's quarantine key is %s, want the kind's first type (ADR-0031 Karar 3)", cv.Media.UploadKey)
	}
}

// ADR-0035 Karar 4 says the database holds the key rules too: a final key
// under another kind's prefix, with an extension its type does not have,
// or a type the kind does not accept is refused by a CHECK, whatever the
// application writes.
func TestDatabaseHoldsTheKeyRules(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	s := a.start(t, owner, media.KindProfilePicture)
	id := s.Media.ID
	for name, tt := range map[string]struct{ key, typ string }{
		"another kind's prefix":       {"identity-documents/" + id.String() + ".jpg", "image/jpeg"},
		"another type's extension":    {"images/" + id.String() + ".png", "image/jpeg"},
		"a type the kind refuses":     {"images/" + id.String() + ".pdf", "application/pdf"},
		"another row's id in the key": {"images/" + uuid.Must(uuid.NewV7()).String() + ".jpg", "image/jpeg"},
	} {
		_, err := mediadb.New(a.db.Pool()).MarkReady(t.Context(), mediadb.MarkReadyParams{
			Now: a.clock.Now(), ObjectKey: tt.key, ContentType: tt.typ, SizeBytes: 1, ID: id,
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: MarkReady(%s, %s) = %v, want a check violation", name, tt.key, tt.typ, err)
		}
	}
	// The right one passes, so the refusals above are not a broken query.
	_, err := mediadb.New(a.db.Pool()).MarkReady(t.Context(), mediadb.MarkReadyParams{
		Now: a.clock.Now(), ObjectKey: "images/" + id.String() + ".jpg", ContentType: "image/jpeg", SizeBytes: 1, ID: id,
	})
	if err != nil {
		t.Fatalf("a well formed READY row: %v", err)
	}
}

// ready starts an upload, writes a JPEG and completes it.
func (a *app) ready(t *testing.T, owner uuid.UUID, kind media.Kind) media.Media {
	t.Helper()
	s := a.start(t, owner, kind)
	body, _ := jpegWithGPS(t, 1)
	a.put(s, body)
	m := a.complete(t, owner, s.Media.ID)
	if m.Status != media.StatusReady {
		t.Fatalf("setup: %+v", m)
	}
	return m
}

// The status gate (ADR-0030, ADR-0035 Karar 2). A READY upload's write
// address still works for its fifteen minutes; a client that writes a
// second object of another type there and completes again must get
// nothing. Three mechanisms stand in the way and each is held on its own,
// so deleting one turns its subtest red even though the other two would
// still stop the orphan:
//
//   - "request": the request does not queue a job for a decided row. The
//     first job's row is removed from river_job first (River's cleanup
//     does that a day later), because the unique option would otherwise
//     refuse the second job and keep this subtest green without the gate;
//   - "job": the job does not call the pipeline for a decided row; the
//     store sees no read of the quarantine object;
//   - "orphan": a run that wrote an object for a row deleted meanwhile
//     queues its deletion in its recording transaction (MED-006). The
//     owner's own deletion job runs before the object is written, so only
//     the run's job can remove it.
//   - "decided": the same for a row decided meanwhile (the sweep or a
//     person rejected it); its deletion is queued from the row's branch.
func TestCompletionGateHoldsForADecidedUpload(t *testing.T) {
	t.Parallel()

	png := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		return finalKey(t, id, storage.ExtPNG).String()
	}

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		m := a.ready(t, owner, media.KindProfilePicture)
		if _, err := a.db.Pool().Exec(t.Context(), "DELETE FROM river_job WHERE kind = 'media_complete'"); err != nil {
			t.Fatal(err)
		}
		got, err := a.svc.RequestCompletion(t.Context(), owner, m.ID)
		if err != nil || got.Status != media.StatusReady {
			t.Fatalf("RequestCompletion() = %+v, %v, want the READY row as it is", got, err)
		}
		if n := a.queued(t, "media_complete", m.ID); n != 0 {
			t.Errorf("a READY upload queued %d completion jobs, want none", n)
		}
	})
	t.Run("job", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		m := a.ready(t, owner, media.KindProfilePicture)
		a.fake.Seed(m.UploadKey.String(), fakes.S3Object{Body: pngBytes(t, 4, 4)})
		path := "/" + testBucket + "/" + m.UploadKey.String()
		before := len(a.fake.RequestsTo(http.MethodGet, path)) + len(a.fake.RequestsTo(http.MethodHead, path))
		if err := a.run(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if after := len(a.fake.RequestsTo(http.MethodGet, path)) + len(a.fake.RequestsTo(http.MethodHead, path)); after != before {
			t.Errorf("the job read the quarantine of a READY upload (%d requests, want %d)", after, before)
		}
		if _, ok := a.fake.Object(png(t, m.ID)); ok {
			t.Error("a second final object was written for a READY upload")
		}
	})
	t.Run("orphan", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		a.put(s, pngBytes(t, 4, 4))
		m, err := a.svc.RequestCompletion(t.Context(), owner, s.Media.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The owner deletes the upload while the job writes its object,
		// and the deletion's own job runs before the object lands.
		hook := make(chan error, 1)
		ctx := t.Context()
		a.fake.Before(http.MethodPut, "/"+testBucket+"/"+png(t, s.Media.ID), func() {
			if err := a.svc.Delete(ctx, owner, s.Media.ID); err != nil {
				hook <- err
				return
			}
			hook <- a.runDeleteJobs(ctx)
		})
		if err := a.run(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := <-hook; err != nil {
			t.Fatalf("the delete during the write: %v", err)
		}
		if _, ok := a.fake.Object(png(t, s.Media.ID)); !ok {
			t.Fatal("setup: the object was not written after the deletion's job ran")
		}
		a.deleteObjects(t)
		if _, ok := a.fake.Object(png(t, s.Media.ID)); ok {
			t.Error("MED-006: the object of a row deleted meanwhile was kept; no row will ever name it")
		}
	})
	t.Run("decided", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		a.put(s, pngBytes(t, 4, 4))
		m, err := a.svc.RequestCompletion(t.Context(), owner, s.Media.ID)
		if err != nil {
			t.Fatal(err)
		}
		hook := make(chan error, 1)
		ctx := t.Context()
		a.fake.Before(http.MethodPut, "/"+testBucket+"/"+png(t, s.Media.ID), func() {
			_, err := mediadb.New(a.db.Pool()).MarkRejected(ctx, mediadb.MarkRejectedParams{
				Now: a.clock.Now(), Rejection: string(media.RejectionNotUploaded), ID: s.Media.ID,
			})
			hook <- err
		})
		if err := a.run(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := <-hook; err != nil {
			t.Fatal(err)
		}
		if n := a.queued(t, "media_delete_objects", s.Media.ID); n != 1 {
			t.Fatalf("%d object deletions queued for a row rejected meanwhile, want 1", n)
		}
		a.deleteObjects(t)
		if _, ok := a.fake.Object(png(t, s.Media.ID)); ok {
			t.Error("MED-006: the object of a row decided meanwhile was kept")
		}
	})
}

// MED-006: an object whose recording failed with an unknown outcome is
// kept, and the retry records it. The recording transaction fails because
// its pool is gone, not because a context ended: the object is deleted on
// a context of its own (Pipeline.Discard), so a test that canceled the
// job's context would stay green with WriteIndeterminate turned into
// WriteFailed. This one is red then.
func TestCompletionKeepsAnObjectItCouldNotRecord(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	s := a.start(t, owner, media.KindProfilePicture)
	a.put(s, pngBytes(t, 4, 4))
	m, err := a.svc.RequestCompletion(t.Context(), owner, s.Media.ID)
	if err != nil {
		t.Fatal(err)
	}
	final := finalKey(t, s.Media.ID, storage.ExtPNG).String()

	// After the object is written and before the outcome is recorded, the
	// service's pool closes: the recording transaction cannot begin.
	old := a.db
	a.fake.Before(http.MethodDelete, "/"+testBucket+"/"+s.Media.UploadKey.String(), old.Close)
	if err := a.run(t.Context(), m); err == nil {
		t.Fatal("MED-006: Complete() = nil with a recording that could not run")
	}
	if _, ok := a.fake.Object(final); !ok {
		t.Fatal("MED-006: the object of an unrecorded outcome was deleted")
	}

	// The retry, on a working pool, finds the object at its final key and
	// records it.
	a.reopen(t)
	if got, err := a.svc.Get(t.Context(), owner, s.Media.ID); err != nil || got.Status != media.StatusPending {
		t.Fatalf("after the failed run: %+v, %v, want PENDING", got, err)
	}
	if err := a.run(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	got, err := a.svc.Get(t.Context(), owner, s.Media.ID)
	if err != nil || got.Status != media.StatusReady || got.ObjectKey.String() != final {
		t.Fatalf("MED-006: after the retry %+v, %v, want READY at %s", got, err, final)
	}
}

// ADR-0035 Karar 4, the orphan sweep of the final prefixes: a completion
// that wrote its object and was never recorded leaves an object under
// images/ that no lifecycle rule covers. The paths that remove it, each
// held here on its own: the sweep of an expired upload queues the deletion
// of every final key the row's id can have; a later completion of the
// same id queues the deletion of the extensions it did not write; a
// completion whose row is gone deletes the id's objects itself.
func TestOrphanedFinalObjectsAreSwept(t *testing.T) {
	t.Parallel()

	t.Run("expired upload", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		left := finalKey(t, s.Media.ID, storage.ExtJPEG).String()
		a.fake.Seed(left, fakes.S3Object{Body: []byte("written, never recorded"), ContentType: "image/jpeg"})
		a.clock.Advance(media.UploadRetention + time.Minute)
		if _, err := a.svc.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		a.deleteObjects(t)
		if _, ok := a.fake.Object(left); ok {
			t.Error("the sweep left a final object no row names")
		}
	})
	t.Run("other extension", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		left := finalKey(t, s.Media.ID, storage.ExtPNG).String()
		a.fake.Seed(left, fakes.S3Object{Body: []byte("an earlier run's PNG"), ContentType: "image/png"})
		body, _ := jpegWithGPS(t, 1)
		a.put(s, body)
		m := a.complete(t, owner, s.Media.ID)
		if m.Status != media.StatusReady || m.ObjectKey.Ext() != storage.ExtJPEG {
			t.Fatalf("completion: %+v", m)
		}
		if _, ok := a.fake.Object(left); ok {
			t.Error("the completion left the id's other extension in the store")
		}
		if _, ok := a.fake.Object(m.ObjectKey.String()); !ok {
			t.Error("the completion's deletion job took the row's own object")
		}
	})
	t.Run("recorded object", func(t *testing.T) {
		t.Parallel()
		// An earlier run wrote the object and its recording failed; the
		// quarantine is gone. The completion records the object.
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		left := finalKey(t, s.Media.ID, storage.ExtJPEG).String()
		a.fake.Seed(left, fakes.S3Object{Body: []byte("written, never recorded"), ContentType: "image/jpeg"})
		if m := a.complete(t, owner, s.Media.ID); m.Status != media.StatusReady || m.ObjectKey.String() != left {
			t.Fatalf("MED-006: %+v, want READY at %s", m, left)
		}
		if _, ok := a.fake.Object(left); !ok {
			t.Error("MED-006: the object of a processed upload was deleted")
		}
	})
	t.Run("row gone", func(t *testing.T) {
		t.Parallel()
		// Spec D4: an earlier run wrote the object, its recording failed,
		// and the row was deleted before the retry. The retry finds no row
		// and deletes the id's objects as its own work.
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		m, err := a.svc.RequestCompletion(t.Context(), owner, s.Media.ID)
		if err != nil {
			t.Fatal(err)
		}
		left := finalKey(t, s.Media.ID, storage.ExtJPEG).String()
		if err := a.svc.Delete(t.Context(), owner, s.Media.ID); err != nil {
			t.Fatal(err)
		}
		a.deleteObjects(t)
		a.fake.Seed(left, fakes.S3Object{Body: []byte("written after the deletion's job"), ContentType: "image/jpeg"})
		if err := a.run(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if _, ok := a.fake.Object(left); ok {
			t.Error("a completion run whose row is gone left the id's object")
		}
	})
}

// ADR-0035 Karar 7: a deletion soft-deletes the row and queues the
// deletion of its objects; the job deletes them unless an active row
// names them, and never touches another media's object (MED-006).
func TestDeletionTakesTheObjectsOfItsMediaOnly(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	first := a.ready(t, owner, media.KindCompanyImage)
	second := a.ready(t, owner, media.KindCompanyImage)
	if err := a.svc.Delete(t.Context(), owner, first.ID); err != nil {
		t.Fatal(err)
	}
	if n := a.queued(t, "media_delete_objects", first.ID); n != 2 {
		t.Fatalf("%d object deletion jobs, want the completion's and the deletion's", n)
	}
	_, err := a.svc.Get(t.Context(), owner, first.ID)
	wantCode(t, err, media.CodeNotFound)

	// A job for the still active media keeps the object its row names.
	if err := a.svc.DeleteObjects(t.Context(), second.ID, media.KindCompanyImage, second.UploadKey.String()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.fake.Object(second.ObjectKey.String()); !ok {
		t.Fatal("the deletion job deleted an active media's object")
	}
	a.deleteObjects(t)
	if _, ok := a.fake.Object(first.ObjectKey.String()); ok {
		t.Error("the deleted media's object is still in the store")
	}
	if _, ok := a.fake.Object(second.ObjectKey.String()); !ok {
		t.Error("MED-006: deleting one media deleted another's object")
	}

	// Another user's media is the same not found, for a deletion too.
	stranger := a.user(t)
	err = a.svc.Delete(t.Context(), stranger, second.ID)
	wantCode(t, err, media.CodeNotFound)
}

// ADR-0035 Karar 6: at most MaxOpenUploads open uploads, and DailyUploads
// starts a day, both 429 with a wait. Deleting an open upload frees its
// place in the open cap and not in the budget: the budget counts its own
// ledger, so a budget that counted media rows would be refilled by the
// deletions below and this test would be red.
func TestUploadBudget(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)

	var open []uuid.UUID
	opened := a.clock.Now()
	for range media.MaxOpenUploads {
		open = append(open, a.start(t, owner, media.KindCompanyImage).Media.ID)
	}
	a.clock.Advance(time.Minute)
	_, err := a.svc.Start(t.Context(), owner, media.KindCompanyImage, 1024, noCommit)
	wantCode(t, err, media.CodePendingLimit)
	wait, ok := media.RetryAfter(err)
	if want := opened.Add(media.UploadRetention).Sub(a.clock.Now()); !ok || wait != want {
		t.Errorf("open cap RetryAfter = %v, %v, want %v (the oldest open upload is swept then)", wait, ok, want)
	}

	// Start and delete until the day's budget is spent.
	for _, id := range open {
		if err := a.svc.Delete(t.Context(), owner, id); err != nil {
			t.Fatal(err)
		}
	}
	for range media.DailyUploads - media.MaxOpenUploads {
		s := a.start(t, owner, media.KindCompanyImage)
		if err := a.svc.Delete(t.Context(), owner, s.Media.ID); err != nil {
			t.Fatal(err)
		}
		a.clock.Advance(time.Second)
	}
	_, err = a.svc.Start(t.Context(), owner, media.KindCompanyImage, 1024, noCommit)
	wantCode(t, err, media.CodeDailyLimit)
	wait, ok = media.RetryAfter(err)
	if want := opened.Add(media.UploadBudgetWindow).Sub(a.clock.Now()); !ok || wait != want {
		t.Errorf("budget RetryAfter = %v, %v, want %v (the oldest start leaves the window)", wait, ok, want)
	}

	// Another user's budget is its own.
	if _, err := a.svc.Start(t.Context(), a.user(t), media.KindCompanyImage, 1024, noCommit); err != nil {
		t.Errorf("another user's start = %v", err)
	}
	// The window moves: a day after the first start there is room again,
	// and the sweep purges the ledger rows it no longer needs.
	a.clock.Advance(media.UploadBudgetWindow)
	swept, err := a.svc.Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if swept.Starts == 0 {
		t.Error("the sweep purged no ledger row")
	}
	if _, err := a.svc.Start(t.Context(), owner, media.KindCompanyImage, 1024, noCommit); err != nil {
		t.Errorf("a start a day later = %v", err)
	}
}

// The uploader's advisory lock (ADR-0035 Karar 6): the caps are counted
// and spent in one transaction under it, so of N concurrent starts at one
// place below a cap exactly one passes. Without the lock every start
// counts the same rows before any commits, and more than one passes; the
// race needs no luck with N goroutines released at once, but it is a
// race, so a single run of the mutation may pass -- it fails nearly every
// run.
func TestUploadCapsHoldUnderConcurrency(t *testing.T) {
	t.Parallel()
	const n = 8

	race := func(t *testing.T, a *app, owner uuid.UUID) (passed int, codes []apperr.Code) {
		t.Helper()
		var mu sync.Mutex
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for range n {
			wg.Go(func() {
				<-gate
				_, err := a.svc.Start(t.Context(), owner, media.KindCompanyImage, 1024, noCommit)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					passed++
					return
				}
				var ae *apperr.Error
				if errors.As(err, &ae) {
					codes = append(codes, ae.Code)
				} else {
					codes = append(codes, apperr.Code(err.Error()))
				}
			})
		}
		close(gate)
		wg.Wait()
		return passed, codes
	}

	t.Run("daily budget", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		// One place left in the day's budget.
		for range media.DailyUploads - 1 {
			_, err := a.db.Pool().Exec(t.Context(), "INSERT INTO media_upload_starts (id, user_id, created_at) VALUES ($1, $2, $3)",
				uuid.Must(uuid.NewV7()), owner, a.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
		}
		passed, codes := race(t, a, owner)
		if passed != 1 {
			t.Errorf("%d of %d concurrent starts passed the last place of the budget, want 1 (refusals %v)", passed, n, codes)
		}
		for _, c := range codes {
			if c != media.CodeDailyLimit {
				t.Errorf("a refusal was %s, want %s", c, media.CodeDailyLimit)
			}
		}
	})
	t.Run("open uploads", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		for range media.MaxOpenUploads - 1 {
			a.start(t, owner, media.KindCompanyImage)
		}
		passed, codes := race(t, a, owner)
		if passed != 1 {
			t.Errorf("%d of %d concurrent starts passed the last open place, want 1 (refusals %v)", passed, n, codes)
		}
		for _, c := range codes {
			if c != media.CodePendingLimit {
				t.Errorf("a refusal was %s, want %s", c, media.CodePendingLimit)
			}
		}
	})
}

// A caller whose account was deleted after authentication let the request
// through gets the session's 401, not a 500 and not a 404 (ADR-0035
// Karar 2; CLAUDE.md, ErrNotActive).
func TestStartOfADeletedCallerIsUnauthenticated(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	err := a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		_, err := softdelete.New(a.catalog).Delete(ctx, q, softdelete.DeleteRequest{
			Table: "users", ID: owner, Actor: softdelete.Actor{Kind: softdelete.ActorSystem},
			Reason: softdelete.ReasonAccountDeletion, At: a.clock.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.svc.Start(t.Context(), owner, media.KindProfilePicture, 1024, noCommit)
	wantCode(t, err, authn.CodeSessionInactive)
	var ae *apperr.Error
	if errors.As(err, &ae) && ae.Kind != apperr.KindUnauthenticated {
		t.Errorf("kind %v, want KindUnauthenticated", ae.Kind)
	}
}

// ADR-0035 Karar 10: without an object store no upload starts (an
// internal error that names the missing configuration in the log) and no
// row is written; a read of an existing row answers without an address.
func TestWithoutAnObjectStore(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	m := a.ready(t, owner, media.KindProfilePicture)

	logger := log.New(&bytes.Buffer{}, slog.LevelDebug)
	queue, err := jobs.NewClient(a.db.Pool(), jobs.Config{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	bare, err := media.NewService(a.db, queue, nil, softdelete.New(a.catalog), a.clock, logger)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bare.Start(t.Context(), owner, media.KindProfilePicture, 1024, noCommit)
	if err == nil || apperr.From(err).Kind != apperr.KindInternal || !strings.Contains(err.Error(), "KOALA_OBJECT_") {
		t.Fatalf("Start() without a store = %v, want an internal error naming the configuration", err)
	}
	var rows int
	if err := a.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM active_media WHERE owner_id = $1", owner).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("%d media rows, want only the one written with a store", rows)
	}
	got, err := bare.Get(t.Context(), owner, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if addr, err := bare.Address(t.Context(), got, true); err != nil || addr.URL != "" {
		t.Errorf("Address() without a store = %+v, %v, want none", addr, err)
	}
}

// ADR-0035 Karar 8: a READY media nothing bound is swept UnboundRetention
// after it was decided, objects and all; a bound one stays; an upload
// whose completion was asked for within SweepGrace is not swept even past
// its retention (a job may be writing its object).
func TestSweepTakesUnboundMediaAndSparesRecentCompletions(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	unbound := a.ready(t, owner, media.KindIdentityDocument)
	bound := a.ready(t, owner, media.KindProfilePicture)
	if err := a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		return media.Bind(ctx, q, a.clock.Now(), owner, bound.ID)
	}); err != nil {
		t.Fatal(err)
	}

	// An upload whose completion is asked for just before its retention
	// ends.
	late := a.start(t, owner, media.KindProfilePicture)
	a.clock.Advance(media.UploadRetention - time.Minute)
	if _, err := a.svc.RequestCompletion(t.Context(), owner, late.Media.ID); err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(2 * time.Minute)
	if _, err := a.svc.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := a.svc.Get(t.Context(), owner, late.Media.ID); err != nil || got.Status != media.StatusPending {
		t.Errorf("an upload completed within the grace was swept: %+v, %v", got, err)
	}

	a.clock.Advance(media.UnboundRetention)
	swept, err := a.svc.Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if swept.Unbound != 1 {
		t.Errorf("sweep = %+v, want one unbound media", swept)
	}
	_, err = a.svc.Get(t.Context(), owner, unbound.ID)
	wantCode(t, err, media.CodeNotFound)
	if got, err := a.svc.Get(t.Context(), owner, bound.ID); err != nil || got.Status != media.StatusReady {
		t.Errorf("a bound media was swept: %+v, %v", got, err)
	}
	a.deleteObjects(t)
	if _, ok := a.fake.Object(unbound.ObjectKey.String()); ok {
		t.Error("an unbound identity photo's object is still in the store")
	}
	if _, ok := a.fake.Object(bound.ObjectKey.String()); !ok {
		t.Error("a bound media's object was deleted")
	}
}

// Bind is the one writer of bound_at (ADR-0035 Karar 8): only the owner's
// READY media binds; another user's is not found, a PENDING one is not
// ready.
func TestBind(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	ready := a.ready(t, owner, media.KindCompanyLogo)
	pending := a.start(t, owner, media.KindCompanyLogo)
	bind := func(who, id uuid.UUID) error {
		return a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
			return media.Bind(ctx, q, a.clock.Now(), who, id)
		})
	}
	if err := bind(owner, ready.ID); err != nil {
		t.Fatalf("binding a READY media: %v", err)
	}
	if err := bind(owner, ready.ID); err != nil {
		t.Fatalf("binding it again: %v", err)
	}
	wantCode(t, bind(owner, pending.Media.ID), media.CodeNotReady)
	wantCode(t, bind(a.user(t), ready.ID), media.CodeNotFound)
	wantCode(t, bind(owner, uuid.Must(uuid.NewV7())), media.CodeNotFound)
}

// The sweep pages past a media it cannot retire (ADR-0035 Karar 4). A
// consuming table on a restrict edge makes the soft deletion of a media it
// points at fail: here the budget ledger plays that table, since every
// started upload has a ledger row with its id. A full batch of such rows,
// older than everything else, must not keep the sweep from the deletable
// row behind them; a sweep that stopped on a batch it could not retire
// (the earlier len(taken) loop) never reached it.
func TestSweepPagesPastMediaItCannotRetire(t *testing.T) {
	t.Parallel()
	catalog, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog(), []byte(
		"edges:\n  - child: media_upload_starts\n    column: id\n    parent: media\n    policy: restrict\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := newAppWith(t, catalog)
	owner := a.user(t)
	q := mediadb.New(a.db.Pool())
	insert := func(withLedger bool) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		if _, err := q.InsertMedia(t.Context(), mediadb.InsertMediaParams{
			ID: id, OwnerID: owner, Kind: string(media.KindProfilePicture),
			UploadKey: "incoming/" + id.String() + ".jpg", DeclaredSize: 1, CreatedAt: a.clock.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if withLedger {
			if err := q.InsertUploadStart(t.Context(), mediadb.InsertUploadStartParams{
				ID: id, UserID: owner, CreatedAt: a.clock.Now(),
			}); err != nil {
				t.Fatal(err)
			}
		}
		a.clock.Advance(time.Millisecond)
		return id
	}
	const stuck = 100 // one full sweep batch
	for range stuck {
		insert(true)
	}
	behind := insert(false)
	// The budget window ends first so the ledger rows are not purged by
	// the same sweep; the media are past their retention.
	a.clock.Advance(media.UploadRetention + time.Minute)
	if _, err := a.db.Pool().Exec(t.Context(), "UPDATE media_upload_starts SET created_at = $1", a.clock.Now()); err != nil {
		t.Fatal(err)
	}
	swept, err := a.svc.Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if swept.Kept != stuck {
		t.Errorf("sweep = %+v, want the %d restricted media kept", swept, stuck)
	}
	_, err = a.svc.Get(t.Context(), owner, behind)
	wantCode(t, err, media.CodeNotFound)
}

// ADR-0035 Karar 3 and Karar 5, MED-014: a CV is a PDF, a DOC or a DOCX,
// written as it was validated, served only through a five minute signed
// download that getMedia asks for, with the detected type; a spreadsheet,
// an empty file, a file that is only a signature and a file over ten
// megabytes are refused; an identity document has no address at all;
// bytes that carry a JPEG's magic and do not decode are stored as CORRUPT
// and rendered as MEDIA_TYPE_UNSUPPORTED.
func TestAddressesAndRejectionsByKind(t *testing.T) {
	t.Parallel()

	t.Run("cv", func(t *testing.T) {
		t.Parallel()
		for name, tt := range map[string]struct {
			body []byte
			typ  imaging.Type
		}{
			"pdf":  {[]byte("%PDF-1.7\n1 0 obj\n<< /Author (Yusuf) >>\nendobj\n%%EOF\n"), imaging.TypePDF},
			"doc":  {wordBinaryDoc("Yusuf"), imaging.TypeDOC},
			"docx": {wordOOXMLDoc(t, "Yusuf"), imaging.TypeDOCX},
		} {
			a := newApp(t)
			owner := a.user(t)
			s := a.start(t, owner, media.KindCV)
			a.put(s, tt.body)
			m := a.complete(t, owner, s.Media.ID)
			if m.Status != media.StatusReady || m.ContentType != tt.typ {
				t.Fatalf("MED-014 %s: %+v", name, m)
			}
			stored, _ := a.fake.Object(m.ObjectKey.String())
			if !bytes.Equal(stored.Body, tt.body) || stored.ContentDisposition != "attachment" ||
				stored.CacheControl != "no-store" {
				t.Errorf("MED-014 %s: stored as %+v, want the document as it is, an uncached attachment", name, stored)
			}
			if addr, err := a.svc.Address(t.Context(), m, false); err != nil || addr.URL != "" {
				t.Errorf("%s: a CV outside getMedia has address %+v, %v", name, addr, err)
			}
			addr, err := a.svc.Address(t.Context(), m, true)
			if err != nil || addr.ExpiresAt == nil || !addr.ExpiresAt.Equal(a.clock.Now().Add(media.DownloadTTL)) {
				t.Fatalf("%s: download address %+v, %v, want one that expires in %v", name, addr, err, media.DownloadTTL)
			}
			u, err := url.Parse(addr.URL)
			if err != nil || u.Query().Get("X-Amz-Expires") != "300" ||
				u.Query().Get("response-content-disposition") != "attachment" ||
				u.Query().Get("response-cache-control") != "no-store" ||
				u.Query().Get("response-content-type") != string(tt.typ) ||
				!strings.HasPrefix(u.Path, "/"+testBucket+"/files/") {
				t.Errorf("%s: download address %s, want a signed, uncached %s attachment under files/", name, addr.URL, tt.typ)
			}
		}
	})
	t.Run("refused cv", func(t *testing.T) {
		t.Parallel()
		oversize := append([]byte("%PDF-1.7\n"), make([]byte, 10<<20)...)
		for name, tt := range map[string]struct {
			body []byte
			want media.Rejection
		}{
			"spreadsheet":        {zipOf(t, "[Content_Types].xml", "xl/workbook.xml"), media.RejectionTypeUnsupported},
			"empty file":         {[]byte{}, media.RejectionTypeUnsupported},
			"only the signature": {[]byte("PK\x03\x04"), media.RejectionTypeUnsupported},
			"over ten megabytes": {oversize, media.RejectionTooLarge},
		} {
			a := newApp(t)
			owner := a.user(t)
			s := a.start(t, owner, media.KindCV)
			a.put(s, tt.body)
			m := a.complete(t, owner, s.Media.ID)
			if m.Status != media.StatusRejected || m.Rejection != tt.want {
				t.Errorf("MED-014 %s: %+v, want REJECTED with %s", name, m, tt.want)
			}
		}
	})
	t.Run("identity document", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindIdentityDocument)
		body, marker := jpegWithGPS(t, 1)
		a.put(s, body)
		m := a.complete(t, owner, s.Media.ID)
		stored, _ := a.fake.Object(m.ObjectKey.String())
		if m.Status != media.StatusReady || bytes.Contains(stored.Body, marker) {
			t.Fatalf("MED-005: identity document %+v kept its GPS marker or was not stored", m)
		}
		for _, download := range []bool{false, true} {
			if addr, err := a.svc.Address(t.Context(), m, download); err != nil || addr.URL != "" {
				t.Errorf("IDN-007: an identity document has address %+v, %v", addr, err)
			}
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		a.put(s, append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 64)...))
		m := a.complete(t, owner, s.Media.ID)
		if m.Status != media.StatusRejected || m.Rejection != media.RejectionCorrupt {
			t.Fatalf("the row is %+v, want REJECTED with the pipeline's own CORRUPT", m)
		}
		if code, ok := m.Rejection.Code(); !ok || code != media.CodeTypeUnsupported {
			t.Errorf("CORRUPT renders %q, want MEDIA_TYPE_UNSUPPORTED", code)
		}
	})
}
