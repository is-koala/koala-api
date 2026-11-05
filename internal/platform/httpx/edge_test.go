package httpx_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// doRaw serves one request with exactly the given headers: no client
// headers are added.
func doRaw(h http.Handler, method, target string, header http.Header, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)
	for k, v := range header {
		req.Header[k] = v
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem: %v: %s", err, rec.Body)
	}
	return p
}

func clientHeader(platform, version, device string) http.Header {
	h := http.Header{}
	for k, v := range map[string]string{
		httpx.HeaderClientPlatform: platform, httpx.HeaderAppVersion: version, httpx.HeaderDeviceID: device,
	} {
		if v != "" {
			h.Set(k, v)
		}
	}
	return h
}

// newGateRouter has a client route, an admin route, an exempt route and a
// stream route; each reports the platform and device the gate accepted.
func newGateRouter(t *testing.T) (http.Handler, *prometheus.Registry) {
	t.Helper()
	logger, _ := newLogger()
	opts, reg := testRouterOptions(t)
	opts.Clients.ExemptRoutes = []string{"/v1/callback"}
	r, err := httpx.NewRouter(logger, httpx.NewHealth(logger), opts)
	if err != nil {
		t.Fatal(err)
	}
	report := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Seen-Platform", string(httpx.ClientPlatformFrom(req.Context())))
			w.Header().Set("X-Seen-Device", httpx.DeviceIDFrom(req.Context()))
			w.WriteHeader(status)
		}
	}
	r.Get("/v1/jobs", report(http.StatusNoContent))
	r.Get("/v1/admin/users", report(http.StatusNoContent))
	r.Post("/v1/callback", report(http.StatusNoContent))
	r.Stream("/v1/realtime", report(http.StatusTeapot))
	return r, reg
}

// TestClientGate pins the client header table of ADR-0021.
func TestClientGate(t *testing.T) {
	t.Parallel()

	const device = "abcdefgh12345678"
	browserUpgrade := http.Header{"Origin": {"https://app.koala.test"}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"}}
	nativeUpgrade := clientHeader("IOS", "0.9.0", device)
	nativeUpgrade.Set("Connection", "Upgrade")
	nativeUpgrade.Set("Upgrade", "websocket")
	tests := []struct {
		name         string
		method       string
		target       string
		header       http.Header
		wantStatus   int
		wantCode     string
		wantFields   []map[string]any
		wantPlatform string
	}{
		{"current iOS build", "GET", "/v1/jobs", clientHeader("IOS", "1.0.0", device), 204, "", nil, "IOS"},
		{"newer Android build", "GET", "/v1/jobs", clientHeader("ANDROID", "1.10.0+42", device), 204, "", nil, "ANDROID"},
		{"web needs no version", "GET", "/v1/jobs", clientHeader("WEB", "", device), 204, "", nil, "WEB"},
		{"old iOS build", "GET", "/v1/jobs", clientHeader("IOS", "0.9.32", device), 400, httpx.CodeAppVersionUnsupported, nil, ""},
		{"pre-release of the minimum", "GET", "/v1/jobs", clientHeader("ANDROID", "1.0.0-beta.1", device), 400, httpx.CodeAppVersionUnsupported, nil, ""},
		{"mobile without version", "GET", "/v1/jobs", clientHeader("ANDROID", "", device), 400, httpx.CodeAppVersionUnsupported, nil, ""},
		{"mobile with malformed version", "GET", "/v1/jobs", clientHeader("IOS", "1.0", device), 400, httpx.CodeAppVersionUnsupported, nil, ""},
		{"old build without device: update first", "GET", "/v1/jobs", clientHeader("IOS", "0.1.0", ""), 400, httpx.CodeAppVersionUnsupported, nil, ""},
		{
			"no platform", "GET", "/v1/jobs", clientHeader("", "1.0.0", device), 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderClientPlatform, "HEADER", httpx.CodeFieldRequired)}, "",
		},
		{
			"platform in the wrong case", "GET", "/v1/jobs", clientHeader("ios", "1.0.0", device), 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderClientPlatform, "HEADER", httpx.CodeFieldValueInvalid)}, "",
		},
		{
			"ADMIN outside /v1/admin", "GET", "/v1/jobs", clientHeader("ADMIN", "", device), 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderClientPlatform, "HEADER", httpx.CodeFieldValueInvalid)}, "",
		},
		{
			"no device", "GET", "/v1/jobs", clientHeader("WEB", "", ""), 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderDeviceID, "HEADER", httpx.CodeFieldRequired)}, "",
		},
		{
			"device too short", "GET", "/v1/jobs", clientHeader("IOS", "1.0.0", "short"), 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderDeviceID, "HEADER", httpx.CodeFieldValueInvalid)}, "",
		},
		{
			"device with a forbidden character", "GET", "/v1/jobs", clientHeader("IOS", "1.0.0", "abcdefgh/1234"), 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderDeviceID, "HEADER", httpx.CodeFieldValueInvalid)}, "",
		},
		{
			"no headers at all", "GET", "/v1/jobs", nil, 400, httpx.CodeRequestInvalid,
			[]map[string]any{
				paramErr(httpx.HeaderClientPlatform, "HEADER", httpx.CodeFieldRequired),
				paramErr(httpx.HeaderDeviceID, "HEADER", httpx.CodeFieldRequired),
			}, "",
		},
		{"admin without headers", "GET", "/v1/admin/users", nil, 204, "", nil, "ADMIN"},
		{"admin naming itself", "GET", "/v1/admin/users", clientHeader("ADMIN", "", ""), 204, "", nil, "ADMIN"},
		{
			"mobile platform on the admin API", "GET", "/v1/admin/users", clientHeader("IOS", "1.0.0", device), 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderClientPlatform, "HEADER", httpx.CodeFieldValueInvalid)}, "",
		},
		{"health probe", "GET", "/livez", nil, 200, "", nil, ""},
		{"exempt route", "POST", "/v1/callback", nil, 204, "", nil, ""},
		{"unknown path is a 404, not a header error", "GET", "/v1/nope", nil, 404, httpx.CodeRouteNotFound, nil, ""},
		{"browser WebSocket handshake", "GET", "/v1/realtime", browserUpgrade, 418, "", nil, "WEB"},
		{"old native WebSocket build meets the gate", "GET", "/v1/realtime", nativeUpgrade, 400, httpx.CodeAppVersionUnsupported, nil, ""},
		{
			"handshake without Origin is not a browser", "GET", "/v1/realtime",
			http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, httpx.HeaderDeviceID: {device}}, 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderClientPlatform, "HEADER", httpx.CodeFieldRequired)}, "",
		},
		{
			"plain request with Origin is not a handshake", "GET", "/v1/realtime",
			http.Header{"Origin": {"https://app.koala.test"}, httpx.HeaderDeviceID: {device}}, 400, httpx.CodeRequestInvalid,
			[]map[string]any{paramErr(httpx.HeaderClientPlatform, "HEADER", httpx.CodeFieldRequired)}, "",
		},
	}
	h, _ := newGateRouter(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := doRaw(h, tt.method, tt.target, tt.header, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := rec.Header().Get("X-Seen-Platform"); got != tt.wantPlatform {
				t.Errorf("handler saw platform %q, want %q", got, tt.wantPlatform)
			}
			if tt.wantCode == "" {
				return
			}
			p := decodeProblem(t, rec)
			if p.Code != tt.wantCode {
				t.Fatalf("code %s, want %s", p.Code, tt.wantCode)
			}
			if len(p.Errors) != len(tt.wantFields) {
				t.Fatalf("errors %v, want %v", p.Errors, tt.wantFields)
			}
			for i := range p.Errors {
				for k, v := range tt.wantFields[i] {
					if p.Errors[i][k] != v {
						t.Fatalf("errors %v, want %v", p.Errors, tt.wantFields)
					}
				}
			}
			minimum := rec.Header().Get(httpx.HeaderMinimumAppVersion)
			if (tt.wantCode == httpx.CodeAppVersionUnsupported) != (minimum == testVersion) {
				t.Fatalf("%s = %q with code %s", httpx.HeaderMinimumAppVersion, minimum, p.Code)
			}
		})
	}
}

func TestClientGateDeviceReachesHandler(t *testing.T) {
	t.Parallel()

	h, reg := newGateRouter(t)
	rec := doRaw(h, http.MethodGet, "/v1/jobs", clientHeader("ANDROID", "2.0.0", "device:01.ab_CD-ef"), "")
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Seen-Device") != "device:01.ab_CD-ef" {
		t.Fatalf("status %d, device %q", rec.Code, rec.Header().Get("X-Seen-Device"))
	}
	doRaw(h, http.MethodGet, "/v1/jobs", clientHeader("ANDROID", "0.1.0", "device:01.ab_CD-ef"), "")
	if got := counterValue(t, reg, httpx.MetricAppVersionRejected, "platform=ANDROID"); got != 1 {
		t.Fatalf("%s{platform=ANDROID} = %v, want 1", httpx.MetricAppVersionRejected, got)
	}
}

// TestCanonicalPath: non-canonical paths are refused before routing, also
// when the handler would have matched (a catch-all here).
func TestCanonicalPath(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	r := newRouter(t, logger, httpx.NewHealth(logger))
	r.Get("/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := withClient(r)

	accepted := []string{"/", "/v1/jobs/42", "/v1/jobs/", "/v1/a%20b", "/v1/caf%C3%A9", "/v1/a.b/..c/c..", "/v1/%2E%2Ex"}
	refused := []string{
		"//v1/jobs", "/v1//jobs", "/v1/./jobs", "/v1/../jobs", "/v1/jobs/..", "/v1/jobs/.",
		"/v1/%2e%2e/jobs", "/v1/.%2E/jobs", "/v1/%2E/jobs", "/uploads/images/..%2Ffiles/x",
		"/v1/a%2fb", "/v1/a%2Fb", "/v1/a%5cb", "/v1/a%5Cb", "/v1/%252e%252e/x", "/v1/a%00b",
		"/v1/a%0Ab", "/v1/a%7Fb", "/v1/%c0%ae%c0%ae/x", "/v1/a%5C..%5Cb",
	}
	for _, target := range accepted {
		if rec := do(h, http.MethodGet, target, nil); rec.Code != http.StatusNoContent {
			t.Errorf("GET %s = %d, want 204: %s", target, rec.Code, rec.Body)
		}
	}
	for _, target := range refused {
		rec := do(h, http.MethodGet, target, nil)
		if rec.Code != http.StatusBadRequest || decodeProblem(t, rec).Code != httpx.CodePathInvalid {
			t.Errorf("GET %s = %d %s, want 400 %s", target, rec.Code, rec.Body, httpx.CodePathInvalid)
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "*", http.NoBody)
	setClientHeaders(req.Header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("OPTIONS * = %d, want 400", rec.Code)
	}
}

// FuzzCanonicalPath is the path traversal fuzz target of plan §6.1 (Faz
// 2's exit criterion): whatever request target the middleware lets
// through, its decoded path is canonical and, joined under a storage
// prefix, stays inside it. CI runs the seed corpus with go test; a longer
// run is `go test -fuzz FuzzCanonicalPath ./internal/platform/httpx`.
func FuzzCanonicalPath(f *testing.F) {
	for _, s := range []string{
		"/", "/v1/jobs/42", "/uploads/images/..%2Ffiles/x", "/v1/%2e%2e/%2e%2e/etc/passwd",
		"/v1/..%2f..%2fetc", "/v1/%252e%252e%252f", "/v1/a//b", "/v1/.%2e/x", "/v1/%c0%ae%c0%ae/",
		"/v1/a%5c..%5cb", "/v1/..;/x", "/v1/%u002e%u002e/x", "/v1/a%00/b", "*", "/v1/jobs/",
	} {
		f.Add(s)
	}
	logger, _ := newLogger()
	f.Fuzz(func(t *testing.T, target string) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://api.test"+target, http.NoBody)
		if err != nil || req.URL.Host != "api.test" || req.URL.RawQuery != "" || req.URL.Fragment != "" {
			return
		}
		var got *http.Request
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })
		httpx.CanonicalPath(logger)(next).ServeHTTP(httptest.NewRecorder(), req)
		if got == nil {
			return
		}
		p := got.URL.Path
		if strings.Contains(p, "\\") || strings.Contains(p, "//") || strings.ContainsRune(p, 0) {
			t.Fatalf("accepted %q: decoded path %q", target, p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "." || seg == ".." {
				t.Fatalf("accepted %q: dot segment in %q", target, p)
			}
		}
		raw := strings.ToLower(got.URL.EscapedPath())
		for _, enc := range []string{"%2f", "%5c", "%25"} {
			if strings.Contains(raw, enc) {
				t.Fatalf("accepted %q: %s in %q", target, enc, raw)
			}
		}
		const root = "/srv/uploads"
		if joined := root + p; !strings.HasPrefix(cleanJoin(joined), root) {
			t.Fatalf("accepted %q: %q escapes %s", target, p, root)
		}
	})
}

// cleanJoin resolves dot segments the way a storage layer would.
func cleanJoin(p string) string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/")
}

func TestClientIPResolver(t *testing.T) {
	t.Parallel()

	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:ffff::/48")}
	tests := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     []string
		extra   http.Header
		want    string
	}{
		{"no proxies configured: header ignored", nil, "203.0.113.9:5000", []string{"198.51.100.1"}, nil, "203.0.113.9"},
		{"untrusted peer: header ignored", trusted, "203.0.113.9:5000", []string{"198.51.100.1"}, nil, "203.0.113.9"},
		{"one proxy", trusted, "10.0.0.2:5000", []string{"203.0.113.5"}, nil, "203.0.113.5"},
		{"spoofed entry left of the proxy's", trusted, "10.0.0.2:5000", []string{"198.51.100.1, 203.0.113.5"}, nil, "203.0.113.5"},
		{"two proxies", trusted, "10.0.0.2:5000", []string{"198.51.100.1, 203.0.113.5, 10.9.9.9"}, nil, "203.0.113.5"},
		{"repeated header lines", trusted, "10.0.0.2:5000", []string{"198.51.100.1", "203.0.113.5"}, nil, "203.0.113.5"},
		{"all hops trusted", trusted, "10.0.0.2:5000", []string{"10.2.2.2, 10.1.1.1"}, nil, "10.2.2.2"},
		{"trusted peer without header", trusted, "10.0.0.2:5000", nil, nil, "10.0.0.2"},
		{"garbage from the proxy", trusted, "10.0.0.2:5000", []string{"203.0.113.5, not-an-ip"}, nil, "10.0.0.2"},
		{"garbage left of a trusted hop", trusted, "10.0.0.2:5000", []string{"not-an-ip, 10.1.1.1"}, nil, "10.1.1.1"},
		{"Forwarded is not read", trusted, "10.0.0.2:5000", nil, http.Header{"Forwarded": {"for=198.51.100.1"}}, "10.0.0.2"},
		{"IPv6 proxy and client", trusted, "[2001:db8:ffff::1]:443", []string{"[2001:db8:1::7]:51000"}, nil, "2001:db8:1::7"},
		{"IPv4-mapped peer", trusted, "[::ffff:10.0.0.2]:5000", []string{"203.0.113.5"}, nil, "203.0.113.5"},
	}
	for _, tt := range tests {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
		req.RemoteAddr = tt.remote
		for _, v := range tt.xff {
			req.Header.Add(httpx.HeaderForwardedFor, v)
		}
		for k, v := range tt.extra {
			req.Header[k] = v
		}
		if got := httpx.NewClientIPResolver(tt.trusted).Resolve(req); got.String() != tt.want {
			t.Errorf("%s: Resolve() = %s, want %s", tt.name, got, tt.want)
		}
	}
}

type fakeNow struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeNow) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeNow) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// newLimitedRouter routes /v1/jobs/{id} (read), POST /v1/login (auth,
// named by x-rate-limit) and /v1/me (read, per identity after a stand-in
// for authentication).
func newLimitedRouter(t *testing.T, clock *fakeNow) (http.Handler, *prometheus.Registry) {
	t.Helper()
	logger, _ := newLogger()
	opts, _ := testRouterOptions(t)
	reg := prometheus.NewRegistry()
	m, err := httpx.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := httpx.NewRateLimiter(httpx.RateLimitConfig{
		Groups: map[string]httpx.GroupLimits{
			httpx.GroupRead:  {PerIP: httpx.Rate{Every: time.Second, Burst: 2}, PerIdentity: httpx.Rate{Every: time.Minute, Burst: 1}},
			httpx.GroupWrite: {PerIP: httpx.Rate{Every: time.Second, Burst: 2}},
			"auth":           {PerIP: httpx.Rate{Every: time.Minute, Burst: 1}},
		},
		Routes: map[string]string{"POST /v1/login": "auth"},
		Now:    clock.Now,
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	opts.Metrics, opts.RateLimit = m, limiter
	opts.ClientIPs = httpx.NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	r, err := httpx.NewRouter(logger, httpx.NewHealth(logger), opts)
	if err != nil {
		t.Fatal(err)
	}
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	r.Get("/v1/jobs/{id}", ok)
	r.Post("/v1/login", ok)
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(httpx.WithRateLimitIdentity(req.Context(), "user:1")))
		})
	}
	r.With(authenticate, limiter.ByIdentity(logger)).Get("/v1/me", ok)
	return withClient(r), reg
}

func TestRateLimit(t *testing.T) {
	t.Parallel()

	clock := &fakeNow{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	h, reg := newLimitedRouter(t, clock)
	get := func(target, remote string, header http.Header) *httptest.ResponseRecorder {
		return doRaw(h, http.MethodGet, target, header, remote)
	}

	for range 2 {
		if rec := get("/v1/jobs/1", "203.0.113.1:1", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("within burst: %d", rec.Code)
		}
	}
	rec := get("/v1/jobs/2", "203.0.113.1:2", nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("over burst: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if p := decodeProblem(t, rec); p.Code != httpx.CodeRateLimited || p.Instance != "/v1/jobs/{id}" {
		t.Fatalf("problem %+v, want %s on /v1/jobs/{id}", p, httpx.CodeRateLimited)
	}
	if got := counterValue(t, reg, httpx.MetricRateLimited, "group=read,key=ip"); got != 1 {
		t.Fatalf("%s = %v, want 1", httpx.MetricRateLimited, got)
	}

	// Another address has its own bucket; so has another group.
	if rec := get("/v1/jobs/1", "203.0.113.2:1", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("other address: %d", rec.Code)
	}
	if rec := doRaw(h, http.MethodPost, "/v1/login", nil, "203.0.113.1:1"); rec.Code != http.StatusNoContent {
		t.Fatalf("other group: %d", rec.Code)
	}
	rec = doRaw(h, http.MethodPost, "/v1/login", nil, "203.0.113.1:1")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("x-rate-limit group: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}

	// A spoofed X-Forwarded-For from an untrusted peer changes nothing.
	if rec := get("/v1/jobs/1", "203.0.113.1:3", http.Header{httpx.HeaderForwardedFor: {"198.51.100.77"}}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed header escaped the limit: %d", rec.Code)
	}
	// Through the trusted proxy, the client address is the bucket.
	for _, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests} {
		if rec := get("/v1/jobs/1", "10.0.0.5:1", http.Header{httpx.HeaderForwardedFor: {"198.51.100.8"}}); rec.Code != want {
			t.Fatalf("behind proxy: %d, want %d", rec.Code, want)
		}
	}

	// One IPv6 /64 is one client.
	get("/v1/jobs/1", "[2001:db8:1:2::1]:1", nil)
	get("/v1/jobs/1", "[2001:db8:1:2::2]:1", nil)
	if rec := get("/v1/jobs/1", "[2001:db8:1:2::3]:1", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("same /64: %d", rec.Code)
	}

	// Unmatched paths are limited too.
	get("/nope", "203.0.113.3:1", nil)
	get("/nope", "203.0.113.3:1", nil)
	if rec := get("/nope", "203.0.113.3:1", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("unmatched path: %d", rec.Code)
	}

	clock.Advance(time.Second)
	if rec := get("/v1/jobs/1", "203.0.113.1:1", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("after refill: %d", rec.Code)
	}

	// Per identity: one token a minute whatever the address.
	if rec := get("/v1/me", "203.0.113.10:1", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("identity first: %d", rec.Code)
	}
	if rec := get("/v1/me", "203.0.113.11:1", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("identity second, other address: %d", rec.Code)
	}
	if got := counterValue(t, reg, httpx.MetricRateLimited, "group=read,key=identity"); got != 1 {
		t.Fatalf("identity counter = %v, want 1", got)
	}
}

// TestRateLimiterBoundsMemory: a flood of keys never holds more than
// MaxKeys buckets, evictions are counted, and idle buckets are dropped.
func TestRateLimiterBoundsMemory(t *testing.T) {
	t.Parallel()

	clock := &fakeNow{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	reg := prometheus.NewRegistry()
	l, err := httpx.NewRateLimiter(httpx.RateLimitConfig{
		Groups: map[string]httpx.GroupLimits{
			httpx.GroupRead:  {PerIP: httpx.Rate{Every: time.Second, Burst: 5}},
			httpx.GroupWrite: {PerIP: httpx.Rate{Every: time.Second, Burst: 5}},
		},
		MaxKeys: 2000,
		Now:     clock.Now,
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10_000 {
		if ok, _ := l.Allow(httpx.GroupRead, httpx.KeyIP, "flood-"+strconv.Itoa(i)); !ok {
			t.Fatal("a new key was refused")
		}
		if i%997 != 0 {
			continue
		}
		if keys := rateLimitKeys(t, reg); keys > 2000 {
			t.Fatalf("%v buckets held, cap 2000", keys)
		}
	}
	if keys := rateLimitKeys(t, reg); keys > 2000 {
		t.Fatalf("%v buckets held, cap 2000", keys)
	}
	if got := counterValue(t, reg, httpx.MetricRateLimitEvicted, ""); got == 0 {
		t.Fatal("no eviction counted")
	}

	clock.Advance(time.Hour)
	for i := range 1500 {
		l.Allow(httpx.GroupRead, httpx.KeyIP, "later-"+strconv.Itoa(i))
	}
	if keys := rateLimitKeys(t, reg); keys > 1500 {
		t.Fatalf("idle buckets kept: %v held", keys)
	}
}

func TestRateLimiterRefusesBadConfig(t *testing.T) {
	t.Parallel()

	r := httpx.Rate{Every: time.Second, Burst: 1}
	tests := map[string]httpx.RateLimitConfig{
		"no write group": {Groups: map[string]httpx.GroupLimits{httpx.GroupRead: {PerIP: r}}},
		"read without per-IP rate": {Groups: map[string]httpx.GroupLimits{
			httpx.GroupRead: {PerIdentity: r}, httpx.GroupWrite: {PerIP: r},
		}},
		"group without a rate": {Groups: map[string]httpx.GroupLimits{
			httpx.GroupRead: {PerIP: r}, httpx.GroupWrite: {PerIP: r}, "auth": {},
		}},
		"route in an unknown group": {
			Groups: map[string]httpx.GroupLimits{httpx.GroupRead: {PerIP: r}, httpx.GroupWrite: {PerIP: r}},
			Routes: map[string]string{"POST /v1/login": "auth"},
		},
	}
	for name, cfg := range tests {
		if _, err := httpx.NewRateLimiter(cfg, prometheus.NewRegistry()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRateLimitRoutes(t *testing.T) {
	t.Parallel()

	load := func(ext string) *openapi3.T {
		doc, err := openapi3.NewLoader().LoadFromData([]byte(`openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /v1/login:
    post:
      ` + ext + `
      responses: {default: {description: x}}
  /v1/jobs:
    get:
      responses: {default: {description: x}}
`))
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	routes, err := httpx.RateLimitRoutes(load("x-rate-limit: auth"))
	if err != nil || len(routes) != 1 || routes["POST /v1/login"] != "auth" {
		t.Fatalf("RateLimitRoutes() = %v, %v", routes, err)
	}
	if _, err := httpx.RateLimitRoutes(load("x-rate-limit: 5")); err == nil {
		t.Fatal("a non-string group was accepted")
	}
}

func TestNewRouterRequiresEdgeOptions(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	full, _ := testRouterOptions(t)
	for name, mutate := range map[string]func(*httpx.RouterOptions){
		"no client IPs":  func(o *httpx.RouterOptions) { o.ClientIPs = nil },
		"no rate limit":  func(o *httpx.RouterOptions) { o.RateLimit = nil },
		"no metrics":     func(o *httpx.RouterOptions) { o.Metrics = nil },
		"all set (ok)":   func(*httpx.RouterOptions) {},
		"zero (refused)": func(o *httpx.RouterOptions) { *o = httpx.RouterOptions{} },
	} {
		opts := full
		mutate(&opts)
		_, err := httpx.NewRouter(logger, httpx.NewHealth(logger), opts)
		if (err == nil) != (name == "all set (ok)") {
			t.Errorf("%s: NewRouter error = %v", name, err)
		}
	}
}

// TestMetricsUseRouteTemplates: the HTTP histogram is labeled with the
// route template, never the raw path, and with the status actually sent;
// a request refused before routing keeps the route it was headed for.
func TestMetricsUseRouteTemplates(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	opts, reg := testRouterOptions(t)
	r, err := httpx.NewRouter(logger, httpx.NewHealth(logger), opts)
	if err != nil {
		t.Fatal(err)
	}
	r.Get("/v1/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := withClient(r)
	do(h, http.MethodGet, "/v1/jobs/4242", nil)
	do(h, http.MethodGet, "/v1/jobs/4343", nil)
	doRaw(h, http.MethodGet, "/v1/jobs/4444", nil, "")
	do(h, "PURGE", "/v1/jobs/1", nil)

	got := series(t, reg, httpx.MetricRequestDuration)
	want := map[string]float64{
		"http_request_method=GET,http_response_status_code=204,http_route=/v1/jobs/{id}": 2,
		"http_request_method=GET,http_response_status_code=400,http_route=/v1/jobs/{id}": 1,
		"http_request_method=_OTHER,http_response_status_code=405,http_route=":           1,
	}
	if len(got) != len(want) {
		t.Fatalf("series %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("series %v, want %v", got, want)
		}
	}
	for k := range got {
		if strings.Contains(k, "4242") {
			t.Fatalf("raw path in a label: %q", k)
		}
	}
}

// TestServerStreamsKeepTheForceCloseMargin: streams get ShutdownTimeout
// minus the force-close margin, so cutting stragglers fits in the
// ShutdownTimeout the stop grace period is sized for.
func TestServerStreamsKeepTheForceCloseMargin(t *testing.T) {
	t.Parallel()

	ds := &deadlineStream{got: make(chan time.Duration, 1)}
	ts := startTestServer(t, httpx.ServerOptions{ShutdownTimeout: 8 * time.Second, Streams: []httpx.Stream{ds}})
	start := time.Now()
	ts.cancel()
	select {
	case left := <-ds.got:
		budget := 8*time.Second - httpx.StreamForceCloseMargin
		if left > budget || left < budget-2*time.Second {
			t.Fatalf("stream context ends in %s, want about %s (started %s ago)", left, budget, time.Since(start))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown not called")
	}
}

type deadlineStream struct{ got chan time.Duration }

func (*deadlineStream) StartDraining() {}

func (d *deadlineStream) Shutdown(ctx context.Context) error {
	dl, _ := ctx.Deadline()
	d.got <- time.Until(dl)
	return nil
}

// series returns the samples of the metric family name, keyed by their
// labels ("name=value,…" in label-name order): a counter's or gauge's
// value, a histogram's sample count. It is nil when the family has no
// sample.
func series(t *testing.T, reg *prometheus.Registry, name string) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		out = map[string]float64{}
		for _, m := range mf.GetMetric() {
			var parts []string
			for _, l := range m.GetLabel() {
				parts = append(parts, l.GetName()+"="+l.GetValue())
			}
			var v float64
			switch {
			case m.GetCounter() != nil:
				v = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				v = m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				v = float64(m.GetHistogram().GetSampleCount())
			}
			out[strings.Join(parts, ",")] = v
		}
	}
	return out
}

func counterValue(t *testing.T, reg *prometheus.Registry, name, labels string) float64 {
	t.Helper()
	s := series(t, reg, name)
	if s == nil {
		t.Fatalf("metric %s not registered", name)
	}
	return s[labels]
}

func rateLimitKeys(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	return counterValue(t, reg, httpx.MetricRateLimitKeys, "")
}
