package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/jsontime"
)

// HeaderDeviceID is the device header of ADR-0015. Native clients must
// send it on the handshake too; browsers cannot.
const HeaderDeviceID = httpx.HeaderDeviceID

// Retry-After values (seconds) of refused handshakes.
const (
	retryAfterDraining = "5"
	retryAfterBusy     = "30"
)

// Authenticator resolves the caller of a REST request. Faz 1's session
// middleware provides it; an unauthenticated request yields an
// *apperr.Error of KindUnauthenticated.
type Authenticator interface {
	Principal(r *http.Request) (Principal, error)
}

// TicketResponse is the body of POST /v1/realtime/tickets (the contract's
// RealtimeTicket; api.TestRealtimeTicketMatchesContract compares them).
type TicketResponse struct {
	Ticket    string       `json:"ticket"`
	ExpiresAt jsontime.UTC `json:"expiresAt"`
}

// HandlerOptions configure the HTTP side of the realtime layer.
type HandlerOptions struct {
	// AllowedOrigins are the web origins (https://host[:port]) that may
	// connect from a browser. Requests without an Origin header are
	// native clients and must send X-Device-Id instead.
	AllowedOrigins []string
	// TicketInterval and TicketBurst limit ticket issuance per user: a
	// burst of TicketBurst, then one per TicketInterval.
	TicketInterval time.Duration
	TicketBurst    int
}

// DefaultHandlerOptions returns the production ticket limits: a burst of
// 5, then 10 per minute, which covers reconnect backoff with room to
// spare. Origins come from configuration.
func DefaultHandlerOptions() HandlerOptions {
	return HandlerOptions{TicketInterval: 6 * time.Second, TicketBurst: 5}
}

// Handler serves the realtime routes.
type Handler struct {
	hub     *Hub
	tickets *Tickets
	auth    Authenticator
	logger  *slog.Logger
	origins []string
	limiter *ticketLimiter
}

// NewHandler returns the realtime HTTP handler.
func NewHandler(hub *Hub, tickets *Tickets, auth Authenticator, logger *slog.Logger, opts HandlerOptions) (*Handler, error) {
	for _, o := range opts.AllowedOrigins {
		if err := validateOrigin(o); err != nil {
			return nil, err
		}
	}
	if opts.TicketInterval <= 0 || opts.TicketBurst <= 0 {
		return nil, errors.New("realtime: ticket limits must be positive")
	}
	return &Handler{
		hub:     hub,
		tickets: tickets,
		auth:    auth,
		logger:  logger,
		origins: append([]string(nil), opts.AllowedOrigins...),
		limiter: newTicketLimiter(rate.Every(opts.TicketInterval), opts.TicketBurst),
	}, nil
}

// Mount registers the ticket endpoint as a bounded route and the
// WebSocket endpoint as a stream route.
func (h *Handler) Mount(r *httpx.Router) {
	r.Post(PathTickets, h.IssueTicket)
	r.Stream(PathConnect, h.Connect)
}

// IssueTicket handles POST /v1/realtime/tickets: it returns a ticket bound
// to the caller's device session, valid for TicketTTL and one handshake.
func (h *Handler) IssueTicket(w http.ResponseWriter, r *http.Request) {
	p, err := h.auth.Principal(r)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	if ok, wait := h.limiter.allow(p.UserID, h.tickets.clock.Now()); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		httpx.WriteError(w, r, h.logger, apperr.New(apperr.KindRateLimited, CodeTicketRateLimited))
		return
	}
	t, err := h.tickets.Issue(r.Context(), p)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	body, err := json.Marshal(TicketResponse{Ticket: t.Value, ExpiresAt: jsontime.From(t.ExpiresAt)})
	if err != nil {
		httpx.WriteError(w, r, h.logger, fmt.Errorf("realtime: encode ticket: %w", err))
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write(body); err != nil {
		h.logger.DebugContext(r.Context(), "write ticket response", slog.Any("error", err))
	}
}

// Connect handles GET /v1/realtime. Everything that can fail is checked
// before the ticket is redeemed, and the ticket before the upgrade, so a
// rejected handshake is a plain HTTP problem and never an open socket:
//
//	upgrade headers + koala.v1 → origin → device → not draining → ticket → join → session → upgrade
func (h *Handler) Connect(w http.ResponseWriter, r *http.Request) {
	ticket, err := handshakeTicket(r.Header)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	if !h.originAllowed(r) {
		httpx.WriteError(w, r, h.logger, apperr.New(apperr.KindForbidden, CodeOriginForbidden))
		return
	}
	device := r.Header.Get(HeaderDeviceID)
	if device == "" && r.Header.Get("Origin") == "" {
		// No Origin: a native client, which can and must name its device.
		httpx.WriteError(w, r, h.logger, apperr.New(apperr.KindUnauthenticated, CodeDeviceRequired))
		return
	}
	if h.hub.isDraining() {
		writeHandshakeError(w, r, h.logger, apperr.New(apperr.KindUnavailable, CodeDraining))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.hub.opts.HandshakeTimeout)
	p, err := h.tickets.Redeem(ctx, ticket, device)
	cancel()
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	h.hub.serve(w, r, p, h.origins)
}

// writeHandshakeError writes err and, for a refusal the client should
// retry later (503 drain, 429 connection cap), a Retry-After header.
func writeHandshakeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	kind := apperr.From(err).Kind
	if kind == apperr.KindUnavailable {
		w.Header().Set("Retry-After", retryAfterDraining)
	}
	if kind == apperr.KindRateLimited {
		w.Header().Set("Retry-After", retryAfterBusy)
	}
	httpx.WriteError(w, r, logger, err)
}

// handshakeTicket checks that the request is a well-formed version 13
// WebSocket upgrade offering koala.v1 and returns the ticket from its
// Sec-WebSocket-Protocol list. websocket.Accept checks the same headers,
// but only after the ticket would have been spent.
func handshakeTicket(hdr http.Header) (string, error) {
	if !headerHasToken(hdr, "Connection", "upgrade") || !headerHasToken(hdr, "Upgrade", "websocket") ||
		hdr.Get("Sec-WebSocket-Version") != "13" || !validWebSocketKey(hdr.Values("Sec-WebSocket-Key")) {
		return "", apperr.New(apperr.KindInvalid, CodeProtocolUnsupported)
	}
	var (
		hasVersion bool
		tickets    []string
	)
	for _, p := range headerTokens(hdr, "Sec-WebSocket-Protocol") {
		switch {
		case p == Subprotocol:
			hasVersion = true
		case strings.HasPrefix(p, TicketProtocolPrefix):
			tickets = append(tickets, strings.TrimPrefix(p, TicketProtocolPrefix))
		}
	}
	if !hasVersion {
		return "", apperr.New(apperr.KindInvalid, CodeProtocolUnsupported)
	}
	if len(tickets) != 1 {
		return "", apperr.New(apperr.KindUnauthenticated, CodeTicketInvalid)
	}
	return tickets[0], nil
}

// validWebSocketKey applies RFC 6455 §4.2.1: exactly one key, the base64
// encoding of 16 bytes.
func validWebSocketKey(keys []string) bool {
	if len(keys) != 1 {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))
	return err == nil && len(b) == 16
}

// originAllowed applies the rule of websocket.Accept with OriginPatterns
// set to exact origins, before the ticket is spent, and is stricter in one
// point: no Origin header (native client), a listed origin, or the
// request's own host over https. websocket.Accept compares the host only,
// so a page served over plain http on the API's host would pass it; such a
// page can be altered in transit, which is why the list refuses http too
// (ADR-0022). The scheme of the request itself is not known behind the
// TLS-terminating proxy, so https is required outright.
func (h *Handler) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "https" && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, o := range h.origins {
		if strings.EqualFold(o, u.Scheme+"://"+u.Host) {
			return true
		}
	}
	return false
}

// validateOrigin accepts "https://host[:port]" with no path, no explicit
// default port and no pattern characters (websocket.Accept would treat
// those as globs). Plain http is refused: a page served over http can be
// altered in transit.
func validateOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() == "443" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		strings.ContainsAny(o, `*?[]\`) {
		return fmt.Errorf("realtime: allowed origin %q: want https://host[:port]", o)
	}
	return nil
}

func headerTokens(hdr http.Header, key string) []string {
	var tokens []string
	for _, v := range hdr.Values(key) {
		for t := range strings.SplitSeq(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tokens = append(tokens, t)
			}
		}
	}
	return tokens
}

func headerHasToken(hdr http.Header, key, token string) bool {
	for _, t := range headerTokens(hdr, key) {
		if strings.EqualFold(t, token) {
			return true
		}
	}
	return false
}

// ticketLimiter limits ticket issuance per user. Users whose bucket is
// full again are pruned, so memory follows the active users.
type ticketLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	users   map[UserID]*rate.Limiter
	pruneAt int
}

const minTicketLimiterPrune = 1024

func newTicketLimiter(limit rate.Limit, burst int) *ticketLimiter {
	return &ticketLimiter{limit: limit, burst: burst, users: make(map[UserID]*rate.Limiter), pruneAt: minTicketLimiterPrune}
}

// allow takes a token for user at now, or reports how long to wait.
func (l *ticketLimiter) allow(user UserID, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim := l.users[user]
	if lim == nil {
		if len(l.users) >= l.pruneAt {
			l.prune(now)
		}
		lim = rate.NewLimiter(l.limit, l.burst)
		l.users[user] = lim
	}
	r := lim.ReserveN(now, 1)
	if wait := r.DelayFrom(now); wait > 0 {
		r.CancelAt(now)
		return false, wait
	}
	return true, 0
}

func (l *ticketLimiter) prune(now time.Time) {
	for u, lim := range l.users {
		if lim.TokensAt(now) >= float64(l.burst) {
			delete(l.users, u)
		}
	}
	l.pruneAt = max(minTicketLimiterPrune, 2*len(l.users))
}

// retryAfterSeconds renders d as a Retry-After value, rounded up.
func retryAfterSeconds(d time.Duration) string {
	return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10)
}
