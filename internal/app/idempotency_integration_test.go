//go:build integration

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
)

// newRegistrationToken writes an open registration for email at its
// second step, as verifyRegistration leaves it, and returns its
// registration token.
func (a *authApp) newRegistrationToken(t *testing.T, email string) string {
	t.Helper()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	mailbox := make([]byte, 32)
	_, _ = rand.Read(mailbox)
	digest := sha256.Sum256([]byte(token))
	now := a.clock.Now()
	_, err := a.db.Pool().Exec(t.Context(), `
		INSERT INTO pending_registrations (id, email, mailbox_key, first_name, last_name, employer, created_at,
			registration_token_hash, registration_expires_at)
		VALUES ($1, $2, $3, 'Ayşe', 'Yılmaz', false, $4, $5, $6)`,
		uuid.Must(uuid.NewV7()), email, mailbox, now, digest[:], now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// completionBody is a completeRegistration body that grants every
// mandatory agreement.
func completionBody(token, password string) string {
	return `{"registrationToken":"` + token + `","password":"` + password + `","consents":[` +
		`{"documentCode":"TERMS_OF_SERVICE","version":"1.0","granted":true},` +
		`{"documentCode":"PRIVACY_POLICY","version":"1.0","granted":true},` +
		`{"documentCode":"EXPLICIT_CONSENT","version":"1.0","granted":true},` +
		`{"documentCode":"COMMERCIAL_MESSAGE","version":"1.0","granted":false}]}`
}

// postKeyed sends a POST with an Idempotency-Key from a current iOS build.
func (a *authApp) postKeyed(t *testing.T, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.1:40000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpx.HeaderIdempotencyKey, key)
	setClient(req.Header)
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

// counted reads koala_idempotency_requests_total for op and result.
func (a *authApp) counted(t *testing.T, op, result string) float64 {
	t.Helper()
	mfs, err := a.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != idempotency.MetricRequests {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["operation"] == op && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("%s{operation=%s,result=%s} is not registered", idempotency.MetricRequests, op, result)
	return 0
}

func isReplay(rec *httptest.ResponseRecorder) bool {
	return rec.Header().Get(httpx.HeaderIdempotentReplayed) == "true"
}

// idempotentFixtures build one valid request of every operation the store
// binds, on a fresh app: its path and body. The test holds this list
// equal to the contract, so the handler PR that lifts x-not-implemented
// from an operation that takes the key adds its fixture here.
func idempotentFixtures() map[string]func(t *testing.T, a *authApp) (path, body string) {
	return map[string]func(t *testing.T, a *authApp) (string, string){
		"completeRegistration": func(t *testing.T, a *authApp) (string, string) {
			t.Helper()
			return "/v1/auth/registrations/complete", completionBody(a.newRegistrationToken(t, "yeni@example.com"), userPw)
		},
	}
}

// TestIdempotentOperationsReplay is the dynamic half of the binding of the
// Idempotency-Key store (ADR-0034 Karar 6; the static half is archtest
// TestIdempotentHandlersCommitTheirKey). Its operations come from the
// contract: every one that takes the key and has a handler, at least one.
// Each runs once through the wired router on the database and is sent
// again with the same key:
//
//   - the first answer is a success the operation committed in its own
//     transaction (counted as committed, never as unbound: a handler that
//     stores its success after the transaction fails here);
//   - the second is the first again, byte for byte, marked as a replay.
func TestIdempotentOperationsReplay(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := idempotency.Operations(spec)
	if err != nil {
		t.Fatal(err)
	}
	var bound []string
	for _, op := range ops {
		if !op.Frozen {
			bound = append(bound, op.ID)
		}
	}
	fixtures := idempotentFixtures()
	if have := slices.Sorted(maps.Keys(fixtures)); !slices.Equal(have, bound) {
		t.Fatalf("fixtures for %v, but the contract binds %v: write a fixture for every bound operation", have, bound)
	}
	if len(bound) == 0 {
		t.Fatal("the contract binds no operation; the check would prove nothing")
	}
	for _, op := range bound {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			a := newAuthApp(t, unlimitedGroups())
			path, body := fixtures[op](t, a)
			first := a.postKeyed(t, path, "fixture-"+op, body)
			if first.Code < 200 || first.Code > 299 || isReplay(first) {
				t.Fatalf("first answer %d (replay %v): %s", first.Code, isReplay(first), first.Body)
			}
			if a.counted(t, op, idempotency.ResultCommitted) != 1 || a.counted(t, op, idempotency.ResultUnbound) != 0 {
				t.Fatalf("%s's success was not committed in its transaction (committed %v, unbound %v)", op,
					a.counted(t, op, idempotency.ResultCommitted), a.counted(t, op, idempotency.ResultUnbound))
			}
			again := a.postKeyed(t, path, "fixture-"+op, body)
			if !isReplay(again) || again.Code != first.Code || !bytes.Equal(again.Body.Bytes(), first.Body.Bytes()) ||
				again.Header().Get("Location") != first.Header().Get("Location") {
				t.Fatalf("second answer %d %s (replay %v), want the first, %d %s",
					again.Code, again.Body, isReplay(again), first.Code, first.Body)
			}
		})
	}
}

// count runs a count query.
func (a *authApp) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := a.db.Pool().QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const (
	usersByEmail    = "SELECT count(*) FROM active_users WHERE email = $1"
	sessionsByEmail = "SELECT count(*) FROM user_sessions s JOIN active_users u ON u.id = s.user_id WHERE u.email = $1"
)

// completeRegistration has no caller; the key belongs to the registration
// token (ADR-0034 Karar 1). Someone who knows or guesses the victim's key
// cannot read the victim's token pair: with another token the key is
// another key, and with the victim's token but not its password the
// request is another request (422). Only the victim's own retry gets the
// stored pair.
func TestRegistrationKeyBelongsToTheToken(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	victim := a.newRegistrationToken(t, "kurban@example.com")
	attacker := a.newRegistrationToken(t, "saldirgan@example.com")
	const key = "0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a10"
	const path = "/v1/auth/registrations/complete"

	first := a.postKeyed(t, path, key, completionBody(victim, userPw))
	wantStatus(t, first, http.StatusCreated, "")
	pair := decodeBody[tokens](t, first)
	leaks := func(rec *httptest.ResponseRecorder) bool {
		return strings.Contains(rec.Body.String(), pair.AccessToken) || strings.Contains(rec.Body.String(), pair.RefreshToken)
	}

	// The attacker's own token: the same key is a new key of another
	// actor, and the attacker registers its own address.
	own := a.postKeyed(t, path, key, completionBody(attacker, userPw))
	wantStatus(t, own, http.StatusCreated, "")
	if isReplay(own) || leaks(own) {
		t.Fatal("another registration token reached the victim's stored response")
	}
	// A guessed token: no such registration, nothing stored is reached.
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	guessed := a.postKeyed(t, path, key, completionBody(base64.RawURLEncoding.EncodeToString(raw), userPw))
	wantStatus(t, guessed, http.StatusBadRequest, string(authn.CodeRegistrationInvalid))
	if isReplay(guessed) || leaks(guessed) {
		t.Fatal("a guessed token reached the victim's stored response")
	}
	// The victim's token without its password: another request with the
	// victim's key.
	stolen := a.postKeyed(t, path, key, completionBody(victim, "bambaşka uzun bir parola"))
	wantStatus(t, stolen, http.StatusUnprocessableEntity, httpx.CodeIdempotencyKeyReused)
	if leaks(stolen) {
		t.Fatal("the token alone reached the victim's stored response")
	}

	// The victim's own retry: the same pair, and one account, one session.
	retry := a.postKeyed(t, path, key, completionBody(victim, userPw))
	if !isReplay(retry) || retry.Code != first.Code || !bytes.Equal(retry.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("the victim's retry = %d %s (replay %v), want the stored pair", retry.Code, retry.Body, isReplay(retry))
	}
	if a.count(t, usersByEmail, "kurban@example.com") != 1 || a.count(t, sessionsByEmail, "kurban@example.com") != 1 {
		t.Fatal("the retry registered or signed in again")
	}
	// The stored pair is sealed: the table does not carry the tokens.
	var leaked int
	err := a.db.Pool().QueryRow(t.Context(),
		"SELECT count(*) FROM idempotency_keys WHERE position($1::bytea IN response) > 0 OR position($2::bytea IN response) > 0",
		[]byte(pair.AccessToken), []byte(pair.RefreshToken)).Scan(&leaked)
	if err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("idempotency_keys stores the token pair in the clear")
	}
}

// Concurrent completions with one key: exactly one registers; the others
// get 409 IDEMPOTENCY_KEY_IN_USE or the same pair, and there is one
// account and one session.
func TestConcurrentRegistrationCompletions(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	body := completionBody(a.newRegistrationToken(t, "yeni@example.com"), userPw)

	const n = 6
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { recs[i] = a.postKeyed(t, "/v1/auth/registrations/complete", "concurrent-key-1", body) })
	}
	wg.Wait()
	var ran *httptest.ResponseRecorder
	for _, rec := range recs {
		switch {
		case rec.Code == http.StatusCreated && !isReplay(rec):
			if ran != nil {
				t.Fatalf("two completions ran: %s and %s", ran.Body, rec.Body)
			}
			ran = rec
		case rec.Code == http.StatusCreated:
		case rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), httpx.CodeIdempotencyKeyInUse):
		default:
			t.Fatalf("answer %d %s", rec.Code, rec.Body)
		}
	}
	if ran == nil {
		t.Fatal("no completion ran")
	}
	for _, rec := range recs {
		if rec.Code == http.StatusCreated && !bytes.Equal(rec.Body.Bytes(), ran.Body.Bytes()) {
			t.Fatal("a replay carries another pair")
		}
	}
	if a.count(t, usersByEmail, "yeni@example.com") != 1 || a.count(t, sessionsByEmail, "yeni@example.com") != 1 {
		t.Fatal("the completion happened more than once")
	}
}
