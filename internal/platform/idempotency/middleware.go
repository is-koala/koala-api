package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// extNotImplemented marks an operation whose handler has not landed
// (ADR-0029 §1); it answers 501 and is not bound.
const extNotImplemented = "x-not-implemented"

// Operation is an operation whose contract takes the Idempotency-Key
// parameter.
type Operation struct {
	ID, Method, Path string
	// Public: the operation has no caller (security: []); its actor comes
	// from the request (Config.Public).
	Public bool
	// Frozen: x-not-implemented; the store binds it once that goes.
	Frozen bool
}

// Operations lists the operations of spec that take the Idempotency-Key
// header parameter, by operation id. It reads the contract, so the list
// grows by itself with the contract.
func Operations(spec *openapi3.T) ([]Operation, error) {
	if spec == nil || spec.Paths == nil {
		return nil, errors.New("idempotency: spec has no paths")
	}
	var out []Operation
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if !takesKey(item, op) {
				continue
			}
			if op.OperationID == "" {
				return nil, fmt.Errorf("idempotency: %s %s takes an Idempotency-Key but has no operationId", method, path)
			}
			reqs := spec.Security
			if op.Security != nil {
				reqs = *op.Security
			}
			_, frozen := op.Extensions[extNotImplemented]
			out = append(out, Operation{ID: op.OperationID, Method: method, Path: path, Public: len(reqs) == 0, Frozen: frozen})
		}
	}
	slices.SortFunc(out, func(a, b Operation) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// takesKey reports whether the operation (or its path item) declares the
// Idempotency-Key header parameter.
func takesKey(item *openapi3.PathItem, op *openapi3.Operation) bool {
	for _, params := range []openapi3.Parameters{item.Parameters, op.Parameters} {
		for _, p := range params {
			if p != nil && p.Value != nil && p.Value.In == openapi3.ParameterInHeader &&
				http.CanonicalHeaderKey(p.Value.Name) == httpx.HeaderIdempotencyKey {
				return true
			}
		}
	}
	return false
}

// Caller names the authenticated caller of a request ("user:<id>",
// "staff:<id>"); false when it has none.
type Caller func(ctx context.Context) (string, bool)

// RequestActor names the actor of a public operation from its validated
// body: a digest of a capability only the rightful caller holds, so no
// other client can address its keys (ADR-0034 Karar 1). false when the
// body carries none.
type RequestActor func(body any) (string, bool)

// Throttle takes one token from the rate limit bucket a public
// operation's handler takes from for the same request (its capability's
// bucket); false and the wait when the bucket is empty.
type Throttle func(body any) (ok bool, wait time.Duration)

// PublicOperation is how the store serves a bound operation without a
// caller (ADR-0034 Karar 1 and 5):
//
//   - Actor names the owner of its keys;
//   - Throttle is spent by every answer the store gives without running
//     the handler (a replay, 422, 409), so those answers take from the
//     same bucket the handler does: a stolen capability with a known key
//     is not a way past the capability's rate limit (a wrong password
//     answering 422 and the right one the stored 201 would be a password
//     oracle otherwise).
//
// Only a success is kept for such an operation; a 4xx frees the key, so
// requests with random capabilities and fresh keys write no row.
type PublicOperation struct {
	Actor    RequestActor
	Throttle Throttle
}

// Config wires the middleware.
type Config struct {
	Store  *Store
	Caller Caller
	// Public maps the id of every bound public operation to how it is
	// served. A bound public operation without an entry (or with an entry
	// missing its actor or throttle), and an entry for anything else,
	// stop the server.
	Public  map[string]PublicOperation
	Metrics prometheus.Registerer
	Logger  *slog.Logger
}

// binding is a bound operation as the middleware serves it.
type binding struct {
	operation string
	public    *PublicOperation // nil: the caller is the actor
}

// Middleware binds every operation of the contract that takes the
// Idempotency-Key and has a handler to the store.
type Middleware struct {
	store   *Store
	caller  Caller
	routes  map[string]binding // "METHOD /pattern"
	metrics *metrics
	logger  *slog.Logger
}

// NewMiddleware binds the operations of spec. It fails closed: a bound
// public operation needs an actor in cfg.Public, since neither a client's
// declared device nor its address is one (ADR-0034 Karar 1).
func NewMiddleware(spec *openapi3.T, cfg Config) (*Middleware, error) {
	if cfg.Store == nil || cfg.Caller == nil || cfg.Metrics == nil || cfg.Logger == nil {
		return nil, errors.New("idempotency middleware: store, caller, metrics and logger are required")
	}
	ops, err := Operations(spec)
	if err != nil {
		return nil, err
	}
	routes := map[string]binding{}
	var bound []string
	for _, op := range ops {
		if op.Frozen {
			continue
		}
		b := binding{operation: op.ID}
		public, named := cfg.Public[op.ID]
		switch {
		case op.Public && !named:
			return nil, fmt.Errorf("idempotency middleware: %s has no caller and no request actor; name one in Config.Public", op.ID)
		case !op.Public && named:
			return nil, fmt.Errorf("idempotency middleware: %s has a caller; it takes no request actor", op.ID)
		case op.Public && (public.Actor == nil || public.Throttle == nil):
			return nil, fmt.Errorf("idempotency middleware: %s needs both a request actor and a throttle", op.ID)
		case op.Public:
			b.public = &public
		}
		routes[op.Method+" "+op.Path] = b
		bound = append(bound, op.ID)
	}
	for _, id := range slices.Sorted(maps.Keys(cfg.Public)) {
		if !slices.Contains(bound, id) {
			return nil, fmt.Errorf("idempotency middleware: Config.Public names %s, which is not a bound operation", id)
		}
	}
	m, err := newMetrics(cfg.Metrics, bound)
	if err != nil {
		return nil, err
	}
	return &Middleware{store: cfg.Store, caller: cfg.Caller, routes: routes, metrics: m, logger: cfg.Logger}, nil
}

// Bound returns the ids of the operations the middleware binds.
func (m *Middleware) Bound() []string {
	out := make([]string, 0, len(m.routes))
	for _, b := range m.routes {
		out = append(out, b.operation)
	}
	slices.Sort(out)
	return out
}

// finishTimeout bounds the write after the handler: it runs on a context
// the request's cancellation does not reach, so a timed-out request still
// frees its key.
const finishTimeout = 5 * time.Second

// Handler serves a bound operation through the store. It goes after
// authentication (the caller is the actor) and validation (the hash is
// taken from the validated value; a request the validator refuses takes
// no key, so the client may correct and retry it with the same key).
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())
		if rctx == nil {
			httpx.WriteError(w, r, m.logger, errors.New("idempotency: no chi route context"))
			return
		}
		b, ok := m.routes[r.Method+" "+rctx.RoutePattern()]
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// The lease assumes the request cannot outlive it: a bound route
		// mounted outside httpx.Timeout (a Stream route) would break that,
		// so it is refused before it takes a key.
		if deadline, bounded := r.Context().Deadline(); !bounded || time.Until(deadline) >= Lease {
			httpx.WriteError(w, r, m.logger, fmt.Errorf("idempotency: %s runs without a request deadline shorter than the lease", b.operation))
			return
		}
		m.serve(w, r, rctx, b, next)
	})
}

func (m *Middleware) serve(w http.ResponseWriter, r *http.Request, rctx *chi.Context, b binding, next http.Handler) {
	ctx := r.Context()
	key, ok := httpx.IdempotencyKey(r.Header)
	if !ok {
		// The contract requires the header; validation refused it already.
		httpx.WriteError(w, r, m.logger, errors.New("idempotency: no valid Idempotency-Key after validation"))
		return
	}
	body := validatedValue(r)
	var actor string
	if b.public != nil {
		actor, ok = b.public.Actor(body)
	} else {
		actor, ok = m.caller(ctx)
	}
	if !ok {
		httpx.WriteError(w, r, m.logger, fmt.Errorf("idempotency: %s: the request names no actor", b.operation))
		return
	}
	canonical, err := canonicalRequest(ctx, r, rctx, body)
	if err != nil {
		httpx.WriteError(w, r, m.logger, err)
		return
	}
	id := rowID{actor: actor, operation: b.operation, key: key}
	res, err := m.store.claim(ctx, id, canonical)
	if err != nil {
		httpx.WriteError(w, r, m.logger, err)
		return
	}
	if res.outcome != outcomeClaimed && b.public != nil {
		if allowed, wait := b.public.Throttle(body); !allowed {
			m.metrics.inc(b.operation, ResultThrottled)
			httpx.WriteError(w, r, m.logger, httpx.RateLimitedError(w, wait))
			return
		}
	}
	switch res.outcome {
	case outcomeReplay:
		resp, err := m.store.openResponse(id, canonical, res.sealed)
		if err != nil {
			httpx.WriteError(w, r, m.logger, err)
			return
		}
		m.metrics.inc(b.operation, ResultReplayed)
		replay(w, resp)
		return
	case outcomeReused:
		m.metrics.inc(b.operation, ResultReused)
		httpx.WriteError(w, r, m.logger, apperr.New(apperr.KindUnprocessable, CodeIdempotencyKeyReused))
		return
	case outcomeInUse:
		m.metrics.inc(b.operation, ResultInUse)
		httpx.WriteError(w, r, m.logger, apperr.New(apperr.KindConflict, CodeIdempotencyKeyInUse))
		return
	case outcomeClaimed:
	}
	if res.takenOver {
		m.metrics.inc(b.operation, ResultTakenOver)
	}

	c := res.claim
	cw := &captureWriter{ResponseWriter: w}
	finished := false
	defer func() {
		if !finished {
			// A panic: the key is freed and the panic goes on to the
			// recoverer, which answers 500.
			m.releaseKey(ctx, c)
		}
	}()
	next.ServeHTTP(cw, r.WithContext(withClaim(ctx, c)))
	finished = true
	m.finish(ctx, w.Header(), cw, c, b.public != nil)
}

// finish keeps or frees the key by the response the handler wrote:
//
//   - a success the operation committed in its transaction stays as is;
//   - a success it did not commit is stored now, and reported: the
//     operation is not bound to its transaction (ResultUnbound);
//   - a permanent 4xx of an operation with a caller is stored: the
//     request changed nothing and the same request would be refused the
//     same way (Stripe). For a public operation it frees the key: anyone
//     can send such requests with fresh keys, and a row each would be
//     storage an attacker fills (ADR-0034 Karar 5);
//   - anything else (5xx, 408, 425, 429, 3xx) frees the key so the client
//     can retry. A row the operation's transaction completed although the
//     handler answered 5xx (an ambiguous commit) is not freed: its retry
//     gets the stored success.
//
// Every write is conditional on this attempt still holding the key, so
// the 409 IDEMPOTENCY_KEY_IN_USE of an attempt fenced by a takeover
// changes nothing.
func (m *Middleware) finish(ctx context.Context, header http.Header, cw *captureWriter, c *claim, public bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	status := cw.statusCode()
	op := c.id.operation
	switch {
	case success(status) && c.oversized:
		// Committed without its response (Commit refused to keep more
		// than maxStoredBody): the key is freed, so a retry runs again.
		m.logger.ErrorContext(ctx, "idempotency: the response is too large to keep; the key is freed",
			slog.String("operation", op))
		m.metrics.inc(op, ResultTooLarge)
		m.releaseKey(ctx, c)
	case success(status) && c.committed:
		m.metrics.inc(op, ResultCommitted)
		if sent, err := json.Marshal(cw.stored(header)); err != nil || cw.overflow || sha256.Sum256(sent) != c.digest {
			m.logger.ErrorContext(ctx, "idempotency: the response sent differs from the one committed",
				slog.String("operation", op))
		}
	case success(status):
		m.logger.ErrorContext(ctx, "idempotency: a success was not committed in its transaction",
			slog.String("operation", op))
		m.metrics.inc(op, ResultUnbound)
		m.storeAfter(ctx, header, cw, c)
	case permanent(status) && !public:
		m.metrics.inc(op, ResultStored)
		m.storeAfter(ctx, header, cw, c)
	default:
		m.metrics.inc(op, ResultReleased)
		m.releaseKey(ctx, c)
	}
}

// permanent reports a 4xx the same request would get again. 408, 425 and
// 429 are transient: a retry may succeed.
func permanent(status int) bool {
	transient := status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests
	return !transient && status >= 400 && status <= 499
}

func (m *Middleware) storeAfter(ctx context.Context, header http.Header, cw *captureWriter, c *claim) {
	if cw.overflow {
		m.logger.ErrorContext(ctx, "idempotency: the response is too large to keep; the key is freed",
			slog.String("operation", c.id.operation))
		m.metrics.inc(c.id.operation, ResultTooLarge)
		m.releaseKey(ctx, c)
		return
	}
	kept, err := m.store.complete(ctx, c, cw.stored(header))
	if err != nil {
		m.logger.ErrorContext(ctx, "idempotency: store the response", slog.String("operation", c.id.operation), slog.Any("error", err))
		return
	}
	if !kept {
		m.logger.WarnContext(ctx, "idempotency: the key was taken over before the response was stored",
			slog.String("operation", c.id.operation))
	}
}

// releaseKey frees c's key. A failure only delays the retry until the
// lease runs out.
func (m *Middleware) releaseKey(ctx context.Context, c *claim) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := m.store.release(ctx, c); err != nil {
		m.logger.ErrorContext(ctx, "idempotency: release the key", slog.String("operation", c.id.operation), slog.Any("error", err))
	}
}

// replay writes a stored response, marked as a replay and never cached.
func replay(w http.ResponseWriter, resp storedResponse) {
	h := w.Header()
	for k, v := range resp.Header {
		h[http.CanonicalHeaderKey(k)] = slices.Clone(v)
	}
	h.Set("Cache-Control", "no-store")
	h.Set(httpx.HeaderIdempotentReplayed, "true")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body) // the client went away; nothing to do
}

// maxStoredBody bounds the response body a key keeps, in Commit and after
// the handler: the bound operations answer small JSON documents. A larger
// response is sent but not kept, and its key is freed (ADR-0034 Karar 5).
const maxStoredBody = 1 << 20

// captureWriter passes the response on and keeps a copy of it.
type captureWriter struct {
	http.ResponseWriter
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.overflow {
		if w.body.Len()+len(p) > maxStoredBody {
			w.overflow = true
			w.body.Reset()
		} else {
			w.body.Write(p)
		}
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		return n, fmt.Errorf("idempotency: write the response: %w", err)
	}
	return n, nil
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusCode is the status the handler sent; one that wrote nothing sent
// 200.
func (w *captureWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *captureWriter) stored(header http.Header) storedResponse {
	return storedResponse{Status: w.statusCode(), Header: keptHeaders(header), Body: bytes.Clone(w.body.Bytes())}
}
