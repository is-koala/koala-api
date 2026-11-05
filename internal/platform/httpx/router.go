// Package httpx is the HTTP platform: router and middleware chain, problem
// documents, health endpoints and the graceful server lifecycle.
package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Health endpoint paths.
const (
	PathLivez  = "/livez"
	PathReadyz = "/readyz"
)

// Problem codes owned by the HTTP platform.
const (
	CodeRouteNotFound    = "ROUTE_NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeBodyTooLarge     = "REQUEST_BODY_TOO_LARGE"
)

// Router is the root handler. Its middleware chain is split in two,
// because chi cannot take a parent's middleware off a child route:
//
//	root:   RequestID → route lookup → SecurityHeaders → AccessLog →
//	        metrics → Recover → CanonicalPath → client address →
//	        client headers → rate limit per address
//	routes: root + Timeout → BodyLimit → route
//	stream: root → route
//
// The access log sees the request id and the 500 written by Recover, and
// every response, including 404/405 problems, carries the security
// headers. The client header gate and the per-address rate limit are in the
// root chain on purpose: a Stream route (WebSocket) skips everything else,
// and a gate outside the root chain would not see it (ADR-0018, ADR-0021).
// They run before routing, so the router looks the route up once, early,
// for them, for the access log and for the metrics.
//
// Router embeds the bounded routes: modules mount on it with the usual chi
// methods (Get, Post, Mount, With, ...) and get Timeout and BodyLimit. Only
// Stream routes, long-lived connections that manage their own deadlines
// (WebSocket, ADR-0011), skip them. Middleware every request needs,
// streams included, belongs in the root chain here.
type Router struct {
	chi.Router
	root    *chi.Mux
	logger  *slog.Logger
	streams map[string]bool
}

// RouterOptions are the edge rules every request meets (ADR-0021). All
// are required: a router without them would serve unprotected.
type RouterOptions struct {
	// Clients is the X-Client-Platform, X-App-Version and X-Device-Id rule.
	Clients ClientPolicy
	// ClientIPs resolves the client address behind trusted proxies.
	ClientIPs *ClientIPResolver
	// RateLimit limits every request per client address before routing.
	RateLimit *RateLimiter
	// Metrics observes every request.
	Metrics *Metrics
}

// NewRouter returns the root router with the platform middleware chain
// and health endpoints.
func NewRouter(logger *slog.Logger, health *Health, opts RouterOptions) (*Router, error) {
	if opts.ClientIPs == nil || opts.RateLimit == nil || opts.Metrics == nil {
		return nil, errors.New("httpx: router needs ClientIPs, RateLimit and Metrics")
	}
	root := chi.NewRouter()
	rt := &Router{root: root, logger: logger, streams: map[string]bool{}}
	gate := newClientGate(opts.Clients, rt.isStream, opts.Metrics, logger)
	root.Use(
		RequestID,
		lookupRoute(root),
		SecurityHeaders,
		AccessLog(logger),
		opts.Metrics.instrument,
		Recover(logger),
		CanonicalPath(logger),
		opts.ClientIPs.middleware,
		gate.middleware,
		opts.RateLimit.byIP(logger),
	)

	// Set before With so that the bounded group copies them.
	root.NotFound(func(w http.ResponseWriter, req *http.Request) {
		WriteProblem(w, logger, req, NewProblem(req, http.StatusNotFound, CodeRouteNotFound))
	})
	root.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		if allow := allowedMethods(root, req.URL.Path); allow != "" {
			w.Header().Set("Allow", allow)
		}
		WriteProblem(w, logger, req, NewProblem(req, http.StatusMethodNotAllowed, CodeMethodNotAllowed))
	})

	bounded := root.With(Timeout(RequestTimeout), BodyLimit(DefaultBodyLimit))
	bounded.Get(PathLivez, health.Live)
	bounded.Head(PathLivez, health.Live)
	bounded.Get(PathReadyz, health.Ready)
	bounded.Head(PathReadyz, health.Ready)
	rt.Router = bounded
	return rt, nil
}

// ServeHTTP serves every request through the root chain.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.root.ServeHTTP(w, r)
}

// Stream registers a GET route for a long-lived connection such as a
// WebSocket. It runs the root chain only: no Timeout and no BodyLimit,
// so the handler owns its deadlines. http.Server.Shutdown neither closes
// nor waits for hijacked connections; their owner goes into
// ServerOptions.Streams.
//
// The pattern answers every method: GET reaches h, the rest get 405 with
// Allow: GET. Registering GET alone would let other methods fall through
// to a bounded Mount on an enclosing prefix (for example "/v1") and come
// back as that sub-router's 404.
//
// Routes are registered before the server starts; isStream reads the set
// without a lock.
func (rt *Router) Stream(pattern string, h http.HandlerFunc) {
	rt.streams[pattern] = true
	rt.root.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			WriteProblem(w, rt.logger, req, NewProblem(req, http.StatusMethodNotAllowed, CodeMethodNotAllowed))
			return
		}
		h(w, req)
	})
}

func (rt *Router) isStream(pattern string) bool { return rt.streams[pattern] }

type lookupKey struct{}

// lookupRoute finds, before routing, the route template the request will
// match ("" for none, also for a method the route does not have) and
// stores it for the root chain, which runs before chi sets RoutePattern.
// It walks the same tree on the same path chi routes on.
func lookupRoute(root *chi.Mux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.RawPath
			if path == "" {
				path = r.URL.Path
			}
			pattern := root.Find(chi.NewRouteContext(), r.Method, path)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), lookupKey{}, pattern)))
		})
	}
}

// lookedUpRoute returns the route template lookupRoute found.
func lookedUpRoute(r *http.Request) string {
	p, _ := r.Context().Value(lookupKey{}).(string)
	return p
}

// routeOf returns the matched route template: chi's once routing ran,
// otherwise the one looked up before routing (a request refused in the
// root chain), otherwise "".
func routeOf(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return lookedUpRoute(r)
}

var probeMethods = [...]string{ //nolint:gochecknoglobals // read-only method list
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// allowedMethods computes the Allow header, which chi only sets in its
// default 405 handler.
func allowedMethods(routes chi.Routes, path string) string {
	var allowed []string
	for _, m := range probeMethods {
		if routes.Match(chi.NewRouteContext(), m, path) {
			allowed = append(allowed, m)
		}
	}
	return strings.Join(allowed, ", ")
}
