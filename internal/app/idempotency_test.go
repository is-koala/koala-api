package app

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
	"github.com/is-koala/koala-api/internal/platform/log"
)

// Every operation whose contract takes the Idempotency-Key declares the
// two refusals the store answers for it, 409 IDEMPOTENCY_KEY_IN_USE and
// 422 IDEMPOTENCY_KEY_REUSED, frozen or not: an operation bound once its
// handler lands would otherwise answer a status its contract does not
// list (ADR-0034).
func TestIdempotentOperationsDeclareTheirRefusals(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := idempotency.Operations(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) < 5 {
		t.Fatalf("the contract has %d operations that take the key; Operations no longer reads them", len(ops))
	}
	for _, op := range ops {
		responses := spec.Paths.Value(op.Path).GetOperation(op.Method).Responses
		for _, status := range []string{"409", "422"} {
			if responses.Value(status) == nil {
				t.Errorf("%s takes an Idempotency-Key but declares no %s response", op.ID, status)
			}
		}
	}
}

// The wired middleware binds the contract's operations: completeRegistration,
// the first, is bound with its request actor. Without that actor the
// server does not start: a public operation has no caller to own its keys
// and the store does not fall back to a client's declared device or its
// address (ADR-0034 Karar 1).
func TestIdempotencyBindsTheContract(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	store := testModulesOn(t, lazyDB(t), clock.Real{}).idempotency
	m, err := idempotency.NewMiddleware(spec, idempotencyConfig(store, prometheus.NewRegistry(), logger))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Bound(), "completeRegistration") {
		t.Fatalf("bound operations = %v, want completeRegistration among them", m.Bound())
	}

	cfg := idempotencyConfig(store, prometheus.NewRegistry(), logger)
	delete(cfg.Public, "completeRegistration")
	if _, err := idempotency.NewMiddleware(spec, cfg); err == nil || !strings.Contains(err.Error(), "completeRegistration") {
		t.Fatalf("NewMiddleware without completeRegistration's actor = %v, want it refused", err)
	}
}
