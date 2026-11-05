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
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// mountAPI mounts the generated /v1 operations on router. Each request is
// validated against the embedded contract before the generated handler
// binds it; every error leaves as a problem document (ADR-0006, ADR-0007).
func mountAPI(ctx context.Context, router chi.Router, logger *slog.Logger) error {
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
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			// Rare: the validator has already parsed the body. A body over
			// its limit still becomes 413 in WriteError.
			httpx.WriteError(w, r, logger, apperr.Wrap(err, apperr.KindInvalid, httpx.CodeBodyMalformed))
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteError(w, r, logger, err)
		},
	})
	router.Group(func(g chi.Router) {
		g.Use(validate)
		apiv1.HandlerWithOptions(strict, apiv1.ChiServerOptions{
			BaseRouter: g,
			ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
				httpx.WriteError(w, r, logger, apperr.Wrap(err, apperr.KindInvalid, httpx.CodeRequestInvalid))
			},
		})
	})
	return nil
}
