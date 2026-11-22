//go:build integration

package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/media/mediadb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/fakes"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// newMedia writes one of owner's media of kind as an upload start leaves
// it (PENDING), and READY with its object when ready is set, as a
// completion records it.
func (a *authApp) newMedia(t *testing.T, owner uuid.UUID, kind media.Kind, ready bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	ext, contentType, prefix := "jpg", "image/jpeg", "images/"
	if kind == media.KindCV {
		ext, contentType, prefix = "pdf", "application/pdf", "files/"
	}
	q := mediadb.New(a.db.Pool())
	_, err := q.InsertMedia(t.Context(), mediadb.InsertMediaParams{
		ID: id, OwnerID: owner, Kind: string(kind), UploadKey: "incoming/" + id.String() + "." + ext,
		DeclaredSize: 1024, CreatedAt: a.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		n, err := q.MarkReady(t.Context(), mediadb.MarkReadyParams{
			Now: a.clock.Now(), ObjectKey: prefix + id.String() + "." + ext, ContentType: contentType, SizeBytes: 1024, ID: id,
		})
		if err != nil || n != 1 {
			t.Fatalf("mark ready: %d, %v", n, err)
		}
	}
	return id
}

// mediaState is whether a media is active and whether it is bound.
func (a *authApp) mediaState(t *testing.T, id uuid.UUID) (active, bound bool) {
	t.Helper()
	err := a.maint.Pool().QueryRow(t.Context(),
		"SELECT deleted_at IS NULL, bound_at IS NOT NULL FROM media WHERE id = $1", id).Scan(&active, &bound)
	if err != nil {
		t.Fatal(err)
	}
	return active, bound
}

// The profile's media links (ADR-0036 Karar 10, ADR-0035 Karar 8): a new
// picture or CV is the caller's (MEDIA_NOT_FOUND), of the member's kind
// (REQUEST_INVALID at the member) and READY (MEDIA_NOT_READY); it is bound
// in the patch's transaction, so the sweep leaves it; the one it replaces,
// or that null clears, is released in the same transaction, after the row
// moved, with its objects' deletion queued.
func TestProfileMediaLinks(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	const email = "fotograf@example.com"
	tok := a.addSignedIn(t, email)
	owner := a.userID(t, email)
	a.addSignedIn(t, "baskasi@example.com")
	stranger := a.userID(t, "baskasi@example.com")

	first := a.newMedia(t, owner, media.KindProfilePicture, true)
	second := a.newMedia(t, owner, media.KindProfilePicture, true)
	cv := a.newMedia(t, owner, media.KindCV, true)
	pendingCV := a.newMedia(t, owner, media.KindCV, false)
	theirs := a.newMedia(t, stranger, media.KindProfilePicture, true)
	patch := func(body string) *profileUpdate {
		t.Helper()
		rec := a.patchProfile(t, tok, body)
		wantStatus(t, rec, http.StatusOK, "")
		up := decodeBody[profileUpdate](t, rec)
		return &up
	}

	// The refusals, each before anything is written.
	for body, want := range map[string]struct {
		status int
		code   string
	}{
		`{"profilePictureId":"` + cv.String() + `"}`:           {http.StatusBadRequest, httpx.CodeRequestInvalid},
		`{"cvId":"` + first.String() + `"}`:                    {http.StatusBadRequest, httpx.CodeRequestInvalid},
		`{"cvId":"` + pendingCV.String() + `"}`:                {http.StatusConflict, string(media.CodeNotReady)},
		`{"profilePictureId":"` + theirs.String() + `"}`:       {http.StatusNotFound, string(media.CodeNotFound)},
		`{"profilePictureId":"` + uuid.NewString() + `"}`:      {http.StatusNotFound, string(media.CodeNotFound)},
		`{"jobTitle":"x","cvId":"` + pendingCV.String() + `"}`: {http.StatusConflict, string(media.CodeNotReady)},
	} {
		rec := a.patchProfile(t, tok, body)
		wantStatus(t, rec, want.status, want.code)
		if want.status == http.StatusBadRequest && (!strings.Contains(rec.Body.String(), `"pointer":"/`) ||
			!strings.Contains(rec.Body.String(), `"code":"`+httpx.CodeFieldValueInvalid+`"`)) {
			t.Errorf("%s: %s, want the member named with %s", body, rec.Body, httpx.CodeFieldValueInvalid)
		}
	}
	if a.text(t, "SELECT coalesce(job_title, '-') FROM active_users WHERE id = $1", owner) != "-" {
		t.Fatal("a refused media link left the patch's other fields written")
	}
	if _, bound := a.mediaState(t, cv); bound {
		t.Fatal("a media refused for its kind was bound")
	}

	up := patch(`{"profilePictureId":"` + first.String() + `","cvId":"` + cv.String() + `"}`)
	if up.Profile.ProfilePictureID == nil || *up.Profile.ProfilePictureID != first.String() {
		t.Fatalf("the profile names %v, want %s", up.Profile.ProfilePictureID, first)
	}
	for _, id := range []uuid.UUID{first, cv} {
		if active, bound := a.mediaState(t, id); !active || !bound {
			t.Fatalf("%s after the link: active %v bound %v, want both", id, active, bound)
		}
	}

	// A new picture: bound, and the old one released in the same
	// transaction with its objects' deletion queued.
	jobs := func(id uuid.UUID) int {
		t.Helper()
		return a.count(t, "SELECT count(*) FROM river_job WHERE kind = 'media_delete_objects' AND args->>'media' = $1", id.String())
	}
	patch(`{"profilePictureId":"` + second.String() + `"}`)
	if active, bound := a.mediaState(t, second); !active || !bound {
		t.Fatalf("the new picture: active %v bound %v", active, bound)
	}
	if active, _ := a.mediaState(t, first); active || jobs(first) != 1 {
		t.Fatalf("the replaced picture: active %v, %d deletion jobs; want released", active, jobs(first))
	}
	// null clears the CV and releases it.
	up = patch(`{"cvId":null}`)
	if up.Profile.CvID != nil {
		t.Fatalf("cvId = %v after null", *up.Profile.CvID)
	}
	if active, _ := a.mediaState(t, cv); active || jobs(cv) != 1 {
		t.Fatalf("the cleared CV: active %v, %d deletion jobs; want released", active, jobs(cv))
	}

	// ADR-0035 Karar 8 with a real consumer: a media an active profile
	// names cannot be released or deleted (SOFT_DELETE_RESTRICTED).
	err := a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		return a.mods.media.Release(ctx, q, a.clock.Now(), owner, second)
	})
	if apperr.From(err).Code != softdelete.CodeRestricted {
		t.Fatalf("Release of the profile's picture = %v, want %s", err, softdelete.CodeRestricted)
	}
	wantStatus(t, a.call(t, http.MethodDelete, "/v1/me/media/"+second.String(), tok, ""),
		http.StatusConflict, string(softdelete.CodeRestricted))

	// The sweep leaves the bound picture after its unbound retention.
	a.clock.Advance(media.UnboundRetention + time.Hour)
	swept, err := a.mods.media.Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := a.mediaState(t, second); !active || swept.Adopted != 0 {
		t.Fatalf("the bound picture after a sweep: active %v, sweep %+v", active, swept)
	}
}

// The account's own deletion is not held back by its profile's media: it
// marks the user first, so the restrict edges users.profile_picture_id and
// users.cv_id find no active consumer, and the cascade media.owner_id →
// users takes the media along (ADR-0035 Karar 7).
func TestAccountDeletionTakesItsProfileMedia(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	const email = "hesap-silinir@example.com"
	tok := a.addSignedIn(t, email)
	owner := a.userID(t, email)
	picture := a.newMedia(t, owner, media.KindProfilePicture, true)
	cv := a.newMedia(t, owner, media.KindCV, true)
	wantStatus(t, a.patchProfile(t, tok, `{"profilePictureId":"`+picture.String()+`","cvId":"`+cv.String()+`"}`),
		http.StatusOK, "")

	cat := dbtest.SoftDeleteCatalog(t)
	err := a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		_, err := softdelete.New(cat).Delete(ctx, q, softdelete.DeleteRequest{
			Table: "users", ID: owner, Actor: softdelete.Actor{Kind: softdelete.ActorUser, ID: owner},
			Reason: softdelete.ReasonAccountDeletion, At: a.clock.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("the account's deletion = %v, want its media taken along", err)
	}
	for _, id := range []uuid.UUID{picture, cv} {
		if active, _ := a.mediaState(t, id); active {
			t.Errorf("%s is still active after its owner's deletion", id)
		}
	}
}

// An account deleted with its profile's media comes back with them
// (ADR-0035 Karar 7): softdelete.Restore reopens the user and the media of
// the same deletion across the two-way edges (users → media restrict,
// media → users cascade). The profile names the same media, both are
// active and still bound, no object deletion was queued, and the objects
// are still in the store.
func TestAccountRestoreBringsItsProfileMediaBack(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	const email = "geri-gelir@example.com"
	tok := a.addSignedIn(t, email)
	owner := a.userID(t, email)
	picture := a.newMedia(t, owner, media.KindProfilePicture, true)
	cv := a.newMedia(t, owner, media.KindCV, true)
	objects := []string{"images/" + picture.String() + ".jpg", "files/" + cv.String() + ".pdf"}
	for _, key := range objects {
		a.s3.Seed(key, fakes.S3Object{Body: []byte("nesne"), ContentType: "application/octet-stream"})
	}
	wantStatus(t, a.patchProfile(t, tok, `{"profilePictureId":"`+picture.String()+`","cvId":"`+cv.String()+`"}`),
		http.StatusOK, "")

	cat := dbtest.SoftDeleteCatalog(t)
	var deleted softdelete.Deleted
	err := a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		var err error
		deleted, err = softdelete.New(cat).Delete(ctx, q, softdelete.DeleteRequest{
			Table: "users", ID: owner, Actor: softdelete.Actor{Kind: softdelete.ActorUser, ID: owner},
			Reason: softdelete.ReasonAccountDeletion, At: a.clock.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(time.Hour)
	// Restore works on the base tables, as koala_maint.
	err = a.maint.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		_, err := softdelete.New(cat).Restore(ctx, q, softdelete.RestoreRequest{
			DeletionID: deleted.DeletionID, Actor: softdelete.Actor{Kind: softdelete.ActorSystem}, At: a.clock.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("the account's restore = %v, want it and its media back", err)
	}

	var active bool
	var pictureID, cvID uuid.UUID
	err = a.maint.Pool().QueryRow(t.Context(),
		"SELECT deleted_at IS NULL, profile_picture_id, cv_id FROM users WHERE id = $1", owner).Scan(&active, &pictureID, &cvID)
	if err != nil {
		t.Fatal(err)
	}
	if !active || pictureID != picture || cvID != cv {
		t.Fatalf("the restored user: active %v, picture %s, cv %s", active, pictureID, cvID)
	}
	for _, id := range []uuid.UUID{picture, cv} {
		if active, bound := a.mediaState(t, id); !active || !bound {
			t.Errorf("%s after the restore: active %v bound %v, want both", id, active, bound)
		}
	}
	if n := a.count(t, "SELECT count(*) FROM river_job WHERE kind = 'media_delete_objects'"); n != 0 {
		t.Errorf("%d object deletions queued for an account that was only soft-deleted and restored", n)
	}
	for _, key := range objects {
		if _, ok := a.s3.Object(key); !ok {
			t.Errorf("%s is gone from the store", key)
		}
	}
}
