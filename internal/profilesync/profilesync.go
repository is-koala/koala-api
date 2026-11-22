// Package profilesync is the outbox of the payment provider's profile
// sync (USR-022; ADR-0029 Sonuçlar, ADR-0033 Karar 8, ADR-0036).
//
// Java publishes a user's profile to the payment provider after every
// change, through an outbox row its caller's transaction writes
// (UserProfileSyncManager#enqueue). Go keeps the guarantee: Record writes
// the user's one open intent in the transaction of the change it stands
// for, so the change and the intent to publish it commit together or not
// at all, and a change while an intent is open refreshes that one instead
// of opening a second. The intent holds no profile data: the dispatcher
// reads the profile as it is when it publishes, so an intent cannot go
// stale and carries no personal data of its own beyond who and where the
// change came from.
//
// The dispatcher (claiming an intent, publishing it once, retrying) and
// the provider call come with Faz 6's payment provider client (USR-023);
// until then intents only accumulate, at most one open per user.
package profilesync

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/profilesync/profilesyncdb"
)

// maxOrigin is the longest request id or device key an intent keeps
// (the table's CHECKs); httpx accepts request ids of at most 64 bytes and
// device ids of at most 128.
const maxOrigin = 128

// Origin is where a profile change came from: the request, and the
// session and device of the caller who made it.
type Origin struct {
	RequestID string
	SessionID uuid.UUID
	DeviceKey string
}

// Record writes the user's open profile sync intent in q's transaction,
// the one that writes the change, or refreshes the open one with the time
// and origin of this change (USR-022).
func Record(ctx context.Context, q db.Querier, userID uuid.UUID, origin Origin, now time.Time) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("profilesync: new intent id: %w", err)
	}
	err = profilesyncdb.New(q).RecordIntent(ctx, profilesyncdb.RecordIntentParams{
		ID: id, UserID: userID, Now: now,
		RequestID: optional(origin.RequestID),
		SessionID: uuid.NullUUID{UUID: origin.SessionID, Valid: origin.SessionID != uuid.Nil},
		DeviceKey: optional(origin.DeviceKey),
	})
	if err != nil {
		return fmt.Errorf("profilesync: record the intent: %w", err)
	}
	return nil
}

// optional is s, or nil when it is empty or longer than maxOrigin bytes.
func optional(s string) *string {
	if s == "" || len(s) > maxOrigin {
		return nil
	}
	return &s
}
