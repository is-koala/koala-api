package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/realtime"
)

// TestDefaultLimits pins the production limits recorded in ADR-0018.
func TestDefaultLimits(t *testing.T) {
	t.Parallel()

	o := realtime.DefaultOptions()
	got := []any{
		o.MaxMessageBytes, o.SendBuffer, o.PingInterval, o.IdleTimeout, o.CommandRate, o.CommandBurst,
		o.MaxRateStrikes, o.MaxConnsPerUser, o.SessionCheckInterval, o.HandshakeTimeout, o.CommandTimeout,
		o.WriteTimeout, o.MaxSessionCheckFailures,
	}
	want := []any{
		int64(64 << 10), 64, 20 * time.Second, 60 * time.Second, 5.0, 10,
		20, 10, time.Minute, 5 * time.Second, 5 * time.Second,
		10 * time.Second, 3,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("DefaultOptions() limits = %v, want %v", got, want)
	}
	h := realtime.DefaultHandlerOptions()
	if h.TicketInterval != 6*time.Second || h.TicketBurst != 5 {
		t.Fatalf("DefaultHandlerOptions() = %+v, want 6s interval, burst 5", h)
	}
}

// TestTicketIsSingleUse: a ticket opens one connection; replaying it is
// rejected before the upgrade.
func TestTicketIsSingleUse(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
	spec := dialSpec{protocols: protocols(e.ticket(t, alice, "phone")), header: deviceHeader("phone")}

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

	phone := deviceHeader("phone")
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
			return dialSpec{protocols: protocols(ticket), header: deviceHeader("tablet")}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		// The router's client gate answers before the handler: a native
		// handshake meets the same header rules as REST (ADR-0021).
		{"native client without device", func(_ *env, ticket string) dialSpec {
			return dialSpec{protocols: protocols(ticket)}
		}, http.StatusBadRequest, httpx.CodeRequestInvalid},
		{"outdated native build", func(_ *env, ticket string) dialSpec {
			h := deviceHeader("phone")
			h.Set(httpx.HeaderAppVersion, "0.9.0")
			return dialSpec{protocols: protocols(ticket), header: h}
		}, http.StatusBadRequest, httpx.CodeAppVersionUnsupported},
		{"session revoked after issue", func(e *env, ticket string) dialSpec {
			e.sessions.Revoke(alicePhoneSession())
			return dialSpec{protocols: protocols(ticket), header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"session revoked between redeem and join", func(e *env, ticket string) dialSpec {
			// Call 1 is Redeem, call 2 the re-check after join.
			e.sessions.RevokeOnCall(alicePhoneSession(), 2)
			return dialSpec{protocols: protocols(ticket), header: phone}
		}, http.StatusUnauthorized, realtime.CodeTicketInvalid},
		{"foreign origin", func(_ *env, ticket string) dialSpec {
			return dialSpec{protocols: protocols(ticket), header: http.Header{"Origin": {"https://evil.example"}}}
		}, http.StatusForbidden, realtime.CodeOriginForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, defaultEnv())
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

// TestPreTicketRejectionsKeepTicket: origin, device and handshake header
// checks run before the ticket is spent; a browser from an allowed origin
// connects without a device header.
func TestPreTicketRejectionsKeepTicket(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
	ticket := e.ticket(t, alice, "phone")

	foreign := e.dial(t, dialSpec{protocols: protocols(ticket), header: http.Header{"Origin": {"https://evil.example"}}})
	if foreign.status != http.StatusForbidden {
		t.Fatalf("foreign origin: status %d, want 403", foreign.status)
	}
	native := e.dial(t, dialSpec{protocols: protocols(ticket)})
	if native.status != http.StatusBadRequest || native.code != httpx.CodeRequestInvalid {
		t.Fatalf("native without device: status %d %s", native.status, native.code)
	}

	// A malformed Sec-WebSocket-Key, sent by hand: the client library
	// always sends a valid one.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.base+realtime.PathConnect, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "not-sixteen-bytes")
	req.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols(ticket), ", "))
	req.Header.Set(realtime.HeaderDeviceID, deviceID("phone"))
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad Sec-WebSocket-Key: status %d, want 400", resp.StatusCode)
	}

	browser := e.dial(t, dialSpec{protocols: protocols(ticket), header: http.Header{"Origin": {allowedOrigin}}})
	if browser.err != nil {
		t.Fatalf("allowed origin: status %d %s: %v", browser.status, browser.code, browser.err)
	}
}

func TestPlainRequestsAreRejected(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
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
		req.Header.Set(realtime.HeaderDeviceID, deviceID("phone"))
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

// TestTicketIssueRateLimit: the production ticket limit per user (burst 5,
// then one per 6 s) with Retry-After; other users are not affected.
func TestTicketIssueRateLimit(t *testing.T) {
	t.Parallel()

	cfg := defaultEnv()
	cfg.handler = realtime.DefaultHandlerOptions()
	e := newEnv(t, cfg)
	for i := range cfg.handler.TicketBurst {
		if status, _, _ := e.issue(t, alice, "phone"); status != http.StatusCreated {
			t.Fatalf("ticket %d: status %d, want 201", i, status)
		}
	}
	if status, retry, _ := e.issue(t, alice, "phone"); status != http.StatusTooManyRequests || retry != "6" {
		t.Fatalf("over the burst: status %d, Retry-After %q; want 429, 6", status, retry)
	}
	if status, _, _ := e.issue(t, bob, "phone"); status != http.StatusCreated {
		t.Fatalf("other user: status %d, want 201", status)
	}
	e.clock.Advance(cfg.handler.TicketInterval)
	if status, _, _ := e.issue(t, alice, "phone"); status != http.StatusCreated {
		t.Fatalf("after the interval: status %d, want 201", status)
	}
}

// TestUnauthorizedEventsAreNotReceived: a connection only gets events the
// server addressed to its user; membership comes from Conversations.
func TestUnauthorizedEventsAreNotReceived(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
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

	e := newEnv(t, defaultEnv())
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

	cfg := defaultEnv()
	e := newEnv(t, cfg)
	a := e.connect(t, alice, "phone")

	a.send(t, strings.Repeat(" ", int(cfg.hub.MaxMessageBytes)))
	if got := a.nextError(t); got.Code != realtime.CodeCommandInvalid {
		t.Fatalf("message at the limit: %+v, want %s", got, realtime.CodeCommandInvalid)
	}
	a.send(t, strings.Repeat(" ", int(cfg.hub.MaxMessageBytes)+1))
	if got := a.closeStatus(t); got != websocket.StatusMessageTooBig {
		t.Fatalf("close status %d, want %d", got, websocket.StatusMessageTooBig)
	}
}

// TestBinaryMessageClosesConnection: the protocol is JSON text only.
func TestBinaryMessageClosesConnection(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
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

// TestCommandRateLimit runs the production command limits (5/s, burst 10,
// 20 strikes): the burst passes, the next command is answered with
// REALTIME_RATE_LIMITED, tokens refill with time, and a client that keeps
// flooding is closed with 1008.
func TestCommandRateLimit(t *testing.T) {
	t.Parallel()

	cfg := defaultEnv()
	cfg.hub = realtime.DefaultOptions()
	e := newEnv(t, cfg)
	a := e.connect(t, alice, "phone")
	b := e.connect(t, bob, "phone")

	for range cfg.hub.CommandBurst {
		a.send(t, typing(convAB, "burst"))
	}
	a.send(t, typing(convAB, "over"))
	if got := a.nextError(t); got.Code != realtime.CodeRateLimited {
		t.Fatalf("over the burst: %+v, want %s", got, realtime.CodeRateLimited)
	}

	e.clock.Advance(250 * time.Millisecond) // one token at 5/s (1.25)
	a.send(t, typing(convAB, "refilled"))
	for range cfg.hub.MaxRateStrikes {
		a.send(t, typing(convAB, "flood"))
	}
	if got := a.closeStatus(t); got != websocket.StatusPolicyViolation {
		t.Fatalf("close status %d, want %d", got, websocket.StatusPolicyViolation)
	}
	// The flood's REALTIME_RATE_LIMITED may or may not be written: a
	// close request wins over queued events. Nothing else may arrive.
	for _, typ := range a.remaining() {
		if typ != realtime.EventError {
			t.Fatalf("flooding client received %s", typ)
		}
	}

	// bob saw the burst and the refilled command, nothing else.
	for i := range cfg.hub.CommandBurst + 1 {
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

	cfg := defaultEnv()
	cfg.hub.PingInterval = 50 * time.Millisecond
	cfg.hub.IdleTimeout = 500 * time.Millisecond
	e := newEnv(t, cfg)
	live := e.connect(t, bob, "phone")
	res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "phone")), header: deviceHeader("phone"), ignorePings: true})
	if res.err != nil {
		t.Fatalf("dial: status %d %s: %v", res.status, res.code, res.err)
	}

	if got := res.peer.closeStatus(t); got != websocket.StatusPolicyViolation {
		t.Fatalf("silent peer: close status %d, want %d", got, websocket.StatusPolicyViolation)
	}
	e.assertNothingQueued(t, live, bob)
}

// TestSessionRevocationClosesOnlyThatDevice: a user's event reaches all
// of their devices; revoking one session closes only that device with
// 4401, and nothing published after the revocation reaches it.
func TestSessionRevocationClosesOnlyThatDevice(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
	phone := e.connect(t, alice, "phone")
	tablet := e.connect(t, alice, "tablet")

	const before, after realtime.EventType = "test.before", "test.after"
	if err := e.hub.PublishUser(alice, before, struct{}{}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*peer{"phone": phone, "tablet": tablet} {
		if ev := p.next(t); ev.Type != before {
			t.Fatalf("%s got %s, want %s", name, ev.Type, before)
		}
	}

	e.hub.DisconnectSession(alice, alicePhoneSession())
	if err := e.hub.PublishUser(alice, after, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if got := phone.closeStatus(t); got != realtime.CloseSessionRevoked {
		t.Fatalf("revoked phone: close status %d, want %d", got, realtime.CloseSessionRevoked)
	}
	if got := phone.remaining(); len(got) != 0 {
		t.Fatalf("revoked phone received %v after the revocation", got)
	}
	if ev := tablet.next(t); ev.Type != after {
		t.Fatalf("tablet got %s, want %s", ev.Type, after)
	}
}

// TestSessionCheckFailuresCloseTheConnection: one failed periodic check
// keeps the connection and a success resets the count; after
// MaxSessionCheckFailures failures in a row the connection is closed with
// 1013 so a revoked session cannot stay connected through an outage of the
// session store (ADR-0022).
func TestSessionCheckFailuresCloseTheConnection(t *testing.T) {
	t.Parallel()

	cfg := defaultEnv()
	cfg.hub.SessionCheckInterval = 20 * time.Millisecond
	cfg.hub.MaxSessionCheckFailures = 3
	e := newEnv(t, cfg)
	phone := e.connect(t, alice, "phone")
	tablet := e.connect(t, alice, "tablet")

	session := alicePhoneSession()
	e.sessions.FailAfter(session, []bool{true, true, false})
	if got := phone.closeStatus(t); got != websocket.StatusTryAgainLater {
		t.Fatalf("close status %d, want %d", got, websocket.StatusTryAgainLater)
	}
	// Two failures, a success that resets the count, then three more.
	if got := e.sessions.Failures(session); got != 5 {
		t.Fatalf("closed after %d failed checks, want 5", got)
	}
	e.assertNothingQueued(t, tablet, alice)
}

// TestHandlerRequiresDeviceOfNativeClients: the handler keeps its own
// device check for a handshake that reached it without the router's
// client gate.
func TestHandlerRequiresDeviceOfNativeClients(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, realtime.PathConnect, http.NoBody)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols(strings.Repeat("A", 43)), ", "))
	rec := httptest.NewRecorder()
	e.handler.Connect(rec, req)
	var p httpx.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != http.StatusUnauthorized || p.Code != realtime.CodeDeviceRequired {
		t.Fatalf("status %d %s, want 401 %s", rec.Code, rec.Body, realtime.CodeDeviceRequired)
	}
}

// TestSameHostOriginNeedsHTTPS: the request's own host is an allowed
// origin only over https (ADR-0022); an allowed https page connects.
func TestSameHostOriginNeedsHTTPS(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
	host := strings.TrimPrefix(e.base, "http://")
	plain := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "phone")), header: http.Header{"Origin": {"http://" + host}}})
	if plain.status != http.StatusForbidden || plain.code != realtime.CodeOriginForbidden {
		t.Fatalf("http same-host origin: status %d %s, want 403 %s", plain.status, plain.code, realtime.CodeOriginForbidden)
	}
	secure := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "phone")), header: http.Header{"Origin": {"https://" + host}}})
	if secure.err != nil {
		t.Fatalf("https same-host origin: status %d %s: %v", secure.status, secure.code, secure.err)
	}
}

// TestPeriodicSessionCheck: a revocation the hub was not told about (for
// example made on another instance) still ends the connection at the next
// session check.
func TestPeriodicSessionCheck(t *testing.T) {
	t.Parallel()

	cfg := defaultEnv()
	cfg.hub.SessionCheckInterval = 50 * time.Millisecond
	e := newEnv(t, cfg)
	phone := e.connect(t, alice, "phone")
	tablet := e.connect(t, alice, "tablet")

	e.sessions.Revoke(alicePhoneSession())
	if got := phone.closeStatus(t); got != realtime.CloseSessionRevoked {
		t.Fatalf("close status %d, want %d", got, realtime.CloseSessionRevoked)
	}
	e.assertNothingQueued(t, tablet, alice)
}

// TestSlowConsumerIsClosed: a client that stops reading fills its queue
// and is closed with 1013 once it reads again.
func TestSlowConsumerIsClosed(t *testing.T) {
	t.Parallel()

	cfg := defaultEnv()
	cfg.hub.SendBuffer = 4
	e := newEnv(t, cfg)
	res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "phone")), header: deviceHeader("phone"), paused: true})
	if res.err != nil {
		t.Fatalf("dial: status %d %s: %v", res.status, res.code, res.err)
	}

	// 400 × 128 KiB is far more than the socket buffers of both ends, so
	// the writer blocks and the queue overflows.
	payload := struct {
		Pad string `json:"pad"`
	}{Pad: strings.Repeat("x", 128<<10)}
	for range 400 {
		if err := e.hub.PublishUser(alice, "test.bulk", payload); err != nil {
			t.Fatal(err)
		}
	}
	res.peer.resume()
	if got := res.peer.closeStatus(t); got != websocket.StatusTryAgainLater {
		t.Fatalf("close status %d, want %d", got, websocket.StatusTryAgainLater)
	}
}

// TestShutdownCutsUnresponsiveConnections: a peer that never answers the
// close frame is cut when the shutdown deadline passes, and Shutdown
// reports it.
func TestShutdownCutsUnresponsiveConnections(t *testing.T) {
	t.Parallel()

	e := newEnv(t, defaultEnv())
	res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "phone")), header: deviceHeader("phone"), paused: true})
	if res.err != nil {
		t.Fatalf("dial: status %d %s: %v", res.status, res.code, res.err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.hub.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "cut 1 connections") {
		t.Fatalf("Shutdown() = %v, want 1 connection cut at the deadline", err)
	}
	// The close handshake alone would wait 5 s for the peer.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %s", elapsed)
	}
}

// TestConnectionLimitPerUser: the connection over MaxConnsPerUser is
// refused with 429 and Retry-After; closing one frees a slot.
func TestConnectionLimitPerUser(t *testing.T) {
	t.Parallel()

	cfg := defaultEnv()
	cfg.hub.MaxConnsPerUser = 2
	e := newEnv(t, cfg)
	first := e.connect(t, alice, "phone")
	e.connect(t, alice, "tablet")

	over := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "laptop")), header: deviceHeader("laptop")})
	if over.status != http.StatusTooManyRequests || over.code != realtime.CodeTooManyConnections || over.retryAfter != "30" {
		t.Fatalf("third connection: status %d %s Retry-After %q, want 429 %s 30",
			over.status, over.code, over.retryAfter, realtime.CodeTooManyConnections)
	}

	if err := first.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitTimeout)
	for {
		res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, alice, "laptop")), header: deviceHeader("laptop")})
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
// draining starts, new WebSockets get 503 with Retry-After while open ones
// keep working; after the drain delay they get a 1001 close frame and
// Serve returns nil.
func TestShutdownDrainsThenClosesStreams(t *testing.T) {
	t.Parallel()

	cfg := defaultEnv()
	cfg.drainDelay = 3 * time.Second
	e := newEnv(t, cfg)
	a := e.connect(t, alice, "phone")

	e.stop()
	// Draining starts asynchronously; a handshake that wins the race is
	// an ordinary connection and gets the 1001 below.
	deadline := time.Now().Add(waitTimeout)
	for {
		res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, bob, "phone")), header: deviceHeader("phone")})
		if res.status == http.StatusServiceUnavailable && res.code == realtime.CodeDraining {
			if res.retryAfter != "5" {
				t.Fatalf("Retry-After %q, want 5", res.retryAfter)
			}
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

func TestHandlerRejectsUnsafeOrigins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		origin string
		ok     bool
	}{
		{"https://app.koala.test", true},
		{"https://app.koala.test:8443", true},
		{"http://app.koala.test", false},
		{"https://app.koala.test:443", false},
		{"https://*.koala.test", false},
		{"https://app.koala.test/path", false},
		{"app.koala.test", false},
	}
	for _, tt := range tests {
		opts := realtime.DefaultHandlerOptions()
		opts.AllowedOrigins = []string{tt.origin}
		_, err := realtime.NewHandler(nil, nil, headerAuth{}, nil, opts)
		if (err == nil) != tt.ok {
			t.Errorf("NewHandler(%q) error = %v, want ok=%v", tt.origin, err, tt.ok)
		}
	}
}
