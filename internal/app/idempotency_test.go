package app

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/realtime"
)

// allowAll is a rate limiter that never refuses, for wiring tests.
type allowAll struct{}

func (allowAll) Allow(string, httpx.KeyKind, string) (bool, time.Duration) { return true, 0 }

func keyedOperations(t *testing.T, spec *openapi3.T) map[string]idempotency.Operation {
	t.Helper()
	ops, err := idempotency.Operations(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) < 5 {
		t.Fatalf("the contract has %d operations that take the key; Operations no longer reads them", len(ops))
	}
	out := map[string]idempotency.Operation{}
	for _, op := range ops {
		out[op.ID] = op
	}
	return out
}

// The contract of the store's answers (ADR-0034 Karar 6 and 7):
//
//   - every operation that takes the Idempotency-Key declares 409 and
//     422, frozen or not: an operation bound once its handler lands would
//     otherwise answer a status its contract does not list;
//   - every bound operation's success declares Idempotent-Replayed;
//   - an operation without the key neither declares 422 (the status is
//     the store's alone) nor lists the store's codes.
func TestIdempotentOperationsDeclareTheirRefusals(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	keyed := keyedOperations(t, spec)
	storeCodes := map[string]bool{}
	for _, c := range idempotency.Codes() {
		storeCodes[string(c)] = true
	}
	plain := 0
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for method, op := range spec.Paths.Value(path).Operations() {
			k, isKeyed := keyed[op.OperationID]
			if !isKeyed {
				plain++
				if op.Responses.Value(strconv.Itoa(http.StatusUnprocessableEntity)) != nil {
					t.Errorf("%s %s (%s) takes no Idempotency-Key but declares 422, the store's status", method, path, op.OperationID)
				}
				if codes, ok := op.Extensions["x-error-codes"].([]any); ok {
					for _, c := range codes {
						if s, ok := c.(string); ok && storeCodes[s] {
							t.Errorf("%s takes no Idempotency-Key but lists %s, a code of the store", op.OperationID, s)
						}
					}
				}
				continue
			}
			for _, status := range []string{"409", "422"} {
				if op.Responses.Value(status) == nil {
					t.Errorf("%s takes an Idempotency-Key but declares no %s response", op.OperationID, status)
				}
			}
			if k.Frozen {
				continue
			}
			declared := false
			for status, resp := range op.Responses.Map() {
				if !strings.HasPrefix(status, "2") || resp.Value == nil {
					continue
				}
				declared = true
				if resp.Value.Headers[httpx.HeaderIdempotentReplayed] == nil {
					t.Errorf("%s's %s response does not declare %s", op.OperationID, status, httpx.HeaderIdempotentReplayed)
				}
			}
			if !declared {
				t.Errorf("%s declares no success response", op.OperationID)
			}
		}
	}
	if plain < 20 {
		t.Errorf("only %d operations without the key were checked", plain)
	}
}

// The wired middleware binds the contract's operations: completeRegistration,
// the first, is bound with its request actor and throttle. Without that
// entry the server does not start: a public operation has no caller to own
// its keys and the store does not fall back to a client's declared device
// or its address (ADR-0034 Karar 1).
func TestIdempotencyBindsTheContract(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	store := testModulesOn(t, lazyDB(t), clock.Real{}).idempotency
	newConfig := func() idempotency.Config {
		return idempotencyConfig(store, allowAll{}, apiLimitGroups(), prometheus.NewRegistry(), logger)
	}
	m, err := idempotency.NewMiddleware(spec, newConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Bound(), "completeRegistration") {
		t.Fatalf("bound operations = %v, want completeRegistration among them", m.Bound())
	}

	cfg := newConfig()
	delete(cfg.Public, "completeRegistration")
	if _, err := idempotency.NewMiddleware(spec, cfg); err == nil || !strings.Contains(err.Error(), "completeRegistration") {
		t.Fatalf("NewMiddleware without completeRegistration's actor = %v, want it refused", err)
	}
}

// No operation that takes the key is a Stream route: the lease assumes
// httpx.Timeout bounds the request (ADR-0034 Karar 4). The middleware
// refuses such a request at run time too (TestUnboundedRequestIsRefused).
func TestIdempotentOperationsAreBounded(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	router, err := newRouter(context.Background(), config.Config{Env: config.EnvTest}, logger, httpx.NewHealth(logger),
		prometheus.NewRegistry(), unlimitedGroups(), testModules(t))
	if err != nil {
		t.Fatal(err)
	}
	if !router.IsStream(realtime.PathConnect) {
		t.Fatal("the WebSocket route is not seen as a Stream route; the check would prove nothing")
	}
	for id, op := range keyedOperations(t, spec) {
		if router.IsStream(op.Path) {
			t.Errorf("%s takes an Idempotency-Key but is a Stream route, outside httpx.Timeout", id)
		}
	}
}
