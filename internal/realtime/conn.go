package realtime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// client is one WebSocket connection. Three goroutines serve it: the HTTP
// handler goroutine reads, a writer drains send, and a heartbeat pings and
// re-checks the session. Only the writer writes data and close frames.
type client struct {
	id        string
	principal Principal
	send      chan []byte
	// kill cancels the connection context; the library then closes the
	// socket without a close frame. Used when a graceful close is not
	// possible (write failure, shutdown deadline).
	kill context.CancelFunc

	closing     chan struct{}
	closeOnce   sync.Once
	closeCode   websocket.StatusCode // set before closing is closed
	closeReason string

	// seen is set by every inbound message and pong; heartbeat resets it.
	seen atomic.Bool

	// Read loop only.
	limiter *rate.Limiter
	strikes int
}

// requestClose asks the writer to send a close frame with code and end
// the connection. The first request wins. Callers go through
// Hub.closeClient, which also stops the fanout to c.
func (c *client) requestClose(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closeCode = code
		c.closeReason = reason
		close(c.closing)
	})
}

func (c *client) isClosing() bool {
	select {
	case <-c.closing:
		return true
	default:
		return false
	}
}

// enqueue queues msg without blocking and reports whether it fit.
func (c *client) enqueue(msg []byte) bool {
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

// serve joins, re-checks the session, upgrades and runs the connection
// until it ends. It runs in the HTTP handler goroutine of a Router.Stream
// route, so r's context has no request deadline.
func (h *Hub) serve(w http.ResponseWriter, r *http.Request, p Principal, allowedOrigins []string) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &client{
		id:        rand.Text(),
		principal: p,
		send:      make(chan []byte, h.opts.SendBuffer),
		kill:      cancel,
		closing:   make(chan struct{}),
		limiter:   rate.NewLimiter(rate.Limit(h.opts.CommandRate), h.opts.CommandBurst),
	}
	hello, err := h.encode(EventHello, HelloData{
		ConnectionID:        c.id,
		HeartbeatIntervalMs: h.opts.PingInterval.Milliseconds(),
		MaxMessageBytes:     h.opts.MaxMessageBytes,
	})
	if err != nil {
		writeHandshakeError(w, r, h.logger, err)
		return
	}
	if err := h.join(c, hello); err != nil {
		writeHandshakeError(w, r, h.logger, err)
		return
	}
	defer h.leave(c)

	// A revocation between Redeem and join found no connection to close;
	// checking once more after join closes that window.
	if err := h.checkSession(ctx, p); err != nil {
		writeHandshakeError(w, r, h.logger, err)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{Subprotocol},
		OriginPatterns: allowedOrigins,
		OnPongReceived: func(context.Context, []byte) { c.seen.Store(true) },
	})
	if err != nil {
		// Accept has written the HTTP error response.
		h.logger.InfoContext(ctx, "realtime upgrade failed", slog.Any("error", err))
		return
	}
	ws.SetReadLimit(h.opts.MaxMessageBytes)
	h.logger.InfoContext(ctx, "realtime connected", slog.String("connection_id", c.id))
	h.run(ctx, ws, c)
	h.logger.InfoContext(ctx, "realtime disconnected", slog.String("connection_id", c.id))
}

// checkSession reports a session that is no longer active as 401
// REALTIME_TICKET_INVALID, within HandshakeTimeout.
func (h *Hub) checkSession(ctx context.Context, p Principal) error {
	sctx, cancel := context.WithTimeout(ctx, h.opts.HandshakeTimeout)
	defer cancel()
	active, err := h.sessions.Active(sctx, p)
	if err != nil {
		return fmt.Errorf("realtime: check session: %w", err)
	}
	if !active {
		return apperr.New(apperr.KindUnauthenticated, CodeTicketInvalid)
	}
	return nil
}

// run serves an accepted connection and returns when all its goroutines
// have stopped.
func (h *Hub) run(ctx context.Context, ws *websocket.Conn, c *client) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { h.writeLoop(ctx, ws, c, done) })
	wg.Go(func() { h.heartbeat(ctx, ws, c, done) })
	h.readLoop(ctx, ws, c)
	close(done)
	wg.Wait()
	_ = ws.CloseNow() // no-op when the writer already closed it
}

// readLoop reads client commands until the connection ends.
func (h *Hub) readLoop(ctx context.Context, ws *websocket.Conn, c *client) {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				// The library has sent the 1009 close frame. Let the
				// writer finish the handshake, which also reads past the
				// rest of the oversized message; closing the socket with
				// unread input would reset the connection before the
				// client sees the close frame.
				h.closeClient(c, websocket.StatusMessageTooBig, "message too big")
			}
			h.logger.DebugContext(ctx, "realtime read ended",
				slog.String("connection_id", c.id),
				slog.Int("close_status", int(websocket.CloseStatus(err))),
				slog.Any("error", err))
			return
		}
		c.seen.Store(true)
		if typ != websocket.MessageText {
			h.closeClient(c, websocket.StatusUnsupportedData, "text messages only")
			return
		}
		if !c.limiter.AllowN(h.clock.Now(), 1) {
			c.strikes++
			if c.strikes >= h.opts.MaxRateStrikes {
				h.closeClient(c, websocket.StatusPolicyViolation, "rate limit exceeded")
				return
			}
			if c.strikes == 1 {
				h.sendError(ctx, c, "", apperr.New(apperr.KindRateLimited, CodeRateLimited))
			}
			continue
		}
		c.strikes = 0
		h.dispatch(ctx, c, data)
	}
}

// writeLoop writes queued events and, when asked, the close frame. A close
// request wins over queued events: nothing is written to a connection
// after it was asked to close, for example after its session was revoked.
func (h *Hub) writeLoop(ctx context.Context, ws *websocket.Conn, c *client, done <-chan struct{}) {
	for {
		if c.isClosing() {
			h.closeConn(ctx, ws, c)
			return
		}
		select {
		case msg := <-c.send:
			if c.isClosing() {
				h.closeConn(ctx, ws, c)
				return
			}
			wctx, cancel := context.WithTimeout(ctx, h.opts.WriteTimeout)
			err := ws.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				h.logger.DebugContext(ctx, "realtime write failed",
					slog.String("connection_id", c.id),
					slog.Any("error", err))
				c.kill()
				return
			}
		case <-c.closing:
			h.closeConn(ctx, ws, c)
			return
		case <-done:
			// The reader stopped. If it asked for a close frame (binary
			// message, rate limit, oversized message), send it; otherwise
			// the connection is already gone.
			if c.isClosing() {
				h.closeConn(ctx, ws, c)
			}
			return
		}
	}
}

// closeConn performs the close handshake with the requested code. Queued
// events are dropped; clients backfill over REST (ADR-0011).
func (h *Hub) closeConn(ctx context.Context, ws *websocket.Conn, c *client) {
	if err := ws.Close(c.closeCode, c.closeReason); err != nil {
		h.logger.DebugContext(ctx, "realtime close handshake failed",
			slog.String("connection_id", c.id),
			slog.Any("error", err))
	}
	// Close has closed the socket; this ends a read that is still blocked.
	c.kill()
}

// heartbeat pings every PingInterval, closes the connection when nothing,
// not even a pong, arrived for IdleTimeout, and re-checks the session
// every SessionCheckInterval.
func (h *Hub) heartbeat(ctx context.Context, ws *websocket.Conn, c *client, done <-chan struct{}) {
	ping := time.NewTicker(h.opts.PingInterval)
	defer ping.Stop()
	session := time.NewTicker(h.opts.SessionCheckInterval)
	defer session.Stop()
	var (
		idle     time.Duration
		failures int
	)
	for {
		select {
		case <-done:
			return
		case <-c.closing:
			return
		case <-session.C:
			failures = h.recheckSession(ctx, c, failures)
			if c.isClosing() {
				return
			}
			continue
		case <-ping.C:
		}
		if c.seen.Swap(false) {
			idle = 0
		} else {
			idle += h.opts.PingInterval
		}
		if idle >= h.opts.IdleTimeout {
			h.closeClient(c, websocket.StatusPolicyViolation, "idle timeout")
			return
		}
		// Ping waits for the pong. A missing pong is not an error here:
		// the idle check above closes the connection.
		pctx, cancel := context.WithTimeout(ctx, h.opts.PingInterval)
		_ = ws.Ping(pctx)
		cancel()
	}
}

// recheckSession closes c with CloseSessionRevoked when its session ended
// and returns the number of consecutive failed checks: 0 after a
// successful check, failures+1 after a failed one. A failed lookup is
// logged at WARN and, below MaxSessionCheckFailures, keeps the connection:
// a database blip must not disconnect every client at once. The failure
// that reaches MaxSessionCheckFailures closes the connection with 1013
// (try again later); the client reconnects, and a new ticket needs an
// active session (ADR-0022).
func (h *Hub) recheckSession(ctx context.Context, c *client, failures int) int {
	err := h.checkSession(ctx, c.principal)
	var ae *apperr.Error
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ae) && ae.Kind == apperr.KindUnauthenticated:
		h.closeClient(c, CloseSessionRevoked, "session revoked")
		return 0
	}
	failures++
	h.logger.WarnContext(ctx, "realtime session check failed",
		slog.String("connection_id", c.id),
		slog.Int("consecutive_failures", failures),
		slog.Any("error", err))
	if failures >= h.opts.MaxSessionCheckFailures {
		h.closeClient(c, websocket.StatusTryAgainLater, "session check unavailable")
	}
	return failures
}
