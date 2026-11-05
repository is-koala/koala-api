package realtime

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// client is one WebSocket connection. Three goroutines serve it: the HTTP
// handler goroutine reads, a writer drains send, and a heartbeat pings.
// Only the writer writes data and close frames.
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
	bucket  tokenBucket
	strikes int
}

// requestClose asks the writer to send a close frame with code and end
// the connection. The first request wins.
func (c *client) requestClose(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closeCode = code
		c.closeReason = reason
		close(c.closing)
	})
}

// enqueue queues msg without blocking. A client whose queue is full is
// too slow to keep up; it is closed with 1013 and backfills over REST
// after reconnecting.
func (c *client) enqueue(msg []byte) {
	select {
	case c.send <- msg:
	default:
		c.requestClose(websocket.StatusTryAgainLater, "slow consumer")
	}
}

// serve upgrades an authenticated handshake and runs the connection until
// it ends. It runs in the HTTP handler goroutine of a Router.Stream route,
// so r's context has no request deadline.
func (h *Hub) serve(w http.ResponseWriter, r *http.Request, p Principal, allowedOrigins []string) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &client{
		id:        rand.Text(),
		principal: p,
		send:      make(chan []byte, h.opts.SendBuffer),
		kill:      cancel,
		closing:   make(chan struct{}),
		bucket:    newTokenBucket(h.opts.CommandRate, h.opts.CommandBurst),
	}
	hello, err := h.encode(EventHello, HelloData{
		ConnectionID:        c.id,
		HeartbeatIntervalMs: h.opts.PingInterval.Milliseconds(),
		MaxMessageBytes:     h.opts.MaxMessageBytes,
	})
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	if err := h.join(c, hello); err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	defer h.leave(c)

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
				c.requestClose(websocket.StatusMessageTooBig, "message too big")
			}
			h.logger.DebugContext(ctx, "realtime read ended",
				slog.String("connection_id", c.id),
				slog.Int("close_status", int(websocket.CloseStatus(err))),
				slog.Any("error", err))
			return
		}
		c.seen.Store(true)
		if typ != websocket.MessageText {
			c.requestClose(websocket.StatusUnsupportedData, "text messages only")
			return
		}
		if !c.bucket.allow(h.clock.Now()) {
			c.strikes++
			if c.strikes >= h.opts.MaxRateStrikes {
				c.requestClose(websocket.StatusPolicyViolation, "rate limit exceeded")
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

// writeLoop writes queued events and, when asked, the close frame.
func (h *Hub) writeLoop(ctx context.Context, ws *websocket.Conn, c *client, done <-chan struct{}) {
	for {
		select {
		case msg := <-c.send:
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
			// message, rate limit), send it; otherwise the connection is
			// already gone.
			select {
			case <-c.closing:
				h.closeConn(ctx, ws, c)
			default:
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

// heartbeat pings every PingInterval and closes the connection when
// nothing, not even a pong, arrived for IdleTimeout.
func (h *Hub) heartbeat(ctx context.Context, ws *websocket.Conn, c *client, done <-chan struct{}) {
	t := time.NewTicker(h.opts.PingInterval)
	defer t.Stop()
	var idle time.Duration
	for {
		select {
		case <-done:
			return
		case <-c.closing:
			return
		case <-t.C:
		}
		if c.seen.Swap(false) {
			idle = 0
		} else {
			idle += h.opts.PingInterval
		}
		if idle >= h.opts.IdleTimeout {
			c.requestClose(websocket.StatusPolicyViolation, "idle timeout")
			return
		}
		// Ping waits for the pong. A missing pong is not an error here:
		// the idle check above closes the connection.
		pctx, cancel := context.WithTimeout(ctx, h.opts.PingInterval)
		_ = ws.Ping(pctx)
		cancel()
	}
}

// tokenBucket limits commands per connection. It takes the time from the
// hub's clock, so tests drive it with a fake clock.
type tokenBucket struct {
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int) tokenBucket {
	return tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

func (b *tokenBucket) allow(now time.Time) bool {
	if !b.last.IsZero() {
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens = min(b.burst, b.tokens+elapsed.Seconds()*b.rate)
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
