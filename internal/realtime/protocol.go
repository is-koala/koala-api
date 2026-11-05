// Package realtime is the WebSocket layer of ADR-0011: a short-lived,
// single-use ticket from REST authenticates the handshake, the server
// pushes JSON events it decided on, and clients only send typed commands.
//
// Clients never choose a topic. Every event a connection receives was
// addressed by the server to that connection's user, and every client
// command is decoded into a closed set of command types and turned into a
// new, server-built event. A client therefore cannot forge a server event
// for anyone, which was possible with Java's STOMP broker (plan §1.7,
// N01 §8.3).
//
// Fanout is in memory, so one process serves all connections (plan §9
// R9). Running several instances needs a cross-instance channel such as
// Postgres LISTEN/NOTIFY; see ADR-0018.
package realtime

import (
	"encoding/json"
	"time"

	"github.com/coder/websocket"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// Protocol identifiers. The protocol version is the WebSocket subprotocol:
// a breaking change ships as koala.v2 next to koala.v1. Within a version
// only additive changes are allowed (new event types, new optional
// fields); clients ignore event types and fields they do not know.
const (
	// Subprotocol is the only protocol the server speaks.
	Subprotocol = "koala.v1"
	// TicketProtocolPrefix carries the ticket in the client's
	// Sec-WebSocket-Protocol list: "koala.v1, koala.ticket.<ticket>". The
	// server answers with koala.v1 only and never echoes the ticket.
	TicketProtocolPrefix = "koala.ticket."
)

// Route paths.
const (
	PathTickets = "/v1/realtime/tickets"
	PathConnect = "/v1/realtime"
)

// EventType names a server-to-client event. Event types and command types
// are separate Go types so a command can never be sent as an event.
type EventType string

// Event types of this package. Domain modules add their own (Faz 4:
// message.created, message.read, presence.changed).
const (
	// EventHello is the first event of every connection.
	EventHello EventType = "hello"
	// EventError answers a command that was rejected. The connection stays
	// open.
	EventError EventType = "error"
	// EventTyping tells the other participants that a user is typing.
	EventTyping EventType = "conversation.typing"
)

// CommandType names a client-to-server command.
type CommandType string

// Command types. ADR-0011 also lists presence.watch, which arrives with
// presence in Faz 4; until then it is an unknown command.
const (
	CommandTyping CommandType = "typing"
)

// UserID identifies a user.
type UserID string

// ConversationID identifies a conversation.
type ConversationID string

// Event is the server-to-client envelope. id is unique per event; clients
// deduplicate by it and fill gaps after a reconnect from REST with
// ?since= (ADR-0011).
type Event struct {
	Type       EventType       `json:"type"`
	ID         string          `json:"id"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// Command is the client-to-server envelope. id is optional and chosen by
// the client; an error event echoes it as commandId. There is no
// timestamp: the server does not take time from clients.
type Command struct {
	Type CommandType     `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// HelloData is the data of EventHello.
type HelloData struct {
	ConnectionID        string `json:"connectionId"`
	HeartbeatIntervalMs int64  `json:"heartbeatIntervalMs"`
	MaxMessageBytes     int64  `json:"maxMessageBytes"`
}

// ErrorData is the data of EventError. Code comes from the same set as
// the REST problem codes.
type ErrorData struct {
	Code      apperr.Code `json:"code"`
	CommandID string      `json:"commandId,omitempty"`
}

// TypingCommand is the data of CommandTyping.
type TypingCommand struct {
	ConversationID ConversationID `json:"conversationId"`
}

// TypingData is the data of EventTyping. UserID is the authenticated
// sender, never a value the client sent.
type TypingData struct {
	ConversationID ConversationID `json:"conversationId"`
	UserID         UserID         `json:"userId"`
}

// Error codes of the realtime layer.
const (
	CodeTicketInvalid       apperr.Code = "REALTIME_TICKET_INVALID"
	CodeTicketRateLimited   apperr.Code = "REALTIME_TICKET_RATE_LIMITED"
	CodeDeviceRequired      apperr.Code = "REALTIME_DEVICE_REQUIRED"
	CodeProtocolUnsupported apperr.Code = "REALTIME_PROTOCOL_UNSUPPORTED"
	CodeOriginForbidden     apperr.Code = "REALTIME_ORIGIN_FORBIDDEN"
	CodeDraining            apperr.Code = "REALTIME_DRAINING"
	CodeTooManyConnections  apperr.Code = "REALTIME_TOO_MANY_CONNECTIONS"
	CodeCommandUnknown      apperr.Code = "REALTIME_COMMAND_UNKNOWN"
	CodeCommandInvalid      apperr.Code = "REALTIME_COMMAND_INVALID"
	CodeRateLimited         apperr.Code = "REALTIME_RATE_LIMITED"
	// CodeConversationNotFound answers both a missing conversation and one
	// the user is not part of (ADR-0014: no existence leak). It moves to
	// the messaging module in Faz 4.
	CodeConversationNotFound apperr.Code = "CONVERSATION_NOT_FOUND"
)

// CloseSessionRevoked closes the connections of a session that ended
// (logout, revocation). It is a private-use code (RFC 6455 §7.4.2) and
// tells the client to authenticate again instead of reconnecting. The
// other close codes are standard; see ADR-0011 for the full table.
const CloseSessionRevoked websocket.StatusCode = 4401
