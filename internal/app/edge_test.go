package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/metrics"
)

// TestRateLimitGroups pins the production limits to ADR-0021's table
// (requests per minute and burst, per address and per identity).
func TestRateLimitGroups(t *testing.T) {
	t.Parallel()

	type limits struct{ ipPerMin, ipBurst, idPerMin, idBurst, customPerMin, customBurst int }
	perMin := func(r httpx.Rate) int {
		if r.Every == 0 {
			return 0
		}
		return int(time.Minute / r.Every)
	}
	want := map[string]limits{
		httpx.GroupRead:  {600, 120, 300, 60, 0, 0},
		httpx.GroupWrite: {300, 60, 60, 20, 0, 0},
		groupAuth:        {30, 10, 0, 0, 10, 5},
		groupUpload:      {100, 20, 20, 5, 0, 0},
		groupMessage:     {150, 30, 30, 10, 0, 0},
		groupSitemap:     {30, 10, 0, 0, 0, 0},
		groupBoarding:    {600, 60, 120, 20, 0, 0},
	}
	got := rateLimitGroups()
	if len(got) != len(want) {
		t.Fatalf("%d groups, want %d", len(got), len(want))
	}
	for name, w := range want {
		g := got[name]
		l := limits{
			perMin(g.PerIP), g.PerIP.Burst, perMin(g.PerIdentity), g.PerIdentity.Burst,
			perMin(g.PerCustom), g.PerCustom.Burst,
		}
		if l != w {
			t.Errorf("group %s = %+v, want %+v", name, l, w)
		}
	}
}

// The production limits apply through the wired router: a client over the
// write burst gets 429 with Retry-After, before the request is validated.
func TestWiredRouterLimitsPerAddress(t *testing.T) {
	t.Parallel()

	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	reg, err := metrics.NewRegistry("test")
	if err != nil {
		t.Fatal(err)
	}
	router, err := newRouter(context.Background(), config.Config{Env: config.EnvTest}, logger,
		httpx.NewHealth(logger), reg, rateLimitGroups())
	if err != nil {
		t.Fatal(err)
	}
	burst := rateLimitGroups()[httpx.GroupWrite].PerIP.Burst
	var last *httptest.ResponseRecorder
	for range burst + 1 {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/meta/ping", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		setClient(req.Header)
		last = httptest.NewRecorder()
		router.ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Fatalf("request %d: status %d, Retry-After %q; want 429", burst+1, last.Code, last.Header().Get("Retry-After"))
	}
}

// externalMetrics are exported by something other than the service: the
// backup script and the blackbox exporter.
var externalMetrics = map[string]bool{
	"koala_backup_last_success_timestamp_seconds": true,
	"probe_ssl_earliest_cert_expiry":              true,
}

// TestAlertRulesUseRegisteredMetrics: every metric an alert rule reads is
// registered by the wired service (or listed as external). A rule over a
// series that never exists would stay green forever.
func TestAlertRulesUseRegisteredMetrics(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../ops/monitoring/prometheus-alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	reg, err := metrics.NewRegistry("test")
	if err != nil {
		t.Fatal(err)
	}
	router, err := newRouter(context.Background(), config.Config{Env: config.EnvTest}, logger,
		httpx.NewHealth(logger), reg, rateLimitGroups())
	if err != nil {
		t.Fatal(err)
	}
	// One request, so the histogram has a series to gather.
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, httpx.PathLivez, http.NoBody))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, mf := range mfs {
		registered[mf.GetName()] = true
	}

	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		expr, ok := strings.CutPrefix(strings.TrimSpace(line), "expr:")
		if ok {
			names = append(names, promqlMetrics(expr)...)
		}
	}
	if len(names) == 0 {
		t.Fatal("no metric found in the alert rules")
	}
	for _, n := range names {
		base := n
		for _, suffix := range []string{"_count", "_sum", "_bucket"} {
			if b, ok := strings.CutSuffix(n, suffix); ok && registered[b] {
				base = b
			}
		}
		if !registered[base] && !externalMetrics[n] {
			t.Errorf("alert rule reads %s, which the service does not register", n)
		}
	}
}

var (
	promqlSelector = regexp.MustCompile(`\{[^}]*\}|\[[^\]]*\]|"[^"]*"|\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	promqlIdent    = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*\s*\(?`)
	promqlKeywords = map[string]bool{"bool": true, "and": true, "or": true, "unless": true, "offset": true}
)

// promqlMetrics returns the metric names in a PromQL expression: the
// identifiers outside label matchers, ranges and strings that are not
// function calls or keywords.
func promqlMetrics(expr string) []string {
	expr = promqlSelector.ReplaceAllString(expr, " ")
	var out []string
	for _, m := range promqlIdent.FindAllString(expr, -1) {
		if strings.HasSuffix(m, "(") {
			continue
		}
		if id := strings.TrimSpace(m); !promqlKeywords[id] {
			out = append(out, id)
		}
	}
	return out
}

func TestPromqlMetrics(t *testing.T) {
	t.Parallel()

	got := promqlMetrics(`sum by (group) (rate(a_total{x=~"5.."}[5m])) / clamp_min(sum(rate(b_count[5m])), 0.001) > 0.05 and time() - c_seconds > 1`)
	want := []string{"a_total", "b_count", "c_seconds"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("promqlMetrics() = %v, want %v", got, want)
	}
}
