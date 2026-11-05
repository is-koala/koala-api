package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// newRouter builds the HTTP router: the edge rules of ADR-0021 from cfg
// and the contract (client headers, trusted proxies, rate limit groups,
// metrics on reg) and the /v1 operations.
func newRouter(ctx context.Context, cfg config.Config, logger *slog.Logger, health *httpx.Health,
	reg prometheus.Registerer, groups map[string]httpx.GroupLimits, mods modules,
) (*httpx.Router, error) {
	spec, err := api.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	routes, err := httpx.RateLimitRoutes(spec)
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	limiter, err := httpx.NewRateLimiter(httpx.RateLimitConfig{Groups: groups, Routes: routes}, reg)
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	m, err := httpx.NewMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	// ExemptRoutes stays empty until the first route for a caller that is
	// not a Koala app (a payment provider's callback, an e-mail tracking
	// pixel) lands; it is listed here with its test. Health endpoints are
	// exempt in httpx.
	var policy httpx.ClientPolicy
	if cfg.MinAppVersion != nil {
		policy.MinAppVersion = *cfg.MinAppVersion
	}
	router, err := httpx.NewRouter(logger, health, httpx.RouterOptions{
		Clients:   policy,
		ClientIPs: httpx.NewClientIPResolver(cfg.TrustedProxies),
		RateLimit: limiter,
		Metrics:   m,
	})
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	if err := mountAPI(spec, router, limiter, logger, mods); err != nil {
		return nil, err
	}
	// The ticket endpoint is a bounded route, the WebSocket a stream
	// route; both authenticate themselves (realtimeAuth, the ticket).
	mods.realtime.Mount(router)
	return router, nil
}

// mountAPI mounts the generated /v1 operations on router. Each request is
// validated against the embedded contract before the generated handler
// binds it; every error leaves as a problem document (ADR-0007, ADR-0019).
//
// Authentication comes first on this group, then the per-identity rate
// limit and validation, so an unauthenticated caller learns nothing about
// the request shape (ADR-0019). Authentication names the caller with
// httpx.WithRateLimitIdentity; a public operation has no identity and only
// the per-address limit of the root chain applies to it. The sign-in,
// refresh and staff password operations also take a token from their
// target's bucket in the auth group (authn.StrictMiddleware, ADR-0021 §5).
func mountAPI(spec *openapi3.T, router chi.Router, limiter *httpx.RateLimiter, logger *slog.Logger, mods modules) error {
	validate, err := httpx.ValidateRequests(spec, logger)
	if err != nil {
		return fmt.Errorf("mount api: %w", err)
	}
	// Modules implement their part of the strict interface; wire<Module>
	// functions compose them here as they land.
	authenticate, err := authn.NewMiddleware(spec, mods.users, mods.staff, logger)
	if err != nil {
		return fmt.Errorf("mount api: %w", err)
	}
	middlewares := []apiv1.StrictMiddlewareFunc{mods.authHTTP.StrictMiddleware(limiter, groupAuth, groupAuthAccount)}
	strict := apiv1.NewStrictHandlerWithOptions(mods.authHTTP, middlewares, apiv1.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: bindError(logger),
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteError(w, r, logger, err)
		},
	})
	router.Group(func(g chi.Router) {
		g.Use(authenticate.Handler, limiter.ByIdentity(logger), validate)
		apiv1.HandlerWithOptions(strict, apiv1.ChiServerOptions{
			BaseRouter:       g,
			ErrorHandlerFunc: bindError(logger),
		})
	})
	return nil
}

// bindError handles a request the generated code could not bind after the
// validator accepted it: a body or parameter whose Go type is narrower than
// its schema. That is a server-side gap, not a client mistake, so it is a
// 500 and logged. The binder's message may quote the value, so only its
// type is logged; the raw body cannot be read again, so there is no
// second, unvalidated decode to fall back on.
func bindError(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		cause := err
		for errors.Unwrap(cause) != nil {
			cause = errors.Unwrap(cause)
		}
		httpx.WriteError(w, r, logger, fmt.Errorf("bind validated request: %T (cause %T)", err, cause))
	}
}
