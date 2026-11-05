package app

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/metrics"
)

const adr0021 = "../../docs/adr/0021-http-edge-clients-rate-limit-metrics.md"

// adrRateLimitTable reads ADR-0021's rate limit table: group → per
// address, per identity and custom rates as "per minute / burst" ("—"
// for none).
func adrRateLimitTable(t *testing.T) map[string][3]string {
	t.Helper()
	raw, err := os.ReadFile(adr0021)
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(raw), "<!-- rate-limit-groups:")
	if !ok {
		t.Fatal("ADR-0021 has no rate-limit-groups table marker")
	}
	out := map[string][3]string{}
	for _, line := range strings.Split(table, "\n")[1:] {
		if !strings.HasPrefix(line, "|") {
			if len(out) > 0 {
				break
			}
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 6 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cells[1]), "`")
		out[name] = [3]string{strings.TrimSpace(cells[2]), strings.TrimSpace(cells[3]), strings.TrimSpace(cells[4])}
	}
	return out
}

// TestRateLimitGroups: the production limits are exactly ADR-0021's
// table, the single source of the values (ADR-0021 §5).
func TestRateLimitGroups(t *testing.T) {
	t.Parallel()

	format := func(r httpx.Rate) string {
		if r.Every == 0 {
			return "—"
		}
		return strconv.Itoa(int(time.Minute/r.Every)) + " / " + strconv.Itoa(r.Burst)
	}
	want := adrRateLimitTable(t)
	got := rateLimitGroups()
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("code has %d groups, ADR-0021 lists %d", len(got), len(want))
	}
	for name, g := range got {
		row := [3]string{format(g.PerIP), format(g.PerIdentity), format(g.PerCustom)}
		if row != want[name] {
			t.Errorf("group %s: code %q, ADR-0021 %q", name, row, want[name])
		}
	}
}

// unregisteredRoutes returns the "METHOD /route" keys that name no
// registered route of router.
func unregisteredRoutes(t *testing.T, router *httpx.Router, keys []string) []string {
	t.Helper()
	registered := map[string]bool{}
	err := router.Walk(func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, k := range keys {
		if !registered[k] {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	return missing
}

// TestRateLimitRoutesAreRegistered: every operation the contract puts in
// a rate limit group (x-rate-limit) is a route the wired router serves,
// in every environment. A group on a route that does not exist (a typo,
// a path changed in only one place) would leave the real route in its
// method's default group without anyone noticing.
func TestRateLimitRoutesAreRegistered(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	routes, err := httpx.RateLimitRoutes(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []config.Env{config.EnvTest, config.EnvProduction} {
		logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
		router, err := newRouter(context.Background(), config.Config{Env: env}, logger,
			httpx.NewHealth(logger), prometheus.NewRegistry(), rateLimitGroups(), testModules(t))
		if err != nil {
			t.Fatal(err)
		}
		if missing := unregisteredRoutes(t, router, slices.Collect(maps.Keys(routes))); len(missing) > 0 {
			t.Errorf("%s: x-rate-limit on routes that are not registered: %v", env, missing)
		}
		// The check itself finds a missing route.
		got := unregisteredRoutes(t, router, []string{"GET /livez", "POST /v1/nope"})
		if !slices.Equal(got, []string{"POST /v1/nope"}) {
			t.Fatalf("%s: unregisteredRoutes = %v, want [POST /v1/nope]", env, got)
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
		httpx.NewHealth(logger), reg, rateLimitGroups(), testModules(t))
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
// registered by the wired service (or listed as external), and every label
// it selects or groups by is one of that metric's labels. A rule over a
// series or a label that never exists would stay green forever.
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
		httpx.NewHealth(logger), reg, rateLimitGroups(), testModules(t))
	if err != nil {
		t.Fatal(err)
	}
	// One request, so the histogram has a series to gather.
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, httpx.PathLivez, http.NoBody))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]map[string]bool{} // family → label names
	for _, mf := range mfs {
		set := map[string]bool{}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				set[l.GetName()] = true
			}
		}
		labels[mf.GetName()] = set
	}
	family := func(name string) string {
		for _, suffix := range []string{"_count", "_sum", "_bucket"} {
			if b, ok := strings.CutSuffix(name, suffix); ok && labels[b] != nil {
				return b
			}
		}
		return name
	}

	exprs := alertExprs(string(raw))
	if len(exprs) < 5 {
		t.Fatalf("found %d alert expressions, the file has at least 5", len(exprs))
	}
	for _, expr := range exprs {
		grouping := map[string]bool{}
		for _, l := range promqlGroupingLabels(expr) {
			grouping[l] = true
		}
		for _, n := range promqlMetrics(expr) {
			if externalMetrics[n] {
				continue
			}
			f := family(n)
			if labels[f] == nil {
				t.Errorf("alert rule reads %s, which the service does not register", n)
				continue
			}
			for l := range grouping {
				if labels[f][l] {
					delete(grouping, l)
				}
			}
		}
		for _, sel := range promqlSelectors(expr) {
			if externalMetrics[sel.metric] {
				continue
			}
			for _, l := range sel.labels {
				if set := labels[family(sel.metric)]; set != nil && !set[l] {
					t.Errorf("alert rule selects %s{%s=...}: %s has no such label", sel.metric, l, family(sel.metric))
				}
			}
		}
		for l := range grouping {
			t.Errorf("alert rule groups by %s, which none of its metrics has: %s", l, expr)
		}
	}
}

// alertExprs returns the expr of every rule, also when it is a YAML block
// scalar (expr: | or expr: >) spread over several lines; quotes around a
// one-line expr are removed.
func alertExprs(doc string) []string {
	lines := strings.Split(doc, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		rest, ok := strings.CutPrefix(trimmed, "expr:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" || strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
			indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
			var parts []string
			for i+1 < len(lines) {
				next := lines[i+1]
				if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " ")) <= indent {
					break
				}
				parts = append(parts, strings.TrimSpace(next))
				i++
			}
			out = append(out, strings.TrimSpace(strings.Join(parts, " ")))
			continue
		}
		if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') && rest[len(rest)-1] == rest[0] {
			rest = rest[1 : len(rest)-1]
		}
		out = append(out, rest)
	}
	return out
}

type promqlSelector struct {
	metric string
	labels []string
}

var (
	promqlStripped  = regexp.MustCompile(`\{[^}]*\}|\[[^\]]*\]|"[^"]*"|\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	promqlIdent     = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*\s*\(?`)
	promqlKeywords  = map[string]bool{"bool": true, "and": true, "or": true, "unless": true, "offset": true}
	promqlSelect    = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\s*\{([^}]*)\}`)
	promqlMatcher   = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)`)
	promqlGrouping  = regexp.MustCompile(`\b(by|without)\s*\(([^)]*)\)`)
	promqlLabelName = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)
)

// promqlMetrics returns the metric names in a PromQL expression: the
// identifiers outside label matchers, ranges, strings and grouping
// clauses that are not function calls or keywords.
func promqlMetrics(expr string) []string {
	expr = promqlStripped.ReplaceAllString(expr, " ")
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

// promqlSelectors returns each metric{...} selector with its label names.
func promqlSelectors(expr string) []promqlSelector {
	var out []promqlSelector
	for _, m := range promqlSelect.FindAllStringSubmatch(expr, -1) {
		sel := promqlSelector{metric: m[1]}
		for _, l := range promqlMatcher.FindAllStringSubmatch(m[2], -1) {
			sel.labels = append(sel.labels, l[1])
		}
		out = append(out, sel)
	}
	return out
}

// promqlGroupingLabels returns the labels of every by (...) and
// without (...) clause.
func promqlGroupingLabels(expr string) []string {
	var out []string
	for _, m := range promqlGrouping.FindAllStringSubmatch(expr, -1) {
		out = append(out, promqlLabelName.FindAllString(m[2], -1)...)
	}
	return out
}

func TestPromqlParsing(t *testing.T) {
	t.Parallel()

	expr := `sum by (group, key) (rate(a_total{x=~"5..", y!="z"}[5m])) / clamp_min(sum(rate(b_count[5m])), 0.001) > 0.05 and time() - c_seconds > 1`
	if got, want := promqlMetrics(expr), []string{"a_total", "b_count", "c_seconds"}; !slices.Equal(got, want) {
		t.Errorf("promqlMetrics() = %v, want %v", got, want)
	}
	sels := promqlSelectors(expr)
	if len(sels) != 1 || sels[0].metric != "a_total" || !slices.Equal(sels[0].labels, []string{"x", "y"}) {
		t.Errorf("promqlSelectors() = %+v", sels)
	}
	if got := promqlGroupingLabels(expr); !slices.Equal(got, []string{"group", "key"}) {
		t.Errorf("promqlGroupingLabels() = %v", got)
	}

	doc := "      - alert: A\n        expr: |\n          sum(rate(a_total[5m]))\n            > 1\n        for: 1m\n" +
		"      - alert: B\n        expr: >-\n          b_count\n          > 2\n      - alert: C\n        expr: \"c_seconds > 3\"\n"
	want := []string{"sum(rate(a_total[5m])) > 1", "b_count > 2", "c_seconds > 3"}
	if got := alertExprs(doc); !slices.Equal(got, want) {
		t.Errorf("alertExprs() = %q, want %q", got, want)
	}
}
