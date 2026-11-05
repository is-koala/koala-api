package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// Security schemes of the contract (api/openapi.yaml). An operation names
// at most one; `security: []` is public.
const (
	SchemeMarketplace = "marketplaceBearer"
	SchemeStaff       = "adminBearer"
	// schemeRealtimeTicket is the WebSocket ticket; internal/realtime
	// checks it on its own route, outside this middleware.
	schemeRealtimeTicket = "realtimeTicket"
)

// passwordChangeOpen are the staff operations a member who must change the
// password still reaches (AUTH-014): the change itself and logout.
func passwordChangeOpen(route string) bool {
	return route == "POST /v1/admin/auth/password" || route == "POST /v1/admin/auth/logout"
}

type (
	userKey  struct{}
	staffKey struct{}
)

// UserFrom returns the authenticated marketplace caller.
func UserFrom(ctx context.Context) (UserPrincipal, bool) {
	p, ok := ctx.Value(userKey{}).(UserPrincipal)
	return p, ok
}

// StaffFrom returns the authenticated staff caller.
func StaffFrom(ctx context.Context) (StaffPrincipal, bool) {
	p, ok := ctx.Value(staffKey{}).(StaffPrincipal)
	return p, ok
}

// Middleware authenticates the API group (ADR-0019: before validation, so
// an unauthenticated caller learns nothing about the request shape). Each
// route gets the scheme its operation names in the contract; a route the
// contract does not know is refused, never guessed at (AUTH-015).
type Middleware struct {
	users  *Service
	staff  *StaffService
	routes map[string]string // "METHOD /pattern" → scheme, "" public
	logger *slog.Logger
}

// NewMiddleware reads every operation's security from spec.
func NewMiddleware(spec *openapi3.T, users *Service, staff *StaffService, logger *slog.Logger) (*Middleware, error) {
	if spec == nil || spec.Paths == nil {
		return nil, errors.New("authn middleware: spec has no paths")
	}
	routes := map[string]string{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			reqs := spec.Security
			if op.Security != nil {
				reqs = *op.Security
			}
			scheme, err := schemeOf(reqs)
			if err != nil {
				return nil, fmt.Errorf("authn middleware: %s %s: %w", method, path, err)
			}
			routes[method+" "+path] = scheme
		}
	}
	return &Middleware{users: users, staff: staff, routes: routes, logger: logger}, nil
}

func schemeOf(reqs openapi3.SecurityRequirements) (string, error) {
	switch len(reqs) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", errors.New("alternative security requirements are not supported")
	}
	if len(reqs[0]) != 1 {
		return "", errors.New("a security requirement must name exactly one scheme")
	}
	var name string
	for n := range reqs[0] {
		name = n
	}
	switch name {
	case SchemeMarketplace, SchemeStaff, schemeRealtimeTicket:
		return name, nil
	default:
		return "", fmt.Errorf("unknown security scheme %q", name)
	}
}

// Handler authenticates the request by its route's scheme and puts the
// principal into the context, named for the per-identity rate limit.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())
		if rctx == nil {
			httpx.WriteError(w, r, m.logger, errors.New("authn: no chi route context"))
			return
		}
		route := r.Method + " " + rctx.RoutePattern()
		scheme, ok := m.routes[route]
		if !ok {
			// A mounted route without an operation is a wiring bug.
			httpx.WriteError(w, r, m.logger, fmt.Errorf("authn: no operation for %s", route))
			return
		}
		ctx := r.Context()
		switch scheme {
		case "":
		case SchemeMarketplace:
			p, err := m.users.Authenticate(ctx, BearerToken(r), httpx.DeviceIDFrom(ctx))
			if err != nil {
				m.refuse(w, r, err)
				return
			}
			ctx = context.WithValue(ctx, userKey{}, p)
			ctx = httpx.WithRateLimitIdentity(ctx, "user:"+p.UserID.String())
		case SchemeStaff:
			p, err := m.staff.Authenticate(ctx, BearerToken(r))
			if err != nil {
				m.refuse(w, r, err)
				return
			}
			if p.MustChangePassword && !passwordChangeOpen(route) {
				m.refuse(w, r, apperr.New(apperr.KindForbidden, CodeStaffPasswordChangeRequired))
				return
			}
			ctx = context.WithValue(ctx, staffKey{}, p)
			ctx = httpx.WithRateLimitIdentity(ctx, "staff:"+p.StaffID.String())
		default:
			httpx.WriteError(w, r, m.logger, fmt.Errorf("authn: %s is not served by this middleware", scheme))
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// refuse writes the authentication error; a 401 carries the RFC 6750
// challenge.
func (m *Middleware) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.StatusOf(apperr.From(err).Kind) == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="koala"`)
	}
	httpx.WriteError(w, r, m.logger, err)
}

// BearerToken returns the token of "Authorization: Bearer <token>" (the
// scheme is case-insensitive, RFC 9110 §11.1), or "".
func BearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
