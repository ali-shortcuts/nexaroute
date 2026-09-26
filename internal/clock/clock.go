// Package clock provides an injectable clock so recovery/cooldown tests can
// advance time without waiting for real 30-minute windows.
package clock

import (
	"sync"
	"time"
)

// Timer is the subset of *time.Timer used by recovery scheduling.
type Timer interface {
	Stop() bool
}

// Clock is a source of time and delayed callbacks.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	Until(t time.Time) time.Duration
	AfterFunc(d time.Duration, fn func()) Timer
}

type realClock struct{}

// Real returns the operating-system clock.
func Real() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

func (realClock) Until(t time.Time) time.Duration { return time.Until(t) }

func (realClock) AfterFunc(d time.Duration, fn func()) Timer {
	return time.AfterFunc(d, fn)
}

type fakeTimer struct {
	f       *Fake
	when    time.Time
	fn      func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// Fake is a deterministic clock for tests.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// NewFake starts at the provided instant.
func NewFake(now time.Time) *Fake {
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	return &Fake{now: now}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration {
	return f.Now().Sub(t)
}

func (f *Fake) Until(t time.Time) time.Duration {
	return t.Sub(f.Now())
}

func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	if d < 0 {
		d = 0
	}
	f.mu.Lock()
	when := f.now.Add(d)
	t := &fakeTimer{f: f, when: when, fn: fn}
	if d == 0 {
		f.mu.Unlock()
		fn()
		t.fired = true
		return t
	}
	f.timers = append(f.timers, t)
	f.mu.Unlock()
	return t
}

// Advance moves the fake clock forward and fires due timers.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		d = 0
	}
	for {
		f.mu.Lock()
		f.now = f.now.Add(d)
		d = 0
		var due []*fakeTimer
		live := f.timers[:0]
		for _, t := range f.timers {
			if t.stopped || t.fired {
				continue
			}
			if !t.when.After(f.now) {
				t.fired = true
				due = append(due, t)
				continue
			}
			live = append(live, t)
		}
		f.timers = live
		f.mu.Unlock()
		if len(due) == 0 {
			return
		}
		for _, t := range due {
			t.fn()
		}
		// Callbacks may have scheduled delay-0 or already-due timers.
	}
}
