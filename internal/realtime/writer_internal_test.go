package realtime

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// TestWriterSendsNothingAfterCloseRequest proves the writer's isClosing
// checks: a connection whose queue is full and whose close was requested
// before the writer ran (a revoked session of a client that was not
// reading) gets the close frame and none of the queued events. Without the
// checks the writer's select would pick a queued event about half of the
// time; 20 rounds make a false pass vanishingly unlikely.
func TestWriterSendsNothingAfterCloseRequest(t *testing.T) {
	t.Parallel()

	opts := DefaultOptions()
	opts.SendBuffer = 8
	h, err := NewHub(nil, nil, nil, slog.New(slog.DiscardHandler), opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		c := &client{id: "test", send: make(chan []byte, opts.SendBuffer), kill: cancel, closing: make(chan struct{})}
		for range opts.SendBuffer {
			if !c.enqueue([]byte(`{"type":"queued"}`)) {
				t.Error("queue full too early")
			}
		}
		c.requestClose(CloseSessionRevoked, "session revoked")
		h.writeLoop(ctx, ws, c, make(chan struct{}))
		_ = ws.CloseNow()
	}))
	t.Cleanup(srv.Close)

	for round := range 20 {
		conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		var events int
		for {
			_, _, err := conn.Read(t.Context())
			if err != nil {
				if got := websocket.CloseStatus(err); got != CloseSessionRevoked {
					t.Fatalf("round %d: close status %d (%v), want %d", round, got, err, CloseSessionRevoked)
				}
				break
			}
			events++
		}
		if events != 0 {
			t.Fatalf("round %d: %d queued events written after the close request", round, events)
		}
	}
}
