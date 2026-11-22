package idempotency_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
)

// testSecret stands for the login pepper.
func testSecret() []byte { return bytes.Repeat([]byte("0123456789abcdef"), 2) }

func loadSpec(tb testing.TB) *openapi3.T {
	tb.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("testdata/notes.yaml")
	if err != nil {
		tb.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return doc
}

// lazyStore is a store on a pool that never connects: these tests do not
// reach the database.
func lazyStore(tb testing.TB) *idempotency.Store {
	tb.Helper()
	database, err := db.Open(context.Background(), db.Config{
		URL:  "postgres://koala@127.0.0.1:1/koala?sslmode=disable&connect_timeout=1",
		Role: db.RoleApp, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(database.Close)
	store, err := idempotency.NewStore(database, testSecret(), clock.Real{})
	if err != nil {
		tb.Fatal(err)
	}
	return store
}

func caller(context.Context) (string, bool) { return "user:test", true }

func allow(any) (bool, time.Duration) { return true, 0 }

// redeem is how the tests serve redeemNote, their public operation.
func redeem() idempotency.PublicOperation {
	return idempotency.PublicOperation{Actor: ticketActor, Throttle: allow}
}

func ticketActor(body any) (string, bool) {
	m, ok := body.(map[string]any)
	if !ok {
		return "", false
	}
	t, ok := m["ticket"].(string)
	return "ticket:" + t, ok
}

// Operations reads the operations that take the key from the contract:
// with their caller kind and whether they are frozen; an operation
// without the parameter is not one.
func TestOperationsReadTheContract(t *testing.T) {
	t.Parallel()

	ops, err := idempotency.Operations(loadSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []idempotency.Operation{
		{ID: "archiveNote", Method: http.MethodPost, Path: "/notes/{id}/archive", Frozen: true},
		{ID: "createNote", Method: http.MethodPost, Path: "/notes"},
		{ID: "redeemNote", Method: http.MethodPost, Path: "/notes/redeem", Public: true},
	}
	if !slices.Equal(ops, want) {
		t.Fatalf("Operations = %+v, want %+v", ops, want)
	}
}

// The middleware binds every operation that takes the key and has a
// handler, and fails closed on an actor question nobody answered: a
// public operation without a request actor, a request actor for an
// operation with a caller, a request actor for no bound operation.
func TestNewMiddlewareBindsTheContract(t *testing.T) {
	t.Parallel()

	spec, store := loadSpec(t), lazyStore(t)
	cfg := func(public map[string]idempotency.PublicOperation) idempotency.Config {
		return idempotency.Config{
			Store: store, Caller: caller, Public: public,
			Metrics: prometheus.NewRegistry(), Logger: slog.New(slog.DiscardHandler),
		}
	}
	m, err := idempotency.NewMiddleware(spec, cfg(map[string]idempotency.PublicOperation{"redeemNote": redeem()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Bound(); !slices.Equal(got, []string{"createNote", "redeemNote"}) {
		t.Fatalf("Bound = %v, want the two operations with a handler", got)
	}

	for name, tc := range map[string]struct {
		public map[string]idempotency.PublicOperation
		want   string
	}{
		"public without actor":    {nil, "redeemNote has no caller and no request actor"},
		"public without throttle": {map[string]idempotency.PublicOperation{"redeemNote": {Actor: ticketActor}}, "redeemNote needs both a request actor and a throttle"},
		"public without its actor": {
			map[string]idempotency.PublicOperation{"redeemNote": {Throttle: allow}}, "redeemNote needs both a request actor and a throttle",
		},
		"actor for a caller":     {map[string]idempotency.PublicOperation{"redeemNote": redeem(), "createNote": redeem()}, "createNote has a caller"},
		"actor for frozen":       {map[string]idempotency.PublicOperation{"redeemNote": redeem(), "archiveNote": redeem()}, "archiveNote, which is not a bound operation"},
		"actor for no operation": {map[string]idempotency.PublicOperation{"redeemNote": redeem(), "nope": redeem()}, "nope, which is not a bound operation"},
	} {
		_, err := idempotency.NewMiddleware(spec, cfg(tc.public))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: NewMiddleware = %v, want an error naming %q", name, err, tc.want)
		}
	}
}

// Commit outside a claimed request (a job, a service test) does nothing:
// it neither renders nor writes.
func TestCommitWithoutAClaimDoesNothing(t *testing.T) {
	t.Parallel()

	err := idempotency.Commit(context.Background(), db.Querier{}, func(http.ResponseWriter) error {
		t.Error("rendered without a claim")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A bound operation reached without a request deadline shorter than the
// lease (a Stream route, a route outside httpx.Timeout) is refused before
// it takes a key: the lease assumes the request cannot outlive it
// (ADR-0034 Karar 4). The store's pool never connects, so a refusal that
// came after the claim would fail differently.
func TestUnboundedRequestIsRefused(t *testing.T) {
	t.Parallel()

	m, err := idempotency.NewMiddleware(loadSpec(t), idempotency.Config{
		Store: lazyStore(t), Caller: caller, Public: map[string]idempotency.PublicOperation{"redeemNote": redeem()},
		Metrics: prometheus.NewRegistry(), Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	for name, chain := range map[string][]func(http.Handler) http.Handler{
		"no deadline":           {m.Handler},
		"deadline past a lease": {httpx.Timeout(idempotency.Lease + time.Minute), m.Handler},
	} {
		r := chi.NewRouter()
		// A group, as mountAPI mounts the API: its middleware runs once
		// the route is matched.
		r.Group(func(g chi.Router) {
			g.Use(chain...)
			g.Post("/notes", func(http.ResponseWriter, *http.Request) { ran = true })
		})
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/notes", strings.NewReader(`{"body":"x"}`))
		req.Header.Set(httpx.HeaderIdempotencyKey, "key-unbounded-1")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError || ran {
			t.Errorf("%s: status %d, handler ran %v; want 500 before the handler", name, rec.Code, ran)
		}
	}
}
