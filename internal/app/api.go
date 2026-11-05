package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/meta"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// mountAPI mounts the generated /v1 operations on router. Each request is
// validated against the embedded contract before the generated handler
// binds it; every error leaves as a problem document (ADR-0007, ADR-0019).
//
// Faz 1 puts authentication in front of validation on this group, so an
// unauthenticated caller learns nothing about the request shape.
func mountAPI(ctx context.Context, router chi.Router, logger *slog.Logger, env config.Env) error {
	spec, err := api.Load(ctx)
	if err != nil {
		return fmt.Errorf("mount api: %w", err)
	}
	validate, err := httpx.ValidateRequests(spec, logger)
	if err != nil {
		return fmt.Errorf("mount api: %w", err)
	}

	// Modules implement their part of the strict interface; wire<Module>
	// functions compose them here as they land.
	strict := apiv1.NewStrictHandlerWithOptions(meta.NewHandler(), nil, apiv1.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
			// The validator has accepted the body, so binding its validated
			// value failed (a Go type narrower than the schema). The body
			// cannot be read again: there is no second, unvalidated decode.
			httpx.WriteError(w, r, logger, apperr.New(apperr.KindInvalid, httpx.CodeBodyMalformed))
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteError(w, r, logger, err)
		},
	})
	router.Group(func(g chi.Router) {
		g.Use(validate)
		apiv1.HandlerWithOptions(strict, apiv1.ChiServerOptions{
			BaseRouter: skipRoutes{Router: g, skip: devOnlyPaths(env)},
			ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
				// Parameters were validated before binding; this is a
				// binding gap, reported without the binder's message.
				httpx.WriteError(w, r, logger, apperr.New(apperr.KindInvalid, httpx.CodeRequestInvalid))
			},
		})
	})
	return nil
}

// devOnlyPaths are the operations not mounted in env. In production they
// are not registered at all, so they 404 like any unknown path (and do not
// 405 for another method).
func devOnlyPaths(env config.Env) map[string]bool {
	if env != config.EnvProduction {
		return nil
	}
	return map[string]bool{
		"/v1/meta/ping": true, // contract smoke test; removed with the first real operation
	}
}

// skipRoutes is the router the generated code registers its operations on,
// minus the skipped path templates. The generated HandlerWithOptions puts
// each operation in its own Group and registers it with the method's
// function, so those are the calls it intercepts.
type skipRoutes struct {
	chi.Router
	skip map[string]bool
}

func (s skipRoutes) Group(fn func(chi.Router)) chi.Router {
	return s.Router.Group(func(r chi.Router) { fn(skipRoutes{Router: r, skip: s.skip}) })
}

func (s skipRoutes) Get(p string, h http.HandlerFunc)     { s.handle(http.MethodGet, p, h) }
func (s skipRoutes) Head(p string, h http.HandlerFunc)    { s.handle(http.MethodHead, p, h) }
func (s skipRoutes) Post(p string, h http.HandlerFunc)    { s.handle(http.MethodPost, p, h) }
func (s skipRoutes) Put(p string, h http.HandlerFunc)     { s.handle(http.MethodPut, p, h) }
func (s skipRoutes) Patch(p string, h http.HandlerFunc)   { s.handle(http.MethodPatch, p, h) }
func (s skipRoutes) Delete(p string, h http.HandlerFunc)  { s.handle(http.MethodDelete, p, h) }
func (s skipRoutes) Options(p string, h http.HandlerFunc) { s.handle(http.MethodOptions, p, h) }
func (s skipRoutes) Trace(p string, h http.HandlerFunc)   { s.handle(http.MethodTrace, p, h) }

func (s skipRoutes) handle(method, pattern string, h http.HandlerFunc) {
	if !s.skip[pattern] {
		s.Router.MethodFunc(method, pattern, h)
	}
}
