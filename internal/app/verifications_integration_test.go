//go:build integration

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
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
	v, err := authn.NewVerifications(a.mods.users, queue, noMail{}, texts, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SendCode(t.Context(), id, authn.ChannelPhone); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`[0-9]{6}`).FindString(texts.last)
	if m == "" {
		t.Fatalf("no code in %q", texts.last)
	}
	return m
}

// startOverHTTP starts a phone verification as bearer and returns its id.
func (a *authApp) startOverHTTP(t *testing.T, bearer, number string) uuid.UUID {
	t.Helper()
	rec := a.call(t, http.MethodPost, "/v1/me/verifications", bearer, `{"channel":"PHONE","target":"`+number+`"}`)
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
	rec := a.call(t, http.MethodPost, "/v1/me/verifications", tok, `{"channel":"PHONE","target":"+905321234567"}`)
	wantStatus(t, rec, http.StatusTooManyRequests, string(authn.CodeVerificationResendTooSoon))
	if ra := rec.Header().Get("Retry-After"); ra != "60" {
		t.Errorf("USR-010: Retry-After = %q, want 60", ra)
	}
	rec = a.call(t, http.MethodPost, "/v1/me/verifications", tok, `{"channel":"PHONE","target":"+447700900123"}`)
	wantStatus(t, rec, http.StatusBadRequest, "REQUEST_INVALID")

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
		// The instance differs by the path; the rest is the same.
		same("USR-011 "+op.method+op.suffix, theirs,
			withBody(none, strings.ReplaceAll(none.Body.String(), missing.String(), id.String())))
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

// withBody is rec with its body replaced.
func withBody(rec *httptest.ResponseRecorder, body string) *httptest.ResponseRecorder {
	out := httptest.NewRecorder()
	out.Code = rec.Code
	_, _ = out.WriteString(body)
	return out
}
