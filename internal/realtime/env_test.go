package realtime_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/realtime"
)

// waitTimeout bounds every wait in these tests.
const waitTimeout = 5 * time.Second

// allowedOrigin is the browser origin the test handler accepts.
const allowedOrigin = "https://app.koala.test"

// Users and conversations of the fixture: carol shares no conversation
// with alice.
const (
	alice realtime.UserID = "alice"
	bob   realtime.UserID = "bob"
	carol realtime.UserID = "carol"

	convAB realtime.ConversationID = "c-ab"
	convBC realtime.ConversationID = "c-bc"
)

// sentinel is a test-only event type used to prove that nothing else was
// queued for a connection.
const sentinel realtime.EventType = "test.sentinel"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeConversations stands in for authz (Faz 4). It is read-only.
type fakeConversations map[realtime.ConversationID][]realtime.UserID

func (f fakeConversations) IsParticipant(_ context.Context, conv realtime.ConversationID, user realtime.UserID) (bool, error) {
	return slices.Contains(f[conv], user), nil
}

func (f fakeConversations) Participants(_ context.Context, conv realtime.ConversationID) ([]realtime.UserID, error) {
	return f[conv], nil
}

func fixtureConversations() fakeConversations {
	return fakeConversations{
		convAB: {alice, bob},
		convBC: {bob, carol},
	}
}

// fakeSessions reports every session active until it is revoked.
type fakeSessions struct {
	mu      sync.Mutex
	revoked map[string]bool
}

func newFakeSessions() *fakeSessions { return &fakeSessions{revoked: make(map[string]bool)} }

func (f *fakeSessions) Active(_ context.Context, p realtime.Principal) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.revoked[p.SessionID], nil
}

func (f *fakeSessions) Revoke(session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[session] = true
}

// sessionOf is the session id headerAuth gives a user on a device.
func sessionOf(user realtime.UserID, device string) string {
	return "session-" + string(user) + "-" + device
}

// headerAuth authenticates REST calls by X-Test-User and X-Device-Id, in
// place of Faz 1's session middleware.
type headerAuth struct{}

func (headerAuth) Principal(r *http.Request) (realtime.Principal, error) {
	user := realtime.UserID(r.Header.Get("X-Test-User"))
	device := r.Header.Get(realtime.HeaderDeviceID)
	if user == "" || device == "" {
		return realtime.Principal{}, apperr.New(apperr.KindUnauthenticated, "UNAUTHENTICATED")
	}
	return realtime.Principal{UserID: user, SessionID: sessionOf(user, device), DeviceID: device}, nil
}

func testOptions() realtime.Options {
	opts := realtime.DefaultOptions()
	opts.CommandBurst = 50
	return opts
}

// env is a running httpx.Server with the realtime routes and the hub as
// its stream, wired as internal/app will wire it.
type env struct {
	base      string
	hub       *realtime.Hub
	clock     *fakeClock
	sessions  *fakeSessions
	client    *http.Client
	transport *http.Transport

	stop       context.CancelFunc
	served     chan error
	stopOnce   sync.Once
	stopResult error
}

func newEnv(t *testing.T, opts realtime.Options, drainDelay time.Duration) *env {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	clock := newFakeClock()
	sessions := newFakeSessions()
	hub, err := realtime.NewHub(fixtureConversations(), clock, logger, opts)
	if err != nil {
		t.Fatal(err)
	}
	tickets := realtime.NewTickets(realtime.NewMemoryTicketStore(), sessions, clock, logger)
	handler, err := realtime.NewHandler(hub, tickets, headerAuth{}, logger, []string{allowedOrigin})
	if err != nil {
		t.Fatal(err)
	}
	health := httpx.NewHealth(logger)
	router := httpx.NewRouter(logger, health)
	handler.Mount(router)

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httpx.NewServer(router, health, logger, httpx.ServerOptions{
		DrainDelay:      drainDelay,
		ShutdownTimeout: waitTimeout,
		Streams:         []httpx.Stream{hub},
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	transport := &http.Transport{}
	e := &env{
		base:      "http://" + ln.Addr().String(),
		hub:       hub,
		clock:     clock,
		sessions:  sessions,
		client:    &http.Client{Transport: transport, Timeout: waitTimeout},
		transport: transport,
		stop:      cancel,
		served:    served,
	}
	t.Cleanup(func() {
		if err := e.shutdown(); err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	})
	return e
}

// shutdown cancels the server and waits for Serve; it is idempotent.
func (e *env) shutdown() error {
	e.stopOnce.Do(func() {
		e.stop()
		select {
		case e.stopResult = <-e.served:
		case <-time.After(2 * waitTimeout):
			e.stopResult = context.DeadlineExceeded
		}
		e.transport.CloseIdleConnections()
	})
	return e.stopResult
}

// ticket gets a ticket over REST as user on device.
func (e *env) ticket(t *testing.T, user realtime.UserID, device string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+realtime.PathTickets, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", string(user))
	req.Header.Set(realtime.HeaderDeviceID, device)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201", realtime.PathTickets, resp.StatusCode)
	}
	var body realtime.TicketResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Ticket
}

// protocols is the Sec-WebSocket-Protocol list a client sends.
func protocols(ticket string) []string {
	return []string{realtime.Subprotocol, realtime.TicketProtocolPrefix + ticket}
}

// dialSpec describes one handshake.
type dialSpec struct {
	protocols []string
	header    http.Header
	// ignorePings makes the client drop pings instead of answering with
	// a pong, like a peer that has gone away.
	ignorePings bool
}

// dialResult is a handshake's outcome: a peer, or the HTTP status and
// problem code of the rejection.
type dialResult struct {
	peer   *peer
	status int
	code   apperr.Code
	err    error
}

func (e *env) dial(t *testing.T, spec dialSpec) dialResult {
	t.Helper()
	opts := &websocket.DialOptions{
		HTTPClient:   e.client,
		HTTPHeader:   spec.header,
		Subprotocols: spec.protocols,
	}
	if spec.ignorePings {
		opts.OnPingReceived = func(context.Context, []byte) bool { return false }
	}
	conn, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(e.base, "http")+realtime.PathConnect, opts)
	var res dialResult
	res.err = err
	if resp != nil {
		res.status = resp.StatusCode
		if resp.Body != nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var p httpx.Problem
			if json.Unmarshal(b, &p) == nil {
				res.code = p.Code
			}
		}
	}
	if conn != nil {
		res.peer = newPeer(t, conn)
	}
	return res
}

// connect opens an authenticated connection for user on device and reads
// its hello event.
func (e *env) connect(t *testing.T, user realtime.UserID, device string) *peer {
	t.Helper()
	res := e.dial(t, dialSpec{
		protocols: protocols(e.ticket(t, user, device)),
		header:    http.Header{realtime.HeaderDeviceID: {device}},
	})
	if res.err != nil {
		t.Fatalf("connect %s: status %d %s: %v", user, res.status, res.code, res.err)
	}
	if got := res.peer.conn.Subprotocol(); got != realtime.Subprotocol {
		t.Fatalf("negotiated subprotocol %q, want %q", got, realtime.Subprotocol)
	}
	if ev := res.peer.next(t); ev.Type != realtime.EventHello {
		t.Fatalf("first event %s, want hello", ev.Type)
	}
	return res.peer
}

// assertNothingQueued publishes a sentinel to user and checks it is the
// next event p receives. The hub serializes fanouts, so an event queued
// for p earlier would arrive first.
func (e *env) assertNothingQueued(t *testing.T, p *peer, user realtime.UserID) {
	t.Helper()
	if err := e.hub.PublishUser(user, sentinel, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if ev := p.next(t); ev.Type != sentinel {
		t.Fatalf("%s received %s %s, want nothing before the sentinel", user, ev.Type, ev.Data)
	}
}

// peer is a client connection with a background reader, so it answers
// pings while the test is busy elsewhere.
type peer struct {
	conn     *websocket.Conn
	events   chan realtime.Event
	overflow atomic.Bool
	done     chan struct{}
	status   websocket.StatusCode // valid after done is closed
}

func newPeer(t *testing.T, conn *websocket.Conn) *peer {
	t.Helper()
	p := &peer{conn: conn, events: make(chan realtime.Event, 256), done: make(chan struct{})}
	go p.readAll()
	t.Cleanup(func() {
		_ = conn.CloseNow()
		<-p.done
	})
	return p
}

func (p *peer) readAll() {
	defer close(p.done)
	for {
		_, data, err := p.conn.Read(context.Background())
		if err != nil {
			p.status = websocket.CloseStatus(err)
			return
		}
		var ev realtime.Event
		if err := json.Unmarshal(data, &ev); err != nil {
			p.overflow.Store(true) // reported by next as a broken stream
			continue
		}
		select {
		case p.events <- ev:
		default:
			p.overflow.Store(true)
		}
	}
}

// next returns the next event or fails the test.
func (p *peer) next(t *testing.T) realtime.Event {
	t.Helper()
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	select {
	case ev := <-p.events:
		if p.overflow.Load() {
			t.Fatal("peer dropped or could not decode an event")
		}
		return ev
	case <-p.done:
		select {
		case ev := <-p.events:
			return ev
		default:
		}
		t.Fatalf("connection closed with %d while waiting for an event", p.status)
	case <-timer.C:
		t.Fatal("no event within the timeout")
	}
	return realtime.Event{}
}

// nextError returns the data of the next event, which must be an error.
func (p *peer) nextError(t *testing.T) realtime.ErrorData {
	t.Helper()
	ev := p.next(t)
	if ev.Type != realtime.EventError {
		t.Fatalf("event %s %s, want error", ev.Type, ev.Data)
	}
	var data realtime.ErrorData
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// closeStatus waits until the server closes the connection and returns
// the close code. Events still in flight are discarded.
func (p *peer) closeStatus(t *testing.T) websocket.StatusCode {
	t.Helper()
	select {
	case <-p.done:
		return p.status
	case <-time.After(waitTimeout):
		t.Fatal("connection not closed within the timeout")
		return 0
	}
}

// send writes one raw client message.
func (p *peer) send(t *testing.T, raw string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	if err := p.conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// typing is the raw typing command for conv.
func typing(conv realtime.ConversationID, id string) string {
	return `{"type":"typing","id":"` + id + `","data":{"conversationId":"` + string(conv) + `"}}`
}
