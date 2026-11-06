package retrysafety

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// Agent-D1 coverage: ECONNREFUSED carried by an error whose own text
// does not say "connection refused" (errno path), and the default
// StatusOutcome branch for ordinary success/redirect statuses.

// wrappedErr hides the wrapped errno from the error message while
// keeping it reachable via errors.As, so ClassifyTransportError must
// find ECONNREFUSED through the chain rather than the text.
type agentD1WrappedErr struct{ err error }

func (w agentD1WrappedErr) Error() string { return "boom" }
func (w agentD1WrappedErr) Unwrap() error { return w.err }

func TestAgentD1ClassifyErrnoRefusedWithoutText(t *testing.T) {
	err := agentD1WrappedErr{err: syscall.ECONNREFUSED}
	if got := ClassifyTransportError(err); got != SentNotSent {
		t.Fatalf("ClassifyTransportError(wrapped ECONNREFUSED) = %v, want SentNotSent", got)
	}
	// The same errno surfaced plainly is also pre-dispatch; the text
	// path is not the only route.
	if got := ClassifyTransportError(syscall.ECONNREFUSED); got != SentNotSent {
		t.Fatalf("ClassifyTransportError(ECONNREFUSED) = %v, want SentNotSent", got)
	}
	// Sanity: a genuinely unknown error stays ambiguous.
	if got := ClassifyTransportError(errors.New("boom")); got != SentUnknown {
		t.Fatalf("ClassifyTransportError(unknown) = %v, want SentUnknown", got)
	}
}

func TestAgentD1StatusOutcomeDefaultBranch(t *testing.T) {
	for _, status := range []int{200, 201, 302, 418} {
		o := StatusOutcome(status, 1, 4)
		if o.Sent != SentConfirmed {
			t.Fatalf("status %d: Sent = %v, want SentConfirmed", status, o.Sent)
		}
		if o.Execution != ExecutionUnknown {
			t.Fatalf("status %d: Execution = %v, want ExecutionUnknown", status, o.Execution)
		}
		if o.Retryable {
			t.Fatalf("status %d: Retryable = true, want false (not an error class)", status)
		}
		if d := Decide(o, time.Now()); d.Allow {
			t.Fatalf("status %d: Decide allowed a retry for a non-retryable outcome", status)
		}
	}
}
