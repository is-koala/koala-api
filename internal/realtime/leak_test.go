package realtime_test

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/realtime"
)

// leakFrames mark goroutines that belong to the server, the hub, a
// WebSocket or the test's HTTP plumbing. A goroutine whose stack has none
// of them (the runtime's, the testing package's) is not counted.
var leakFrames = []string{
	"github.com/coder/websocket.",
	"github.com/is-koala/koala-api/internal/realtime.",
	// The test's own helpers. Not the whole package: the paused
	// parallel tests' goroutines are realtime_test frames too.
	"github.com/is-koala/koala-api/internal/realtime_test.(*peer)",
	"github.com/is-koala/koala-api/internal/realtime_test.newEnv",
	"github.com/is-koala/koala-api/internal/platform/httpx.",
	"net/http.(*conn).serve",
	"net/http.(*persistConn)",
}

// serverGoroutines returns the stacks of the other goroutines that carry
// one of leakFrames.
func serverGoroutines() []string {
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var found []string
	for i, g := range bytes.Split(buf, []byte("\n\n")) {
		if i == 0 {
			continue // the calling goroutine
		}
		stack := string(g)
		if strings.Contains(stack, "testing.(*T).Parallel(") {
			// A parallel test waiting for the sequential ones to finish
			// owns nothing yet; its frame names the package all the same
			// (the internal tests live in package realtime).
			continue
		}
		for _, f := range leakFrames {
			if strings.Contains(stack, f) {
				found = append(found, stack)
				break
			}
		}
	}
	return found
}

// TestShutdownLeavesNoGoroutines runs connections through traffic and a
// server shutdown, then checks that no server, hub, WebSocket or HTTP
// goroutine is left. It is the only sequential test of the package: the
// parallel tests wait in t.Parallel meanwhile and own no such goroutine
// yet.
//
//nolint:paralleltest // inspects every goroutine of the process; parallel tests would own some
func TestShutdownLeavesNoGoroutines(t *testing.T) {
	if before := serverGoroutines(); len(before) > 0 {
		t.Fatalf("server goroutines before the test:\n%s", strings.Join(before, "\n\n"))
	}

	e := newEnv(t, defaultEnv())
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
	for {
		left := serverGoroutines()
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines left after shutdown:\n%s", len(left), strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
