// Package clock is the source of time (plan §3.8). Domain code takes the
// time from a Clock instead of calling time.Now; production wires Real,
// tests wire a *Fake they move by hand.
//
// Consumers that only need the time keep declaring their own one-method
// interface (realtime.Clock is Now() time.Time); Real and *Fake satisfy it.
package clock

import "time"

// Clock tells the time and makes timers that fire on that time.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

// Timer is a single event, like *time.Timer with Go 1.23+ semantics: the
// channel is synchronous, so a timer whose value has not been received
// has not fired yet. Stop and Reset therefore report true for it, and no
// stale value is received after they return.
type Timer interface {
	// C delivers the time the timer fired.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether the timer
	// was still pending, which includes fired but not received.
	Stop() bool
	// Reset makes the timer fire d from now. It reports whether the timer
	// was still pending, which includes fired but not received.
	Reset(d time.Duration) bool
}

// Ticker delivers ticks at an interval, like *time.Ticker. A slow
// receiver misses ticks; they are not queued.
type Ticker interface {
	// C delivers the tick times.
	C() <-chan time.Time
	// Stop turns the ticker off.
	Stop()
	// Reset stops the ticker and restarts it with interval d.
	Reset(d time.Duration)
}

// Real is the system clock.
type Real struct{}

var _ Clock = Real{}

// Now returns the current time.
func (Real) Now() time.Time { return time.Now() }

// NewTimer returns a timer that fires after d.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{t: time.NewTimer(d)} }

// NewTicker returns a ticker with interval d. It panics if d <= 0, as
// time.NewTicker does.
func (Real) NewTicker(d time.Duration) Ticker { return realTicker{t: time.NewTicker(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time   { return r.t.C }
func (r realTicker) Stop()                 { r.t.Stop() }
func (r realTicker) Reset(d time.Duration) { r.t.Reset(d) }
