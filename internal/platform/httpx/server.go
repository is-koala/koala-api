package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server timeouts. Streaming endpoints (WebSocket, Faz 4) will need their
// own write deadline handling.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
)

// RequestTimeout bounds every request's context (Timeout middleware). It is
// shorter than the server's write timeout so handlers can still write an
// error before net/http closes the connection. The shutdown timeout must be
// at least this long (config.MinShutdownTimeout), so a request accepted
// during the drain is not force-closed before its own deadline.
const RequestTimeout = writeTimeout - 5*time.Second

// ServerOptions configures the shutdown sequence.
type ServerOptions struct {
	// DrainDelay: time between readiness turning 503 and the listener
	// closing.
	DrainDelay time.Duration
	// ShutdownTimeout: time in-flight requests get to finish. In
	// production it is at least RequestTimeout; config enforces that.
	ShutdownTimeout time.Duration
}

// Server runs an http.Server with the ordered shutdown of plan §3.10:
//
//  1. ctx is canceled (SIGTERM),
//  2. /readyz reports 503 for DrainDelay,
//  3. the listener closes and in-flight requests get ShutdownTimeout,
//  4. Serve returns; the caller then stops workers and closes pools.
type Server struct {
	srv    *http.Server
	health *Health
	logger *slog.Logger
	opts   ServerOptions
}

// NewServer returns a server for handler.
func NewServer(handler http.Handler, health *Health, logger *slog.Logger, opts ServerOptions) *Server {
	return &Server{
		srv: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
		health: health,
		logger: logger,
		opts:   opts,
	}
}

// Serve serves on ln until ctx is canceled, then shuts down in order.
// It returns nil after a clean shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Request contexts must not be canceled by the shutdown signal;
	// in-flight requests finish during the drain.
	s.srv.BaseContext = func(net.Listener) context.Context { return context.WithoutCancel(ctx) }

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.srv.Serve(ln) }()
	s.logger.InfoContext(ctx, "http server listening", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx := context.WithoutCancel(ctx)
	s.health.StartDraining()
	s.logger.InfoContext(shutdownCtx, "draining", slog.String("drain_delay", s.opts.DrainDelay.String()))
	if s.opts.DrainDelay > 0 {
		t := time.NewTimer(s.opts.DrainDelay)
		<-t.C
	}

	sctx, cancel := context.WithTimeout(shutdownCtx, s.opts.ShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(sctx); err != nil {
		_ = s.srv.Close()
		return fmt.Errorf("http shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	s.logger.InfoContext(shutdownCtx, "http server stopped")
	return nil
}
