package realtime_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/jsontime"
	"github.com/is-koala/koala-api/internal/realtime"
)

// TestWireFormatGolden pins the JSON of the envelope and every payload
// byte for byte (CLAUDE.md: wire formats get golden tests).
func TestWireFormatGolden(t *testing.T) {
	t.Parallel()

	// Set in Istanbul time: the wire type writes it in UTC with Z.
	at := jsontime.From(time.Date(2026, 11, 5, 12, 30, 0, 123_000_000, time.FixedZone("TRT", 3*60*60)))
	tests := []struct {
		name string
		v    any
		want string
	}{
		{
			"event",
			realtime.Event{
				Type: realtime.EventTyping, ID: "EVT1", OccurredAt: at,
				Data: json.RawMessage(`{"conversationId":"c1","userId":"u1"}`),
			},
			`{"type":"conversation.typing","id":"EVT1","occurredAt":"2026-11-05T09:30:00.123Z","data":{"conversationId":"c1","userId":"u1"}}`,
		},
		{
			"command",
			realtime.Command{Type: realtime.CommandTyping, ID: "c-1", Data: json.RawMessage(`{"conversationId":"c1"}`)},
			`{"type":"typing","id":"c-1","data":{"conversationId":"c1"}}`,
		},
		{
			"hello",
			realtime.HelloData{ConnectionID: "CONN1", HeartbeatIntervalMs: 25000, MaxMessageBytes: 65536},
			`{"connectionId":"CONN1","heartbeatIntervalMs":25000,"maxMessageBytes":65536}`,
		},
		{
			"error with command id",
			realtime.ErrorData{Code: realtime.CodeCommandUnknown, CommandID: "c-1"},
			`{"code":"REALTIME_COMMAND_UNKNOWN","commandId":"c-1"}`,
		},
		{
			"error without command id",
			realtime.ErrorData{Code: realtime.CodeRateLimited},
			`{"code":"REALTIME_RATE_LIMITED"}`,
		},
		{
			"typing",
			realtime.TypingData{ConversationID: "c1", UserID: "u1"},
			`{"conversationId":"c1","userId":"u1"}`,
		},
		{
			"ticket",
			realtime.TicketResponse{Ticket: "abc", ExpiresAt: at},
			`{"ticket":"abc","expiresAt":"2026-11-05T09:30:00.123Z"}`,
		},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.v)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if string(got) != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
}
