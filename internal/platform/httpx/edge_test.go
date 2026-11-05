package httpx_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
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
		// Header values match case-insensitively; the canonical form is upper case.
		{"platform in lower case", "GET", "/v1/jobs", clientHeader("ios", "1.0.0", device), 204, "", nil, "IOS"},
		{"web as the web client sends it", "GET", "/v1/jobs", clientHeader("web", "", device), 204, "", nil, "WEB"},
		{
			"non-ASCII lookalike does not fold", "GET", "/v1/jobs", clientHeader("androıd", "1.0.0", device), 400, httpx.CodeRequestInvalid,
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
// 2's exit criterion). The request target goes the way a real request
// goes: parsed by net/http (http.ReadRequest, ParseRequestURI) and served
// by the router, with its root chain, to a route with a path parameter
// and to a catch-all. Whatever reaches a handler must be safe to use as a
// storage key:
//
//   - the decoded path is canonical (no empty, "." or ".." segment, no
//     backslash, no NUL, no encoded slash, backslash or percent);
//   - the decoded value of {name} is one segment: no "/", "\", "%", NUL,
//     and not "." or "..";
//   - the decoded catch-all, joined under a storage prefix and resolved
//     like a file system would, stays inside the prefix.
//
// CI runs the seed corpus with go test; a longer run is
// `go test -fuzz FuzzCanonicalPath ./internal/platform/httpx`.
func FuzzCanonicalPath(f *testing.F) {
	for _, s := range []string{
		"/v1/files/report.pdf", "/v1/raw/a/b.txt", "/v1/files/..%2Fsecret", "/v1/raw/..%2F..%2Fetc/passwd",
		"/v1/raw/%2e%2e/%2e%2e/etc/passwd", "/v1/files/%2e%2e", "/v1/raw/%252e%252e%252f", "/v1/raw/a//b",
		"/v1/raw/.%2e/x", "/v1/raw/%c0%ae%c0%ae/", "/v1/files/a%5c..%5cb", "/v1/raw/..;/x", "/v1/files/a%00",
		"*", "/v1/files/", "http://api.test/v1/raw/../x", "/v1/files/caf%C3%A9", "/v1/raw/a%20b/c",
	} {
		f.Add(s)
	}
	logger, _ := newLogger()
	r := newRouter(f, logger, httpx.NewHealth(logger))
	type seen struct{ path, escaped, name, rest string }
	record := func(w http.ResponseWriter, req *http.Request) {
		got, ok := req.Context().Value(seenKey{}).(*seen)
		if ok {
			*got = seen{req.URL.Path, req.URL.EscapedPath(), chi.URLParam(req, "name"), chi.URLParam(req, "*")}
		}
		w.WriteHeader(http.StatusNoContent)
	}
	r.Get("/v1/files/{name}", record)
	r.Get("/v1/raw/*", record)

	f.Fuzz(func(t *testing.T, target string) {
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET " + target + " HTTP/1.1\r\nHost: api.test\r\n\r\n")))
		if err != nil {
			return
		}
		got := &seen{}
		req = req.WithContext(context.WithValue(context.Background(), seenKey{}, got))
		req.RemoteAddr = "192.0.2.1:1234"
		setClientHeaders(req.Header)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			return // refused, or no route
		}
		p := got.path
		if strings.Contains(p, "\\") || strings.Contains(p, "//") || strings.ContainsRune(p, 0) || strings.Contains(p, "%") {
			t.Fatalf("served %q: decoded path %q", target, p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "." || seg == ".." {
				t.Fatalf("served %q: dot segment in %q", target, p)
			}
		}
		for _, enc := range []string{"%2f", "%5c", "%25"} {
			if strings.Contains(strings.ToLower(got.escaped), enc) {
				t.Fatalf("served %q: %s in %q", target, enc, got.escaped)
			}
		}
		if got.name != "" {
			name, err := url.PathUnescape(got.name)
			if err != nil || name == "." || name == ".." || strings.ContainsAny(name, "/\\%\x00") {
				t.Fatalf("served %q: {name} %q decodes to %q (%v)", target, got.name, name, err)
			}
		}
		rest, err := url.PathUnescape(got.rest)
		if err != nil {
			t.Fatalf("served %q: catch-all %q does not decode: %v", target, got.rest, err)
		}
		const root = "/srv/uploads"
		for _, v := range []string{rest, p} {
			if joined := cleanJoin(root + "/" + v); joined != root && !strings.HasPrefix(joined, root+"/") {
				t.Fatalf("served %q: %q escapes %s as %q", target, v, root, joined)
			}
		}
	})
}

type seenKey struct{}

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
		// Only the last 16 entries are read: a client can prepend any number.
		{"long client-written prefix", trusted, "10.0.0.2:5000", []string{"198.51.100.1, 198.51.100.2, 198.51.100.3, 198.51.100.4, 198.51.100.5, 198.51.100.6, 198.51.100.7, 198.51.100.8, 198.51.100.9, 198.51.100.10, 198.51.100.11, 198.51.100.12, 198.51.100.13, 198.51.100.14, 198.51.100.15, 198.51.100.16, 198.51.100.17, 198.51.100.18, 198.51.100.19, 198.51.100.20, 203.0.113.5"}, nil, "203.0.113.5"},
		{"more trusted hops than are read", trusted, "10.0.0.2:5000", []string{"198.51.100.9, 10.0.1.1, 10.0.1.2, 10.0.1.3, 10.0.1.4, 10.0.1.5, 10.0.1.6, 10.0.1.7, 10.0.1.8, 10.0.1.9, 10.0.1.10, 10.0.1.11, 10.0.1.12, 10.0.1.13, 10.0.1.14, 10.0.1.15, 10.0.1.16, 10.0.1.17"}, nil, "10.0.1.2"},
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

func newFloodLimiter(t *testing.T, clock *fakeNow, maxKeys int) (*httpx.RateLimiter, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	every := httpx.Rate{Every: time.Second, Burst: 5}
	l, err := httpx.NewRateLimiter(httpx.RateLimitConfig{
		Groups: map[string]httpx.GroupLimits{
			httpx.GroupRead:  {PerIP: every, PerIdentity: httpx.Rate{Every: time.Minute, Burst: 2}},
			httpx.GroupWrite: {PerIP: every},
			"auth":           {PerIP: every, PerCustom: httpx.Rate{Every: time.Minute, Burst: 2}},
		},
		MaxKeys: maxKeys,
		Now:     clock.Now,
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return l, reg
}

// TestRateLimiterPruneKeepsHeadroom: when dropping idle buckets leaves
// the limiter above 90% of the cap, it still evicts down to 90%, so the
// next tenth of the cap is inserted without another scan (no rescan on
// every new key when almost nothing is idle).
func TestRateLimiterPruneKeepsHeadroom(t *testing.T) {
	t.Parallel()

	clock := &fakeNow{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	l, reg := newFloodLimiter(t, clock, 1000)
	allow := func(prefix string, n int) {
		for i := range n {
			l.Allow(httpx.GroupRead, httpx.KeyIP, prefix+strconv.Itoa(i))
		}
	}
	allow("idle-", 50)
	clock.Advance(time.Hour) // the first 50 buckets are full again
	allow("busy-", 950)
	if keys := rateLimitKeys(t, reg); keys != 1000 {
		t.Fatalf("%v buckets before the prune, want 1000", keys)
	}
	allow("new-", 1) // prune: 50 idle dropped, 950 > 900, so 50 evicted
	evicted := func() float64 { return counterValue(t, reg, httpx.MetricRateLimitEvicted, "") }
	if keys, ev := rateLimitKeys(t, reg), evicted(); keys != 901 || ev != 50 {
		t.Fatalf("after the prune: %v buckets, %v evicted; want 901, 50", keys, ev)
	}
	allow("more-", 99) // up to the cap without another eviction
	if keys, ev := rateLimitKeys(t, reg), evicted(); keys != 1000 || ev != 50 {
		t.Fatalf("headroom: %v buckets, %v evicted; want 1000, 50", keys, ev)
	}
	allow("last-", 1)
	if keys, ev := rateLimitKeys(t, reg), evicted(); keys != 901 || ev != 150 {
		t.Fatalf("second prune: %v buckets, %v evicted; want 901, 150", keys, ev)
	}
}

// TestRateLimiterEvictsAddressesFirst: a flood of new addresses evicts
// address buckets, never the exhausted bucket of a login target (custom)
// or of a user (identity). Otherwise a brute-force attempt could reset
// its own target's limit by flooding from many addresses.
func TestRateLimiterEvictsAddressesFirst(t *testing.T) {
	t.Parallel()

	clock := &fakeNow{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	l, reg := newFloodLimiter(t, clock, 1000)
	for range 2 {
		l.Allow("auth", httpx.KeyCustom, "target-hash")
		l.Allow(httpx.GroupRead, httpx.KeyIdentity, "user:1")
	}
	if ok, _ := l.Allow("auth", httpx.KeyCustom, "target-hash"); ok {
		t.Fatal("target bucket not exhausted")
	}
	for i := range 20_000 {
		l.Allow(httpx.GroupRead, httpx.KeyIP, "flood-"+strconv.Itoa(i))
	}
	if counterValue(t, reg, httpx.MetricRateLimitEvicted, "") == 0 {
		t.Fatal("the flood evicted nothing")
	}
	if ok, _ := l.Allow("auth", httpx.KeyCustom, "target-hash"); ok {
		t.Fatal("the flood reset the login target's bucket")
	}
	if ok, _ := l.Allow(httpx.GroupRead, httpx.KeyIdentity, "user:1"); ok {
		t.Fatal("the flood reset the user's bucket")
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
	r.Stream("/v1/stream", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := withClient(r)
	do(h, http.MethodGet, "/v1/stream", nil) // a stream is not a request latency: no series
	do(h, http.MethodGet, "/v1/jobs/4242", nil)
	do(h, http.MethodGet, "/v1/jobs/4343", nil)
	doRaw(r, http.MethodGet, "/v1/jobs/4444", nil, "") // no client headers: 400
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
