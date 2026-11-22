package idempotency_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
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
	cfg := func(public map[string]idempotency.RequestActor) idempotency.Config {
		return idempotency.Config{
			Store: store, Caller: caller, Public: public,
			Metrics: prometheus.NewRegistry(), Logger: slog.New(slog.DiscardHandler),
		}
	}
	m, err := idempotency.NewMiddleware(spec, cfg(map[string]idempotency.RequestActor{"redeemNote": ticketActor}))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Bound(); !slices.Equal(got, []string{"createNote", "redeemNote"}) {
		t.Fatalf("Bound = %v, want the two operations with a handler", got)
	}

	for name, tc := range map[string]struct {
		public map[string]idempotency.RequestActor
		want   string
	}{
		"public without actor":   {nil, "redeemNote has no caller and no request actor"},
		"actor for a caller":     {map[string]idempotency.RequestActor{"redeemNote": ticketActor, "createNote": ticketActor}, "createNote has a caller"},
		"actor for frozen":       {map[string]idempotency.RequestActor{"redeemNote": ticketActor, "archiveNote": ticketActor}, "archiveNote, which is not a bound operation"},
		"actor for no operation": {map[string]idempotency.RequestActor{"redeemNote": ticketActor, "nope": ticketActor}, "nope, which is not a bound operation"},
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
