package realtime

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// HeaderDeviceID is the device header of ADR-0015. Native clients send it
// on the handshake too; browsers cannot.
const HeaderDeviceID = "X-Device-Id"

// Authenticator resolves the caller of a REST request. Faz 1's session
// middleware provides it; an unauthenticated request yields an
// *apperr.Error of KindUnauthenticated.
type Authenticator interface {
	Principal(r *http.Request) (Principal, error)
}

// TicketResponse is the body of POST /v1/realtime/tickets.
type TicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Handler serves the realtime routes.
type Handler struct {
	hub     *Hub
	tickets *Tickets
	auth    Authenticator
	logger  *slog.Logger
	// origins are the browser origins allowed to open a WebSocket, as
	// exact "scheme://host[:port]" values.
	origins []string
}

// NewHandler returns the realtime HTTP handler. allowedOrigins lists the
// web origins (for example https://iskoala.com) that may connect from a
// browser; requests without an Origin header (native clients) are allowed.
func NewHandler(hub *Hub, tickets *Tickets, auth Authenticator, logger *slog.Logger, allowedOrigins []string) (*Handler, error) {
	for _, o := range allowedOrigins {
		if err := validateOrigin(o); err != nil {
			return nil, err
		}
	}
	return &Handler{
		hub:     hub,
		tickets: tickets,
		auth:    auth,
		logger:  logger,
		origins: append([]string(nil), allowedOrigins...),
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
	t, err := h.tickets.Issue(r.Context(), p)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	body, err := json.Marshal(TicketResponse{Ticket: t.Value, ExpiresAt: t.ExpiresAt.UTC()})
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
//	WebSocket upgrade + koala.v1 → origin → not draining → ticket → join → upgrade
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
	if h.hub.isDraining() {
		httpx.WriteError(w, r, h.logger, apperr.New(apperr.KindUnavailable, CodeDraining))
		return
	}
	p, err := h.tickets.Redeem(r.Context(), ticket, r.Header.Get(HeaderDeviceID))
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	h.hub.serve(w, r, p, h.origins)
}

// handshakeTicket checks that the request is a version 13 WebSocket
// upgrade offering koala.v1 and returns the ticket from its
// Sec-WebSocket-Protocol list.
func handshakeTicket(hdr http.Header) (string, error) {
	if !headerHasToken(hdr, "Connection", "upgrade") || !headerHasToken(hdr, "Upgrade", "websocket") ||
		hdr.Get("Sec-WebSocket-Version") != "13" {
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

// originAllowed applies the same rule as websocket.Accept with
// OriginPatterns set to exact origins, before the ticket is spent: no
// Origin header (native client), the request's own host, or a listed
// origin.
func (h *Handler) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, o := range h.origins {
		if strings.EqualFold(o, u.Scheme+"://"+u.Host) {
			return true
		}
	}
	return false
}

// validateOrigin accepts "scheme://host[:port]" with no path and no
// pattern characters (websocket.Accept would treat those as globs).
func validateOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		strings.ContainsAny(o, `*?[]\`) {
		return fmt.Errorf("realtime: allowed origin %q: want scheme://host[:port]", o)
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
