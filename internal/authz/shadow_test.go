package authz_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/is-koala/koala-api/internal/authz"
)

// fixedChecker answers every check the same way.
type fixedChecker struct {
	allowed bool
	err     error
}

func (c fixedChecker) Check(context.Context, authz.Subject, authz.Action, authz.Object, ...authz.CheckOption) (authz.Decision, error) {
	return authz.Decision{Allowed: c.allowed, Conceal: !c.allowed}, c.err
}

func (c fixedChecker) CheckBatch(_ context.Context, _ authz.Subject, _ authz.Action, objs []authz.Object, _ ...authz.CheckOption) (map[authz.Object]authz.Decision, error) {
	out := map[authz.Object]authz.Decision{}
	for _, o := range objs {
		out[o] = authz.Decision{Allowed: c.allowed, Conceal: !c.allowed}
	}
	return out, c.err
}

// countingCounter is the test MismatchCounter.
type countingCounter struct {
	mu sync.Mutex
	n  int
}

func (c *countingCounter) AuthzMismatch(string, authz.Action, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func TestShadowChecker(t *testing.T) {
	t.Parallel()
	errShadow := errors.New("shadow down")
	errPrimary := errors.New("primary down")
	objs := []authz.Object{{Type: "job", ID: "1"}, {Type: "job", ID: "2"}}
	tests := []struct {
		name       string
		primary    fixedChecker
		shadow     fixedChecker
		wantErr    error
		wantAllow  bool
		mismatches int // per Check; CheckBatch counts once per object
	}{
		{"agree allow", fixedChecker{allowed: true}, fixedChecker{allowed: true}, nil, true, 0},
		{"agree deny", fixedChecker{}, fixedChecker{}, nil, false, 0},
		{"shadow denies", fixedChecker{allowed: true}, fixedChecker{}, nil, true, 1},
		{"shadow allows", fixedChecker{}, fixedChecker{allowed: true}, nil, false, 1},
		{"shadow error", fixedChecker{allowed: true}, fixedChecker{err: errShadow}, nil, true, 1},
		{"primary error", fixedChecker{err: errPrimary}, fixedChecker{}, errPrimary, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counter := &countingCounter{}
			c := authz.NewShadowChecker(tt.primary, tt.shadow, counter, slog.New(slog.DiscardHandler))
			d, err := c.Check(context.Background(), authz.Subject{}, "can_view", objs[0])
			if !errors.Is(err, tt.wantErr) || (err == nil && d.Allowed != tt.wantAllow) {
				t.Fatalf("Check: %+v, %v", d, err)
			}
			ds, err := c.CheckBatch(context.Background(), authz.Subject{}, "can_view", objs)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CheckBatch: %v", err)
			}
			if err == nil && (ds[objs[0]].Allowed != tt.wantAllow || ds[objs[1]].Allowed != tt.wantAllow) {
				t.Fatalf("CheckBatch: %+v", ds)
			}
			if want := tt.mismatches * (1 + len(objs)); counter.n != want {
				t.Fatalf("mismatches %d, want %d", counter.n, want)
			}
		})
	}
}
