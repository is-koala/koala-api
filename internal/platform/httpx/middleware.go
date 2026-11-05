package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
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

// AccessLog logs one line per request when it completes, also when the
// response is aborted by a panic. The route template is logged; the raw path
// only for unmatched requests (404), and never the query string, which may
// carry personal data. Health probes are logged at debug level.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			defer finishAccessLog(logger, ww, r, time.Now())
			next.ServeHTTP(ww, r)
		})
	}
}

// finishAccessLog must be deferred directly so recover() sees an aborting
// panic; it logs the line and re-raises the panic.
func finishAccessLog(logger *slog.Logger, ww middleware.WrapResponseWriter, r *http.Request, start time.Time) {
	aborted := recover()

	status := ww.Status()
	if status == 0 && aborted == nil {
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
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("route", route),
		slog.Int("status", status),
		slog.Int("bytes", ww.BytesWritten()),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if route == "" {
		attrs = append(attrs, slog.String("path", r.URL.Path))
	}
	if aborted != nil {
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	logger.LogAttrs(r.Context(), level, "http request", attrs...)

	if aborted != nil {
		panic(aborted)
	}
}

// Recover turns a handler panic into a logged 500 INTERNAL problem. If the
// handler had already sent the status line (WriteHeader, Write or Flush), a
// 500 can no longer be sent: the panic is logged and the response is aborted
// with http.ErrAbortHandler so the client sees a broken response instead of
// a truncated success. http.ErrAbortHandler itself is re-raised untouched.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hw := &headerWatcher{ResponseWriter: w}
			defer recoverPanic(hw, r, logger)
			next.ServeHTTP(hw, r)
		})
	}
}

// recoverPanic must be deferred directly so recover() sees the panic.
func recoverPanic(hw *headerWatcher, r *http.Request, logger *slog.Logger) {
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
	if hw.wroteHeader {
		panic(http.ErrAbortHandler)
	}
	WriteProblem(hw, logger, r, NewProblem(r, http.StatusInternalServerError, apperr.CodeInternal))
}

// headerWatcher records whether the response header has been sent. A status
// code alone is not enough: Flush, ReadFrom and Hijack commit the response
// without a WriteHeader call, and 1xx informational headers do not commit
// it. Errors from the underlying writer pass through unchanged (wrapcheck
// exempts these signatures in .golangci.yml).
type headerWatcher struct {
	http.ResponseWriter
	wroteHeader bool
}

func (hw *headerWatcher) WriteHeader(code int) {
	if code >= http.StatusOK || code == http.StatusSwitchingProtocols {
		hw.wroteHeader = true
	}
	hw.ResponseWriter.WriteHeader(code)
}

func (hw *headerWatcher) Write(b []byte) (int, error) {
	hw.wroteHeader = true
	return hw.ResponseWriter.Write(b)
}

// ReadFrom implements io.ReaderFrom so io.Copy keeps the sendfile path of
// the underlying writer.
func (hw *headerWatcher) ReadFrom(r io.Reader) (int64, error) {
	hw.wroteHeader = true
	if rf, ok := hw.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	// Hide this method from io.Copy so it does not call ReadFrom again.
	return io.Copy(writerOnly{hw}, r) //nolint:wrapcheck // ResponseWriter contract: the copy error passes through unchanged
}

type writerOnly struct{ io.Writer }

// FlushError flushes the underlying writer; http.ResponseController uses
// it. Flushing sends the header (status 200 if none was set).
func (hw *headerWatcher) FlushError() error {
	hw.wroteHeader = true
	return http.NewResponseController(hw.ResponseWriter).Flush()
}

// Flush implements http.Flusher, which has no error return.
func (hw *headerWatcher) Flush() { _ = hw.FlushError() }

// Hijack implements http.Hijacker. After a successful hijack the
// connection belongs to the handler; a failed one leaves the response
// untouched.
func (hw *headerWatcher) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(hw.ResponseWriter).Hijack()
	if err == nil {
		hw.wroteHeader = true
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (hw *headerWatcher) Unwrap() http.ResponseWriter { return hw.ResponseWriter }

// SecurityHeaders sets headers every response carries: no MIME sniffing and
// no framing (clickjacking). Proposal of this PR (standards review).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// Timeout bounds each request's context to d. NewRouter applies
// RequestTimeout to every route except Router.Stream routes; it is shorter
// than the server's WriteTimeout so that handlers and database calls give
// up before net/http cuts the connection.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// DefaultBodyLimit is the request body cap NewRouter applies to every route
// except Router.Stream routes.
const DefaultBodyLimit int64 = 1 << 20 // 1 MiB

type originalBodyKey struct{}

// BodyLimit caps the request body at n bytes. NewRouter applies
// DefaultBodyLimit; a route that needs more raises it with
// r.With(httpx.BodyLimit(n)). The limit always wraps the original
// body, so a route-level limit replaces the default instead of nesting
// inside it.
//
// Reading past n fails with *http.MaxBytesError, which WriteError maps to
// 413 REQUEST_BODY_TOO_LARGE; handlers return the decode error as is. The
// check is on read, not on Content-Length: the default and a route's raised
// limit are both in the chain, and only the innermost one may decide, which
// the shared original body makes possible.
func BodyLimit(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			orig, ok := ctx.Value(originalBodyKey{}).(io.ReadCloser)
			if !ok {
				orig = r.Body
				ctx = context.WithValue(ctx, originalBodyKey{}, orig)
			}
			r2 := r.WithContext(ctx)
			r2.Body = http.MaxBytesReader(w, orig, n)
			next.ServeHTTP(w, r2)
		})
	}
}
