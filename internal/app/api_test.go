package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/realtime"
)

// unlimitedGroups keeps the rate limits out of tests about other things.
func unlimitedGroups() map[string]httpx.GroupLimits {
	groups := rateLimitGroups()
	unlimited := httpx.Rate{Every: time.Nanosecond, Burst: 1 << 30}
	for name := range groups {
		groups[name] = httpx.GroupLimits{PerIP: unlimited, PerIdentity: unlimited, PerCustom: unlimited}
	}
	return groups
}

func newAPIRouter(tb testing.TB, env config.Env) http.Handler {
	tb.Helper()
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	router, err := newRouter(context.Background(), config.Config{Env: env}, logger, httpx.NewHealth(logger),
		prometheus.NewRegistry(), unlimitedGroups(), testModules(tb))
	if err != nil {
		tb.Fatal(err)
	}
	return router
}

// testModules wires the modules on a pool that never connects: these
// tests do not reach the database (integration tests wire them on
// dbtest).
func testModules(tb testing.TB) modules {
	tb.Helper()
	return testModulesOn(tb, lazyDB(tb), clock.Real{})
}

// lazyDB is a pool whose first connection would fail; nothing here opens
// one.
func lazyDB(tb testing.TB) *db.DB {
	tb.Helper()
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	database, err := db.Open(context.Background(), db.Config{
		URL: "postgres://koala@127.0.0.1:1/koala?sslmode=disable&connect_timeout=1", Role: db.RoleApp, Logger: logger,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(database.Close)
	return database
}

func testModulesOn(tb testing.TB, database *db.DB, clk clock.Clock) modules {
	tb.Helper()
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	mods, err := wireModules(context.Background(), config.Config{Env: config.EnvTest}, database, clk, logger, prometheus.NewRegistry())
	if err != nil {
		tb.Fatal(err)
	}
	return mods
}

// setClient sets the headers of a current iOS build.
func setClient(h http.Header) {
	h.Set(httpx.HeaderClientPlatform, string(httpx.PlatformIOS))
	h.Set(httpx.HeaderAppVersion, "1.0.0")
	h.Set(httpx.HeaderDeviceID, "app-test-device-01")
}

// postLogin sends a sign-in request body.
func postLogin(h http.Handler, method, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, "/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(httpx.HeaderRequestID, "req-1")
	setClient(req.Header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// POST /v1/auth/login runs the contract path up to the handler: the
// OpenAPI 3.1 spec is loaded and the request is validated against it
// before anything reaches the database (ADR-0019). The bodies are the
// wire format of the problem documents, byte for byte. (Was the
// /v1/meta/ping smoke test; the 200 path runs against Postgres in the
// integration tests.)
func TestLoginRequestValidation(t *testing.T) {
	t.Parallel()

	h := newAPIRouter(t, config.EnvTest)
	const problem = `{"type":"https://api.iskoala.com/problems/request-invalid","title":"Bad Request",` +
		`"status":400,"code":"REQUEST_INVALID","instance":"/v1/auth/login","requestId":"req-1","errors":`
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantBody    string
	}{
		{
			name:       "missing member",
			body:       `{"email":"ayse@example.com"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/password","code":"FIELD_REQUIRED"}]}`,
		},
		{
			name:       "wrong type",
			body:       `{"email":42,"password":"x"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/email","code":"FIELD_TYPE_INVALID"}]}`,
		},
		{
			name:       "unknown member",
			body:       `{"email":"ayse@example.com","password":"x","Remember":true}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/Remember","code":"FIELD_UNKNOWN"}]}`,
		},
		{
			name:       "password over the field limit",
			body:       `{"email":"ayse@example.com","password":"` + strings.Repeat("x", 1025) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/password","code":"FIELD_OUT_OF_RANGE"}]}`,
		},
		{
			name:        "unsupported content type",
			contentType: "text/plain",
			body:        `{"email":"ayse@example.com","password":"x"}`,
			wantStatus:  http.StatusUnsupportedMediaType,
			wantBody: `{"type":"https://api.iskoala.com/problems/content-type-unsupported",` +
				`"title":"Unsupported Media Type","status":415,"code":"CONTENT_TYPE_UNSUPPORTED",` +
				`"instance":"/v1/auth/login","requestId":"req-1"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ct := tt.contentType
			if ct == "" {
				ct = "application/json"
			}
			rec := postLogin(h, http.MethodPost, ct, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := strings.TrimSuffix(rec.Body.String(), "\n"); got != tt.wantBody {
				t.Fatalf("body mismatch\n got: %s\nwant: %s", got, tt.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); ct != httpx.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %q", ct, httpx.ContentTypeProblem)
			}
		})
	}
}

func TestLoginWrongMethodIsProblem(t *testing.T) {
	t.Parallel()

	h := newAPIRouter(t, config.EnvTest)
	rec := postLogin(h, http.MethodGet, "application/json", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// The smoke test of Faz 0 is gone: /v1/meta/ping is an unknown path in
// every environment (ADR-0025 §9).
func TestPingIsGone(t *testing.T) {
	t.Parallel()

	for _, env := range []config.Env{config.EnvTest, config.EnvProduction} {
		h := newAPIRouter(t, env)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/meta/ping", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		setClient(req.Header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", env, rec.Code)
		}
	}
}

// A request the validator accepted but the generated code cannot bind is a
// server-side gap: 500, logged by type only, never with the value.
func TestBindErrorIsALoggedServerError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := log.New(&logs, slog.LevelInfo)
	const secret = "TR330006100519786457841326"
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/login", http.NoBody)
	bindError(logger)(rec, req, fmt.Errorf("can't decode JSON body: %w", errors.New("bad value "+secret)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "bind validated request") {
		t.Fatalf("not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the bound value leaked: log %s body %s", logs.String(), rec.Body)
	}
}

// Every code the platform returns (HTTP, soft-delete, realtime, authn) is in the
// contract's ErrorCode list, from which the clients' code lists are
// generated (ADR-0007 madde 5).
func TestPlatformCodesAreInTheContract(t *testing.T) {
	t.Parallel()

	for _, c := range slices.Concat(httpx.PlatformCodes(), softdelete.Codes(), realtime.Codes(), authn.Codes()) {
		if !apiv1.ErrorCode(c).Valid() {
			t.Errorf("code %s is not in api/components/problem.yaml ErrorCode", c)
		}
	}
}
