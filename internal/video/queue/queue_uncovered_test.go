package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Enqueue must fail fast on an already-expired context instead of
// admitting the job: the pre-check returns the context error, and the
// queue is left untouched for later valid work.
func TestQueueEnqueueExpiredContext(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		q := New(1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := q.Enqueue(ctx, queueJob("one", "")); !errors.Is(err, context.Canceled) {
			t.Fatalf("Enqueue cancelled err = %v, want %v", err, context.Canceled)
		}
		if got := q.Len(); got != 0 {
			t.Fatalf("Len after cancelled enqueue = %d, want 0", got)
		}
	})

	t.Run("deadline exceeded", func(t *testing.T) {
		q := New(1)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
		defer cancel()
		if err := q.Enqueue(ctx, queueJob("one", "")); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Enqueue expired err = %v, want %v", err, context.DeadlineExceeded)
		}
		if got := q.Len(); got != 0 {
			t.Fatalf("Len after expired enqueue = %d, want 0", got)
		}
	})

	t.Run("cancelled while full can surface context error", func(t *testing.T) {
		// Enqueue's select has a send case, a Done case, and a
		// default case. Once the queue is full the send case can
		// never proceed, but when both Done and default are ready
		// the runtime picks one pseudo-randomly, so a single call
		// may return ErrFull instead of the context error. Loop
		// with a fresh context per attempt until the Done case is
		// taken; each attempt is cheap and sleep-free, and the
		// loop bound makes a false failure astronomically
		// unlikely rather than timing-dependent.
		q := New(1)
		if err := q.Enqueue(context.Background(), queueJob("one", "")); err != nil {
			t.Fatalf("fill queue: %v", err)
		}
		hit := false
		for i := 0; i < 1000 && !hit; i++ {
			ctx := &flipContext{}
			err := q.Enqueue(ctx, queueJob("two", ""))
			if errors.Is(err, context.Canceled) {
				hit = true
			} else if err != nil && !errors.Is(err, ErrFull) {
				t.Fatalf("Enqueue attempt %d err = %v, want Canceled or ErrFull", i, err)
			}
		}
		if !hit {
			t.Fatal("Done case never taken in 1000 attempts")
		}
		if got := q.Len(); got != 1 {
			t.Fatalf("Len = %d, want 1 (nothing admitted)", got)
		}
	})

	t.Run("queue still usable afterwards", func(t *testing.T) {
		q := New(1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = q.Enqueue(ctx, queueJob("one", ""))
		if err := q.Enqueue(context.Background(), queueJob("two", "")); err != nil {
			t.Fatalf("Enqueue after expired attempt: %v", err)
		}
		if got := q.Len(); got != 1 {
			t.Fatalf("Len = %d, want 1", got)
		}
	})
}

// flipContext passes Enqueue's pre-check (first Err call) and then
// reports cancellation (later Err calls), with an already-closed Done
// channel so the select's Done case is ready. It exists only to reach
// the Done case of a select that also has a default case.
type flipContext struct {
	calls int
}

func (c *flipContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *flipContext) Done() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
func (c *flipContext) Err() error {
	c.calls++
	if c.calls == 1 {
		return nil
	}
	return context.Canceled
}
func (c *flipContext) Value(any) any { return nil }
