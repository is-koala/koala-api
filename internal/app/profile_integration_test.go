//go:build integration

package app

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/pending"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/users"
)

// patchFrom sends a profile patch (application/merge-patch+json) as
// bearer from client address addr.
func (a *authApp) patchFrom(t *testing.T, addr, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/v1/me/profile", strings.NewReader(body))
	req.RemoteAddr = addr + ":40000"
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set(httpx.HeaderRequestID, "req-profile")
	setClient(req.Header)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

// patchProfile sends a profile patch from the default address.
func (a *authApp) patchProfile(t *testing.T, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return a.patchFrom(t, "192.0.2.1", bearer, body)
}

// userID is the id of the active user with email.
func (a *authApp) userID(t *testing.T, email string) uuid.UUID {
	t.Helper()
	return uuid.MustParse(a.text(t, "SELECT id::text FROM active_users WHERE email = $1", email))
}

// profileBody is the part of the profile answer the tests read.
type profileBody struct {
	ID                   uuid.UUID `json:"id"`
	FirstName            string    `json:"firstName"`
	LastName             string    `json:"lastName"`
	PhoneNumber          *string   `json:"phoneNumber"`
	PhoneVerified        bool      `json:"phoneVerified"`
	BirthDate            *string   `json:"birthDate"`
	Gender               *string   `json:"gender"`
	JobTitle             *string   `json:"jobTitle"`
	MaskedIdentityNumber *string   `json:"maskedIdentityNumber"`
	MaskedIban           *string   `json:"maskedIban"`
	ProfilePictureID     *string   `json:"profilePictureId"`
	Companies            []any     `json:"companies"`
}

type profileUpdate struct {
	Profile              profileBody `json:"profile"`
	PendingVerifications []struct {
		ID           uuid.UUID `json:"id"`
		Channel      string    `json:"channel"`
		MaskedTarget string    `json:"maskedTarget"`
	} `json:"pendingVerifications"`
}

// The profile is read and patched over the contract: the fields are
// written, a text sent empty or null is cleared, the links of later
// modules are null and the memberships an empty list, and the identity
// number is null until the identity is verified (USR-027).
func TestProfileOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "profil@example.com")
	id := a.userID(t, "profil@example.com")

	rec := a.call(t, http.MethodGet, "/v1/me/profile", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	got := decodeBody[profileBody](t, rec)
	if got.ID != id || got.MaskedIdentityNumber != nil || got.MaskedIban != nil || got.ProfilePictureID != nil ||
		got.Companies == nil || len(got.Companies) != 0 || got.PhoneNumber != nil {
		t.Fatalf("profile = %s", rec.Body)
	}

	rec = a.patchProfile(t, tok, `{"firstName":"  Ayşe ","lastName":"Kaya","birthDate":"1996-04-17","gender":"FEMALE","jobTitle":"Depo görevlisi"}`)
	wantStatus(t, rec, http.StatusOK, "")
	up := decodeBody[profileUpdate](t, rec)
	if up.Profile.FirstName != "Ayşe" || up.Profile.LastName != "Kaya" || up.Profile.BirthDate == nil ||
		*up.Profile.BirthDate != "1996-04-17" || up.Profile.Gender == nil || *up.Profile.Gender != "FEMALE" ||
		up.PendingVerifications == nil || len(up.PendingVerifications) != 0 {
		t.Fatalf("patched = %s", rec.Body)
	}
	// PATCH is JSON Merge Patch only (stil rehberi §3): a plain JSON body
	// is refused before the handler; null clears and an absent member stays.
	wantStatus(t, a.call(t, http.MethodPatch, "/v1/me/profile", tok, `{"jobTitle":"x"}`),
		http.StatusUnsupportedMediaType, httpx.CodeContentTypeUnsupported)
	rec = a.patchProfile(t, tok, `{"jobTitle":"  ","gender":null}`)
	wantStatus(t, rec, http.StatusOK, "")
	up = decodeBody[profileUpdate](t, rec)
	if up.Profile.JobTitle != nil || up.Profile.Gender != nil || up.Profile.FirstName != "Ayşe" {
		t.Fatalf("cleared = %s", rec.Body)
	}
	rec = a.call(t, http.MethodGet, "/v1/me/profile", tok, "")
	if got := decodeBody[profileBody](t, rec); got.JobTitle != nil || got.LastName != "Kaya" {
		t.Fatalf("read back = %s", rec.Body)
	}

	// The rules the schema cannot say.
	for body, pointer := range map[string]string{
		`{"firstName":"   "}`:        "/firstName",
		`{"lastName":" "}`:           "/lastName",
		`{"birthDate":"2999-01-01"}`: "/birthDate",
		`{"birthDate":"1850-01-01"}`: "/birthDate",
	} {
		rec := a.patchProfile(t, tok, body)
		wantStatus(t, rec, http.StatusBadRequest, httpx.CodeRequestInvalid)
		if !strings.Contains(rec.Body.String(), `"pointer":"`+pointer+`"`) {
			t.Errorf("%s: %s, want the pointer %s", body, rec.Body, pointer)
		}
	}

	// The identity is verified: the name and the birth date freeze, the
	// value on file and the gender do not (USR-024, USR-029); the
	// identity number shows as a full mask (USR-027).
	if _, err := a.maint.Pool().Exec(t.Context(), "UPDATE users SET identity_verified = true WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"firstName":"Başka"}`, `{"birthDate":"1990-01-01"}`, `{"birthDate":null}`} {
		wantStatus(t, a.patchProfile(t, tok, body), http.StatusConflict, string(users.CodeFieldImmutable))
	}
	rec = a.patchProfile(t, tok, `{"firstName":"Ayşe","birthDate":"1996-04-17","gender":"NOT_SPECIFIED"}`)
	wantStatus(t, rec, http.StatusOK, "")
	up = decodeBody[profileUpdate](t, rec)
	if up.Profile.MaskedIdentityNumber == nil || *up.Profile.MaskedIdentityNumber != "***********" ||
		up.Profile.Gender == nil || *up.Profile.Gender != "NOT_SPECIFIED" {
		t.Fatalf("frozen = %s", rec.Body)
	}
}

// The links to the media and address modules are not live yet: an id is
// 501, a null changes nothing (ADR-0036 Karar 5).
func TestProfileLinksWaitForTheirModules(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "bag@example.com")
	for _, member := range []string{"profilePictureId", "cvId", "addressId"} {
		rec := a.patchProfile(t, tok, `{"`+member+`":"`+uuid.NewString()+`"}`)
		wantStatus(t, rec, http.StatusNotImplemented, string(pending.CodeNotImplemented))
		wantStatus(t, a.patchProfile(t, tok, `{"`+member+`":null}`), http.StatusOK, "")
	}
}

// USR-014: a new number is not written by the patch, it starts a
// verification; the number on file and its flag stay until the code is
// confirmed, and sending the number on file again starts nothing.
func TestProfileContactChangeWaitsForConfirmation(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "numara@example.com")
	id := a.userID(t, "numara@example.com")
	const n1, n2 = "+905321110001", "+905321110002"
	// The number on file is N1, not verified.
	if _, err := a.maint.Pool().Exec(t.Context(), "UPDATE users SET phone = $2, phone_verified = false WHERE id = $1", id, n1); err != nil {
		t.Fatal(err)
	}

	rec := a.patchProfile(t, tok, `{"phoneNumber":"0532 111 00 02","currentPassword":"`+userPw+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	up := decodeBody[profileUpdate](t, rec)
	if len(up.PendingVerifications) != 1 || up.PendingVerifications[0].Channel != authn.ChannelPhone ||
		up.PendingVerifications[0].MaskedTarget != "+90******0002" {
		t.Fatalf("USR-014: the patch started %s", rec.Body)
	}
	if up.Profile.PhoneNumber == nil || *up.Profile.PhoneNumber != "+90******0001" || up.Profile.PhoneVerified {
		t.Fatalf("USR-014: the number on file changed before the confirmation: %s", rec.Body)
	}
	if a.text(t, "SELECT phone FROM active_users WHERE id = $1", id) != n1 {
		t.Fatal("USR-014: the new number was written by the patch")
	}
	if a.count(t, "SELECT count(*) FROM user_verifications WHERE user_id = $1 AND target = $2", id, n2) != 1 {
		t.Fatal("USR-014: no verification of the new number")
	}

	// The number on file again: nothing starts.
	rec = a.patchProfile(t, tok, `{"phoneNumber":"`+n1+`","currentPassword":"`+userPw+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	if up := decodeBody[profileUpdate](t, rec); len(up.PendingVerifications) != 0 {
		t.Fatalf("USR-014: the number on file started %s", rec.Body)
	}
	if a.count(t, "SELECT count(*) FROM user_verifications WHERE user_id = $1 AND target = $2", id, n1) != 0 {
		t.Fatal("USR-014: the number on file got a verification")
	}

	// The confirmation writes N2 and its flag, and records the profile
	// sync intent in its transaction (USR-022).
	if a.count(t, "SELECT count(*) FROM user_profile_sync_intents WHERE user_id = $1", id) != 0 {
		t.Fatal("a verification start recorded a profile change")
	}
	code := a.codeFor(t, up.PendingVerifications[0].ID)
	rec = a.call(t, http.MethodPost, "/v1/me/verifications/"+up.PendingVerifications[0].ID.String()+"/confirm", tok, `{"code":"`+code+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	rec = a.call(t, http.MethodGet, "/v1/me/profile", tok, "")
	if got := decodeBody[profileBody](t, rec); got.PhoneNumber == nil || *got.PhoneNumber != "+90******0002" || !got.PhoneVerified {
		t.Fatalf("USR-014: after the confirmation = %s", rec.Body)
	}
	if a.count(t, "SELECT count(*) FROM user_profile_sync_intents WHERE user_id = $1 AND published_at IS NULL", id) != 1 {
		t.Fatal("USR-022: the confirmed number recorded no profile sync intent")
	}
}

// ADR-0033 Karar 1b on the profile: a patch that sends an address or a
// number needs the current password, checked like every start's: a wrong
// one is 403 and counts on the account's sign-in lock, a locked account
// is refused even the right one, and the target's rule comes first, so a
// refused number spends no attempt. A patch of other fields needs none.
func TestProfileContactChangeNeedsThePassword(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	const email = "parola@example.com"
	tok := a.addSignedIn(t, email)
	target := a.mods.users.Target(email)
	failures := func() int64 {
		t.Helper()
		n, err := a.q.CountLoginAttempts(t.Context(), authndb.CountLoginAttemptsParams{PrincipalKind: "USER", TargetHash: target})
		if err != nil {
			t.Fatal(err)
		}
		return n.Failed
	}

	for body, pointer := range map[string]string{
		`{"phoneNumber":"+905321110003"}`:                   "/currentPassword",
		`{"email":"yeni@example.com"}`:                      "/currentPassword",
		`{"email":"yeni@example.com","currentPassword":""}`: "/currentPassword",
	} {
		rec := a.patchProfile(t, tok, body)
		wantStatus(t, rec, http.StatusBadRequest, httpx.CodeRequestInvalid)
		if !strings.Contains(rec.Body.String(), `"pointer":"`+pointer+`"`) {
			t.Errorf("%s: %s, want the pointer %s", body, rec.Body, pointer)
		}
	}
	// The target's rule before the password: no attempt is spent.
	rec := a.patchProfile(t, tok, `{"phoneNumber":"+447700900123","currentPassword":"yanlış ama uzun bir parola"}`)
	wantStatus(t, rec, http.StatusBadRequest, httpx.CodeRequestInvalid)
	if !strings.Contains(rec.Body.String(), `"pointer":"/phoneNumber"`) || failures() != 0 {
		t.Fatalf("a refused number: %s, %d failures", rec.Body, failures())
	}

	rec = a.patchProfile(t, tok, `{"firstName":"Yeni","phoneNumber":"+905321110003","currentPassword":"yanlış ama uzun bir parola"}`)
	wantStatus(t, rec, http.StatusForbidden, string(authn.CodeCurrentMismatch))
	if failures() != 1 {
		t.Fatalf("the wrong password counted %d failures, want 1", failures())
	}
	if a.text(t, "SELECT first_name FROM active_users WHERE email = $1", email) == "Yeni" {
		t.Fatal("a refused patch wrote its other fields")
	}
	// The sign-in lock holds the patch too (ADR-0033 Karar 1b).
	for range authn.UserLockThreshold {
		err := a.q.InsertLoginAttempt(t.Context(), authndb.InsertLoginAttemptParams{
			ID: uuid.Must(uuid.NewV7()), PrincipalKind: "USER", TargetHash: target, Outcome: "FAILED", CreatedAt: a.clock.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	wantStatus(t, a.patchProfile(t, tok, `{"phoneNumber":"+905321110003","currentPassword":"`+userPw+`"}`),
		http.StatusForbidden, string(authn.CodeCurrentMismatch))
	// Other fields need no password, locked or not.
	wantStatus(t, a.patchProfile(t, tok, `{"firstName":"Yeni"}`), http.StatusOK, "")
	a.clock.Advance(authn.UserLockDuration)
	wantStatus(t, a.patchProfile(t, tok, `{"phoneNumber":"+905321110003","currentPassword":"`+userPw+`"}`), http.StatusOK, "")
}

// The patch is no second door past a start's budget (ADR-0036): a number
// a start just got a code for is in its cooldown for the patch too, with
// Retry-After, and the refusal rolls the whole patch back, its other
// fields and the profile sync intent included (USR-022).
func TestProfilePatchSharesTheStartBudget(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "butce@example.com")
	id := a.userID(t, "butce@example.com")
	const number = "+905321110004"

	a.startOverHTTP(t, tok, number)
	rec := a.patchProfile(t, tok, `{"firstName":"Geri","phoneNumber":"`+number+`","currentPassword":"`+userPw+`"}`)
	wantStatus(t, rec, http.StatusTooManyRequests, string(authn.CodeVerificationResendTooSoon))
	if ra := rec.Header().Get("Retry-After"); ra != "60" {
		t.Errorf("Retry-After = %q, want 60", ra)
	}
	if a.text(t, "SELECT first_name FROM active_users WHERE id = $1", id) == "Geri" ||
		a.count(t, "SELECT count(*) FROM user_profile_sync_intents WHERE user_id = $1", id) != 0 {
		t.Fatal("USR-022: a refused patch left its fields or its profile sync intent")
	}
	// The daily cap of the caller counts the patch's starts and the
	// start's together.
	for i := range authn.TextsPerUserDay - 1 {
		a.clock.Advance(authn.VerificationCooldown)
		n := "+90532222" + strconv.Itoa(1000+i)
		wantStatus(t, a.patchProfile(t, tok, `{"phoneNumber":"`+n+`","currentPassword":"`+userPw+`"}`), http.StatusOK, "")
	}
	a.clock.Advance(authn.VerificationCooldown)
	rec = a.call(t, http.MethodPost, "/v1/me/verifications", tok, startBody("+905323333333", userPw))
	wantStatus(t, rec, http.StatusTooManyRequests, string(authn.CodeVerificationResendTooSoon))
}

// The patch takes its tokens from the start's own buckets (ADR-0036):
// the caller's bucket a start emptied refuses the patch, and the
// address's hourly budget counts both.
func TestProfilePatchSharesTheStartBuckets(t *testing.T) {
	t.Parallel()
	var cfg config.Config
	cfg.Verification.StartsPerIP = 3
	groups := serveRateLimitGroups(cfg)
	auth := groups[groupAuth]
	auth.PerCustom = httpx.Rate{Every: time.Hour, Burst: 2}
	groups[groupAuth] = auth
	a := newAuthApp(t, groups)
	first, second := a.addSignedIn(t, "kova1@example.com"), a.addSignedIn(t, "kova2@example.com")
	patch := func(addr, tok, number string) *httptest.ResponseRecorder {
		t.Helper()
		return a.patchFrom(t, addr, tok, `{"phoneNumber":"`+number+`","currentPassword":"`+userPw+`"}`)
	}
	start := func(addr, tok, number string) *httptest.ResponseRecorder {
		t.Helper()
		return a.callFrom(t, addr, http.MethodPost, "/v1/me/verifications", tok, startBody(number, userPw))
	}

	// The caller's bucket (burst 2): one start and one patch empty it.
	wantStatus(t, start("198.51.100.1", first, "+905324440001"), http.StatusCreated, "")
	wantStatus(t, patch("198.51.100.2", first, "+905324440002"), http.StatusOK, "")
	wantStatus(t, patch("198.51.100.3", first, "+905324440003"), http.StatusTooManyRequests, httpx.CodeRateLimited)
	wantStatus(t, start("198.51.100.4", first, "+905324440004"), http.StatusTooManyRequests, httpx.CodeRateLimited)

	// The address's budget (3 an hour): the first caller spent one at
	// 198.51.100.1; a start and a patch of the second spend the rest.
	wantStatus(t, start("198.51.100.1", second, "+905324440005"), http.StatusCreated, "")
	wantStatus(t, patch("198.51.100.1", second, "+905324440006"), http.StatusOK, "")
	rec := a.callFrom(t, "198.51.100.1", http.MethodPost, "/v1/me/verifications", a.addSignedIn(t, "kova3@example.com"),
		startBody("+905324440007", userPw))
	wantStatus(t, rec, http.StatusTooManyRequests, httpx.CodeRateLimited)
	// A patch of other fields takes none of them.
	wantStatus(t, a.patchFrom(t, "198.51.100.1", first, `{"jobTitle":"Kova dışı"}`), http.StatusOK, "")
}

// USR-022: a profile change writes the user's profile sync intent in its
// own transaction (the rollback half is TestProfilePatchSharesTheStartBudget);
// a change while it is open refreshes it, with the origin of the latest
// change, and opens no second one; a patch that changes nothing records
// nothing.
func TestProfileChangeRecordsTheSyncIntent(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "senkron@example.com")
	id := a.userID(t, "senkron@example.com")
	intents := func() int {
		t.Helper()
		return a.count(t, "SELECT count(*) FROM user_profile_sync_intents WHERE user_id = $1", id)
	}

	wantStatus(t, a.patchProfile(t, tok, `{}`), http.StatusOK, "")
	if intents() != 0 {
		t.Fatal("an empty patch recorded a profile change")
	}
	wantStatus(t, a.patchProfile(t, tok, `{"jobTitle":"Forkliftçi"}`), http.StatusOK, "")
	first := a.text(t, "SELECT updated_at::text FROM user_profile_sync_intents WHERE user_id = $1", id)
	if intents() != 1 ||
		a.text(t, "SELECT request_id || ' ' || device_key FROM user_profile_sync_intents WHERE user_id = $1", id) !=
			"req-profile "+testDevice ||
		a.count(t, "SELECT count(*) FROM user_profile_sync_intents i JOIN user_sessions s ON s.id = i.session_id WHERE i.user_id = $1", id) != 1 {
		t.Fatal("USR-022: the intent does not carry the change's origin")
	}
	wantStatus(t, a.patchProfile(t, tok, `{"jobTitle":"Forkliftçi"}`), http.StatusOK, "")
	a.clock.Advance(time.Minute)
	wantStatus(t, a.patchProfile(t, tok, `{"jobTitle":"Depocu"}`), http.StatusOK, "")
	if intents() != 1 || a.text(t, "SELECT updated_at::text FROM user_profile_sync_intents WHERE user_id = $1", id) == first {
		t.Fatal("USR-022: a second change did not refresh the one open intent")
	}
}
