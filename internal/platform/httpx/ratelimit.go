package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// CodeRateLimited answers a request over its rate limit, with Retry-After.
const CodeRateLimited = "RATE_LIMITED"

// Default groups. Every route belongs to a group: the one its operation
// names with x-rate-limit, or else GroupRead for GET, HEAD and OPTIONS and
// GroupWrite for every other method. Nothing is unlimited by omission.
const (
	GroupRead  = "read"
	GroupWrite = "write"
)

// ExtRateLimit is the operation extension that puts the operation in a
// rate limit group other than its method's default.
const ExtRateLimit = "x-rate-limit"

// Rate limit metric names (ADR-0021).
const (
	MetricRateLimited       = "koala_http_rate_limited_total"
	MetricRateLimitKeys     = "koala_http_rate_limit_keys"
	MetricRateLimitEvicted  = "koala_http_rate_limit_evicted_total"
	defaultRateLimitMaxKeys = 100_000
	minRateLimitPrune       = 1024
)

// KeyKind says what a bucket key identifies.
type KeyKind string

// Key kinds.
const (
	// KeyIP is the client address (an IPv6 client by its /64).
	KeyIP KeyKind = "ip"
	// KeyIdentity is the authenticated user or staff member.
	KeyIdentity KeyKind = "identity"
	// KeyCustom is a key a module chooses, such as the hash of a login
	// target (Java's AUTH bucket keyed by path, IP and target).
	KeyCustom KeyKind = "custom"
)

// Rate is a token bucket: Burst requests at once, then one per Every. The
// zero Rate means no limit for that key kind.
type Rate struct {
	Every time.Duration
	Burst int
}

func (r Rate) enabled() bool { return r.Every > 0 && r.Burst > 0 }

// GroupLimits are the buckets of one group: per client address, applied
// to every request before routing, and per identity, applied after
// authentication.
type GroupLimits struct {
	PerIP       Rate
	PerIdentity Rate
	PerCustom   Rate
}

// RateLimitConfig configures a RateLimiter.
type RateLimitConfig struct {
	// Groups must contain GroupRead and GroupWrite, each with a PerIP rate.
	Groups map[string]GroupLimits
	// Routes maps "METHOD /route/{pattern}" to a group; RateLimitRoutes
	// builds it from the contract's x-rate-limit extensions.
	Routes map[string]string
	// MaxKeys caps the buckets kept in memory (default 100 000). Idle
	// buckets (full again) are dropped first; past the cap, arbitrary
	// buckets are evicted, which resets them, and counted.
	MaxKeys int
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

type bucketKey struct {
	group string
	kind  KeyKind
	key   string
}

// RateLimiter keeps token buckets in memory, one process wide. Several
// instances do not share buckets; ADR-0021 records this as a deliberate
// limit until a second instance runs.
type RateLimiter struct {
	groups  map[string]GroupLimits
	routes  map[string]string
	maxKeys int
	now     func() time.Time

	mu      sync.Mutex
	buckets map[bucketKey]*rate.Limiter
	pruneAt int

	limited *prometheus.CounterVec
	evicted prometheus.Counter
}

// NewRateLimiter validates cfg and registers the limiter's metrics on reg.
func NewRateLimiter(cfg RateLimitConfig, reg prometheus.Registerer) (*RateLimiter, error) {
	for _, g := range []string{GroupRead, GroupWrite} {
		if !cfg.Groups[g].PerIP.enabled() {
			return nil, fmt.Errorf("rate limit: group %q needs a per-IP rate", g)
		}
	}
	for name, g := range cfg.Groups {
		if !g.PerIP.enabled() && !g.PerIdentity.enabled() && !g.PerCustom.enabled() {
			return nil, fmt.Errorf("rate limit: group %q has no rate", name)
		}
	}
	for route, g := range cfg.Routes {
		if _, ok := cfg.Groups[g]; !ok {
			return nil, fmt.Errorf("rate limit: %s: unknown group %q", route, g)
		}
	}
	l := &RateLimiter{
		groups:  cfg.Groups,
		routes:  cfg.Routes,
		maxKeys: cfg.MaxKeys,
		now:     cfg.Now,
		buckets: make(map[bucketKey]*rate.Limiter),
		limited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricRateLimited,
			Help: "Requests refused with 429 RATE_LIMITED, by rate limit group and key kind.",
		}, []string{"group", "key"}),
		evicted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: MetricRateLimitEvicted,
			Help: "Rate limit buckets evicted because MaxKeys was reached; non-zero means the cap is too low or a flood of addresses.",
		}),
	}
	if l.maxKeys <= 0 {
		l.maxKeys = defaultRateLimitMaxKeys
	}
	if l.now == nil {
		l.now = time.Now
	}
	l.pruneAt = min(minRateLimitPrune, l.maxKeys)
	keys := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: MetricRateLimitKeys,
		Help: "Rate limit buckets held in memory.",
	}, func() float64 {
		l.mu.Lock()
		defer l.mu.Unlock()
		return float64(len(l.buckets))
	})
	for _, c := range []prometheus.Collector{l.limited, l.evicted, keys} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("rate limit metrics: %w", err)
		}
	}
	for name, g := range cfg.Groups {
		for kind, r := range map[KeyKind]Rate{KeyIP: g.PerIP, KeyIdentity: g.PerIdentity, KeyCustom: g.PerCustom} {
			if r.enabled() {
				l.limited.WithLabelValues(name, string(kind))
			}
		}
	}
	return l, nil
}

// Group returns the rate limit group of a route.
func (l *RateLimiter) Group(method, pattern string) string {
	if g, ok := l.routes[method+" "+pattern]; ok {
		return g
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return GroupRead
	default:
		return GroupWrite
	}
}

// Allow takes one token from the bucket of key in group and reports
// whether the request may proceed and, if not, how long until it may. A
// group without a rate for kind always allows.
func (l *RateLimiter) Allow(group string, kind KeyKind, key string) (bool, time.Duration) {
	g, ok := l.groups[group]
	if !ok {
		return false, 0 // a wiring bug; callers only pass known groups
	}
	var r Rate
	switch kind {
	case KeyIP:
		r = g.PerIP
	case KeyIdentity:
		r = g.PerIdentity
	case KeyCustom:
		r = g.PerCustom
	}
	if !r.enabled() {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	k := bucketKey{group: group, kind: kind, key: key}
	b := l.buckets[k]
	if b == nil {
		if len(l.buckets) >= l.pruneAt {
			l.prune(now)
		}
		b = rate.NewLimiter(rate.Every(r.Every), r.Burst)
		l.buckets[k] = b
	}
	res := b.ReserveN(now, 1)
	if !res.OK() {
		return false, r.Every
	}
	if wait := res.DelayFrom(now); wait > 0 {
		res.CancelAt(now)
		l.limited.WithLabelValues(group, string(kind)).Inc()
		return false, wait
	}
	return true, 0
}

// prune drops buckets that are full again: their key has been idle long
// enough that a new bucket would behave the same. If the cap is still
// reached, arbitrary buckets are evicted down to 90% of it. Map order is
// random, so under a flood of new keys most evictions hit the flood.
func (l *RateLimiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.TokensAt(now) >= float64(b.Burst()) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.maxKeys {
		target := l.maxKeys * 9 / 10
		for k := range l.buckets {
			if len(l.buckets) <= target {
				break
			}
			delete(l.buckets, k)
			l.evicted.Inc()
		}
	}
	l.pruneAt = min(max(minRateLimitPrune, 2*len(l.buckets)), l.maxKeys)
}

// byIP is the root-chain limiter: every request, streams and unmatched
// paths included, takes a token from its client address's bucket in the
// group of the route the router looked up for it.
func (l *RateLimiter) byIP(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			group := l.Group(r.Method, lookedUpRoute(r))
			if ok, wait := l.Allow(group, KeyIP, ipKey(ClientIPFrom(r.Context()))); !ok {
				writeRateLimited(w, r, logger, wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ByIdentity limits authenticated requests per identity. It runs after
// authentication, which names the identity with WithRateLimitIdentity; a
// request without one passes (its address was limited before routing).
func (l *RateLimiter) ByIdentity(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := RateLimitIdentityFrom(r.Context())
			if id != "" {
				pattern := ""
				if rctx := chi.RouteContext(r.Context()); rctx != nil {
					pattern = rctx.RoutePattern()
				}
				if ok, wait := l.Allow(l.Group(r.Method, pattern), KeyIdentity, id); !ok {
					writeRateLimited(w, r, logger, wait)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimitedError returns the 429 error and sets Retry-After on w, for a
// module that limits with Allow and KeyCustom.
func RateLimitedError(w http.ResponseWriter, wait time.Duration) error {
	w.Header().Set("Retry-After", retryAfter(wait))
	return apperr.New(apperr.KindRateLimited, CodeRateLimited)
}

func writeRateLimited(w http.ResponseWriter, r *http.Request, logger *slog.Logger, wait time.Duration) {
	WriteError(w, r, logger, RateLimitedError(w, wait))
}

// retryAfter renders d in whole seconds, rounded up, at least 1.
func retryAfter(d time.Duration) string {
	return strconv.FormatInt(max(1, int64((d+time.Second-1)/time.Second)), 10)
}

// ipKey keys an IPv4 client by its address and an IPv6 client by its /64:
// one host usually gets a whole /64, so a per-address key would let it
// mint buckets.
func ipKey(a netip.Addr) string {
	if !a.IsValid() {
		return "unknown"
	}
	if a.Is6() {
		p, err := a.Prefix(64)
		if err == nil {
			return p.String()
		}
	}
	return a.String()
}

type identityKey struct{}

// WithRateLimitIdentity names the authenticated caller for ByIdentity.
// Authentication (Faz 1) calls it with a stable id such as "user:<id>" or
// "staff:<id>".
func WithRateLimitIdentity(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// RateLimitIdentityFrom returns the identity set by WithRateLimitIdentity.
func RateLimitIdentityFrom(ctx context.Context) string {
	id, _ := ctx.Value(identityKey{}).(string)
	return id
}

// RateLimitRoutes reads the x-rate-limit extension of every operation in
// spec: "METHOD /path/{template}" → group. A value that is not a string is
// an error; whether the group exists is checked by NewRateLimiter.
func RateLimitRoutes(spec *openapi3.T) (map[string]string, error) {
	if spec == nil || spec.Paths == nil {
		return nil, errors.New("rate limit routes: spec has no paths")
	}
	routes := map[string]string{}
	paths := spec.Paths.Map()
	keys := make([]string, 0, len(paths))
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, path := range keys {
		for method, op := range paths[path].Operations() {
			v, ok := op.Extensions[ExtRateLimit]
			if !ok {
				continue
			}
			g, ok := v.(string)
			if !ok || g == "" {
				return nil, fmt.Errorf("rate limit routes: %s %s: %s must be a group name", method, path, ExtRateLimit)
			}
			routes[method+" "+path] = g
		}
	}
	return routes, nil
}
