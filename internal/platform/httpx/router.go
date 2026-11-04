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

// NewRouter returns the root router with the platform middleware chain
// and health endpoints. Middleware order is outermost first:
//
//	RequestID → SecurityHeaders → AccessLog → Recover → Timeout → BodyLimit → route
//
// so the access log sees the request id and the 500 written by Recover,
// and every response, including 404/405 problems, carries the security
// headers. Modules mount their routes on the returned router.
func NewRouter(logger *slog.Logger, health *Health) *chi.Mux {
	r := chi.NewRouter()
	r.Use(
		RequestID,
		SecurityHeaders,
		AccessLog(logger),
		Recover(logger),
		Timeout(RequestTimeout),
		BodyLimit(DefaultBodyLimit),
	)

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		WriteProblem(w, logger, req, NewProblem(req, http.StatusNotFound, CodeRouteNotFound))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		if allow := allowedMethods(r, req.URL.Path); allow != "" {
			w.Header().Set("Allow", allow)
		}
		WriteProblem(w, logger, req, NewProblem(req, http.StatusMethodNotAllowed, CodeMethodNotAllowed))
	})

	r.Get(PathLivez, health.Live)
	r.Head(PathLivez, health.Live)
	r.Get(PathReadyz, health.Ready)
	r.Head(PathReadyz, health.Ready)
	return r
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
