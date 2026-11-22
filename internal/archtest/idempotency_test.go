package archtest

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
)

// commitSite reports the fenced write of idempotency.Commit: the one
// KindConflict IDEMPOTENCY_KEY_IN_USE site of the idempotency package a
// handler can reach. The middleware answers the same code, but no handler
// calls the middleware, so only a call of Commit reaches this site.
func commitSite(s errorSite) bool {
	return s.kind == "KindConflict" && s.code == httpx.CodeIdempotencyKeyInUse &&
		strings.Contains(filepath.ToSlash(s.pos), "internal/platform/idempotency/")
}

// TestIdempotentHandlersCommitTheirKey is the static half of the binding
// of the Idempotency-Key store (ADR-0034 Karar 6). The operations come
// from the contract (idempotency.Operations): every one that takes the key
// and has a handler is bound, so a handler PR that lifts x-not-implemented
// joins this test by itself. For each, the handler's trace (the walk of
// TestHandlersReturnTheirDeclaredErrorCodes) must reach Commit's fenced
// write; a handler that answers a success without storing it in its
// transaction would repeat its side effect on a lost commit.
//
// The other side holds too: no handler of an operation without the key
// reaches Commit, so the site cannot be reached by a path every handler
// shares, which would keep this test green with any handler's Commit
// deleted.
//
// The walk cannot tell whether Commit runs inside the operation's
// transaction; the integration test TestIdempotentOperationsReplay in
// internal/app checks that on the database, through the counter of
// commits made in a transaction.
func TestIdempotentHandlersCommitTheirKey(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := idempotency.Operations(spec)
	if err != nil {
		t.Fatal(err)
	}
	keyed := map[string]bool{}
	for _, op := range ops {
		keyed[op.ID] = true
	}
	m := loadCodeModule(t)
	handlers := m.handlers()

	var bound, plain []string
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for _, op := range spec.Paths.Value(path).Operations() {
			if _, frozen := op.Extensions["x-not-implemented"]; frozen {
				continue
			}
			name := strings.ToUpper(op.OperationID[:1]) + op.OperationID[1:]
			found := handlers[name]
			if len(found) != 1 {
				continue // a non-strict operation; the error binding test owns that case
			}
			sites, _, _ := m.trace(found[0])
			commits := slices.ContainsFunc(sites, commitSite)
			switch {
			case keyed[op.OperationID]:
				bound = append(bound, op.OperationID)
				if !commits {
					t.Errorf("%s takes an Idempotency-Key, but its handler never reaches idempotency.Commit: "+
						"store the success in the operation's transaction (ADR-0034 Karar 4)", op.OperationID)
				}
			case commits:
				t.Errorf("%s takes no Idempotency-Key, but its handler reaches idempotency.Commit", op.OperationID)
			default:
				plain = append(plain, op.OperationID)
			}
		}
	}
	// A check that saw nothing proves nothing.
	if !slices.Contains(bound, "completeRegistration") {
		t.Errorf("bound operations with a handler = %v; completeRegistration, the first, is missing", bound)
	}
	if len(plain) < 10 {
		t.Errorf("only %d handlers without the key were checked; the walk no longer reaches the handlers", len(plain))
	}
}
