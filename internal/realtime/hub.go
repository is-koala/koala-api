package realtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// Conversations answers who takes part in a conversation. The realtime
// layer never decides membership itself; Faz 4 implements this on authz
// (ADR-0014).
type Conversations interface {
	IsParticipant(ctx context.Context, conversation ConversationID, user UserID) (bool, error)
	Participants(ctx context.Context, conversation ConversationID) ([]UserID, error)
}

// Options are the per-connection limits (ADR-0011).
type Options struct {
	// MaxMessageBytes caps one client message; a larger one closes the
	// connection with 1009.
	MaxMessageBytes int64
	// SendBuffer is how many events may wait for a slow client before the
	// connection is closed with 1013.
	SendBuffer int
	// WriteTimeout bounds writing one event.
	WriteTimeout time.Duration
	// PingInterval is the keepalive period. It stays below common proxy
	// idle timeouts (60 s).
	PingInterval time.Duration
	// IdleTimeout closes a connection that sent nothing, not even a pong,
	// for this long. It must exceed PingInterval.
	IdleTimeout time.Duration
	// CommandRate (per second) and CommandBurst form the command token
	// bucket of one connection.
	CommandRate  float64
	CommandBurst int
	// MaxRateStrikes is how many commands in a row may hit the rate limit
	// before the connection is closed with 1008. The first one is answered
	// with REALTIME_RATE_LIMITED; the rest are dropped silently.
	MaxRateStrikes int
	// MaxConnsPerUser caps a user's concurrent connections (devices, tabs).
	MaxConnsPerUser int
}

// DefaultOptions returns the production limits.
func DefaultOptions() Options {
	return Options{
		MaxMessageBytes: 64 << 10,
		SendBuffer:      64,
		WriteTimeout:    10 * time.Second,
		PingInterval:    25 * time.Second,
		IdleTimeout:     60 * time.Second,
		CommandRate:     5,
		CommandBurst:    10,
		MaxRateStrikes:  20,
		MaxConnsPerUser: 10,
	}
}

func (o *Options) validate() error {
	var errs []error
	if o.MaxMessageBytes <= 0 || o.SendBuffer <= 0 || o.CommandBurst <= 0 ||
		o.MaxRateStrikes <= 0 || o.MaxConnsPerUser <= 0 || o.CommandRate <= 0 {
		errs = append(errs, errors.New("limits must be positive"))
	}
	if o.WriteTimeout <= 0 || o.PingInterval <= 0 {
		errs = append(errs, errors.New("timeouts must be positive"))
	}
	if o.IdleTimeout <= o.PingInterval {
		errs = append(errs, errors.New("IdleTimeout must exceed PingInterval"))
	}
	if len(errs) > 0 {
		return fmt.Errorf("realtime options: %w", errors.Join(errs...))
	}
	return nil
}

// commandFunc runs one decoded command for client c. A returned
// *apperr.Error is sent to the client as an error event; any other error
// is logged and sent as INTERNAL.
type commandFunc func(ctx context.Context, c *client, data json.RawMessage) error

// Hub holds the open connections of this process and fans events out to
// them. It decides every recipient itself: by user for PublishUser, by
// Conversations for PublishConversation. It implements httpx.Stream.
type Hub struct {
	conversations Conversations
	clock         Clock
	logger        *slog.Logger
	opts          Options
	commands      map[CommandType]commandFunc

	mu       sync.Mutex
	users    map[UserID]map[*client]struct{}
	draining bool
	// conns counts the connection handlers that joined; Shutdown waits for
	// it.
	conns sync.WaitGroup
}

// NewHub returns an empty hub.
func NewHub(conversations Conversations, clock Clock, logger *slog.Logger, opts Options) (*Hub, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	h := &Hub{
		conversations: conversations,
		clock:         clock,
		logger:        logger,
		opts:          opts,
		users:         make(map[UserID]map[*client]struct{}),
	}
	h.commands = map[CommandType]commandFunc{
		CommandTyping: h.typing,
	}
	return h, nil
}

// PublishUser pushes an event to every connection of user.
func (h *Hub) PublishUser(user UserID, typ EventType, data any) error {
	msg, err := h.encode(typ, data)
	if err != nil {
		return err
	}
	h.fanout([]UserID{user}, "", msg)
	return nil
}

// PublishConversation pushes an event to every connection of every
// participant of conversation, as Conversations reports them.
func (h *Hub) PublishConversation(ctx context.Context, conversation ConversationID, typ EventType, data any) error {
	users, err := h.conversations.Participants(ctx, conversation)
	if err != nil {
		return fmt.Errorf("realtime: participants of %s: %w", conversation, err)
	}
	msg, err := h.encode(typ, data)
	if err != nil {
		return err
	}
	h.fanout(users, "", msg)
	return nil
}

// DisconnectSession closes the connections of an ended session with
// CloseSessionRevoked. Faz 1's logout and revocation call it, so
// revocation also ends open WebSockets (ADR-0015 §2).
func (h *Hub) DisconnectSession(user UserID, session string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.users[user] {
		if c.principal.SessionID == session {
			c.requestClose(CloseSessionRevoked, "session revoked")
		}
	}
}

// StartDraining refuses new connections from now on with 503
// REALTIME_DRAINING. Open connections stay up until Shutdown.
func (h *Hub) StartDraining() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.draining = true
}

// forceCloseWait bounds the wait after Shutdown has canceled the
// remaining connections.
const forceCloseWait = 5 * time.Second

// Shutdown closes every connection with 1001 (going away) and waits until
// their goroutines have returned. When ctx ends first, the remaining
// connections are cut without a close frame and ctx's error is returned.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.draining = true
	open := h.snapshot()
	h.mu.Unlock()
	for _, c := range open {
		c.requestClose(websocket.StatusGoingAway, "server shutting down")
	}

	done := make(chan struct{})
	go func() {
		h.conns.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}

	h.mu.Lock()
	open = h.snapshot()
	h.mu.Unlock()
	for _, c := range open {
		c.kill()
	}
	t := time.NewTimer(forceCloseWait)
	defer t.Stop()
	select {
	case <-done:
		return fmt.Errorf("realtime: shutdown cut %d connections: %w", len(open), ctx.Err())
	case <-t.C:
		return fmt.Errorf("realtime: shutdown: %d connections did not stop: %w", len(open), ctx.Err())
	}
}

// snapshot returns every open connection. h.mu must be held.
func (h *Hub) snapshot() []*client {
	var all []*client
	for _, conns := range h.users {
		for c := range conns {
			all = append(all, c)
		}
	}
	return all
}

func (h *Hub) isDraining() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining
}

// join registers c and queues hello as its first event. On success the
// caller must call leave.
func (h *Hub) join(c *client, hello []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return apperr.New(apperr.KindUnavailable, CodeDraining)
	}
	conns := h.users[c.principal.UserID]
	if len(conns) >= h.opts.MaxConnsPerUser {
		return apperr.New(apperr.KindRateLimited, CodeTooManyConnections)
	}
	if conns == nil {
		conns = make(map[*client]struct{})
		h.users[c.principal.UserID] = conns
	}
	conns[c] = struct{}{}
	c.enqueue(hello)
	h.conns.Add(1)
	return nil
}

func (h *Hub) leave(c *client) {
	h.mu.Lock()
	if conns := h.users[c.principal.UserID]; conns != nil {
		delete(conns, c)
		if len(conns) == 0 {
			delete(h.users, c.principal.UserID)
		}
	}
	h.mu.Unlock()
	h.conns.Done()
}

// fanout queues msg on every connection of users except those of except.
// It holds h.mu for the whole loop, so two fanouts never interleave and
// every connection sees events in publish order.
func (h *Hub) fanout(users []UserID, except UserID, msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[UserID]struct{}, len(users))
	for _, u := range users {
		if _, dup := seen[u]; dup || u == except {
			continue
		}
		seen[u] = struct{}{}
		for c := range h.users[u] {
			c.enqueue(msg)
		}
	}
}

// encode builds an event envelope with a fresh id and the current time.
func (h *Hub) encode(typ EventType, data any) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("realtime: encode %s data: %w", typ, err)
	}
	msg, err := json.Marshal(Event{
		Type:       typ,
		ID:         rand.Text(),
		OccurredAt: h.clock.Now().UTC(),
		Data:       raw,
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: encode %s: %w", typ, err)
	}
	return msg, nil
}

// dispatch decodes one client message and runs its command. Unknown and
// malformed commands are answered with an error event; the connection
// stays open.
func (h *Hub) dispatch(ctx context.Context, c *client, raw []byte) {
	var cmd Command
	if err := decodeStrict(raw, &cmd); err != nil || !validClientID(cmd.ID, false) {
		h.sendError(ctx, c, "", apperr.New(apperr.KindInvalid, CodeCommandInvalid))
		return
	}
	run, ok := h.commands[cmd.Type]
	if !ok {
		h.sendError(ctx, c, cmd.ID, apperr.New(apperr.KindInvalid, CodeCommandUnknown))
		return
	}
	if err := run(ctx, c, cmd.Data); err != nil {
		h.sendError(ctx, c, cmd.ID, err)
	}
}

// sendError answers a command with an error event.
func (h *Hub) sendError(ctx context.Context, c *client, commandID string, err error) {
	e := apperr.From(err)
	code := e.Code
	if e.Kind == apperr.KindInternal || code == "" {
		h.logger.ErrorContext(ctx, "realtime command failed",
			slog.String("connection_id", c.id),
			slog.Any("error", err))
		code = apperr.CodeInternal
	}
	msg, encErr := h.encode(EventError, ErrorData{Code: code, CommandID: commandID})
	if encErr != nil {
		h.logger.ErrorContext(ctx, "realtime error event failed", slog.Any("error", encErr))
		return
	}
	c.enqueue(msg)
}

// typing handles CommandTyping: the sender must take part in the
// conversation; the other participants get EventTyping with the
// sender's authenticated user id.
func (h *Hub) typing(ctx context.Context, c *client, data json.RawMessage) error {
	var cmd TypingCommand
	if err := decodeStrict(data, &cmd); err != nil || !validClientID(string(cmd.ConversationID), true) {
		return apperr.New(apperr.KindInvalid, CodeCommandInvalid)
	}
	ok, err := h.conversations.IsParticipant(ctx, cmd.ConversationID, c.principal.UserID)
	if err != nil {
		return fmt.Errorf("typing: participant check: %w", err)
	}
	if !ok {
		return apperr.New(apperr.KindNotFound, CodeConversationNotFound)
	}
	users, err := h.conversations.Participants(ctx, cmd.ConversationID)
	if err != nil {
		return fmt.Errorf("typing: participants: %w", err)
	}
	msg, err := h.encode(EventTyping, TypingData{ConversationID: cmd.ConversationID, UserID: c.principal.UserID})
	if err != nil {
		return err
	}
	h.fanout(users, c.principal.UserID, msg)
	return nil
}

// decodeStrict decodes exactly one JSON value into v and rejects unknown
// fields and trailing data, so a client cannot smuggle fields such as a
// sender id or a timestamp into a command.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode command: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("decode command: trailing data")
	}
	return nil
}

const maxClientIDLen = 64

// validClientID accepts ids a client chooses or sends: short, and only
// characters that are safe to echo and to log.
func validClientID(id string, required bool) bool {
	if id == "" {
		return !required
	}
	if len(id) > maxClientIDLen {
		return false
	}
	for _, c := range []byte(id) {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}
