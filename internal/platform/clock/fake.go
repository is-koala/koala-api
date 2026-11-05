package clock

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Fake is a Clock for tests. Its time moves only with Advance, and its
// timers and tickers fire as Advance passes their deadline, in deadline
// order (creation order on ties). Each delivers its own deadline as the
// time it fired, as a real timer delivers the time it fired.
//
// A firing never blocks: a timer or ticker channel holds one value, and a
// tick nobody received is dropped, as with time.Ticker.
//
// The usual test shape: start the code under test, wait with BlockUntil
// until it has created its timer, then Advance. Advancing first would let
// the deadline pass before the timer exists.
//
// Limits:
//   - BlockUntil counts every pending timer and ticker of the clock. It
//     cannot tell whose timer it is, and a ticker stays pending until it
//     is stopped; a test that shares one Fake between several timers waits
//     for the total it expects.
//   - Advance fires a ticker once per period it passes, so Advance(time.Hour)
//     over a millisecond ticker loops 3.6 million times. Advance in steps
//     that fit the periods under test. Only one tick is buffered.
//   - Nothing runs on its own: a goroutine woken by a timer runs after
//     Advance returns, in real time. Wait for its effect with a bounded
//     loop or a channel, not with another Advance.
//
// A Fake is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	seq     uint64
	waiters []*waiter
	// changed is closed and replaced whenever the pending set changes;
	// BlockUntil waits on it.
	changed chan struct{}
}

var _ Clock = (*Fake)(nil)

// waiter is a pending timer or ticker.
type waiter struct {
	c      chan time.Time
	when   time.Time
	period time.Duration // 0 for a timer
	seq    uint64
	active bool
}

// NewFake returns a Fake whose time is start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start, changed: make(chan struct{})}
}

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer returns a timer that fires once the fake time reaches Now()+d.
// With d <= 0 it has fired already.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &waiter{c: make(chan time.Time, 1)}
	f.schedule(w, d)
	return fakeTimer{f: f, w: w}
}

// NewTicker returns a ticker that fires every d of fake time. It panics if
// d <= 0, as time.NewTicker does.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &waiter{c: make(chan time.Time, 1), period: d}
	f.schedule(w, d)
	return fakeTicker{f: f, w: w}
}

// Advance moves the fake time forward by d and fires every timer and tick
// whose deadline it passes. It panics if d is negative: time does not go
// back.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic("clock: negative Advance")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for {
		w := f.earliest()
		if w == nil || w.when.After(target) {
			break
		}
		f.now = w.when
		f.fire(w)
	}
	f.now = target
	f.notify()
}

// Pending returns the number of timers and tickers that will fire on a
// future Advance.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntil waits until at least n timers and tickers are pending, that
// is until the code under test has created the timer the test is about to
// fire. It returns ctx's error, wrapped, if ctx ends first.
func (f *Fake) BlockUntil(ctx context.Context, n int) error {
	for {
		f.mu.Lock()
		pending, changed := len(f.waiters), f.changed
		f.mu.Unlock()
		if pending >= n {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("clock: %d of %d timers pending: %w", pending, n, ctx.Err())
		}
	}
}

// schedule arms w to fire d from now; d <= 0 fires it at once. Callers
// hold f.mu.
func (f *Fake) schedule(w *waiter, d time.Duration) {
	f.seq++
	w.seq = f.seq
	w.when = f.now.Add(d)
	w.active = true
	if d <= 0 {
		f.fire(w)
		return
	}
	f.waiters = append(f.waiters, w)
	f.notify()
}

// cancel disarms w and drops a value it delivered but nobody received, so
// that nothing stale is read after Stop or Reset. It reports whether w was
// pending: armed, or fired with its value still unreceived (Go 1.23+
// timers count that as not fired). Callers hold f.mu.
func (f *Fake) cancel(w *waiter) bool {
	drained := false
	select {
	case <-w.c:
		drained = true
	default:
	}
	was := w.active || drained
	w.active = false
	f.remove(w)
	f.notify()
	return was
}

// fire delivers w's deadline; a ticker is re-armed one period later, a
// timer is done. Callers hold f.mu.
func (f *Fake) fire(w *waiter) {
	select {
	case w.c <- w.when:
	default: // the previous value was not received; drop this one
	}
	if w.period > 0 {
		w.when = w.when.Add(w.period)
		return
	}
	w.active = false
	f.remove(w)
}

// earliest returns the pending waiter with the smallest deadline, the
// oldest on ties, or nil. Callers hold f.mu.
func (f *Fake) earliest() *waiter {
	var next *waiter
	for _, w := range f.waiters {
		if next == nil || w.when.Before(next.when) || (w.when.Equal(next.when) && w.seq < next.seq) {
			next = w
		}
	}
	return next
}

func (f *Fake) remove(w *waiter) {
	for i, x := range f.waiters {
		if x == w {
			f.waiters = slices.Delete(f.waiters, i, i+1)
			f.notify()
			return
		}
	}
}

func (f *Fake) notify() {
	close(f.changed)
	f.changed = make(chan struct{})
}

type fakeTimer struct {
	f *Fake
	w *waiter
}

func (t fakeTimer) C() <-chan time.Time { return t.w.c }

func (t fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	return t.f.cancel(t.w)
}

func (t fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	was := t.f.cancel(t.w)
	t.f.schedule(t.w, d)
	return was
}

type fakeTicker struct {
	f *Fake
	w *waiter
}

func (t fakeTicker) C() <-chan time.Time { return t.w.c }

func (t fakeTicker) Stop() {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.cancel(t.w)
}

// Reset panics if d <= 0, as (*time.Ticker).Reset does.
func (t fakeTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: non-positive interval for Ticker.Reset")
	}
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.cancel(t.w)
	t.w.period = d
	t.f.schedule(t.w, d)
}
