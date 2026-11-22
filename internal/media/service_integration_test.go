//go:build integration

package media_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/media"
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
	svc   *media.Service
	db    *db.DB
	fake  *fakes.S3
	clock *fakeclock.Fake
}

func newApp(t *testing.T) *app {
	t.Helper()
	database := dbtest.New(t).Open(t, db.RoleApp)
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
	logger := log.New(&bytes.Buffer{}, slog.LevelDebug)
	queue, err := jobs.NewClient(database.Pool(), jobs.Config{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := media.NewService(database, queue, store, softdelete.New(dbtest.SoftDeleteCatalog(t)), clk, logger)
	if err != nil {
		t.Fatal(err)
	}
	return &app{svc: svc, db: database, fake: fake, clock: clk}
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

// complete requests the completion and runs its job.
func (a *app) complete(t *testing.T, owner, id uuid.UUID) media.Media {
	t.Helper()
	if _, err := a.svc.RequestCompletion(t.Context(), owner, id); err != nil {
		t.Fatalf("RequestCompletion() = %v", err)
	}
	if err := a.svc.Complete(t.Context(), id); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	m, err := a.svc.Get(t.Context(), owner, id)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	return m
}

// queued counts the queued jobs of kind for the media id.
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
// marker and moved) and read; an upload that is never completed is gone
// after its retention.
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
	if got, err := a.svc.Get(t.Context(), owner, s.Media.ID); err != nil || got.Status != media.StatusReady {
		t.Errorf("the sweep touched a READY media: %+v, %v", got, err)
	}
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
//   - "orphan": a run that wrote an object for a row that left PENDING
//     meanwhile deletes it (MED-006).
func TestCompletionGateHoldsForADecidedUpload(t *testing.T) {
	t.Parallel()

	ready := func(t *testing.T) (*app, uuid.UUID, media.Started) {
		t.Helper()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		body, _ := jpegWithGPS(t, 1)
		a.put(s, body)
		if m := a.complete(t, owner, s.Media.ID); m.Status != media.StatusReady {
			t.Fatalf("setup: %+v", m)
		}
		// The second object, of another type, at the same address.
		a.put(s, pngBytes(t, 4, 4))
		return a, owner, s
	}
	png := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		return finalKey(t, id, storage.ExtPNG).String()
	}

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		a, owner, s := ready(t)
		if _, err := a.db.Pool().Exec(t.Context(), "DELETE FROM river_job WHERE kind = 'media_complete'"); err != nil {
			t.Fatal(err)
		}
		m, err := a.svc.RequestCompletion(t.Context(), owner, s.Media.ID)
		if err != nil || m.Status != media.StatusReady {
			t.Fatalf("RequestCompletion() = %+v, %v, want the READY row as it is", m, err)
		}
		if n := a.queued(t, "media_complete", s.Media.ID); n != 0 {
			t.Errorf("a READY upload queued %d completion jobs, want none", n)
		}
	})
	t.Run("job", func(t *testing.T) {
		t.Parallel()
		a, _, s := ready(t)
		path := "/" + testBucket + "/" + s.Media.UploadKey.String()
		before := len(a.fake.RequestsTo(http.MethodGet, path)) + len(a.fake.RequestsTo(http.MethodHead, path))
		if err := a.svc.Complete(t.Context(), s.Media.ID); err != nil {
			t.Fatal(err)
		}
		if after := len(a.fake.RequestsTo(http.MethodGet, path)) + len(a.fake.RequestsTo(http.MethodHead, path)); after != before {
			t.Errorf("the job read the quarantine of a READY upload (%d requests, want %d)", after, before)
		}
		if _, ok := a.fake.Object(png(t, s.Media.ID)); ok {
			t.Error("a second final object was written for a READY upload")
		}
	})
	t.Run("orphan", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindProfilePicture)
		a.put(s, pngBytes(t, 4, 4))
		if _, err := a.svc.RequestCompletion(t.Context(), owner, s.Media.ID); err != nil {
			t.Fatal(err)
		}
		// The owner deletes the upload while the job writes its object.
		deleted := make(chan error, 1)
		ctx := t.Context()
		a.fake.Before(http.MethodPut, "/"+testBucket+"/"+png(t, s.Media.ID), func() {
			deleted <- a.svc.Delete(ctx, owner, s.Media.ID)
		})
		if err := a.svc.Complete(ctx, s.Media.ID); err != nil {
			t.Fatal(err)
		}
		if err := <-deleted; err != nil {
			t.Fatalf("the delete during the write: %v", err)
		}
		if _, ok := a.fake.Object(png(t, s.Media.ID)); ok {
			t.Error("MED-006: the object of a row deleted meanwhile was kept; no row will ever name it")
		}
	})
}

// MED-006: the rollback invariant on the job. An object whose recording
// failed with an unknown outcome is kept, and the retry records it; an
// object whose row is gone is deleted (TestCompletionGateHoldsForADecidedUpload,
// "orphan"); deleting one media leaves another media's object alone.
func TestCompletionKeepsAnObjectItCouldNotRecord(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	s := a.start(t, owner, media.KindProfilePicture)
	a.put(s, pngBytes(t, 4, 4))
	if _, err := a.svc.RequestCompletion(t.Context(), owner, s.Media.ID); err != nil {
		t.Fatal(err)
	}
	final := finalKey(t, s.Media.ID, storage.ExtPNG).String()

	// The context ends after the object is written and before the
	// outcome is recorded: the recording transaction cannot begin.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.fake.Before(http.MethodDelete, "/"+testBucket+"/"+s.Media.UploadKey.String(), cancel)
	if err := a.svc.Complete(ctx, s.Media.ID); err == nil {
		t.Fatal("MED-006: Complete() = nil with a canceled recording")
	}
	if _, ok := a.fake.Object(final); !ok {
		t.Fatal("MED-006: the object of an unrecorded outcome was deleted")
	}
	if m, err := a.svc.Get(t.Context(), owner, s.Media.ID); err != nil || m.Status != media.StatusPending {
		t.Fatalf("after the failed run: %+v, %v, want PENDING", m, err)
	}

	// The retry finds the object at its final key and records it.
	if err := a.svc.Complete(t.Context(), s.Media.ID); err != nil {
		t.Fatal(err)
	}
	m, err := a.svc.Get(t.Context(), owner, s.Media.ID)
	if err != nil || m.Status != media.StatusReady || m.ObjectKey.String() != final {
		t.Fatalf("MED-006: after the retry %+v, %v, want READY at %s", m, err, final)
	}
}

// ADR-0035 Karar 4, the orphan sweep of the final prefixes: a completion
// that wrote its object and was never recorded leaves an object under
// images/ that no lifecycle rule covers. Two paths remove it, each held
// here on its own: the sweep of an expired upload deletes every final key
// the row's id can have, and a later completion of the same id deletes
// the extensions it did not write.
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
		if m := a.complete(t, owner, s.Media.ID); m.Status != media.StatusReady || m.ObjectKey.Ext() != storage.ExtJPEG {
			t.Fatalf("completion: %+v", m)
		}
		if _, ok := a.fake.Object(left); ok {
			t.Error("the completion left the id's other extension in the store")
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
}

// ADR-0035 Karar 7: a deletion soft-deletes the row and queues the
// deletion of its objects; the job deletes them unless the row came back
// meanwhile, and never touches another media's object (MED-006).
func TestDeletionTakesTheObjectsOfItsMediaOnly(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)
	var keys []string
	var ids []uuid.UUID
	for range 2 {
		s := a.start(t, owner, media.KindCompanyImage)
		body, _ := jpegWithGPS(t, 1)
		a.put(s, body)
		m := a.complete(t, owner, s.Media.ID)
		keys = append(keys, m.ObjectKey.String())
		ids = append(ids, m.ID)
	}
	if err := a.svc.Delete(t.Context(), owner, ids[0]); err != nil {
		t.Fatal(err)
	}
	if n := a.queued(t, "media_delete_objects", ids[0]); n != 1 {
		t.Fatalf("%d object deletion jobs, want 1", n)
	}
	_, err := a.svc.Get(t.Context(), owner, ids[0])
	wantCode(t, err, media.CodeNotFound)

	// The job of the still active media does nothing: a restored row
	// keeps its objects.
	if err := a.svc.DeleteObjects(t.Context(), ids[1], media.KindCompanyImage, "incoming/"+ids[1].String()+".jpg"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.fake.Object(keys[1]); !ok {
		t.Fatal("the deletion job deleted an active media's object")
	}
	if err := a.svc.DeleteObjects(t.Context(), ids[0], media.KindCompanyImage, "incoming/"+ids[0].String()+".jpg"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.fake.Object(keys[0]); ok {
		t.Error("the deleted media's object is still in the store")
	}
	if _, ok := a.fake.Object(keys[1]); !ok {
		t.Error("MED-006: deleting one media deleted another's object")
	}

	// Another user's media is the same not found, for a deletion too.
	stranger := a.user(t)
	err = a.svc.Delete(t.Context(), stranger, ids[1])
	wantCode(t, err, media.CodeNotFound)
}

// ADR-0035 Karar 6: at most MaxOpenUploads open uploads, and DailyUploads
// starts a day. Deleting an open upload frees its place in the open cap
// and not in the budget: the budget counts its own ledger, so a budget
// that counted media rows would be refilled by the deletions below and
// this test would be red.
func TestUploadBudget(t *testing.T) {
	t.Parallel()
	a := newApp(t)
	owner := a.user(t)

	var open []uuid.UUID
	for range media.MaxOpenUploads {
		open = append(open, a.start(t, owner, media.KindCompanyImage).Media.ID)
	}
	_, err := a.svc.Start(t.Context(), owner, media.KindCompanyImage, 1024, noCommit)
	wantCode(t, err, media.CodePendingLimit)

	// Start and delete until the day's budget is spent.
	for _, id := range open {
		if err := a.svc.Delete(t.Context(), owner, id); err != nil {
			t.Fatal(err)
		}
	}
	first := a.clock.Now()
	for range media.DailyUploads - media.MaxOpenUploads {
		s := a.start(t, owner, media.KindCompanyImage)
		if err := a.svc.Delete(t.Context(), owner, s.Media.ID); err != nil {
			t.Fatal(err)
		}
		a.clock.Advance(time.Second)
	}
	_, err = a.svc.Start(t.Context(), owner, media.KindCompanyImage, 1024, noCommit)
	wantCode(t, err, media.CodeDailyLimit)
	wait, ok := media.RetryAfter(err)
	if want := first.Add(media.UploadBudgetWindow).Sub(a.clock.Now()); !ok || wait != want {
		t.Errorf("RetryAfter = %v, %v, want %v (the oldest start leaves the window)", wait, ok, want)
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

// ADR-0035 Karar 3 and Karar 5, MED-014: a CV is a PDF, a DOC or a DOCX,
// moved as it is, served only through a five minute signed download that
// getMedia asks for; a spreadsheet is refused as an unsupported type; an
// identity document has no address at all; bytes that carry a JPEG's
// magic and do not decode are stored as CORRUPT and rendered as
// MEDIA_TYPE_UNSUPPORTED.
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
			if !bytes.Contains(stored.Body, []byte("Yusuf")) || stored.ContentDisposition != "attachment" ||
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
				!strings.HasPrefix(u.Path, "/"+testBucket+"/files/") {
				t.Errorf("%s: download address %s, want a signed, uncached attachment under files/", name, addr.URL)
			}
		}
	})
	t.Run("spreadsheet", func(t *testing.T) {
		t.Parallel()
		a := newApp(t)
		owner := a.user(t)
		s := a.start(t, owner, media.KindCV)
		a.put(s, zipOf(t, "[Content_Types].xml", "xl/workbook.xml"))
		m := a.complete(t, owner, s.Media.ID)
		if code, _ := m.Rejection.Code(); m.Status != media.StatusRejected || code != media.CodeTypeUnsupported {
			t.Errorf("MED-014: a spreadsheet as a CV is %+v, want REJECTED with MEDIA_TYPE_UNSUPPORTED", m)
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
