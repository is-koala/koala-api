//go:build integration

package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
)

// Test modes of createNote, chosen by a header the request hash does not
// cover, so the same request can behave differently on its retry.
const (
	headerMode = "X-Test-Mode"
	headerUser = "X-Test-User"
	// headerGate names a gate the handler waits at before anything else.
	headerGate = "X-Test-Gate"

	modeFail     = "fail"     // write, then fail: the transaction rolls back, 500
	modeLie      = "lie"      // write and commit, then answer 500: an ambiguous commit
	modeUnbound  = "unbound"  // write without Commit, 201
	modeSeparate = "separate" // write in one transaction, Commit in another, 201
	modeLarge    = "large"    // write and Commit a response over the stored bound, 201
	modeRefuse   = "refuse"   // 404 before any write
	modeBusy     = "busy"     // 429 before any write
)

type userKey struct{}

// gate holds one request in its handler until the test opens it.
type gate struct {
	started chan struct{}
	release chan struct{}
}

// syncBuffer is a log sink the handler goroutines share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is createNote over the store on a real database: the handler
// inserts a note (the side effect) and commits its response in the same
// transaction, as an operation's handler does.
type harness struct {
	db     *db.DB
	maint  *db.DB // reads the base table's xmin
	store  *idempotency.Store
	clock  *clock.Fake
	reg    *prometheus.Registry
	router http.Handler
	logs   *syncBuffer

	mu    sync.Mutex
	gates map[string]*gate
}

// gate returns the gate of name, made on first use.
func (h *harness) gate(name string) *gate {
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.gates[name]
	if !ok {
		g = &gate{started: make(chan struct{}, 1), release: make(chan struct{})}
		h.gates[name] = g
	}
	return g
}

// reached waits until a request stands at gate name; open lets it on.
func (h *harness) reached(name string) { <-h.gate(name).started }
func (h *harness) open(name string)    { close(h.gate(name).release) }

func newHarness(t *testing.T) *harness {
	t.Helper()
	d := dbtest.New(t)
	database := d.Open(t, db.RoleApp)
	clk := clock.NewFake(time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	store, err := idempotency.NewStore(database, testSecret(), clk)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		db: database, maint: d.Open(t, db.RoleMaint), store: store, clock: clk, reg: prometheus.NewRegistry(),
		logs: &syncBuffer{}, gates: map[string]*gate{},
	}
	spec := loadSpec(t)
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	mw, err := idempotency.NewMiddleware(spec, idempotency.Config{
		Store: store,
		Caller: func(ctx context.Context) (string, bool) {
			u, ok := ctx.Value(userKey{}).(string)
			return "user:" + u, ok && u != ""
		},
		Public:  map[string]idempotency.PublicOperation{"redeemNote": redeem()},
		Metrics: h.reg, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	validate, err := httpx.ValidateRequests(spec, logger)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	// The request timeout the router puts on every bounded route; the
	// store refuses a request without it.
	r.Use(httpx.Timeout(httpx.RequestTimeout))
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, r.Header.Get(headerUser))))
		})
	})
	r.Group(func(g chi.Router) {
		g.Use(validate, mw.Handler)
		g.Post("/notes", h.createNote(logger))
	})
	h.router = r
	return h
}

type note struct {
	ID      string `json:"id"`
	Body    string `json:"body"`
	Padding string `json:"padding,omitempty"`
}

func writeNote(w http.ResponseWriter, n note) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/notes/"+n.ID)
	w.WriteHeader(http.StatusCreated)
	return json.NewEncoder(w).Encode(n)
}

func (h *harness) createNote(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vb, ok := r.Body.(interface{ ValidatedValue() any })
		if !ok {
			httpx.WriteError(w, r, logger, errors.New("no validated body"))
			return
		}
		body, ok := vb.ValidatedValue().(map[string]any)
		if !ok {
			httpx.WriteError(w, r, logger, errors.New("body is not an object"))
			return
		}
		text, ok := body["body"].(string)
		if !ok {
			httpx.WriteError(w, r, logger, errors.New("body has no text"))
			return
		}
		if name := r.Header.Get(headerGate); name != "" {
			g := h.gate(name)
			g.started <- struct{}{}
			select {
			case <-g.release:
			case <-r.Context().Done():
				return
			}
		}
		mode := r.Header.Get(headerMode)
		switch mode {
		case modeRefuse:
			httpx.WriteError(w, r, logger, apperr.New(apperr.KindNotFound, httpx.CodeRouteNotFound))
			return
		case modeBusy:
			httpx.WriteError(w, r, logger, apperr.New(apperr.KindRateLimited, httpx.CodeRateLimited))
			return
		}
		n := note{ID: uuid.NewString(), Body: text}
		if mode == modeLarge {
			n.Padding = strings.Repeat("x", 1<<20)
		}
		render := func(w http.ResponseWriter) error { return writeNote(w, n) }
		err := h.db.InTx(r.Context(), func(ctx context.Context, q db.Querier) error {
			if _, err := q.Exec(ctx, "INSERT INTO active_example_notes (id, body) VALUES ($1, $2)", n.ID, n.Body); err != nil {
				return err
			}
			switch mode {
			case modeFail:
				return errors.New("the operation failed after its write")
			case modeUnbound, modeSeparate:
				return nil
			}
			return idempotency.Commit(ctx, q, render)
		})
		if err == nil && mode == modeSeparate {
			// The wrong way: the response in a transaction of its own.
			err = h.db.InTx(r.Context(), func(ctx context.Context, q db.Querier) error {
				return idempotency.Commit(ctx, q, render)
			})
		}
		if err == nil && mode == modeLie {
			err = errors.New("the commit's outcome is unknown")
		}
		if err != nil {
			httpx.WriteError(w, r, logger, err)
			return
		}
		_ = writeNote(w, n) // an error means the client went away
	}
}

// post sends createNote as user with key and body, in mode.
func (h *harness) post(t *testing.T, user, key, body, mode string) *httptest.ResponseRecorder {
	t.Helper()
	return h.postAt(t, "", user, key, body, mode)
}

// postAt is post with the handler waiting at gate first.
func (h *harness) postAt(t *testing.T, gateName, user, key, body, mode string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/notes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpx.HeaderIdempotencyKey, key)
	req.Header.Set(headerUser, user)
	if mode != "" {
		req.Header.Set(headerMode, mode)
	}
	if gateName != "" {
		req.Header.Set(headerGate, gateName)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// notes counts the side effects: notes with body.
func (h *harness) notes(t *testing.T, body string) int {
	t.Helper()
	var n int
	if err := h.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM active_example_notes WHERE body = $1", body).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// counted reads the store's counter for createNote and result.
func (h *harness) counted(t *testing.T, result string) float64 {
	t.Helper()
	mfs, err := h.reg.Gather()
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
			if labels["operation"] == "createNote" && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("%s{operation=createNote,result=%s} is not registered", idempotency.MetricRequests, result)
	return 0
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("body = %s, want code %s", rec.Body, code)
	}
}

func replayed(rec *httptest.ResponseRecorder) bool {
	return rec.Header().Get(httpx.HeaderIdempotentReplayed) == "true"
}

// wantReplayOf: rec is the stored first response again, byte for byte,
// marked as a replay and not cacheable.
func wantReplayOf(t *testing.T, rec, first *httptest.ResponseRecorder) {
	t.Helper()
	if !replayed(rec) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("not a replay: %d %v", rec.Code, rec.Header())
	}
	if rec.Code != first.Code || !bytes.Equal(rec.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("replay = %d %s, want %d %s", rec.Code, rec.Body, first.Code, first.Body)
	}
	for _, k := range []string{"Content-Type", "Location"} {
		if rec.Header().Get(k) != first.Header().Get(k) {
			t.Fatalf("replay %s = %q, want %q", k, rec.Header().Get(k), first.Header().Get(k))
		}
	}
}

// The same request with the same key gets the stored response, and the
// side effect happens once. Members the schema defaults (copies) and the
// spelling of the body do not make another request.
func TestReplayReturnsTheStoredResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	first := h.post(t, "ayse", "key-replay-1", `{"body":"replay"}`, "")
	wantStatus(t, first, http.StatusCreated, "")
	if replayed(first) {
		t.Fatal("the first response is marked as a replay")
	}
	again := h.post(t, "ayse", "key-replay-1", `{ "copies": 1, "body": "replay" }`, "")
	wantReplayOf(t, again, first)
	if n := h.notes(t, "replay"); n != 1 {
		t.Fatalf("%d notes, want 1", n)
	}
	if h.counted(t, idempotency.ResultCommitted) != 1 || h.counted(t, idempotency.ResultReplayed) != 1 {
		t.Fatal("the commit and the replay are not counted")
	}
}

// The key used with another request is 422 IDEMPOTENCY_KEY_REUSED, and
// the other request is not run.
func TestAnotherRequestWithTheKeyIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	wantStatus(t, h.post(t, "ayse", "key-reused-1", `{"body":"first"}`, ""), http.StatusCreated, "")
	wantStatus(t, h.post(t, "ayse", "key-reused-1", `{"body":"second"}`, ""), http.StatusUnprocessableEntity, string(idempotency.CodeIdempotencyKeyReused))
	wantStatus(t, h.post(t, "ayse", "key-reused-1", `{"body":"first","copies":2}`, ""), http.StatusUnprocessableEntity, string(idempotency.CodeIdempotencyKeyReused))
	if h.notes(t, "second") != 0 || h.notes(t, "first") != 1 {
		t.Fatal("the refused request ran")
	}
}

// Concurrent requests with one key: while the first runs, the same key is
// 409 IDEMPOTENCY_KEY_IN_USE (and another request with it 422); once it
// finished, it is replayed. Of a burst of identical requests exactly one
// runs; the others are 409 or the replay, and the side effect happens
// once.
func TestConcurrentRequestsWithOneKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- h.postAt(t, "first", "ayse", "key-concurrent-1", `{"body":"slow"}`, "") }()
	h.reached("first")
	wantStatus(t, h.post(t, "ayse", "key-concurrent-1", `{"body":"slow"}`, ""), http.StatusConflict, string(idempotency.CodeIdempotencyKeyInUse))
	wantStatus(t, h.post(t, "ayse", "key-concurrent-1", `{"body":"other"}`, ""), http.StatusUnprocessableEntity, string(idempotency.CodeIdempotencyKeyReused))
	h.open("first")
	first := <-firstDone
	wantStatus(t, first, http.StatusCreated, "")
	wantReplayOf(t, h.post(t, "ayse", "key-concurrent-1", `{"body":"slow"}`, ""), first)

	const n = 12
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, n)
	for i := range n {
		wg.Go(func() { recs[i] = h.post(t, "ayse", "key-burst-1", `{"body":"burst"}`, "") })
	}
	wg.Wait()
	var ran *httptest.ResponseRecorder
	for _, rec := range recs {
		switch {
		case rec.Code == http.StatusCreated && !replayed(rec):
			if ran != nil {
				t.Fatalf("two requests ran: %s and %s", ran.Body, rec.Body)
			}
			ran = rec
		case rec.Code == http.StatusCreated, rec.Code == http.StatusConflict:
		default:
			t.Fatalf("burst answer %d %s", rec.Code, rec.Body)
		}
	}
	if ran == nil {
		t.Fatal("no request of the burst ran")
	}
	for _, rec := range recs {
		if rec.Code == http.StatusCreated && !bytes.Equal(rec.Body.Bytes(), ran.Body.Bytes()) {
			t.Fatalf("a replay differs from the response: %s, %s", rec.Body, ran.Body)
		}
	}
	if got := h.notes(t, "burst"); got != 1 {
		t.Fatalf("%d notes from the burst, want 1", got)
	}
	if h.counted(t, idempotency.ResultInUse) < 1 || h.counted(t, idempotency.ResultReused) != 1 {
		t.Fatal("the refusals are not counted")
	}
}

// A 5xx frees the key: the retry with the same key runs, and its success
// is the only side effect (the failed attempt rolled back). A transient
// 4xx (429) frees it too.
func TestServerErrorFreesTheKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	wantStatus(t, h.post(t, "ayse", "key-retry-1", `{"body":"retry"}`, modeFail), http.StatusInternalServerError, string(apperr.CodeInternal))
	if n := h.notes(t, "retry"); n != 0 {
		t.Fatalf("the failed attempt left %d notes", n)
	}
	retry := h.post(t, "ayse", "key-retry-1", `{"body":"retry"}`, "")
	wantStatus(t, retry, http.StatusCreated, "")
	if replayed(retry) || h.notes(t, "retry") != 1 {
		t.Fatal("the retry after a 5xx did not run once")
	}

	wantStatus(t, h.post(t, "ayse", "key-retry-2", `{"body":"busy"}`, modeBusy), http.StatusTooManyRequests, httpx.CodeRateLimited)
	busy := h.post(t, "ayse", "key-retry-2", `{"body":"busy"}`, "")
	wantStatus(t, busy, http.StatusCreated, "")
	if replayed(busy) {
		t.Fatal("a 429 was stored")
	}
	if h.counted(t, idempotency.ResultReleased) != 2 {
		t.Fatalf("released = %v, want 2", h.counted(t, idempotency.ResultReleased))
	}
}

// A permanent 4xx is stored: the same request gets it again without
// running.
func TestPermanentRefusalIsStored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	first := h.post(t, "ayse", "key-refused-1", `{"body":"refused"}`, modeRefuse)
	wantStatus(t, first, http.StatusNotFound, httpx.CodeRouteNotFound)
	again := h.post(t, "ayse", "key-refused-1", `{"body":"refused"}`, "")
	wantReplayOf(t, again, first)
	if h.notes(t, "refused") != 0 || h.counted(t, idempotency.ResultStored) != 1 {
		t.Fatal("the refusal was not stored, or the request ran")
	}
}

// An ambiguous commit (ADR-0017): the operation's transaction committed
// its write and its response, but the handler answered 500. The release
// leaves the completed row alone, so the retry gets the committed success
// and the write does not happen twice.
func TestAmbiguousCommitIsAnsweredByTheStoredSuccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	wantStatus(t, h.post(t, "ayse", "key-ambiguous-1", `{"body":"ambiguous"}`, modeLie), http.StatusInternalServerError, "")
	retry := h.post(t, "ayse", "key-ambiguous-1", `{"body":"ambiguous"}`, "")
	wantStatus(t, retry, http.StatusCreated, "")
	if !replayed(retry) {
		t.Fatal("the retry ran again instead of getting the committed response")
	}
	if n := h.notes(t, "ambiguous"); n != 1 {
		t.Fatalf("%d notes, want the one committed", n)
	}
	var stored note
	if err := json.Unmarshal(retry.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := h.db.Pool().QueryRow(t.Context(), "SELECT id::text FROM active_example_notes WHERE body = 'ambiguous'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if stored.ID != id {
		t.Fatalf("the replay names note %s, the committed note is %s", stored.ID, id)
	}
}

// A key whose attempt outlived its lease is taken over by the next request
// with it. The old attempt's transaction is fenced: its Commit changes no
// row and rolls its write back with 409, so the operation still happens
// once, and the key keeps the new attempt's response.
func TestLeaseExpiryTakesTheKeyOverAndFencesTheOldAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { oldDone <- h.postAt(t, "old", "ayse", "key-lease-1", `{"body":"lease"}`, "") }()
	h.reached("old")
	// Within the lease the key is held.
	h.clock.Advance(idempotency.Lease - time.Second)
	wantStatus(t, h.post(t, "ayse", "key-lease-1", `{"body":"lease"}`, ""), http.StatusConflict, string(idempotency.CodeIdempotencyKeyInUse))
	// Past it, the next request takes the key over and runs.
	h.clock.Advance(2 * time.Second)
	taken := h.post(t, "ayse", "key-lease-1", `{"body":"lease"}`, "")
	wantStatus(t, taken, http.StatusCreated, "")
	if replayed(taken) || h.counted(t, idempotency.ResultTakenOver) != 1 {
		t.Fatal("the key was not taken over")
	}
	// The old attempt wakes up: its commit is refused and rolled back.
	h.open("old")
	wantStatus(t, <-oldDone, http.StatusConflict, string(idempotency.CodeIdempotencyKeyInUse))
	if n := h.notes(t, "lease"); n != 1 {
		t.Fatalf("%d notes, want 1: the fenced attempt committed", n)
	}
	wantReplayOf(t, h.post(t, "ayse", "key-lease-1", `{"body":"lease"}`, ""), taken)
}

// A success the operation did not commit in its transaction is still
// stored (late, not atomically) and reported as unbound: the signal the
// alert and the contract test read.
func TestUnboundSuccessIsReported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	first := h.post(t, "ayse", "key-unbound-1", `{"body":"unbound"}`, modeUnbound)
	wantStatus(t, first, http.StatusCreated, "")
	if h.counted(t, idempotency.ResultUnbound) != 1 || h.counted(t, idempotency.ResultCommitted) != 0 {
		t.Fatal("the unbound success is not reported")
	}
	wantReplayOf(t, h.post(t, "ayse", "key-unbound-1", `{"body":"unbound"}`, ""), first)
}

// Keys belong to their actor: another caller with the same key and the
// same request does not get the first caller's response; it runs its
// own. The stored response is sealed: the row does not carry it.
func TestKeysBelongToTheirActor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	mine := h.post(t, "ayse", "key-actor-1", `{"body":"mine"}`, "")
	wantStatus(t, mine, http.StatusCreated, "")
	theirs := h.post(t, "mehmet", "key-actor-1", `{"body":"mine"}`, "")
	wantStatus(t, theirs, http.StatusCreated, "")
	if replayed(theirs) || bytes.Equal(theirs.Body.Bytes(), mine.Body.Bytes()) {
		t.Fatal("another caller got the first caller's response")
	}
	var n note
	if err := json.Unmarshal(mine.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	var response []byte
	err := h.db.Pool().QueryRow(t.Context(), "SELECT response FROM idempotency_keys WHERE actor = 'user:ayse'").Scan(&response)
	if err != nil {
		t.Fatal(err)
	}
	if len(response) == 0 || bytes.Contains(response, []byte(n.ID)) || bytes.Contains(response, []byte("mine")) {
		t.Fatal("the stored response is not sealed")
	}
}

// A key is kept for Retention: past it the key is new again, and the
// purge removes the rows past theirs.
func TestRetentionAndPurge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	wantStatus(t, h.post(t, "ayse", "key-retain-1", `{"body":"old"}`, ""), http.StatusCreated, "")
	wantStatus(t, h.post(t, "ayse", "key-retain-2", `{"body":"kept"}`, ""), http.StatusCreated, "")
	h.clock.Advance(idempotency.Retention)
	fresh := h.post(t, "ayse", "key-retain-1", `{"body":"new"}`, "")
	wantStatus(t, fresh, http.StatusCreated, "")
	if replayed(fresh) {
		t.Fatal("an expired key was replayed")
	}
	removed, err := h.store.Purge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("purged %d keys, want the expired one", removed)
	}
	var left int
	if err := h.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("%d keys left, want the restarted one", left)
	}
}

// keyRows counts the rows that hold key.
func (h *harness) keyRows(t *testing.T, key string) int {
	t.Helper()
	var n int
	if err := h.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys WHERE key = $1", key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sameTransaction reports whether the note and the key's row were last
// written by one transaction: their xmin is equal. The key's row was
// inserted by the claim and last updated by whoever stored the response.
func (h *harness) sameTransaction(t *testing.T, rec *httptest.ResponseRecorder, key string) bool {
	t.Helper()
	var n note
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	var noteXmin, keyXmin string
	if err := h.maint.Pool().QueryRow(t.Context(), "SELECT xmin::text FROM example_notes WHERE id = $1", n.ID).Scan(&noteXmin); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Pool().QueryRow(t.Context(), "SELECT xmin::text FROM idempotency_keys WHERE key = $1", key).Scan(&keyXmin); err != nil {
		t.Fatal(err)
	}
	return noteXmin == keyXmin
}

// Commit stores the response in the operation's transaction: the note and
// the key's row carry the same xmin. The measurement tells the wrong way
// apart: a handler that commits its response in a transaction of its own
// answers the same 201, and the xmin differ. The app test of every bound
// operation (TestIdempotentOperationsReplay) measures it the same way.
func TestCommitSharesTheOperationsTransaction(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	bound := h.post(t, "ayse", "key-xmin-1", `{"body":"bound"}`, "")
	wantStatus(t, bound, http.StatusCreated, "")
	if !h.sameTransaction(t, bound, "key-xmin-1") {
		t.Fatal("Commit did not write in the operation's transaction")
	}
	separate := h.post(t, "ayse", "key-xmin-2", `{"body":"separate"}`, modeSeparate)
	wantStatus(t, separate, http.StatusCreated, "")
	if h.sameTransaction(t, separate, "key-xmin-2") {
		t.Fatal("a Commit in a transaction of its own passes the measurement")
	}
}

// The fence of the stored response is the attempt, not only the state: an
// old attempt that commits while the attempt that took the key over is
// still running (the row IN_PROGRESS) is refused with 409 and rolled back.
// Without "attempt = $4" in CompleteKey the old attempt would commit here.
func TestOldAttemptCannotCommitOverARunningTakeover(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { oldDone <- h.postAt(t, "old", "ayse", "key-fence-1", `{"body":"fence"}`, "") }()
	h.reached("old")
	h.clock.Advance(idempotency.Lease + time.Second)
	newDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { newDone <- h.postAt(t, "new", "ayse", "key-fence-1", `{"body":"fence"}`, "") }()
	h.reached("new") // the key is the new attempt's, IN_PROGRESS

	h.open("old")
	wantStatus(t, <-oldDone, http.StatusConflict, string(idempotency.CodeIdempotencyKeyInUse))
	if n := h.notes(t, "fence"); n != 0 {
		t.Fatalf("the fenced attempt committed %d notes", n)
	}
	h.open("new")
	taken := <-newDone
	wantStatus(t, taken, http.StatusCreated, "")
	if replayed(taken) || h.notes(t, "fence") != 1 {
		t.Fatal("the attempt that took the key over did not run once")
	}
}

// The release of a failed old attempt does not free a key another attempt
// took over: the new attempt's row stays and its success is stored.
// Without "attempt = $4" in ReleaseKey the old attempt's 5xx would delete
// the new attempt's row.
func TestOldAttemptReleaseSparesTheTakeover(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { oldDone <- h.postAt(t, "old", "ayse", "key-fence-2", `{"body":"spared"}`, modeFail) }()
	h.reached("old")
	h.clock.Advance(idempotency.Lease + time.Second)
	newDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { newDone <- h.postAt(t, "new", "ayse", "key-fence-2", `{"body":"spared"}`, "") }()
	h.reached("new")

	h.open("old")
	wantStatus(t, <-oldDone, http.StatusInternalServerError, "")
	if n := h.keyRows(t, "key-fence-2"); n != 1 {
		t.Fatalf("%d rows hold the key after the old attempt's release, want the new attempt's", n)
	}
	h.open("new")
	taken := <-newDone
	wantStatus(t, taken, http.StatusCreated, "")
	wantReplayOf(t, h.post(t, "ayse", "key-fence-2", `{"body":"spared"}`, ""), taken)
	if n := h.notes(t, "spared"); n != 1 {
		t.Fatalf("%d notes, want the new attempt's", n)
	}
}

// A response over the stored bound is sent but not kept, on the Commit
// path as after the handler: the key is freed, a retry runs again, and the
// log says why, not that the sent response differs from a committed one.
func TestLargeResponseIsNotKept(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	first := h.post(t, "ayse", "key-large-1", `{"body":"large"}`, modeLarge)
	wantStatus(t, first, http.StatusCreated, "")
	if first.Body.Len() <= 1<<20 {
		t.Fatalf("the response has %d bytes, not over the bound", first.Body.Len())
	}
	if h.keyRows(t, "key-large-1") != 0 || h.counted(t, idempotency.ResultTooLarge) != 1 || h.counted(t, idempotency.ResultCommitted) != 0 {
		t.Fatal("the large response was kept")
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "too large to keep") || strings.Contains(logs, "differs") {
		t.Fatalf("logs = %s", logs)
	}
	again := h.post(t, "ayse", "key-large-1", `{"body":"large"}`, "")
	wantStatus(t, again, http.StatusCreated, "")
	if replayed(again) || h.notes(t, "large") != 2 {
		t.Fatal("the retry of a response that was not kept did not run")
	}
}
