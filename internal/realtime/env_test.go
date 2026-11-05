package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/appversion"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/realtime"
)

// waitTimeout bounds every wait in these tests. It is generous on purpose:
// CI runs them with -race on shared runners.
const waitTimeout = 10 * time.Second

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

// memoryTicketStore is the test TicketStore. Production gets a Postgres
// store in Faz 4 (ADR-0018).
type memoryTicketStore struct {
	mu      sync.Mutex
	tickets map[realtime.TicketHash]realtime.Ticket
}

func newMemoryTicketStore() *memoryTicketStore {
	return &memoryTicketStore{tickets: make(map[realtime.TicketHash]realtime.Ticket)}
}

func (s *memoryTicketStore) Put(_ context.Context, hash realtime.TicketHash, t realtime.Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[hash] = t
	return nil
}

func (s *memoryTicketStore) Take(_ context.Context, hash realtime.TicketHash) (realtime.Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, found := s.tickets[hash]
	delete(s.tickets, hash)
	return t, found, nil
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
	calls   map[string]int
	// revokeAt revokes a session on its nth Active call.
	revokeAt map[string]int
	// script makes the next calls for a session fail (true) or succeed
	// (false) in order; past its end every call fails.
	script   map[string][]bool
	failures map[string]int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{
		revoked: make(map[string]bool), calls: make(map[string]int), revokeAt: make(map[string]int),
		script: make(map[string][]bool), failures: make(map[string]int),
	}
}

var errStoreDown = errors.New("session store unreachable")

func (f *fakeSessions) Active(_ context.Context, p realtime.Principal) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[p.SessionID]++
	steps, scripted := f.script[p.SessionID]
	fail := scripted
	if len(steps) > 0 {
		fail, f.script[p.SessionID] = steps[0], steps[1:]
	}
	if fail {
		f.failures[p.SessionID]++
		return false, errStoreDown
	}
	if n, ok := f.revokeAt[p.SessionID]; ok && f.calls[p.SessionID] >= n {
		f.revoked[p.SessionID] = true
	}
	return !f.revoked[p.SessionID], nil
}

func (f *fakeSessions) Revoke(session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[session] = true
}

// RevokeOnCall makes the nth Active call for session (and later ones)
// report it revoked, to hit a race window deterministically.
func (f *fakeSessions) RevokeOnCall(session string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeAt[session] = n
}

// FailAfter makes the session store fail (true) or answer (false) the
// next calls for session per steps, then fail every later call.
func (f *fakeSessions) FailAfter(session string, steps []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script[session] = steps
}

// Failures returns how many calls for session failed.
func (f *fakeSessions) Failures(session string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures[session]
}

// deviceID is the X-Device-Id of a named test device. The contract asks
// for 8 to 128 characters of [A-Za-z0-9._:-].
func deviceID(name string) string { return name + "-0001" }

// sessionID is the session id headerAuth gives a user on a device.
func sessionID(user realtime.UserID, device string) string {
	return "session-" + string(user) + "-" + device
}

// alicePhoneSession is the session of alice's "phone" device, which the
// session tests revoke or break.
func alicePhoneSession() string { return sessionID(alice, deviceID("phone")) }

// headerAuth authenticates REST calls by X-Test-User and X-Device-Id, in
// place of Faz 1's session middleware.
type headerAuth struct{}

func (headerAuth) Principal(r *http.Request) (realtime.Principal, error) {
	user := realtime.UserID(r.Header.Get("X-Test-User"))
	device := r.Header.Get(realtime.HeaderDeviceID)
	if user == "" || device == "" {
		return realtime.Principal{}, apperr.New(apperr.KindUnauthenticated, "UNAUTHENTICATED")
	}
	return realtime.Principal{UserID: user, SessionID: sessionID(user, device), DeviceID: device}, nil
}

// envConfig configures newEnv.
type envConfig struct {
	hub        realtime.Options
	handler    realtime.HandlerOptions
	drainDelay time.Duration
}

// defaultEnv uses the production limits, except that commands and
// tickets are not rate-limited in tests that are not about rate limits.
func defaultEnv() envConfig {
	hub := realtime.DefaultOptions()
	hub.CommandBurst = 1000
	handler := realtime.DefaultHandlerOptions()
	handler.AllowedOrigins = []string{allowedOrigin}
	handler.TicketBurst = 1000
	return envConfig{hub: hub, handler: handler}
}

// env is a running httpx.Server with the realtime routes and the hub as
// its stream, wired as internal/app will wire it.
type env struct {
	base      string
	hub       *realtime.Hub
	handler   *realtime.Handler
	clock     *fakeClock
	sessions  *fakeSessions
	client    *http.Client
	transport *http.Transport

	stop       context.CancelFunc
	served     chan error
	stopOnce   sync.Once
	stopResult error
}

func newEnv(t *testing.T, cfg envConfig) *env {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	clock := newFakeClock()
	sessions := newFakeSessions()
	hub, err := realtime.NewHub(fixtureConversations(), sessions, clock, logger, cfg.hub)
	if err != nil {
		t.Fatal(err)
	}
	tickets := realtime.NewTickets(newMemoryTicketStore(), sessions, clock, logger)
	handler, err := realtime.NewHandler(hub, tickets, headerAuth{}, logger, cfg.handler)
	if err != nil {
		t.Fatal(err)
	}
	health := httpx.NewHealth(logger)
	router, err := httpx.NewRouter(logger, health, edgeOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	handler.Mount(router)

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httpx.NewServer(router, health, logger, httpx.ServerOptions{
		DrainDelay:      cfg.drainDelay,
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
		handler:   handler,
		clock:     clock,
		sessions:  sessions,
		client:    &http.Client{Transport: nativeClient{transport}, Timeout: waitTimeout},
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

// edgeOptions are the router's edge rules with limits no test reaches; the
// client gate is the production rule with minimum version testVersion.
func edgeOptions(t *testing.T) httpx.RouterOptions {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := httpx.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	unlimited := httpx.Rate{Every: time.Nanosecond, Burst: 1 << 30}
	limiter, err := httpx.NewRateLimiter(httpx.RateLimitConfig{Groups: map[string]httpx.GroupLimits{
		httpx.GroupRead: {PerIP: unlimited}, httpx.GroupWrite: {PerIP: unlimited},
	}}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return httpx.RouterOptions{
		Clients:   httpx.ClientPolicy{MinAppVersion: appversion.MustParse(testVersion)},
		ClientIPs: httpx.NewClientIPResolver(nil),
		RateLimit: limiter,
		Metrics:   m,
	}
}

// testVersion is the minimum and the test client's app version.
const testVersion = "1.0.0"

// nativeClient sends what Koala's mobile app sends on every request
// unless the test set it: X-Client-Platform and X-App-Version. A request
// with an Origin is a browser's and gets nothing. The device header is
// always the test's choice.
type nativeClient struct{ base http.RoundTripper }

func (n nativeClient) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Origin") == "" {
		req = req.Clone(req.Context())
		if req.Header.Get(httpx.HeaderClientPlatform) == "" {
			req.Header.Set(httpx.HeaderClientPlatform, string(httpx.PlatformIOS))
		}
		if req.Header.Get(httpx.HeaderAppVersion) == "" {
			req.Header.Set(httpx.HeaderAppVersion, testVersion)
		}
	}
	return n.base.RoundTrip(req)
}

// shutdown cancels the server and waits for Serve; it is idempotent.
func (e *env) shutdown() error {
	e.stopOnce.Do(func() {
		e.stop()
		select {
		case e.stopResult = <-e.served:
		case <-time.After(3 * waitTimeout):
			e.stopResult = context.DeadlineExceeded
		}
		e.transport.CloseIdleConnections()
	})
	return e.stopResult
}

// issue requests a ticket as user on device and returns the response
// status, its Retry-After header and the ticket.
func (e *env) issue(t *testing.T, user realtime.UserID, device string) (status int, retryAfter, ticket string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+realtime.PathTickets, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", string(user))
	req.Header.Set(realtime.HeaderDeviceID, deviceID(device))
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body realtime.TicketResponse
	if resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), body.Ticket
}

// ticket gets a ticket over REST as user on device.
func (e *env) ticket(t *testing.T, user realtime.UserID, device string) string {
	t.Helper()
	status, _, ticket := e.issue(t, user, device)
	if status != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201", realtime.PathTickets, status)
	}
	return ticket
}

// protocols is the Sec-WebSocket-Protocol list a client sends.
func protocols(ticket string) []string {
	return []string{realtime.Subprotocol, realtime.TicketProtocolPrefix + ticket}
}

// deviceHeader is the handshake header of a native client on a named
// device.
func deviceHeader(device string) http.Header {
	return http.Header{realtime.HeaderDeviceID: {deviceID(device)}}
}

// dialSpec describes one handshake.
type dialSpec struct {
	protocols []string
	header    http.Header
	// ignorePings makes the client drop pings instead of answering with
	// a pong, like a peer that has gone away.
	ignorePings bool
	// paused leaves the peer's reader stopped until peer.resume, like a
	// client that does not keep up.
	paused bool
}

// dialResult is a handshake's outcome: a peer, or the HTTP status,
// Retry-After and problem code of the rejection.
type dialResult struct {
	peer       *peer
	status     int
	retryAfter string
	code       apperr.Code
	err        error
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
		res.retryAfter = resp.Header.Get("Retry-After")
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
		res.peer = newPeer(t, conn, !spec.paused)
	}
	return res
}

// connect opens an authenticated native connection for user on device and
// reads its hello event.
func (e *env) connect(t *testing.T, user realtime.UserID, device string) *peer {
	t.Helper()
	res := e.dial(t, dialSpec{protocols: protocols(e.ticket(t, user, device)), header: deviceHeader(device)})
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
	started  sync.Once
	done     chan struct{}
	status   websocket.StatusCode // valid after done is closed
}

func newPeer(t *testing.T, conn *websocket.Conn, start bool) *peer {
	t.Helper()
	conn.SetReadLimit(1 << 20)
	p := &peer{conn: conn, events: make(chan realtime.Event, 1024), done: make(chan struct{})}
	if start {
		p.resume()
	}
	t.Cleanup(func() {
		_ = conn.CloseNow()
		p.resume() // a paused reader must still run to completion
		<-p.done
	})
	return p
}

// resume starts the background reader once.
func (p *peer) resume() {
	p.started.Do(func() { go p.readAll() })
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
// the close code. Events still queued stay readable through remaining.
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

// remaining returns the types of the events received but not yet
// consumed. Call it after closeStatus.
func (p *peer) remaining() []realtime.EventType {
	var types []realtime.EventType
	for {
		select {
		case ev := <-p.events:
			types = append(types, ev.Type)
		default:
			return types
		}
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
