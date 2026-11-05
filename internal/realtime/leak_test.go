package realtime_test

import (
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/realtime"
)

// TestShutdownLeavesNoGoroutines runs connections through traffic and a
// server shutdown, then checks that the process is back to its goroutine
// count. It is the only sequential test of the package: while it runs,
// the parallel tests wait in t.Parallel, so the count is stable.
//
//nolint:paralleltest // counts the process's goroutines; parallel tests would skew the count
func TestShutdownLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	e := newEnv(t, testOptions(), 0)
	a := e.connect(t, alice, "phone")
	b := e.connect(t, bob, "phone")
	b2 := e.connect(t, bob, "tablet")
	a.send(t, typing(convAB, "leak"))
	for _, p := range []*peer{b, b2} {
		if ev := p.next(t); ev.Type != realtime.EventTyping {
			t.Fatalf("got %s, want %s", ev.Type, realtime.EventTyping)
		}
	}

	if err := e.shutdown(); err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
	for _, p := range []*peer{a, b, b2} {
		if got := p.closeStatus(t); got != websocket.StatusGoingAway {
			t.Fatalf("close status %d, want %d", got, websocket.StatusGoingAway)
		}
	}

	deadline := time.Now().Add(waitTimeout)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines before, %d after shutdown:\n%s", before, runtime.NumGoroutine(), buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
