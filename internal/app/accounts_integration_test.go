//go:build integration

package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// The two requests that take an address answer a registered and an
// unknown one with the same status, headers and body bytes, and do the
// same work: one queued job each (USR-001, USR-018, ADR-0032).
func TestAccountRequestsDoNotTellAddressesApart(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addUser(t)

	for _, tc := range []struct{ id, path, kind, body string }{
		{"USR-001", "/v1/auth/registrations", "registration_requested", `{"firstName":"Ayşe","lastName":"Yılmaz","employer":false,"email":"`},
		{"USR-018", "/v1/auth/password-reset", "password_reset_requested", `{"email":"`},
	} {
		known := a.call(t, http.MethodPost, tc.path, "", tc.body+userEmail+`"}`)
		unknown := a.call(t, http.MethodPost, tc.path, "", tc.body+"kimse@example.com"+`"}`)
		wantStatus(t, known, http.StatusAccepted, "")
		if unknown.Code != known.Code || !bytes.Equal(known.Body.Bytes(), unknown.Body.Bytes()) {
			t.Errorf("%s: answers differ: %d %q / %d %q", tc.id, known.Code, known.Body, unknown.Code, unknown.Body)
		}
		for _, h := range []string{"Cache-Control", "Content-Type", "Content-Length"} {
			if known.Header().Get(h) != unknown.Header().Get(h) {
				t.Errorf("%s: %s differs: %q / %q", tc.id, h, known.Header().Get(h), unknown.Header().Get(h))
			}
		}
		if cc := known.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", tc.id, cc)
		}
		var n int
		if err := a.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind = $1", tc.kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("%s: %d jobs queued, want one per request", tc.id, n)
		}
	}
}

// The account operations are wired through the contract: their refusals
// carry the codes the contract lists, and the in-app change answers 204.
func TestAccountOperationsOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addUser(t)

	rec := a.call(t, http.MethodPost, "/v1/auth/registrations/verify", "", `{"verificationToken":"`+strings.Repeat("A", 43)+`"}`)
	wantStatus(t, rec, http.StatusBadRequest, string(authn.CodeRegistrationInvalid))

	complete := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/auth/registrations/complete", strings.NewReader(body))
		req.RemoteAddr = "192.0.2.1:40000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(httpx.HeaderIdempotencyKey, "0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a10")
		setClient(req.Header)
		rec := httptest.NewRecorder()
		a.router.ServeHTTP(rec, req)
		return rec
	}
	rec = complete(`{"registrationToken":"` + strings.Repeat("A", 43) + `","password":"on beş karakter",` +
		`"consents":[{"documentCode":"TERMS_OF_SERVICE","version":"1.0","granted":true}]}`)
	wantStatus(t, rec, http.StatusBadRequest, string(authn.CodeConsentRequired))

	rec = a.call(t, http.MethodPost, "/v1/auth/password-reset/complete", "",
		`{"resetToken":"`+strings.Repeat("A", 43)+`","newPassword":"on beş karakter"}`)
	wantStatus(t, rec, http.StatusBadRequest, string(authn.CodeResetLinkInvalid))

	tok := a.loginUser(t)
	rec = a.call(t, http.MethodPut, "/v1/me/password", tok.AccessToken,
		`{"currentPassword":"yanlış ama uzun parola","newPassword":"on beş karakter","newPasswordConfirmation":"on beş karakter"}`)
	wantStatus(t, rec, http.StatusForbidden, string(authn.CodeCurrentMismatch))
	rec = a.call(t, http.MethodPut, "/v1/me/password", tok.AccessToken,
		`{"currentPassword":"`+userPw+`","newPassword":"on beş karakter","newPasswordConfirmation":"on beş karakter"}`)
	wantStatus(t, rec, http.StatusNoContent, "")
	// The caller's session stays.
	rec = a.call(t, http.MethodGet, "/v1/me", tok.AccessToken, "")
	wantStatus(t, rec, http.StatusOK, "")
}

// The account requests take their tokens from the buckets of their target
// mailbox and their presented token, with the production bursts
// (ADR-0032 Karar 6): a sub-address is the same target, another mailbox
// is not, and one mailed token cannot be hammered either.
func TestAccountRateLimitKeys(t *testing.T) {
	t.Parallel()
	groups := rateLimitGroups()
	slow := func(r httpx.Rate) httpx.Rate { return httpx.Rate{Every: time.Hour, Burst: r.Burst} }
	auth, account := groups[groupAuth], groups[groupAuthAccount]
	auth.PerCustom, account.PerCustom = slow(auth.PerCustom), slow(account.PerCustom)
	groups[groupAuth], groups[groupAuthAccount] = auth, account
	a := newAuthApp(t, groups)
	start := func(addr, email string) *httptest.ResponseRecorder {
		t.Helper()
		return a.callFrom(t, addr, http.MethodPost, "/v1/auth/registrations", "",
			`{"firstName":"A","lastName":"B","employer":false,"email":"`+email+`"}`)
	}

	pair := auth.PerCustom.Burst
	for range pair {
		wantStatus(t, start("192.0.2.10", "hedef@example.com"), http.StatusAccepted, "")
	}
	// The same mailbox, by its sub-address: the (target, address) bucket
	// is spent.
	wantStatus(t, start("192.0.2.10", "hedef+baska@example.com"), http.StatusTooManyRequests, httpx.CodeRateLimited)
	// Another mailbox from the same address: not limited.
	wantStatus(t, start("192.0.2.10", "baska@example.com"), http.StatusAccepted, "")
	// The mailbox's account-wide bucket, from other addresses.
	for range account.PerCustom.Burst - pair {
		wantStatus(t, start("192.0.2.11", "hedef@example.com"), http.StatusAccepted, "")
	}
	wantStatus(t, start("192.0.2.12", "hedef@example.com"), http.StatusTooManyRequests, httpx.CodeRateLimited)

	// One presented token: its bucket, whatever the address.
	token := `{"verificationToken":"` + strings.Repeat("B", 43) + `"}`
	for i := range pair {
		wantStatus(t, a.callFrom(t, "192.0.2."+strconv.Itoa(20+i), http.MethodPost, "/v1/auth/registrations/verify", "", token),
			http.StatusBadRequest, string(authn.CodeRegistrationInvalid))
	}
	wantStatus(t, a.callFrom(t, "192.0.2.30", http.MethodPost, "/v1/auth/registrations/verify", "", token),
		http.StatusTooManyRequests, httpx.CodeRateLimited)
}

// One client address has its own low hourly budget of registration
// starts, whatever the mailboxes, so it cannot fill the global hourly cap
// on its own; another address is not held by it, and a reset request does
// not spend it (ADR-0032 Karar 6).
func TestRegistrationStartsPerAddress(t *testing.T) {
	t.Parallel()
	var cfg config.Config
	cfg.Mail.RegistrationStartsPerIP = 3
	a := newAuthApp(t, serveRateLimitGroups(cfg))
	start := func(addr, email string) *httptest.ResponseRecorder {
		t.Helper()
		return a.callFrom(t, addr, http.MethodPost, "/v1/auth/registrations", "",
			`{"firstName":"A","lastName":"B","employer":false,"email":"`+email+`"}`)
	}
	for i := range 3 {
		wantStatus(t, start("198.51.100.7", "kutu"+strconv.Itoa(i)+"@example.com"), http.StatusAccepted, "")
	}
	wantStatus(t, start("198.51.100.7", "kutu9@example.com"), http.StatusTooManyRequests, httpx.CodeRateLimited)
	wantStatus(t, start("198.51.100.8", "kutu9@example.com"), http.StatusAccepted, "")
	wantStatus(t, a.callFrom(t, "198.51.100.7", http.MethodPost, "/v1/auth/password-reset", "", `{"email":"kutu9@example.com"}`),
		http.StatusAccepted, "")
}
