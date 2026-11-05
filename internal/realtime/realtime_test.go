package realtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/realtime"
)

// TestTicketIsSingleUse: a ticket opens one connection; replaying it is
// rejected before the upgrade.
func TestTicketIsSingleUse(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), 0)
	ticket := e.ticket(t, alice, "phone")
	spec := dialSpec{protocols: protocols(ticket), header: http.Header{realtime.HeaderDeviceID: {"phone"}}}

	first := e.dial(t, spec)
	if first.err != nil {
		t.Fatalf("first use: status %d %s: %v", first.status, first.code, first.err)
	}
	if ev := first.peer.next(t); ev.Type != realtime.EventHello {
		t.Fatalf("first event %s, want hello", ev.Type)
	}

	replay := e.dial(t, spec)
	if replay.err == nil || replay.status != http.StatusUnauthorized || replay.code != realtime.CodeTicketInvalid {
		t.Fatalf("replay: status %d %s, err %v; want 401 %s", replay.status, replay.code, replay.err, realtime.CodeTicketInvalid)
	}
}

// TestHandshakeRejections: every rejected handshake is an HTTP problem,
// and no socket is opened.
func TestHandshakeRejections(t *testing.T) {
	t.Parallel()

	phone := http.Header{realtime.HeaderDeviceID: {"phone"}}
	tests := []struct {
		name   string
		spec   func(e *env, ticket string) dialSpec
		status int
		code   apperr.Code
	}{
		{"no ticket", func(*env, string) dialSpec {
			return dialSpec{protocols: []string{realtime.Subprotocol}, header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"unknown ticket", func(*env, string) dialSpec {
			return dialSpec{protocols: protocols(strings.Repeat("A", 43)), header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"malformed ticket", func(*env, string) dialSpec {
			return dialSpec{protocols: protocols("short"), header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"two tickets", func(_ *env, ticket string) dialSpec {
			return dialSpec{protocols: append(protocols(ticket), realtime.TicketProtocolPrefix+ticket), header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"no koala.v1", func(_ *env, ticket string) dialSpec {
			return dialSpec{protocols: []string{realtime.TicketProtocolPrefix + ticket}, header: phone}
		}, http.StatusBadRequest, realtime.CodeProtocolUnsupported},
		{"expired ticket", func(e *env, ticket string) dialSpec {
			e.clock.Advance(realtime.TicketTTL)
			return dialSpec{protocols: protocols(ticket), header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"other device", func(_ *env, ticket string) dialSpec {
			return dialSpec{protocols: protocols(ticket), header: http.Header{realtime.HeaderDeviceID: {"tablet"}}}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"session revoked after issue", func(e *env, ticket string) dialSpec {
			e.sessions.Revoke(sessionOf(alice, "phone"))
			return dialSpec{protocols: protocols(ticket), header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"foreign origin", func(_ *env, ticket string) dialSpec {
			return dialSpec{protocols: protocols(ticket), header: http.Header{"Origin": {"https://evil.example"}}}
		}, http.StatusForbidden, realtime.CodeOriginForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, testOptions(), 0)
			res := e.dial(t, tt.spec(e, e.ticket(t, alice, "phone")))
			if res.err == nil {
				t.Fatal("handshake succeeded")
			}
			if res.status != tt.status || res.code != tt.code {
				t.Fatalf("status %d %s, want %d %s", res.status, res.code, tt.status, tt.code)
			}
		})
	}
}

// TestOriginRejectionKeepsTicket: the origin is checked before the ticket
// is spent, and a browser from an allowed origin connects without a
// device header.
func TestOriginRejectionKeepsTicket(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), 0)
	ticket := e.ticket(t, alice, "phone")
	foreign := e.dial(t, dialSpec{protocols: protocols(ticket), header: http.Header{"Origin": {"https://evil.example"}}})
	if foreign.status != http.StatusForbidden {
		t.Fatalf("foreign origin: status %d, want 403", foreign.status)
	}
	allowed := e.dial(t, dialSpec{protocols: protocols(ticket), header: http.Header{"Origin": {allowedOrigin}}})
	if allowed.err != nil {
		t.Fatalf("allowed origin: status %d %s: %v", allowed.status, allowed.code, allowed.err)
	}
}

func TestPlainRequestsAreRejected(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), 0)
	tests := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, realtime.PathConnect, http.StatusBadRequest}, // no upgrade
		{http.MethodPost, realtime.PathConnect, http.StatusMethodNotAllowed},
		{http.MethodPost, realtime.PathTickets, http.StatusUnauthorized}, // no session
	}
	for _, tt := range tests {
		req, err := http.NewRequestWithContext(t.Context(), tt.method, e.base+tt.path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
		}
	}
}

// TestUnauthorizedEventsAreNotReceived: a connection only gets events the
// server addressed to its user; membership comes from Conversations.
func TestUnauthorizedEventsAreNotReceived(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), 0)
	a := e.connect(t, alice, "phone")
	b := e.connect(t, bob, "phone")
	c := e.connect(t, carol, "phone")

	// alice types in c-ab: bob is told, carol is not, alice is not echoed.
	a.send(t, typing(convAB, "t1"))
	ev := b.next(t)
	if ev.Type != realtime.EventTyping {
		t.Fatalf("bob got %s, want %s", ev.Type, realtime.EventTyping)
	}
	var td realtime.TypingData
	if err := json.Unmarshal(ev.Data, &td); err != nil {
		t.Fatal(err)
	}
	if td != (realtime.TypingData{ConversationID: convAB, UserID: alice}) {
		t.Fatalf("typing data %+v", td)
	}
	e.assertNothingQueued(t, c, carol)
	e.assertNothingQueued(t, a, alice)

	// A server event for c-ab reaches its participants only.
	const created realtime.EventType = "message.created"
	if err := e.hub.PublishConversation(t.Context(), convAB, created, map[string]string{"text": "hi"}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*peer{"alice": a, "bob": b} {
		if ev := p.next(t); ev.Type != created {
			t.Fatalf("%s got %s, want %s", name, ev.Type, created)
		}
	}
	e.assertNothingQueued(t, c, carol)

	// carol cannot type into c-ab; the answer equals the one for a
	// conversation that does not exist.
	c.send(t, typing(convAB, "t2"))
	if got := c.nextError(t); got != (realtime.ErrorData{Code: realtime.CodeConversationNotFound, CommandID: "t2"}) {
		t.Fatalf("carol typing in c-ab: %+v", got)
	}
	c.send(t, typing("c-none", "t3"))
	if got := c.nextError(t); got != (realtime.ErrorData{Code: realtime.CodeConversationNotFound, CommandID: "t3"}) {
		t.Fatalf("carol typing in a missing conversation: %+v", got)
	}
	e.assertNothingQueued(t, a, alice)
	e.assertNothingQueued(t, b, bob)
}

// TestClientCommandsCannotForgeEvents: a client message is never relayed.
// Server event types are unknown commands, extra fields are rejected, and
// the connection survives every rejection.
func TestClientCommandsCannotForgeEvents(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), 0)
	a := e.connect(t, alice, "phone")
	b := e.connect(t, bob, "phone")

	tests := []struct {
		raw  string
		want realtime.ErrorData
	}{
		{`{"type":"conversation.typing","data":{"conversationId":"c-ab","userId":"alice"}}`,
			realtime.ErrorData{Code: realtime.CodeCommandUnknown}},
		{`{"type":"message.created","id":"f1","data":{"conversationId":"c-ab","text":"forged"}}`,
			realtime.ErrorData{Code: realtime.CodeCommandUnknown, CommandID: "f1"}},
		{`{"type":"hello","id":"f2"}`, realtime.ErrorData{Code: realtime.CodeCommandUnknown, CommandID: "f2"}},
		{`{"type":"error","id":"f3","data":{"code":"INTERNAL"}}`, realtime.ErrorData{Code: realtime.CodeCommandUnknown, CommandID: "f3"}},
		{`{"type":"presence.watch","id":"f4","data":{"userIds":["bob"]}}`, realtime.ErrorData{Code: realtime.CodeCommandUnknown, CommandID: "f4"}},
		{`{"type":"typing","id":"f5","data":{"conversationId":"c-ab","userId":"bob"}}`,
			realtime.ErrorData{Code: realtime.CodeCommandInvalid, CommandID: "f5"}},
		{`{"type":"typing","id":"f6","occurredAt":"2026-11-05T09:00:00Z","data":{"conversationId":"c-ab"}}`,
			realtime.ErrorData{Code: realtime.CodeCommandInvalid}},
		{`{"type":"typing","id":"f7"}`, realtime.ErrorData{Code: realtime.CodeCommandInvalid, CommandID: "f7"}},
		{`{"type":"typing","id":"f8","data":{"conversationId":"c-ab"}} {"type":"typing"}`, realtime.ErrorData{Code: realtime.CodeCommandInvalid}},
		{`{"type":"typing","id":"bad id","data":{"conversationId":"c-ab"}}`, realtime.ErrorData{Code: realtime.CodeCommandInvalid}},
		{`not json`, realtime.ErrorData{Code: realtime.CodeCommandInvalid}},
	}
	for _, tt := range tests {
		a.send(t, tt.raw)
		if got := a.nextError(t); got != tt.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tt.raw, got, tt.want)
		}
	}
	e.assertNothingQueued(t, b, bob)

	// The connection is still usable: a valid command goes through.
	a.send(t, typing(convAB, "ok"))
	if ev := b.next(t); ev.Type != realtime.EventTyping {
		t.Fatalf("bob got %s, want %s", ev.Type, realtime.EventTyping)
	}
}

// TestMessageSizeLimit: a message of MaxMessageBytes is read; one byte
// more closes the connection with 1009.
func TestMessageSizeLimit(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	e := newEnv(t, opts, 0)
	a := e.connect(t, alice, "phone")

	a.send(t, strings.Repeat(" ", int(opts.MaxMessageBytes)))
	if got := a.nextError(t); got.Code != realtime.CodeCommandInvalid {
		t.Fatalf("message at the limit: %+v, want %s", got, realtime.CodeCommandInvalid)
	}
	a.send(t, strings.Repeat(" ", int(opts.MaxMessageBytes)+1))
	if got := a.closeStatus(t); got != websocket.StatusMessageTooBig {
		t.Fatalf("close status %d, want %d", got, websocket.StatusMessageTooBig)
	}
}

// TestBinaryMessageClosesConnection: the protocol is JSON text only.
func TestBinaryMessageClosesConnection(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), 0)
	a := e.connect(t, alice, "phone")
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	if err := a.conn.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if got := a.closeStatus(t); got != websocket.StatusUnsupportedData {
		t.Fatalf("close status %d, want %d", got, websocket.StatusUnsupportedData)
	}
}

// TestCommandRateLimit: the burst passes, the next command is answered
// with REALTIME_RATE_LIMITED, tokens refill with time, and a client that
// keeps flooding is closed with 1008.
func TestCommandRateLimit(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	opts.CommandRate = 1
	opts.CommandBurst = 3
	opts.MaxRateStrikes = 5
	e := newEnv(t, opts, 0)
	a := e.connect(t, alice, "phone")
	b := e.connect(t, bob, "phone")

	for range opts.CommandBurst {
		a.send(t, typing(convAB, "burst"))
	}
	a.send(t, typing(convAB, "over"))
	if got := a.nextError(t); got.Code != realtime.CodeRateLimited {
		t.Fatalf("over the burst: %+v, want %s", got, realtime.CodeRateLimited)
	}

	e.clock.Advance(time.Second)
	a.send(t, typing(convAB, "refilled"))
	for range opts.MaxRateStrikes {
		a.send(t, typing(convAB, "flood"))
	}
	if got := a.nextError(t); got.Code != realtime.CodeRateLimited {
		t.Fatalf("flood: %+v, want %s", got, realtime.CodeRateLimited)
	}
	if got := a.closeStatus(t); got != websocket.StatusPolicyViolation {
		t.Fatalf("close status %d, want %d", got, websocket.StatusPolicyViolation)
	}

	// bob saw the burst and the refilled command, nothing else.
	for i := range opts.CommandBurst + 1 {
		if ev := b.next(t); ev.Type != realtime.EventTyping {
			t.Fatalf("bob event %d: %s, want %s", i, ev.Type, realtime.EventTyping)
		}
	}
	e.assertNothingQueued(t, b, bob)
}

// TestIdleConnectionIsClosed: a peer that stops answering pings is closed
// with 1008; a peer that answers stays connected.
func TestIdleConnectionIsClosed(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	opts.PingInterval = 25 * time.Millisecond
	opts.IdleTimeout = 250 * time.Millisecond
	e := newEnv(t, opts, 0)
	live := e.connect(t, bob, "phone")
	res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "phone")), ignorePings: true})
	if res.err != nil {
		t.Fatalf("dial: status %d %s: %v", res.status, res.code, res.err)
	}

	if got := res.peer.closeStatus(t); got != websocket.StatusPolicyViolation {
		t.Fatalf("silent peer: close status %d, want %d", got, websocket.StatusPolicyViolation)
	}
	e.assertNothingQueued(t, live, bob)
}

// TestUserFanoutAndSessionRevocation: a user's event reaches all of their
// devices; revoking one session closes only that device with 4401.
func TestUserFanoutAndSessionRevocation(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), 0)
	phone := e.connect(t, alice, "phone")
	tablet := e.connect(t, alice, "tablet")

	const note realtime.EventType = "test.note"
	if err := e.hub.PublishUser(alice, note, struct{}{}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*peer{"phone": phone, "tablet": tablet} {
		if ev := p.next(t); ev.Type != note {
			t.Fatalf("%s got %s, want %s", name, ev.Type, note)
		}
	}

	e.hub.DisconnectSession(alice, sessionOf(alice, "phone"))
	if got := phone.closeStatus(t); got != realtime.CloseSessionRevoked {
		t.Fatalf("revoked phone: close status %d, want %d", got, realtime.CloseSessionRevoked)
	}
	e.assertNothingQueued(t, tablet, alice)
}

// TestConnectionLimitPerUser: the connection over MaxConnsPerUser is
// refused with 429; closing one frees a slot.
func TestConnectionLimitPerUser(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	opts.MaxConnsPerUser = 2
	e := newEnv(t, opts, 0)
	first := e.connect(t, alice, "phone")
	e.connect(t, alice, "tablet")

	over := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "laptop")), header: http.Header{realtime.HeaderDeviceID: {"laptop"}}})
	if over.status != http.StatusTooManyRequests || over.code != realtime.CodeTooManyConnections {
		t.Fatalf("third connection: status %d %s, want 429 %s", over.status, over.code, realtime.CodeTooManyConnections)
	}

	if err := first.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitTimeout)
	for {
		res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "laptop")), header: http.Header{realtime.HeaderDeviceID: {"laptop"}}})
		if res.err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed: status %d %s", res.status, res.code)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestShutdownDrainsThenClosesStreams follows httpx.Server's order: once
// draining starts, new WebSockets get 503 while open ones keep working;
// after the drain delay they get a 1001 close frame and Serve returns nil.
func TestShutdownDrainsThenClosesStreams(t *testing.T) {
	t.Parallel()

	e := newEnv(t, testOptions(), time.Second)
	a := e.connect(t, alice, "phone")

	e.stop()
	// Draining starts asynchronously; a handshake that wins the race is
	// an ordinary connection and gets the 1001 below.
	deadline := time.Now().Add(waitTimeout)
	for {
		res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, bob, "phone")), header: http.Header{realtime.HeaderDeviceID: {"phone"}}})
		if res.status == http.StatusServiceUnavailable && res.code == realtime.CodeDraining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never refused while draining: status %d %s", res.status, res.code)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Still inside the drain delay: the open connection works.
	e.assertNothingQueued(t, a, alice)

	if got := a.closeStatus(t); got != websocket.StatusGoingAway {
		t.Fatalf("close status %d, want %d", got, websocket.StatusGoingAway)
	}
	if err := e.shutdown(); err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}
