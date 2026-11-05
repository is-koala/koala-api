// Package httpx is the HTTP platform: router and middleware chain, problem
// documents, health endpoints and the graceful server lifecycle.
package httpx

import (
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
//	root:   RequestID → SecurityHeaders → AccessLog → Recover
//	routes: root + Timeout → BodyLimit → route
//	stream: root → route
//
// The access log sees the request id and the 500 written by Recover, and
// every response, including 404/405 problems, carries the security
// headers. Router embeds the bounded routes: modules mount on it with the
// usual chi methods (Get, Post, Mount, With, ...) and get Timeout and
// BodyLimit. Only Stream routes, long-lived connections that manage their
// own deadlines (WebSocket, ADR-0011), skip them. Middleware every request
// needs, streams included, belongs in the root chain here.
type Router struct {
	chi.Router
	root   *chi.Mux
	logger *slog.Logger
}

// NewRouter returns the root router with the platform middleware chain
// and health endpoints.
func NewRouter(logger *slog.Logger, health *Health) *Router {
	root := chi.NewRouter()
	root.Use(
		RequestID,
		SecurityHeaders,
		AccessLog(logger),
		Recover(logger),
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
	return &Router{Router: bounded, root: root, logger: logger}
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
func (rt *Router) Stream(pattern string, h http.HandlerFunc) {
	rt.root.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			WriteProblem(w, rt.logger, req, NewProblem(req, http.StatusMethodNotAllowed, CodeMethodNotAllowed))
			return
		}
		h(w, req)
	})
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
