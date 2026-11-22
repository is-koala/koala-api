//go:build integration

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/mailer"
	"github.com/is-koala/koala-api/internal/platform/sms"
)

// codeTexts keeps the texts a second verification service sends: the
// wired one drops them (no provider in test), and a service on the same
// user service mints codes under the same key.
type codeTexts struct {
	mu   sync.Mutex
	last string
}

func (c *codeTexts) Send(_ context.Context, m sms.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = m.Text
	return nil
}

type noMail struct{}

func (noMail) Send(context.Context, mailer.Message) error { return nil }

// codeFor mints and returns a fresh code for a verification started over
// HTTP.
func (a *authApp) codeFor(t *testing.T, id uuid.UUID) string {
	t.Helper()
	queue, err := jobs.NewClient(a.db.Pool(), jobs.Config{Logger: log.New(&strings.Builder{}, 0)})
	if err != nil {
		t.Fatal(err)
	}
	texts := &codeTexts{}
	v, err := authn.NewVerifications(a.mods.users, queue, noMail{}, texts, authn.VerificationCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SendCode(t.Context(), id, authn.ChannelPhone); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\d{6}`).FindString(texts.last)
	if m == "" {
		t.Fatalf("no code in %q", texts.last)
	}
	return m
}

// startOverHTTP starts a phone verification as bearer and returns its id.
func (a *authApp) startOverHTTP(t *testing.T, bearer, number string) uuid.UUID {
	t.Helper()
	rec := a.call(t, http.MethodPost, "/v1/me/verifications", bearer, startBody(number, userPw))
	wantStatus(t, rec, http.StatusCreated, "")
	v := decodeBody[struct {
		ID           uuid.UUID `json:"id"`
		MaskedTarget string    `json:"maskedTarget"`
		Status       string    `json:"status"`
	}](t, rec)
	if loc := rec.Header().Get("Location"); loc != "/v1/me/verifications/"+v.ID.String() {
		t.Errorf("Location = %q", loc)
	}
	if v.Status != "PENDING" || strings.Contains(rec.Body.String(), "1234567") {
		t.Errorf("start answer %s: not pending, or the number shows", rec.Body)
	}
	return v.ID
}

// The verification operations are wired through the contract: the start
// answers 201 with the masked target, a resend within the cooldown is
// 429 VERIFICATION_RESEND_TOO_SOON with Retry-After (USR-010), a wrong
// code is 400, and a cancel is 204 and then the 404 of a missing one.
func TestVerificationOperationsOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addUser(t)
	tok := a.loginUser(t).AccessToken

	id := a.startOverHTTP(t, tok, "0532 123 45 67")
	rec := a.call(t, http.MethodPost, "/v1/me/verifications", tok, startBody("+905321234567", userPw))
	wantStatus(t, rec, http.StatusTooManyRequests, string(authn.CodeVerificationResendTooSoon))
	if ra := rec.Header().Get("Retry-After"); ra != "60" {
		t.Errorf("USR-010: Retry-After = %q, want 60", ra)
	}
	rec = a.call(t, http.MethodPost, "/v1/me/verifications", tok, startBody("+447700900123", userPw))
	wantStatus(t, rec, http.StatusBadRequest, "REQUEST_INVALID")
	// The current password is required, and a wrong one is refused
	// (ADR-0033 Karar 1b).
	rec = a.call(t, http.MethodPost, "/v1/me/verifications", tok, `{"channel":"PHONE","target":"+905329876543"}`)
	wantStatus(t, rec, http.StatusBadRequest, "REQUEST_INVALID")
	rec = a.call(t, http.MethodPost, "/v1/me/verifications", tok, startBody("+905329876543", "yanlış ama uzun bir parola"))
	wantStatus(t, rec, http.StatusForbidden, string(authn.CodeCurrentMismatch))

	rec = a.call(t, http.MethodGet, "/v1/me/verifications", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), id.String()) || !strings.Contains(rec.Body.String(), `"maskedTarget":"+90******4567"`) {
		t.Errorf("list = %s", rec.Body)
	}
	rec = a.call(t, http.MethodGet, "/v1/me/verifications?cursor=bozuk", tok, "")
	wantStatus(t, rec, http.StatusBadRequest, string(authn.CodeCursorInvalid))

	code := a.codeFor(t, id)
	wrongCode := "000000"
	if code == wrongCode {
		wrongCode = "000001"
	}
	rec = a.call(t, http.MethodPost, "/v1/me/verifications/"+id.String()+"/confirm", tok, `{"code":"`+wrongCode+`"}`)
	wantStatus(t, rec, http.StatusBadRequest, string(authn.CodeVerificationWrong))
	rec = a.call(t, http.MethodGet, "/v1/me/verifications/"+id.String(), tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), `"remainingAttempts":4`) {
		t.Errorf("after a wrong code = %s", rec.Body)
	}
	rec = a.call(t, http.MethodPost, "/v1/me/verifications/"+id.String()+"/confirm", tok, `{"code":"`+code+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), `"status":"CONFIRMED"`) {
		t.Errorf("confirm = %s", rec.Body)
	}

	a.clock.Advance(authn.VerificationCooldown)
	other := a.startOverHTTP(t, tok, "05329876543")
	rec = a.call(t, http.MethodDelete, "/v1/me/verifications/"+other.String(), tok, "")
	wantStatus(t, rec, http.StatusNoContent, "")
	rec = a.call(t, http.MethodDelete, "/v1/me/verifications/"+other.String(), tok, "")
	wantStatus(t, rec, http.StatusNotFound, string(authn.CodeVerificationNotFound))
}

// A refusal tells nothing it should not, byte for byte: another user's
// verification is the 404 of one that never existed, for every operation
// and even with the right code (USR-011); after the expiry the right and
// a wrong code get the same body (USR-012).
func TestVerificationRefusalsDoNotTellApart(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addUser(t)
	owner := a.loginUser(t).AccessToken
	hash, err := a.passwords.Hash(t.Context(), userPw)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.q.InsertUser(t.Context(), authndb.InsertUserParams{
		ID: uuid.Must(uuid.NewV7()), Email: "baska@example.com", PasswordHash: &hash, Status: authn.UserActive, CreatedAt: a.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rec := a.call(t, http.MethodPost, "/v1/auth/login", "", `{"email":"baska@example.com","password":"`+userPw+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	intruder := decodeBody[tokens](t, rec).AccessToken

	id := a.startOverHTTP(t, owner, "+905321234567")
	code := a.codeFor(t, id)
	missing := uuid.New()
	same := func(what string, x, y *httptest.ResponseRecorder) {
		t.Helper()
		if x.Code != y.Code || x.Body.String() != y.Body.String() {
			t.Errorf("%s: %d %s / %d %s", what, x.Code, x.Body, y.Code, y.Body)
		}
	}
	for _, op := range []struct{ method, suffix, body string }{
		{http.MethodPost, "/confirm", `{"code":"` + code + `"}`},
		{http.MethodGet, "", ""},
		{http.MethodDelete, "", ""},
	} {
		theirs := a.call(t, op.method, "/v1/me/verifications/"+id.String()+op.suffix, intruder, op.body)
		none := a.call(t, op.method, "/v1/me/verifications/"+missing.String()+op.suffix, intruder, op.body)
		wantStatus(t, theirs, http.StatusNotFound, string(authn.CodeVerificationNotFound))
		// The instance is the route template, so the bodies are the same
		// bytes.
		same("USR-011 "+op.method+op.suffix, theirs, none)
	}

	a.clock.Advance(authn.VerificationTTL)
	right := a.call(t, http.MethodPost, "/v1/me/verifications/"+id.String()+"/confirm", owner, `{"code":"`+code+`"}`)
	wrongCode := "000000"
	if code == wrongCode {
		wrongCode = "000001"
	}
	wrong := a.call(t, http.MethodPost, "/v1/me/verifications/"+id.String()+"/confirm", owner, `{"code":"`+wrongCode+`"}`)
	wantStatus(t, right, http.StatusBadRequest, string(authn.CodeVerificationExpired))
	same("USR-012 right and wrong code after the expiry", right, wrong)
}

// startBody is a phone verification start with the current password.
func startBody(number, password string) string {
	return `{"channel":"PHONE","target":"` + number + `","currentPassword":"` + password + `"}`
}

// addSignedIn creates a user with userPw and returns an access token of
// it on testDevice, signed in without the HTTP buckets.
func (a *authApp) addSignedIn(t *testing.T, email string) string {
	t.Helper()
	hash, err := a.passwords.Hash(t.Context(), userPw)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.q.InsertUser(t.Context(), authndb.InsertUserParams{
		ID: uuid.Must(uuid.NewV7()), Email: email, PasswordHash: &hash, Status: authn.UserActive, CreatedAt: a.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	pair, err := a.mods.users.Login(t.Context(), authn.LoginInput{
		Email: email, Password: userPw, DeviceKey: testDevice, Platform: string(httpx.PlatformIOS),
	})
	if err != nil {
		t.Fatal(err)
	}
	return pair.AccessToken
}

// The verification operations take their tokens from the production
// buckets (ADR-0033 Karar 4): a start from the caller's bucket and the
// (keyed target, address) bucket, with no target-wide bucket, so nobody
// can drain the target's own budget; a confirmation from its
// verification's bucket, whatever the address.
func TestVerificationRateLimitKeys(t *testing.T) {
	t.Parallel()
	groups := rateLimitGroups()
	auth := groups[groupAuth]
	auth.PerCustom = httpx.Rate{Every: time.Hour, Burst: auth.PerCustom.Burst}
	groups[groupAuth] = auth
	a := newAuthApp(t, groups)
	start := func(addr, token, number string) *httptest.ResponseRecorder {
		t.Helper()
		return a.callFrom(t, addr, http.MethodPost, "/v1/me/verifications", token, startBody(number, userPw))
	}
	u1, u2, u3 := a.addSignedIn(t, "u1@example.com"), a.addSignedIn(t, "u2@example.com"), a.addSignedIn(t, "u3@example.com")

	burst := auth.PerCustom.Burst
	wantStatus(t, start("192.0.2.10", u1, "+905321234567"), http.StatusCreated, "")
	for range burst - 1 {
		// The bucket lets them through; the target's cooldown refuses.
		wantStatus(t, start("192.0.2.10", u1, "0532 123 45 67"), http.StatusTooManyRequests, string(authn.CodeVerificationResendTooSoon))
	}
	wantStatus(t, start("192.0.2.10", u1, "+905321234567"), http.StatusTooManyRequests, httpx.CodeRateLimited)
	// The caller's bucket: another number from another address.
	wantStatus(t, start("192.0.2.11", u1, "+905329876543"), http.StatusTooManyRequests, httpx.CodeRateLimited)
	// The (target, address) bucket: another caller, same number and
	// address.
	wantStatus(t, start("192.0.2.10", u2, "5321234567"), http.StatusTooManyRequests, httpx.CodeRateLimited)
	// No target-wide bucket: the number's next caller from elsewhere
	// reaches the database budget, not a drained bucket.
	wantStatus(t, start("192.0.2.12", u3, "+905321234567"), http.StatusTooManyRequests, string(authn.CodeVerificationResendTooSoon))

	// A confirmation: its verification's bucket, whatever the address.
	path := "/v1/me/verifications/" + uuid.NewString() + "/confirm"
	for i := range burst {
		wantStatus(t, a.callFrom(t, "192.0.2."+strconv.Itoa(20+i), http.MethodPost, path, u3, `{"code":"000000"}`),
			http.StatusNotFound, string(authn.CodeVerificationNotFound))
	}
	wantStatus(t, a.callFrom(t, "192.0.2.30", http.MethodPost, path, u3, `{"code":"000000"}`),
		http.StatusTooManyRequests, httpx.CodeRateLimited)
}

// One client address has its own hourly budget of verification starts,
// whatever the callers and numbers, so it cannot fill the global text cap
// alone; it is asked first, and another address is not held by it
// (ADR-0033 Karar 4).
func TestVerificationStartsPerAddress(t *testing.T) {
	t.Parallel()
	var cfg config.Config
	cfg.Verification.StartsPerIP = 2
	a := newAuthApp(t, serveRateLimitGroups(cfg))
	tokens := []string{a.addSignedIn(t, "a1@example.com"), a.addSignedIn(t, "a2@example.com"), a.addSignedIn(t, "a3@example.com")}
	start := func(addr string, i int) *httptest.ResponseRecorder {
		t.Helper()
		return a.callFrom(t, addr, http.MethodPost, "/v1/me/verifications", tokens[i], startBody("+90532111223"+strconv.Itoa(i), userPw))
	}
	wantStatus(t, start("198.51.100.7", 0), http.StatusCreated, "")
	wantStatus(t, start("198.51.100.7", 1), http.StatusCreated, "")
	wantStatus(t, start("198.51.100.7", 2), http.StatusTooManyRequests, httpx.CodeRateLimited)
	wantStatus(t, start("198.51.100.8", 2), http.StatusCreated, "")
}
