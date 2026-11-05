package httpx

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

// Metric names (plan §3.10, ADR-0021). The HTTP histogram follows the
// OpenTelemetry HTTP semantic conventions: http.server.request.duration
// with http.request.method, http.route and http.response.status_code.
const (
	MetricRequestDuration    = "http_server_request_duration_seconds"
	MetricAppVersionRejected = "koala_http_app_version_rejected_total"
)

// durationBuckets are the OpenTelemetry semconv buckets of
// http.server.request.duration, in seconds.
var durationBuckets = []float64{ //nolint:gochecknoglobals // read-only bucket boundaries
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

// knownMethods are kept as label values; any other method is "_OTHER"
// (semconv), so a client cannot grow the series count.
var knownMethods = map[string]bool{ //nolint:gochecknoglobals // read-only method set
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
	http.MethodConnect: true, http.MethodTrace: true,
}

// Metrics are the HTTP platform's Prometheus metrics.
type Metrics struct {
	duration        *prometheus.HistogramVec
	versionRejected *prometheus.CounterVec
}

// NewMetrics registers the HTTP metrics on reg.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: MetricRequestDuration,
			Help: "Duration of HTTP server requests. http_route is the matched route template, " +
				"or empty when no route matched.",
			Buckets: durationBuckets,
		}, []string{"http_request_method", "http_route", "http_response_status_code"}),
		versionRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricAppVersionRejected,
			Help: "Requests refused with APP_VERSION_UNSUPPORTED, by client platform.",
		}, []string{"platform"}),
	}
	for _, c := range []prometheus.Collector{m.duration, m.versionRejected} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("httpx metrics: %w", err)
		}
	}
	// Known label values start at zero, so increase() sees the first event.
	for _, p := range []ClientPlatform{PlatformIOS, PlatformAndroid} {
		m.versionRejected.WithLabelValues(string(p))
	}
	return m, nil
}

func (m *Metrics) appVersionRejected(p ClientPlatform) {
	m.versionRejected.WithLabelValues(string(p)).Inc()
}

// instrument observes every request's duration with its route template,
// never the raw path, which would make one series per id (plan §3.10). A
// request refused before routing (400 PATH_INVALID, 429) carries the route
// the router looked up for it. A response aborted by a panic counts as a
// 500: the client did not get the response it was promised.
func (m *Metrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		defer m.observe(ww, r, time.Now())
		next.ServeHTTP(ww, r)
	})
}

// observe must be deferred directly so recover() sees an aborting panic;
// it re-raises it.
func (m *Metrics) observe(ww middleware.WrapResponseWriter, r *http.Request, start time.Time) {
	aborted := recover()
	status := ww.Status()
	switch {
	case aborted != nil:
		status = http.StatusInternalServerError
	case status == 0:
		status = http.StatusOK
	}
	method := r.Method
	if !knownMethods[method] {
		method = "_OTHER"
	}
	m.duration.WithLabelValues(method, routeOf(r), strconv.Itoa(status)).Observe(time.Since(start).Seconds())
	if aborted != nil {
		panic(aborted)
	}
}
