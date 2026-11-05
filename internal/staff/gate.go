package staff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/audit"
	"github.com/is-koala/koala-api/internal/audit/auditdb"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// checker is the staff permission decision (authzsql.StaffChecker).
type checker interface {
	Check(ctx context.Context, s authz.Subject, a authz.Action, o authz.Object, opts ...authz.CheckOption) (authz.Decision, error)
}

// recorder writes the audit trail (audit.Recorder).
type recorder interface {
	Pending(ctx context.Context, q auditdb.DBTX, e audit.Entry) (uuid.UUID, error)
	Result(ctx context.Context, q auditdb.DBTX, pending uuid.UUID, e audit.Entry, succeeded bool) error
	Record(ctx context.Context, q auditdb.DBTX, e audit.Entry, o audit.Outcome) (uuid.UUID, error)
}

// route is how the gate treats one operation.
type route struct {
	operationID string
	// guarded: a staff operation with x-authz-permission; the rest pass
	// (public, marketplace and self-service operations).
	guarded    bool
	permission authz.PermissionInfo
}

// Gate is the x-authz-permission gate of the staff surface (plan §5.5,
// ADR-0014 Karar 5, ADR-0026). It runs after authentication and before
// request validation.
//
// It is fail-closed twice over. At startup every adminBearer operation
// must carry exactly one of x-authz-permission (a known permission) or
// x-authz-exempt (the self-service allow list of the lint); otherwise the
// server does not start. At request time a route the gate does not know
// is a 500, never a pass.
//
// A refusal is 403 STAFF_PERMISSION_DENIED and a DENIED audit row. An
// allowed mutation (any method but GET, HEAD, OPTIONS) gets its PENDING
// audit row before the handler runs, or does not run, and its result row
// after; an allowed read under a sensitive permission gets one row after
// the handler. The rows are written on writes, their own short
// transactions.
type Gate struct {
	routes  map[string]route
	checker checker
	audit   recorder
	writes  auditdb.DBTX
	logger  *slog.Logger
}

// NewGate reads the authorization extensions of every operation of spec.
func NewGate(spec *openapi3.T, c checker, rec recorder, writes auditdb.DBTX, logger *slog.Logger) (*Gate, error) {
	if spec == nil || spec.Paths == nil {
		return nil, errors.New("staff gate: spec has no paths")
	}
	g := &Gate{routes: map[string]route{}, checker: c, audit: rec, writes: writes, logger: logger}
	paths := spec.Paths.Map()
	keys := make([]string, 0, len(paths))
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	var errs []error
	for _, path := range keys {
		for method, op := range paths[path].Operations() {
			rt, err := routeOf(spec, op)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", method, path, err))
				continue
			}
			g.routes[method+" "+path] = rt
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("staff gate: %w", err)
	}
	return g, nil
}

// routeOf decides how the gate treats op.
func routeOf(spec *openapi3.T, op *openapi3.Operation) (route, error) {
	security := spec.Security
	if op.Security != nil {
		security = *op.Security
	}
	staffBearer := slices.ContainsFunc(security, func(req openapi3.SecurityRequirement) bool {
		_, ok := req[authn.SchemeStaff]
		return ok
	})
	rawPermission, hasPermission := op.Extensions[ExtPermission]
	_, hasExempt := op.Extensions[ExtExempt]
	rt := route{operationID: op.OperationID}
	switch {
	case !staffBearer && hasPermission:
		return rt, fmt.Errorf("%s on an operation without %s", ExtPermission, authn.SchemeStaff)
	case !staffBearer:
		return rt, nil
	case hasPermission == hasExempt:
		return rt, fmt.Errorf("a staff operation carries exactly one of %s and %s (fail-closed, ADR-0026)", ExtPermission, ExtExempt)
	case hasExempt:
		return rt, nil
	}
	code, ok := rawPermission.(string)
	if !ok {
		return rt, fmt.Errorf("%s must be a permission code", ExtPermission)
	}
	info, ok := authz.LookupPermission(code)
	if !ok {
		return rt, fmt.Errorf("%s %q is not a staff permission (internal/authz/permissions.go)", ExtPermission, code)
	}
	rt.guarded, rt.permission = true, info
	return rt, nil
}

// Permission returns the permission x-authz-permission names for the
// operation "METHOD /pattern", or false when it names none.
func (g *Gate) Permission(route string) (authz.Permission, bool) {
	rt, ok := g.routes[route]
	if !ok || !rt.guarded {
		return "", false
	}
	return rt.permission.Code, true
}

type grantKey struct{}

// Grant is the decision the gate admitted a request with.
type Grant struct {
	Permission authz.PermissionInfo
	Basis      authz.Basis
}

// GrantFrom returns the gate's decision for the request; false on an
// operation the gate does not guard.
func GrantFrom(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g, ok
}

func mutating(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// Handler is the middleware.
func (g *Gate) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rctx := chi.RouteContext(ctx)
		if rctx == nil {
			httpx.WriteError(w, r, g.logger, errors.New("staff gate: no chi route context"))
			return
		}
		rt, ok := g.routes[r.Method+" "+rctx.RoutePattern()]
		if !ok {
			httpx.WriteError(w, r, g.logger, fmt.Errorf("staff gate: no operation for %s %s", r.Method, rctx.RoutePattern()))
			return
		}
		if !rt.guarded {
			next.ServeHTTP(w, r)
			return
		}
		p, ok := authn.StaffFrom(ctx)
		if !ok {
			httpx.WriteError(w, r, g.logger, errors.New("staff gate: guarded operation without a staff principal"))
			return
		}
		subject := authz.Staff(p.StaffID)
		entry := audit.Entry{
			Actor: subject, Action: rt.operationID, Permission: rt.permission.Code,
			RequestID: httpx.RequestIDFrom(ctx), Mutating: mutating(r.Method), Sensitivity: rt.permission.Sensitivity,
		}
		d, err := g.checker.Check(ctx, subject, rt.permission.Code.Relation(), authz.Platform())
		if err != nil {
			httpx.WriteError(w, r, g.logger, fmt.Errorf("staff gate: decide %s: %w", rt.permission.Code, err))
			return
		}
		if !d.Allowed {
			if _, err := g.audit.Record(context.WithoutCancel(ctx), g.writes, entry, audit.OutcomeDenied); err != nil {
				g.logger.ErrorContext(ctx, "staff audit write failed", slog.String("outcome", string(audit.OutcomeDenied)), slog.Any("error", err))
			}
			httpx.WriteError(w, r, g.logger, apperr.New(apperr.KindForbidden, CodePermissionDenied))
			return
		}
		entry.Basis = d.Basis
		r = r.WithContext(context.WithValue(ctx, grantKey{}, Grant{Permission: rt.permission, Basis: d.Basis}))
		switch {
		case entry.Mutating:
			pending, err := g.audit.Pending(ctx, g.writes, entry)
			if err != nil {
				// No pending row, no mutation (plan §5.5).
				httpx.WriteError(w, r, g.logger, fmt.Errorf("staff gate: %w", err))
				return
			}
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if err := g.audit.Result(context.WithoutCancel(ctx), g.writes, pending, entry, sw.succeeded()); err != nil {
				g.logger.ErrorContext(ctx, "staff audit write failed", slog.String("outcome", "result"), slog.Any("error", err))
			}
		case entry.Sensitivity != authz.SensitivityNormal:
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			o := audit.OutcomeFailed
			if sw.succeeded() {
				o = audit.OutcomeSucceeded
			}
			if _, err := g.audit.Record(context.WithoutCancel(ctx), g.writes, entry, o); err != nil {
				g.logger.ErrorContext(ctx, "staff audit write failed", slog.String("outcome", string(o)), slog.Any("error", err))
			}
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// statusWriter remembers the response status for the audit row.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	if err != nil {
		return n, fmt.Errorf("staff gate: write response: %w", err)
	}
	return n, nil
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) succeeded() bool { return s.status == 0 || s.status < http.StatusBadRequest }
