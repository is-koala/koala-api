package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server timeouts. They cover request/response routes; net/http clears the
// connection deadlines when a Stream route hijacks the connection, and the
// stream's owner sets its own (ADR-0011).
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
)

// RequestTimeout bounds every request's context (Timeout middleware). It is
// shorter than the server's write timeout so handlers can still write an
// error before net/http closes the connection.
const RequestTimeout = writeTimeout - 5*time.Second

// shutdownWriteMargin is the time a request that hit RequestTimeout still
// gets to write its (error) response.
const shutdownWriteMargin = 5 * time.Second

// MinShutdownTimeout is the shortest ShutdownTimeout that never cuts off an
// accepted request early: a connection accepted just before the listener
// closes may take readHeaderTimeout to send its headers, then runs for up
// to RequestTimeout and needs shutdownWriteMargin to write the response.
// config.MinShutdownTimeout must equal it (checked at compile time in
// shutdown_internal_test.go).
const MinShutdownTimeout = readHeaderTimeout + RequestTimeout + shutdownWriteMargin

// ServerOptions configures the shutdown sequence.
type ServerOptions struct {
	// DrainDelay: time between readiness turning 503 and the listener
	// closing.
	DrainDelay time.Duration
	// ShutdownTimeout: time in-flight requests and open streams get to
	// finish. In production it is at least MinShutdownTimeout; config
	// enforces that.
	ShutdownTimeout time.Duration
	// Streams own the hijacked connections of Router.Stream routes.
	// http.Server.Shutdown neither closes nor waits for those, so Serve
	// does it through this interface.
	Streams []Stream
}

// Stream owns long-lived hijacked connections (WebSocket).
type Stream interface {
	// StartDraining refuses new connections; open ones stay up.
	StartDraining()
	// Shutdown closes every open connection with a close frame and waits
	// for its goroutines until ctx ends.
	Shutdown(ctx context.Context) error
}

// Server runs an http.Server with the ordered shutdown of plan §3.10:
//
//  1. ctx is canceled (SIGTERM),
//  2. /readyz reports 503 and streams refuse new connections for
//     DrainDelay,
//  3. the listener closes, streams send close frames, and in-flight
//     requests and streams get ShutdownTimeout,
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
	for _, st := range s.opts.Streams {
		st.StartDraining()
	}
	s.logger.InfoContext(shutdownCtx, "draining", slog.String("drain_delay", s.opts.DrainDelay.String()))
	if s.opts.DrainDelay > 0 {
		t := time.NewTimer(s.opts.DrainDelay)
		<-t.C
	}

	sctx, cancel := context.WithTimeout(shutdownCtx, s.opts.ShutdownTimeout)
	defer cancel()
	streamsDone := make(chan error, 1)
	go func() { streamsDone <- shutdownStreams(sctx, s.opts.Streams) }()
	err := s.srv.Shutdown(sctx)
	streamErr := <-streamsDone
	if err != nil {
		_ = s.srv.Close()
		return fmt.Errorf("http shutdown: %w", errors.Join(err, streamErr))
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	if streamErr != nil {
		return streamErr
	}
	s.logger.InfoContext(shutdownCtx, "http server stopped")
	return nil
}

// shutdownStreams shuts the streams down concurrently and joins their
// errors.
func shutdownStreams(ctx context.Context, streams []Stream) error {
	errs := make([]error, len(streams))
	var wg sync.WaitGroup
	for i, st := range streams {
		wg.Go(func() {
			if err := st.Shutdown(ctx); err != nil {
				errs[i] = fmt.Errorf("stream shutdown: %w", err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
