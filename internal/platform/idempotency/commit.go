package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/idempotency/idempotencydb"
)

// claim is one attempt's hold on a key, carried in the request's context
// from the middleware to the operation's transaction.
type claim struct {
	store     *Store
	id        rowID
	attempt   uuid.UUID
	canonical []byte

	// Set by Commit: the operation stored its response in its own
	// transaction, and the digest of what it stored; or the response was
	// over maxStoredBody and Commit kept nothing (oversized).
	committed bool
	oversized bool
	digest    [sha256.Size]byte
}

// Problem codes of the store (ADR-0005 madde 6, ADR-0034 Karar 7). They
// are universal to the operations Operations lists, and only to them: a
// domain package does not return them, and an operation without the key
// neither lists them nor declares 422 (archtest, app tests).
const (
	// CodeIdempotencyKeyReused: the key was first used with another
	// request (422).
	CodeIdempotencyKeyReused apperr.Code = "IDEMPOTENCY_KEY_REUSED"
	// CodeIdempotencyKeyInUse: a request with the key is still running
	// (409); the client retries later with the same key.
	CodeIdempotencyKeyInUse apperr.Code = "IDEMPOTENCY_KEY_IN_USE"
)

// Codes lists the problem codes of the store.
func Codes() []apperr.Code {
	return []apperr.Code{CodeIdempotencyKeyReused, CodeIdempotencyKeyInUse}
}

type claimKey struct{}

func withClaim(ctx context.Context, c *claim) context.Context {
	return context.WithValue(ctx, claimKey{}, c)
}

func claimFrom(ctx context.Context) (*claim, bool) {
	c, ok := ctx.Value(claimKey{}).(*claim)
	return c, ok && c != nil
}

// Commit stores the operation's success response in the operation's own
// transaction q, so the record that the request happened commits with its
// business data or not at all (ADR-0034 Karar 4; a commit is ambiguous,
// ADR-0017). render writes the response exactly as the handler will
// return it: the generated Visit…Response method of the response object.
// Call it last in the transaction.
//
// The write is fenced by the attempt that claimed the key: when another
// attempt took the key over (this one outlived its lease), it changes no
// row and Commit returns 409 IDEMPOTENCY_KEY_IN_USE, which rolls the
// transaction back. Without a claim in ctx (a request that took no key: a
// job, a test of the service) it does nothing.
func Commit(ctx context.Context, q db.Querier, render func(http.ResponseWriter) error) error {
	c, ok := claimFrom(ctx)
	if !ok {
		return nil
	}
	return c.commit(ctx, q, render)
}

func (c *claim) commit(ctx context.Context, q db.Querier, render func(http.ResponseWriter) error) error {
	if c.committed || c.oversized {
		return errors.New("idempotency: a response was committed twice for one request")
	}
	rec := newRecorder()
	if err := render(rec); err != nil {
		return fmt.Errorf("idempotency: render the response: %w", err)
	}
	resp := rec.stored()
	if !success(resp.Status) {
		return fmt.Errorf("idempotency: a %d response cannot commit; only a success is stored in the operation's transaction", resp.Status)
	}
	if rec.overflow {
		// Too large to keep: the operation commits, the key keeps no
		// response and the middleware frees it (ADR-0034 Karar 5).
		c.oversized = true
		return nil
	}
	sealed, plain, err := c.store.sealResponse(c, resp)
	if err != nil {
		return err
	}
	n, err := idempotencydb.New(q).CompleteKey(ctx, idempotencydb.CompleteKeyParams{
		Actor: c.id.actor, Operation: c.id.operation, Key: c.id.key, Attempt: c.attempt,
		Response: sealed, CompletedAt: ptr(c.store.clock.Now()),
	})
	if err != nil {
		return fmt.Errorf("idempotency: commit the response: %w", err)
	}
	if n == 0 {
		// Another attempt holds the key now; this one must not commit.
		return apperr.New(apperr.KindConflict, CodeIdempotencyKeyInUse)
	}
	c.committed, c.digest = true, sha256.Sum256(plain)
	return nil
}

// success reports a 2xx status.
func success(status int) bool { return status >= 200 && status <= 299 }

// storedHeaders are the response headers a key keeps: the ones that
// describe the representation. Per-request headers (X-Request-Id, rate
// limit, Retry-After) are the replay's own; Cache-Control is always
// no-store on a replay.
func storedHeaders() []string {
	return []string{"Content-Type", "Content-Language", "Location", "ETag", "Last-Modified"}
}

// keptHeaders is the stored part of h.
func keptHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	for _, k := range storedHeaders() {
		if v := h.Values(k); len(v) > 0 {
			out[k] = slices.Clone(v)
		}
	}
	return out
}

// recorder is the ResponseWriter Commit renders into. It keeps at most
// maxStoredBody bytes of the body; past that it only notes the overflow.
type recorder struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func newRecorder() *recorder { return &recorder{header: http.Header{}} }

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.overflow || r.body.Len()+len(p) > maxStoredBody {
		r.overflow = true
		r.body.Reset()
		return len(p), nil
	}
	n, err := r.body.Write(p)
	if err != nil {
		return n, fmt.Errorf("idempotency: record the body: %w", err)
	}
	return n, nil
}

func (r *recorder) stored() storedResponse {
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	return storedResponse{Status: status, Header: keptHeaders(r.header), Body: bytes.Clone(r.body.Bytes())}
}
