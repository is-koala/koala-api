// Package idempotency is the Idempotency-Key store (ADR-0005 madde 6,
// ADR-0034; Stripe's model and draft-ietf-httpapi-idempotency-key-header).
//
// Every operation whose contract takes the Idempotency-Key parameter and
// has a handler is bound by Middleware, from the contract: a new
// operation is bound the moment it loses x-not-implemented. Per (actor,
// operation, key) the store keeps the request's hash and, once it
// finished, its response:
//
//   - the same key with the same request gets the stored response again,
//     byte for byte, marked Idempotent-Replayed: true;
//   - with another request, 422 IDEMPOTENCY_KEY_REUSED;
//   - while the first request runs, 409 IDEMPOTENCY_KEY_IN_USE.
//
// A success is stored by the operation's own transaction (Commit), so the
// business data and the record that it happened commit together or not at
// all; a permanent 4xx after the handler returns; a 5xx or a transient 4xx
// frees the key for a retry.
package idempotency

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency/idempotencydb"
)

// Lease is how long one attempt holds a key without an outcome. It is
// longer than any request can run (httpx.RequestTimeout bounds the
// request's context and with it its transaction), so a key whose lease
// ran out belongs to a request that is gone: a crashed instance, a
// release that did not reach the database. The next request with the
// same key takes it over; the attempt id fences the old one.
const Lease = httpx.RequestTimeout + 35*time.Second

// Retention is how long a key is kept from its first use (Stripe's 24
// hours, M2). Afterwards the key is new again.
const Retention = 24 * time.Hour

// stateCompleted is the state of a key whose response is stored
// (idempotency_keys.state; the other one, IN_PROGRESS, is written by the
// queries).
const stateCompleted = "COMPLETED"

// claimRetries bounds the claim's insert-then-lock loop: the row it
// collided with can be released between the two statements.
const claimRetries = 3

// Store keeps the keys in idempotency_keys.
type Store struct {
	db      *db.DB
	queries *idempotencydb.Queries // outside a transaction
	keys    keys
	clock   clock.Clock
}

// NewStore returns the store on database. secret is the server secret the
// request hash and the response keys are derived from (the login pepper,
// at least tokenkeys.PepperBytes): a new secret makes every stored key
// look reused, which is why it is not rotated within a key's retention.
func NewStore(database *db.DB, secret []byte, clk clock.Clock) (*Store, error) {
	if database == nil || clk == nil {
		return nil, errors.New("idempotency: database and clock are required")
	}
	k, err := deriveKeys(secret)
	if err != nil {
		return nil, err
	}
	return &Store{db: database, queries: idempotencydb.New(database.Pool()), keys: k, clock: clk}, nil
}

// rowID names a key: who used it for which operation.
type rowID struct {
	actor, operation, key string
}

// aad binds a sealed response to its row: a response copied to another
// row does not open.
func (id rowID) aad() []byte {
	return []byte(id.actor + "\x00" + id.operation + "\x00" + id.key)
}

// outcome is what a claim found.
type outcome uint8

const (
	outcomeClaimed outcome = iota // the key is this request's now
	outcomeReplay                 // the same request finished; replay it
	outcomeReused                 // the key was used with another request
	outcomeInUse                  // the same request runs elsewhere
)

type claimResult struct {
	outcome   outcome
	takenOver bool   // claimed from an attempt whose lease ran out
	claim     *claim // outcomeClaimed
	sealed    []byte // outcomeReplay
}

// claim takes the key for this request in a short transaction of its own,
// before the operation runs: insert the row, or lock the one that holds
// the key and decide by it. The request hash is compared before the
// state, so a different request is 422 even while the first one runs.
func (s *Store) claim(ctx context.Context, id rowID, canonical []byte) (claimResult, error) {
	hash := s.keys.hash(canonical)
	now := s.clock.Now()
	attempt := uuid.Must(uuid.NewV7())
	var res claimResult
	err := s.db.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		qs := idempotencydb.New(q)
		for range claimRetries {
			n, err := qs.InsertKey(ctx, idempotencydb.InsertKeyParams{
				Actor: id.actor, Operation: id.operation, Key: id.key, RequestHash: hash, Attempt: attempt,
				LockedUntil: now.Add(Lease), CreatedAt: now, ExpiresAt: now.Add(Retention),
			})
			if err != nil {
				return fmt.Errorf("idempotency: insert the key: %w", err)
			}
			if n == 1 {
				res.outcome = outcomeClaimed
				return nil
			}
			row, err := qs.LockKey(ctx, idempotencydb.LockKeyParams{Actor: id.actor, Operation: id.operation, Key: id.key})
			if errors.Is(err, pgx.ErrNoRows) {
				continue // released after the insert gave way to it
			}
			if err != nil {
				return fmt.Errorf("idempotency: lock the key: %w", err)
			}
			return s.decide(ctx, qs, id, row, hash, attempt, now, &res)
		}
		return errors.New("idempotency: the key changed hands during the claim")
	})
	if err != nil {
		return claimResult{}, fmt.Errorf("idempotency: claim: %w", err)
	}
	if res.outcome == outcomeClaimed {
		res.claim = &claim{store: s, id: id, attempt: attempt, canonical: canonical}
	}
	return res, nil
}

// decide settles a claim on the locked row that holds the key.
func (s *Store) decide(ctx context.Context, qs *idempotencydb.Queries, id rowID, row idempotencydb.LockKeyRow,
	hash []byte, attempt uuid.UUID, now time.Time, res *claimResult,
) error {
	switch {
	case !row.ExpiresAt.After(now):
		// Past its retention but not purged yet: the key is new again.
		err := qs.RestartKey(ctx, idempotencydb.RestartKeyParams{
			Actor: id.actor, Operation: id.operation, Key: id.key, RequestHash: hash, Attempt: attempt,
			LockedUntil: now.Add(Lease), CreatedAt: now, ExpiresAt: now.Add(Retention),
		})
		if err != nil {
			return fmt.Errorf("idempotency: restart the key: %w", err)
		}
		res.outcome = outcomeClaimed
	case !hmac.Equal(row.RequestHash, hash):
		res.outcome = outcomeReused
	case row.State == stateCompleted:
		res.outcome, res.sealed = outcomeReplay, row.Response
	case row.LockedUntil.After(now):
		res.outcome = outcomeInUse
	default:
		err := qs.TakeOverKey(ctx, idempotencydb.TakeOverKeyParams{
			Actor: id.actor, Operation: id.operation, Key: id.key, Attempt: attempt, LockedUntil: now.Add(Lease),
		})
		if err != nil {
			return fmt.Errorf("idempotency: take the key over: %w", err)
		}
		res.outcome, res.takenOver = outcomeClaimed, true
	}
	return nil
}

// storedResponse is what a key keeps of a response: the status, the
// representation headers (storedHeaders) and the body.
type storedResponse struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	Body   []byte              `json:"body,omitempty"`
}

// sealResponse returns resp sealed for c's request and row, and its
// plaintext form.
func (s *Store) sealResponse(c *claim, resp storedResponse) (sealed, plain []byte, err error) {
	plain, err = json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("idempotency: encode the response: %w", err)
	}
	sealed, err = s.keys.seal(c.canonical, c.id.aad(), plain)
	if err != nil {
		return nil, nil, err
	}
	return sealed, plain, nil
}

// openResponse opens a stored response with the replaying request.
func (s *Store) openResponse(id rowID, canonical, sealed []byte) (storedResponse, error) {
	plain, err := s.keys.open(canonical, id.aad(), sealed)
	if err != nil {
		return storedResponse{}, err
	}
	var resp storedResponse
	if err := json.Unmarshal(plain, &resp); err != nil {
		return storedResponse{}, fmt.Errorf("idempotency: decode the stored response: %w", err)
	}
	return resp, nil
}

// complete stores resp for c's attempt outside the operation's
// transaction: the outcome of a request that changed nothing (a 4xx), or
// of an operation that did not commit its own (logged as unbound). false
// when the attempt no longer holds the key.
func (s *Store) complete(ctx context.Context, c *claim, resp storedResponse) (bool, error) {
	sealed, _, err := s.sealResponse(c, resp)
	if err != nil {
		return false, err
	}
	n, err := s.queries.CompleteKey(ctx, idempotencydb.CompleteKeyParams{
		Actor: c.id.actor, Operation: c.id.operation, Key: c.id.key, Attempt: c.attempt,
		Response: sealed, CompletedAt: ptr(s.clock.Now()),
	})
	if err != nil {
		return false, fmt.Errorf("idempotency: store the response: %w", err)
	}
	return n == 1, nil
}

// release frees c's key when its attempt has no outcome to keep. A row the
// operation's transaction completed after all stays: that is the answer to
// an ambiguous commit (ADR-0017), and so does a row another attempt took
// over.
func (s *Store) release(ctx context.Context, c *claim) error {
	_, err := s.queries.ReleaseKey(ctx, idempotencydb.ReleaseKeyParams{
		Actor: c.id.actor, Operation: c.id.operation, Key: c.id.key, Attempt: c.attempt,
	})
	if err != nil {
		return fmt.Errorf("idempotency: release the key: %w", err)
	}
	return nil
}

// Purge hard-deletes the keys past their retention and returns how many
// it removed (the idempotency_purge job, M2).
func (s *Store) Purge(ctx context.Context) (int64, error) {
	n, err := s.queries.PurgeKeys(ctx, s.clock.Now())
	if err != nil {
		return 0, fmt.Errorf("idempotency: purge: %w", err)
	}
	return n, nil
}

func ptr[T any](v T) *T { return &v }
