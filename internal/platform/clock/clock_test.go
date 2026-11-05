package clock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/clock"
)

var start = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// received returns the value waiting on c. A fake fires inside Advance, so
// the value is there when Advance returns or it never comes.
func received(t *testing.T, c <-chan time.Time) time.Time {
	t.Helper()
	var v time.Time
	select {
	case v = <-c:
	default:
		t.Fatal("no value on the channel")
	}
	return v
}

func empty(t *testing.T, c <-chan time.Time) {
	t.Helper()
	select {
	case v := <-c:
		t.Fatalf("unexpected value %v on the channel", v)
	default:
	}
}

func TestFakeTimerFiresWhenAdvancePassesItsDeadline(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(start)
	tm := f.NewTimer(10 * time.Second)

	f.Advance(9 * time.Second)
	empty(t, tm.C())
	if got := f.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1", got)
	}

	f.Advance(time.Second)
	if got, want := received(t, tm.C()), start.Add(10*time.Second); !got.Equal(want) {
		t.Fatalf("fired at %v, want %v", got, want)
	}
	if got := f.Pending(); got != 0 {
		t.Fatalf("Pending after firing = %d, want 0", got)
	}
	if got, want := f.Now(), start.Add(10*time.Second); !got.Equal(want) {
		t.Fatalf("Now = %v, want %v", got, want)
	}
}

func TestFakeAdvanceFiresEveryTimerItPasses(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(start)
	timers := map[time.Duration]clock.Timer{}
	for _, d := range []time.Duration{3 * time.Second, time.Second, 2 * time.Second, 7 * time.Second} {
		timers[d] = f.NewTimer(d)
	}

	f.Advance(5 * time.Second)
	for d, tm := range timers {
		if d > 5*time.Second {
			empty(t, tm.C())
			continue
		}
		if got := received(t, tm.C()); !got.Equal(start.Add(d)) {
			t.Fatalf("timer %v delivered %v, want its deadline %v", d, got, start.Add(d))
		}
	}
	if got, want := f.Now(), start.Add(5*time.Second); !got.Equal(want) {
		t.Fatalf("Now = %v, want the Advance target %v", got, want)
	}
}

func TestFakeTimerWithNonPositiveDurationHasFired(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, -time.Second} {
		f := clock.NewFake(start)
		tm := f.NewTimer(d)
		if got := received(t, tm.C()); !got.Equal(start.Add(d)) {
			t.Fatalf("NewTimer(%v) delivered %v", d, got)
		}
		if f.Pending() != 0 {
			t.Fatalf("NewTimer(%v) left a pending timer", d)
		}
	}
}

func TestFakeTimerStop(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(start)

	pending := f.NewTimer(time.Minute)
	if !pending.Stop() {
		t.Fatal("Stop of a pending timer = false")
	}
	if pending.Stop() {
		t.Fatal("second Stop = true")
	}
	f.Advance(time.Hour)
	empty(t, pending.C())

	// Fired but not received: with Go 1.23+ synchronous timer channels the
	// timer has not fired yet, so Stop reports true and drops the value.
	fired := f.NewTimer(time.Second)
	f.Advance(time.Second)
	if !fired.Stop() {
		t.Fatal("Stop of a fired, unreceived timer = false")
	}
	empty(t, fired.C())

	// Fired and received: Stop reports false.
	done := f.NewTimer(time.Second)
	f.Advance(time.Second)
	received(t, done.C())
	if done.Stop() {
		t.Fatal("Stop of a received timer = true")
	}
}

// The real timer agrees: a *time.Timer whose deadline has passed but whose
// value nobody received reports true from Stop (Go 1.23+). The sleep only
// lets the deadline pass; it waits for no other goroutine.
func TestRealTimerStopSemanticsMatchTheFake(t *testing.T) {
	t.Parallel()
	tm := clock.Real{}.NewTimer(time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if !tm.Stop() {
		t.Fatal("Stop of a fired, unreceived real timer = false")
	}
	select {
	case v := <-tm.C():
		t.Fatalf("stale value %v after Stop", v)
	default:
	}
}

func TestFakeTimerReset(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(start)
	tm := f.NewTimer(time.Minute)

	if !tm.Reset(10 * time.Second) {
		t.Fatal("Reset of a pending timer = false")
	}
	f.Advance(10 * time.Second)
	if got := received(t, tm.C()); !got.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("fired at %v after Reset", got)
	}
	f.Advance(time.Minute)
	empty(t, tm.C())

	// Reset of a received timer reports false.
	if tm.Reset(time.Second) {
		t.Fatal("Reset of a received timer = true")
	}

	// Reset of a fired, unreceived timer reports true (Go 1.23+) and
	// re-arms it without the stale value.
	f.Advance(time.Second)
	if !tm.Reset(time.Second) {
		t.Fatal("Reset of a fired, unreceived timer = false")
	}
	empty(t, tm.C())
	f.Advance(time.Second)
	if got, want := received(t, tm.C()), f.Now(); !got.Equal(want) {
		t.Fatalf("fired at %v, want %v", got, want)
	}
}

func TestFakeTicker(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(start)
	tk := f.NewTicker(time.Second)

	f.Advance(time.Second)
	if got := received(t, tk.C()); !got.Equal(start.Add(time.Second)) {
		t.Fatalf("first tick %v", got)
	}

	// Nobody receives during three periods: the first tick waits, the
	// rest are dropped, like time.Ticker.
	f.Advance(3 * time.Second)
	if got := received(t, tk.C()); !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("buffered tick %v, want the first missed one", got)
	}
	empty(t, tk.C())

	tk.Reset(10 * time.Second)
	f.Advance(9 * time.Second)
	empty(t, tk.C())
	f.Advance(time.Second)
	received(t, tk.C())

	tk.Stop()
	if f.Pending() != 0 {
		t.Fatal("stopped ticker is still pending")
	}
	f.Advance(time.Minute)
	empty(t, tk.C())
}

func TestFakeBlockUntilWaitsForTheTimer(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(start)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan time.Time, 1)
	go func() {
		tm := f.NewTimer(time.Hour)
		select {
		case v := <-tm.C():
			done <- v
		case <-ctx.Done():
		}
	}()

	if err := f.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("BlockUntil: %v", err)
	}
	f.Advance(time.Hour)
	select {
	case got := <-done:
		if !got.Equal(start.Add(time.Hour)) {
			t.Fatalf("goroutine saw %v", got)
		}
	case <-ctx.Done():
		t.Fatal("the goroutine's timer did not fire")
	}
}

func TestFakeBlockUntilReturnsWhenContextEnds(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(start)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.BlockUntil(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("BlockUntil = %v, want context.Canceled", err)
	}
}

func TestFakeRefusesToGoBack(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*clock.Fake){
		"negative Advance":     func(f *clock.Fake) { f.Advance(-time.Nanosecond) },
		"zero ticker interval": func(f *clock.Fake) { f.NewTicker(0) },
		"zero ticker Reset":    func(f *clock.Fake) { f.NewTicker(time.Second).Reset(0) },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("did not panic")
				}
			}()
			call(clock.NewFake(start))
		})
	}
}

func TestRealClock(t *testing.T) {
	t.Parallel()
	var c clock.Clock = clock.Real{}
	if c.Now().IsZero() {
		t.Fatal("Real.Now is zero")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	tm := c.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-ctx.Done():
		t.Fatal("real timer did not fire")
	}
	tk := c.NewTicker(time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-ctx.Done():
		t.Fatal("real ticker did not tick")
	}
	if tm.Stop() {
		t.Fatal("Stop of a fired real timer = true")
	}
}
