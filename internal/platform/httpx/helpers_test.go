package httpx_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/appversion"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
)

// logSink is a goroutine-safe buffer of JSON log lines.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// records returns every log line decoded.
func (s *logSink) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(s.buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %q", err, sc.Text())
		}
		out = append(out, m)
	}
	return out
}

// find returns the first record with the given message.
func (s *logSink) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	for _, r := range s.records(t) {
		if r["msg"] == msg {
			return r
		}
	}
	t.Fatalf("no log record %q", msg)
	return nil
}

func newLogger() (*slog.Logger, *logSink) {
	sink := &logSink{}
	return log.New(sink, slog.LevelDebug), sink
}

// Headers of a current iOS build; withClient adds them to requests that
// do not set them, so tests about other middleware pass the client gate.
const (
	testPlatform = "IOS"
	testVersion  = "1.0.0"
	testDevice   = "test-device-0001"
)

// testRouterOptions opens up the edge rules tests do not exercise:
// minimum app version 1.0.0, no trusted proxy and rate limits no test
// reaches.
func testRouterOptions(tb testing.TB) (httpx.RouterOptions, *prometheus.Registry) {
	tb.Helper()
	reg := prometheus.NewRegistry()
	m, err := httpx.NewMetrics(reg)
	if err != nil {
		tb.Fatal(err)
	}
	unlimited := httpx.Rate{Every: time.Nanosecond, Burst: 1 << 30}
	limiter, err := httpx.NewRateLimiter(httpx.RateLimitConfig{Groups: map[string]httpx.GroupLimits{
		httpx.GroupRead:  {PerIP: unlimited},
		httpx.GroupWrite: {PerIP: unlimited},
	}}, reg)
	if err != nil {
		tb.Fatal(err)
	}
	return httpx.RouterOptions{
		Clients:   httpx.ClientPolicy{MinAppVersion: appversion.MustParse(testVersion)},
		ClientIPs: httpx.NewClientIPResolver(nil),
		RateLimit: limiter,
		Metrics:   m,
	}, reg
}

func newRouter(tb testing.TB, logger *slog.Logger, health *httpx.Health) *httpx.Router {
	tb.Helper()
	opts, _ := testRouterOptions(tb)
	r, err := httpx.NewRouter(logger, health, opts)
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

// withClient adds the test client's headers to every request that lacks
// them.
func withClient(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setClientHeaders(r.Header)
		h.ServeHTTP(w, r)
	})
}

func setClientHeaders(h http.Header) {
	for k, v := range map[string]string{
		httpx.HeaderClientPlatform: testPlatform,
		httpx.HeaderAppVersion:     testVersion,
		httpx.HeaderDeviceID:       testDevice,
	} {
		if _, ok := h[k]; !ok {
			h.Set(k, v)
		}
	}
}
