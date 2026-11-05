package authz_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/authz"
)

// fixedChecker answers every check the same way.
type fixedChecker struct {
	d   authz.Decision
	err error
}

func (c fixedChecker) Check(context.Context, authz.Subject, authz.Action, authz.Object, ...authz.CheckOption) (authz.Decision, error) {
	return c.d, c.err
}

func (c fixedChecker) CheckBatch(_ context.Context, _ authz.Subject, _ authz.Action, objs []authz.Object, _ ...authz.CheckOption) (map[authz.Object]authz.Decision, error) {
	out := map[authz.Object]authz.Decision{}
	for _, o := range objs {
		out[o] = c.d
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
	allow := authz.Decision{Allowed: true}
	conceal := authz.Decision{Conceal: true}
	refuse := authz.Decision{}
	objs := []authz.Object{{Type: "job", ID: "1"}, {Type: "job", ID: "2"}}
	tests := []struct {
		name       string
		primary    fixedChecker
		shadow     fixedChecker
		wantErr    error
		want       authz.Decision
		mismatches int // per Check; CheckBatch counts once per object
	}{
		{"agree allow", fixedChecker{d: allow}, fixedChecker{d: allow}, nil, allow, 0},
		{"agree concealed denial", fixedChecker{d: conceal}, fixedChecker{d: conceal}, nil, conceal, 0},
		{"shadow denies", fixedChecker{d: allow}, fixedChecker{d: conceal}, nil, allow, 1},
		{"shadow allows", fixedChecker{d: conceal}, fixedChecker{d: allow}, nil, conceal, 1},
		{"conceal differs", fixedChecker{d: conceal}, fixedChecker{d: refuse}, nil, conceal, 1},
		{"shadow error", fixedChecker{d: allow}, fixedChecker{err: errShadow}, nil, allow, 1},
		{"primary error", fixedChecker{d: conceal, err: errPrimary}, fixedChecker{d: conceal}, errPrimary, conceal, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counter := &countingCounter{}
			c := authz.NewShadowChecker(tt.primary, tt.shadow, counter, slog.New(slog.DiscardHandler), time.Second)
			d, err := c.Check(context.Background(), authz.Subject{}, "can_view", objs[0])
			if !errors.Is(err, tt.wantErr) || d.Allowed != tt.want.Allowed || d.Conceal != tt.want.Conceal {
				t.Fatalf("Check: %+v, %v", d, err)
			}
			ds, err := c.CheckBatch(context.Background(), authz.Subject{}, "can_view", objs)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CheckBatch: %v", err)
			}
			if err == nil && (ds[objs[0]].Allowed != tt.want.Allowed || ds[objs[1]].Conceal != tt.want.Conceal) {
				t.Fatalf("CheckBatch: %+v", ds)
			}
			if want := tt.mismatches * (1 + len(objs)); counter.n != want {
				t.Fatalf("mismatches %d, want %d", counter.n, want)
			}
		})
	}
}

// ctxState is what the shadow saw of its context during the call.
type ctxState struct {
	err         error
	hasDeadline bool
}

// ctxChecker reports the state of the context it was called with.
type ctxChecker struct {
	fixedChecker
	got chan ctxState
}

func (c ctxChecker) Check(ctx context.Context, s authz.Subject, a authz.Action, o authz.Object, opts ...authz.CheckOption) (authz.Decision, error) {
	_, hasDeadline := ctx.Deadline()
	c.got <- ctxState{err: ctx.Err(), hasDeadline: hasDeadline}
	return c.fixedChecker.Check(ctx, s, a, o, opts...)
}

// TestShadowContext: the shadow runs detached from the request's
// cancellation and under its own deadline.
func TestShadowContext(t *testing.T) {
	t.Parallel()
	shadow := ctxChecker{got: make(chan ctxState, 1)}
	c := authz.NewShadowChecker(fixedChecker{}, shadow, &countingCounter{}, slog.New(slog.DiscardHandler), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the request is already canceled
	if _, err := c.Check(ctx, authz.Subject{}, "can_view", authz.Object{Type: "job", ID: "1"}); err != nil {
		t.Fatal(err)
	}
	got := <-shadow.got
	if got.err != nil {
		t.Errorf("shadow context canceled with the request: %v", got.err)
	}
	if !got.hasDeadline {
		t.Errorf("shadow context has no deadline")
	}
}
