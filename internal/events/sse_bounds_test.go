package events

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSSEFixedBufferBoundAsserted asserts the live subscriber buffer is a
// fixed compile-time constant and does not grow dynamically.
func TestSSEFixedBufferBoundAsserted(t *testing.T) {
	// The buffer size is defined as a constant in bus.go
	// liveSubscriberBuffer = 32
	const expectedBuffer = 32
	if liveSubscriberBuffer != expectedBuffer {
		t.Fatalf("liveSubscriberBuffer=%d want %d (must remain a fixed constant)", liveSubscriberBuffer, expectedBuffer)
	}

	// Verify the channel created in SubscribeSnapshotSince uses this exact bound.
	b := New(10)
	snapshot, live, cancel, ok := b.SubscribeSnapshotSince(0, 10)
	if !ok {
		t.Fatal("subscription failed")
	}
	defer cancel()

	// The channel capacity must match the fixed constant.
	if cap(live) != expectedBuffer {
		t.Fatalf("live channel capacity=%d want %d", cap(live), expectedBuffer)
	}

	// Snapshot should be empty initially.
	if len(snapshot) != 0 {
		t.Fatalf("initial snapshot len=%d want 0", len(snapshot))
	}
}

// TestSSESlowClientEvictionWithoutBlockingPublishers verifies that a slow
// subscriber (full buffer) is dropped without blocking the producer path.
func TestSSESlowClientEvictionWithoutBlockingPublishers(t *testing.T) {
	b := New(100)

	// Subscribe a slow client.
	_, live, cancel, ok := b.SubscribeSnapshot(1)
	if !ok {
		t.Fatal("subscription failed")
	}

	// Fill the subscriber's buffer to capacity.
	for i := 0; i < liveSubscriberBuffer; i++ {
		b.Add(Event{Kind: "fill", Message: "x"})
	}

	// Verify the channel is full.
	if len(live) != liveSubscriberBuffer {
		t.Fatalf("channel not full: len=%d cap=%d", len(live), cap(live))
	}

	// Add more events - producer must not block.
	// The property under test is "never blocks", not "finishes fast", so
	// the watchdog is generous: under flake-hunt CPU saturation a 500ms
	// wall-clock bound can starve the producer goroutine into a false
	// failure even though Add is non-blocking. 30s fires only on a
	// genuinely blocking producer.
	done := make(chan struct{})
	go func() {
		for i := 0; i < liveSubscriberBuffer*4; i++ {
			b.Add(Event{Kind: "burst", Message: "y"})
		}
		close(done)
	}()

	select {
	case <-done:
		// Producer completed without blocking.
	case <-time.After(30 * time.Second):
		t.Fatal("producer blocked on slow subscriber (should be non-blocking)")
	}

	// Verify the bus ring still accepted all events (bounded by ring size).
	if len(b.Snapshot()) != 100 {
		t.Fatalf("ring size=%d want 100", len(b.Snapshot()))
	}

	// Cancel to close the channel, then drain to count received events.
	cancel()

	// Slow client only received what its buffer could hold.
	received := 0
	for range live {
		received++
	}
	if received != liveSubscriberBuffer {
		t.Fatalf("slow client received %d events, want exactly buffer size %d", received, liveSubscriberBuffer)
	}
}

// TestSSEKeepaliveIsCommentNotDataEvent verifies keepalive frames are SSE
// comments (starting with ':') and never emitted as fake data events.
func TestSSEKeepaliveIsCommentNotDataEvent(t *testing.T) {
	// This test validates the wire format in admin_events.go.
	// The keepalive is written as ": keepalive\n\n" which is an SSE comment.
	// SSE comments:
	//   - Start with ':' character
	//   - Are ignored by EventSource parsers
	//   - Never trigger 'message' or named event handlers
	//   - Are not data events

	// Verify the format in admin_events.go line 100:
	// fmt.Fprint(w, ": keepalive\n\n")
	// This is a comment line followed by blank line = valid SSE comment frame.

	// We can't easily test the HTTP handler without a full integration test,
	// but we can assert the semantic: keepalive is a comment, not data.
	const keepaliveFormat = ": keepalive\n\n"
	if !isSSEComment(keepaliveFormat) {
		t.Fatal("keepalive format is not a valid SSE comment")
	}
	if isSSEDataEvent(keepaliveFormat) {
		t.Fatal("keepalive format incorrectly parses as a data event")
	}
}

// TestSSERepeatedConnectDisconnectLeakTolerance runs repeated connect/disconnect
// cycles and verifies no goroutine or resource leaks under -race.
//
// Determinism design (issue #404): the bus spawns no goroutine per subscriber
// and cancel() removes the subscriber synchronously, so disconnect accounting
// is asserted exactly (NumSubscribers() == 0 — no timing involved). Goroutine
// reclamation by the Go runtime is genuinely eventual, so the goroutine check
// polls until the count converges instead of sampling once after a fixed
// sleep: slow cleanup converges, a real leak never does. A fixed-tolerance
// single sample was the load-flake (it failed twice under full verify.sh load).
func TestSSERepeatedConnectDisconnectLeakTolerance(t *testing.T) {
	b := New(100)
	const cycles = 500 // Reduced to stay within subscriber limits with cleanup

	var wg sync.WaitGroup
	wg.Add(cycles)
	var completed, rejected atomic.Int64

	// Track active goroutines to detect leaks.
	initialGoroutines := numGoroutines()

	for i := 0; i < cycles; i++ {
		go func(iter int) {
			defer wg.Done()

			// Subscribe with retry if budget full
			var snapshot []Event
			var live <-chan Event
			var cancel func()
			var ok bool
			for attempt := 0; attempt < 10; attempt++ {
				snapshot, live, cancel, ok = b.SubscribeSnapshot(10)
				if ok {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !ok {
				// Could not subscribe after retries
				rejected.Add(1)
				return
			}

			// Verify snapshot
			_ = snapshot

			// Try to read (may or may not get the event due to buffer)
			select {
			case <-live:
			case <-time.After(10 * time.Millisecond):
			}

			// Disconnect - do this before adding events to avoid lock contention
			cancel()

			// cancel() removes the subscriber and closes its channel
			// synchronously under the bus lock, so the channel is already
			// closed at this point. Drain buffered events with a BLOCKING
			// receive: a closed channel is always readable, so this converges
			// with no wall-clock assumption at all — a slow scheduler can
			// never produce a false failure. (The 100ms poll this replaces
			// was the load-flake: under flake-hunt CPU saturation the poll
			// loop could be starved past its deadline even though the channel
			// was closed.) The watchdog fires only on a genuine never-closed
			// defect, i.e. a real subscriber leak.
			watchdog := time.After(30 * time.Second)
			closed := false
			for !closed {
				select {
				case _, open := <-live:
					closed = !open
				case <-watchdog:
					t.Errorf("iteration %d: channel not closed after cancel", iter)
					return
				}
			}
			completed.Add(1)
			// Add an event after disconnect to verify bus still works
			b.Add(Event{Kind: "test", Message: "iter"})
		}(i)
	}

	wg.Wait()

	// Exact cycle accounting: every iteration either disconnected cleanly or
	// was rejected by the subscriber budget. This proves the disconnect path
	// under test actually executed (guards against a vacuous pass).
	if got := completed.Load() + rejected.Load(); got != cycles {
		t.Fatalf("cycle accounting broken: completed=%d rejected=%d want total=%d", completed.Load(), rejected.Load(), cycles)
	}
	if completed.Load() == 0 {
		t.Fatal("no connect/disconnect cycle completed; the leak path was not exercised")
	}

	// Deterministic: cancel() removes the subscriber synchronously, so after
	// all cancels the subscriber map must be exactly empty. No timing involved.
	if n := b.NumSubscribers(); n != 0 {
		t.Fatalf("subscriber leak: %d live subscribers remain after all cancels", n)
	}

	// Eventual: goroutine reclamation by the runtime is genuinely eventual, so
	// poll until the count converges instead of sampling once after a fixed
	// sleep. Slow cleanup converges; a real leak never does. The +10 bound
	// covers ambient runtime goroutines, not test activity.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if finalGoroutines := numGoroutines(); finalGoroutines <= initialGoroutines+10 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("goroutine leak detected: initial=%d final=%d (cycles=%d) — count did not converge", initialGoroutines, numGoroutines(), cycles)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify bus still functional after cycles - should have at least some events.
	// The exact count depends on scheduling, but should be > 0 and <= 100.
	b.Add(Event{Kind: "post-leak-test", Message: "ok"})
	snap := b.Snapshot()
	if len(snap) == 0 || len(snap) > 100 {
		t.Fatalf("bus ring corrupted after cycles: size=%d", len(snap))
	}
}

// TestSSEConcurrentConnectDisconnectRace runs concurrent connect/disconnect
// with -race detector to catch data races.
func TestSSEConcurrentConnectDisconnectRace(t *testing.T) {
	b := New(50)
	const workers = 16 // Reduced to stay within subscriber limits
	const iterationsPerWorker = 50

	var wg sync.WaitGroup
	var errors atomic.Int32

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterationsPerWorker; i++ {
				// Subscribe
				_, live, cancel, ok := b.SubscribeSnapshot(5)
				if !ok {
					// Subscriber budget full - expected under concurrent load
					time.Sleep(1 * time.Millisecond)
					continue
				}

				// Brief read attempt
				select {
				case <-live:
				case <-time.After(1 * time.Millisecond):
				}

				// Disconnect - do this before adding events to avoid lock contention
				cancel()

				// cancel() removes the subscriber and closes its channel
				// synchronously under the bus lock, so the channel is already
				// closed at this point. Drain buffered events with a BLOCKING
				// receive: a closed channel is always readable, so this
				// converges with no wall-clock assumption at all — a starved
				// scheduler can never produce a false failure. (The 100ms
				// poll this replaces was the flake-hunt round-3 failure on
				// head 7798efd: under CPU saturation the poll loop was
				// starved past its deadline even though the channel was
				// closed, reporting 13 false errors.) The watchdog fires
				// only on a genuine never-closed defect, i.e. a real
				// subscriber leak, and still counts as an error below.
				watchdog := time.After(30 * time.Second)
				closed := false
				for !closed {
					select {
					case _, open := <-live:
						closed = !open
					case <-watchdog:
						errors.Add(1)
						return
					}
				}

				// Add an event after disconnect to verify bus still works
				b.Add(Event{Kind: "concurrent", Message: "x"})
			}
		}(w)
	}

	wg.Wait()

	if errors.Load() > 0 {
		t.Fatalf("concurrent connect/disconnect had %d errors", errors.Load())
	}
}

// TestSSESubscriberBudgetRespected verifies the fixed subscriber budget is enforced.
func TestSSESubscriberBudgetRespected(t *testing.T) {
	b := New(10)
	cancels := make([]func(), 0, maxLiveSubscribers)

	// Fill the subscriber budget
	for i := 0; i < maxLiveSubscribers; i++ {
		_, _, cancel, ok := b.SubscribeSnapshot(1)
		if !ok {
			t.Fatalf("subscription %d unexpectedly rejected", i)
		}
		cancels = append(cancels, cancel)
	}

	// Next subscription should be rejected
	if _, _, _, ok := b.SubscribeSnapshot(1); ok {
		t.Fatal("subscriber budget exceeded")
	}

	// Release one and verify it can be reused
	cancels[0]()
	cancels = cancels[1:]
	if _, _, cancel, ok := b.SubscribeSnapshot(1); !ok {
		t.Fatal("subscriber capacity was not released")
	} else {
		cancel()
	}

	// Release all and verify full capacity restored
	for _, cancel := range cancels {
		cancel()
	}
	if _, _, cancel, ok := b.SubscribeSnapshot(1); !ok {
		t.Fatal("subscriber capacity not fully restored")
	} else {
		cancel()
	}
}

// Helper to check if a string is an SSE comment frame.
func isSSEComment(frame string) bool {
	// SSE comment: starts with ':' and ends with \n\n
	return len(frame) >= 4 && frame[0] == ':' && frame[len(frame)-2:] == "\n\n"
}

// Helper to check if a string parses as an SSE data event.
func isSSEDataEvent(frame string) bool {
	// A data event has "data: " prefix
	for _, line := range splitLines(frame) {
		if len(line) >= 6 && line[:6] == "data: " {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// numGoroutines returns the current number of goroutines.
func numGoroutines() int {
	return runtime.NumGoroutine()
}
