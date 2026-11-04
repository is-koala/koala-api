package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/log"
)

// HeaderRequestID carries the request id in both directions (plan §4.1-7).
const HeaderRequestID = "X-Request-Id"

const maxRequestIDLen = 64

type requestIDKey struct{}

// RequestIDFrom returns the request id stored by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID accepts a well-formed incoming X-Request-Id (so mobile and
// proxy logs correlate) or generates one, echoes it in the response and puts
// it into the context for handlers and logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		ctx = log.WithAttrs(ctx, slog.String("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validRequestID allows only characters that cannot forge log lines or
// headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range []byte(id) {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return hex.EncodeToString(b[:])
}

// AccessLog logs one line per request after it completes. Only the path is
// logged, never the query string, which may carry personal data. Health
// probes are logged at debug level.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			route := ""
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				route = rctx.RoutePattern()
			}
			level := slog.LevelInfo
			if route == PathLivez || route == PathReadyz {
				// Docker and the proxy probe these every few seconds.
				level = slog.LevelDebug
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()))
		})
	}
}

// Recover turns a handler panic into a logged 500 INTERNAL problem.
// http.ErrAbortHandler is re-raised so net/http aborts the response as
// intended.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			defer recoverPanic(ww, r, logger)
			next.ServeHTTP(ww, r)
		})
	}
}

// recoverPanic must be deferred directly so recover() sees the panic.
func recoverPanic(ww middleware.WrapResponseWriter, r *http.Request, logger *slog.Logger) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v)
	}
	logger.ErrorContext(r.Context(), "panic",
		slog.Any("panic", v),
		slog.String("stack", string(debug.Stack())))
	if ww.Status() == 0 {
		WriteProblem(ww, logger, r, NewProblem(r, http.StatusInternalServerError, apperr.CodeInternal))
	}
}
