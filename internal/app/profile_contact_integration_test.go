//go:build integration

package app

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/mailer"
	"github.com/is-koala/koala-api/internal/platform/sms"
)

// codeMails keeps the mails a second verification service sends, like
// codeTexts for texts.
type codeMails struct {
	mu   sync.Mutex
	last string
}

func (c *codeMails) Send(_ context.Context, m mailer.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = m.Text
	return nil
}

type noText struct{}

func (noText) Send(context.Context, sms.Message) error { return nil }

// mailCodeFor mints and returns a fresh code for an e-mail verification.
func (a *authApp) mailCodeFor(t *testing.T, id uuid.UUID) string {
	t.Helper()
	queue, err := jobs.NewClient(a.db.Pool(), jobs.Config{Logger: log.New(&strings.Builder{}, 0)})
	if err != nil {
		t.Fatal(err)
	}
	mails := &codeMails{}
	v, err := authn.NewVerifications(a.mods.users, queue, mails, noText{}, authn.VerificationCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SendCode(t.Context(), id, authn.ChannelEmail); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\d{6}`).FindString(mails.last)
	if m == "" {
		t.Fatal("no code in the mail")
	}
	return m
}

// confirm confirms a verification over HTTP as bearer.
func (a *authApp) confirm(t *testing.T, bearer string, id uuid.UUID, code string) {
	t.Helper()
	wantStatus(t, a.call(t, http.MethodPost, "/v1/me/verifications/"+id.String()+"/confirm", bearer, `{"code":"`+code+`"}`),
		http.StatusOK, "")
}

// The e-mail half of USR-014: a new address in the patch starts an e-mail
// verification and is not written; the address on file, however it is
// spelled, starts nothing; the confirmation writes it and records the
// profile sync intent in its transaction (USR-022).
func TestProfileEmailChangeWaitsForConfirmation(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	const email = "eposta@example.com"
	tok := a.addSignedIn(t, email)
	id := a.userID(t, email)

	rec := a.patchProfile(t, tok, `{"email":"yeni.adres@example.com","currentPassword":"`+userPw+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	up := decodeBody[profileUpdate](t, rec)
	if len(up.PendingVerifications) != 1 || up.PendingVerifications[0].Channel != authn.ChannelEmail ||
		up.PendingVerifications[0].MaskedTarget != "y***@example.com" {
		t.Fatalf("USR-014: the patch started %s", rec.Body)
	}
	if got := a.text(t, "SELECT email FROM active_users WHERE id = $1", id); got != email {
		t.Fatalf("USR-014: the patch wrote the address: %s", got)
	}
	if a.count(t, "SELECT count(*) FROM user_verifications WHERE user_id = $1 AND channel = 'EMAIL' AND target = 'yeni.adres@example.com'", id) != 1 {
		t.Fatal("USR-014: no e-mail verification of the new address")
	}

	// The address on file, in another spelling of the same address.
	rec = a.patchProfile(t, tok, `{"email":" EPOSTA@example.com","currentPassword":"`+userPw+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	if up := decodeBody[profileUpdate](t, rec); len(up.PendingVerifications) != 0 {
		t.Fatalf("USR-014: the address on file started %s", rec.Body)
	}
	if a.count(t, "SELECT count(*) FROM user_verifications WHERE user_id = $1 AND target = $2", id, email) != 0 {
		t.Fatal("USR-014: the address on file got a verification")
	}

	a.confirm(t, tok, up.PendingVerifications[0].ID, a.mailCodeFor(t, up.PendingVerifications[0].ID))
	if got := a.text(t, "SELECT email FROM active_users WHERE id = $1", id); got != "yeni.adres@example.com" {
		t.Fatalf("USR-014: after the code the address is %s", got)
	}
	if a.count(t, "SELECT count(*) FROM user_profile_sync_intents WHERE user_id = $1 AND published_at IS NULL", id) != 1 {
		t.Fatal("USR-022: the confirmed address recorded no profile sync intent")
	}
}

// A number confirmed on another account moves to it (ADR-0033 Karar 10)
// and both profiles change: the new holder's intent carries its own
// origin, the previous holder's only the request that moved the number,
// since that user's session and device made no change (USR-022, ADR-0036).
// A value confirmed again unchanged records nothing.
func TestConfirmationRecordsTheSyncIntents(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	const number = "+905326660001"
	a.addSignedIn(t, "eski-sahip@example.com")
	holder := a.userID(t, "eski-sahip@example.com")
	if _, err := a.maint.Pool().Exec(t.Context(), "UPDATE users SET phone = $2, phone_verified = true WHERE id = $1", holder, number); err != nil {
		t.Fatal(err)
	}
	tok := a.addSignedIn(t, "yeni-sahip@example.com")
	claimer := a.userID(t, "yeni-sahip@example.com")

	ver := a.startOverHTTP(t, tok, number)
	a.confirm(t, tok, ver, a.codeFor(t, ver))
	origin := func(user uuid.UUID) string {
		t.Helper()
		return a.text(t, `
			SELECT coalesce(request_id, '-') || ' ' || coalesce(session_id::text, '-') || ' ' || coalesce(device_key, '-')
			FROM user_profile_sync_intents WHERE user_id = $1 AND published_at IS NULL`, user)
	}
	if got := origin(holder); got != "req-auth - -" {
		t.Fatalf("USR-022: the previous holder's intent = %q, want the request only", got)
	}
	if got := origin(claimer); !strings.HasPrefix(got, "req-auth ") || strings.Contains(got, " - ") ||
		!strings.HasSuffix(got, " "+testDevice) {
		t.Fatalf("USR-022: the new holder's intent = %q, want its request, session and device", got)
	}

	// The same number confirmed again by its holder changes nothing and
	// records nothing.
	// The application never deletes an intent; the owner clears the slate.
	owner := a.dbt.Open(t, db.RoleOwner)
	if _, err := owner.Pool().Exec(t.Context(), "DELETE FROM user_profile_sync_intents WHERE user_id = $1", claimer); err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(authn.VerificationCooldown)
	again := a.startOverHTTP(t, tok, number)
	a.confirm(t, tok, again, a.codeFor(t, again))
	if n := a.count(t, "SELECT count(*) FROM user_profile_sync_intents WHERE user_id = $1", claimer); n != 0 {
		t.Fatalf("USR-022: the number on file confirmed again recorded %d intents", n)
	}
}

// StartTx checks the password under the user's row lock: a password
// changed after Reauthenticate proved the old one refuses the start, and
// nothing is written, no row and no code job. A proof older than
// ReauthenticationTTL is refused the same way (ADR-0033 Karar 1b,
// ADR-0036).
func TestStartTxRechecksThePassword(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	const email = "kilit-alti@example.com"
	a.addSignedIn(t, email)
	id := a.userID(t, email)
	pair, err := a.mods.users.Login(t.Context(), authn.LoginInput{
		Email: email, Password: userPw, DeviceKey: testDevice, Platform: string(httpx.PlatformIOS),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := authn.UserPrincipal{UserID: id, SessionID: pair.SessionID, DeviceKey: testDevice}
	start := func(r authn.Reauthentication, number string) error {
		t.Helper()
		return a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
			_, err := a.mods.verifications.StartTx(ctx, q, p, r, authn.ChannelPhone, number, "/target")
			return err
		})
	}
	written := func() (rows, queued int) {
		t.Helper()
		return a.count(t, "SELECT count(*) FROM user_verifications WHERE user_id = $1", id),
			a.count(t, "SELECT count(*) FROM river_job WHERE kind = 'verification_code'")
	}

	// A stale proof.
	stale, err := a.mods.verifications.Reauthenticate(t.Context(), p, userPw)
	if err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(authn.ReauthenticationTTL + time.Second)
	if err := start(stale, "+905327770001"); apperr.From(err).Code != authn.CodeCurrentMismatch {
		t.Fatalf("a proof past ReauthenticationTTL: %v, want %s", err, authn.CodeCurrentMismatch)
	}
	// A fresh one opens it.
	fresh, err := a.mods.verifications.Reauthenticate(t.Context(), p, userPw)
	if err != nil {
		t.Fatal(err)
	}
	if err := start(fresh, "+905327770002"); err != nil {
		t.Fatalf("a fresh proof: %v", err)
	}
	if rows, queued := written(); rows != 1 || queued != 1 {
		t.Fatalf("a fresh start wrote %d rows and %d jobs, want 1 and 1", rows, queued)
	}

	// The password changes between the proof and the start.
	proof, err := a.mods.verifications.Reauthenticate(t.Context(), p, userPw)
	if err != nil {
		t.Fatal(err)
	}
	const next = "yepyeni ve epey uzun bir parola"
	if err := a.mods.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: userPw, New: next, Confirmation: next}); err != nil {
		t.Fatal(err)
	}
	if err := start(proof, "+905327770003"); apperr.From(err).Code != authn.CodeCurrentMismatch {
		t.Fatalf("a proof of the previous password: %v, want %s", err, authn.CodeCurrentMismatch)
	}
	if rows, queued := written(); rows != 1 || queued != 1 {
		t.Fatalf("a refused start wrote: %d rows and %d jobs, want the fresh start's 1 and 1", rows, queued)
	}
}

// The patch's address takes the start's tokens too (the e-mail branch of
// the shared keys, ADR-0036): with a caller bucket of one, a second
// address from another client address is refused by the bucket.
func TestProfileEmailPatchTakesTheStartBuckets(t *testing.T) {
	t.Parallel()
	groups := rateLimitGroups()
	auth := groups[groupAuth]
	auth.PerCustom = httpx.Rate{Every: time.Hour, Burst: 1}
	groups[groupAuth] = auth
	a := newAuthApp(t, groups)
	tok := a.addSignedIn(t, "kova-eposta@example.com")
	wantStatus(t, a.patchFrom(t, "198.51.100.21", tok, `{"email":"bir@example.com","currentPassword":"`+userPw+`"}`),
		http.StatusOK, "")
	wantStatus(t, a.patchFrom(t, "198.51.100.22", tok, `{"email":"iki@example.com","currentPassword":"`+userPw+`"}`),
		http.StatusTooManyRequests, httpx.CodeRateLimited)
}
