package retrysafety

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestDecideMatrix(t *testing.T) {
	now := time.Now()
	deadline := now.Add(time.Minute)
	cases := []struct {
		name string
		in   Outcome
		want bool
		amb  bool
	}{
		{"timeout before send is safe", TimeoutBeforeSend(1, 4), true, false},
		{"timeout after send is ambiguous but allowed pre-commit for stateless", TimeoutAfterSend(1, 4, false), true, true},
		{"timeout after send is forbidden for durable operations", TimeoutAfterSend(1, 4, true), false, true},
		{"disconnect is ambiguous but allowed pre-commit for stateless", Disconnect(1, 4, false), true, true},
		{"disconnect is forbidden for durable operations", Disconnect(1, 4, true), false, true},
		{"cancellation always wins over budget and retryability", Outcome{Cancelled: true, Retryable: true, AttemptsUsed: 1, MaxAttempts: 4}, false, false},
		{"committed stream never replays", PartialStream(true, 1, 4), false, false},
		{"uncommitted stream failure may fail over", PartialStream(false, 1, 4), true, false},
		{"caller validation error never retries", StatusOutcome(400, 1, 4), false, false},
		{"auth rejection may fail over to another deployment", StatusOutcome(401, 1, 4), true, false},
		{"rate limit is an ambiguous replay", StatusOutcome(429, 1, 4), true, true},
		{"server error is an ambiguous replay", StatusOutcome(500, 1, 4), true, true},
		{"budget exhausted denies even a safe retry", TimeoutBeforeSend(4, 4), false, false},
		{"deadline exceeded denies even a safe retry", func() Outcome {
			o := TimeoutBeforeSend(1, 4)
			o.Deadline = now.Add(-time.Second)
			return o
		}(), false, false},
		{"deadline in future permits", func() Outcome {
			o := TimeoutBeforeSend(1, 4)
			o.Deadline = deadline
			return o
		}(), true, false},
		{"zero max attempts denies", TimeoutBeforeSend(0, 0), false, false},
	}
	for _, tc := range cases {
		got := Decide(tc.in, now)
		if got.Allow != tc.want {
			t.Errorf("%s: Allow=%v want %v (reason %q)", tc.name, got.Allow, tc.want, got.Reason)
		}
		if got.Ambiguous != tc.amb {
			t.Errorf("%s: Ambiguous=%v want %v", tc.name, got.Ambiguous, tc.amb)
		}
		if got.Reason == "" {
			t.Errorf("%s: empty reason", tc.name)
		}
	}
}

func TestClassifyTransportError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: &net.AddrError{Err: "connection refused"}}
	// The OpError above carries Op "dial" regardless of its inner text.
	if got := ClassifyTransportError(refused); got != SentNotSent {
		t.Fatalf("dial error = %v, want SentNotSent", got)
	}
	if got := ClassifyTransportError(syscall.ECONNREFUSED); got != SentNotSent {
		t.Fatalf("ECONNREFUSED = %v, want SentNotSent", got)
	}
	if got := ClassifyTransportError(errors.New("dial tcp: lookup nowhere.invalid: no such host")); got != SentNotSent {
		t.Fatalf("DNS error = %v, want SentNotSent", got)
	}
	if got := ClassifyTransportError(errors.New("all configured provider credentials are cooling down")); got != SentNotSent {
		t.Fatalf("credential exhaustion = %v, want SentNotSent", got)
	}
	// Everything else after dispatch is unknown, including timeouts,
	// resets, EOF, and cancellation text: cancellation is handled by
	// Outcome.Cancelled, never by reclassifying the error as not-sent.
	for _, msg := range []string{
		"context deadline exceeded",
		"read: connection reset by peer",
		"unexpected EOF",
		"context canceled",
		"some provider transport failure",
	} {
		if got := ClassifyTransportError(errors.New(msg)); got != SentUnknown {
			t.Errorf("error %q = %v, want SentUnknown", msg, got)
		}
	}
	if got := ClassifyTransportError(nil); got != SentNotSent {
		t.Fatalf("nil error = %v, want SentNotSent", got)
	}
}

func TestIdempotencyPolicyForAllDialects(t *testing.T) {
	dialects := []string{
		"anthropic_compatible", "openai_compatible", "openai_responses",
		"gemini", "bedrock", "azure_openai", "vertex", "unknown-dialect",
	}
	for _, d := range dialects {
		if got := SupportForDialect(d); got != IdempotencyNone {
			t.Errorf("dialect %q support = %v, want IdempotencyNone (no verified mechanism)", d, got)
		}
		if ForwardClientKey(d) {
			t.Errorf("dialect %q: client key must never be forwarded without a verified mechanism", d)
		}
	}
	if key, ok := GenerateProviderKey(); ok || key != "" {
		t.Fatalf("gateway must not generate a provider idempotency key, got %q ok=%v", key, ok)
	}
	if BodyEqualityProvesDuplicate() {
		t.Fatal("identical bodies must never prove duplication")
	}
}

func TestKeyMatchesRequiresNonEmptyEqualKeys(t *testing.T) {
	if KeyMatches("", "") {
		t.Fatal("empty keys must not match")
	}
	if KeyMatches("k", "") || KeyMatches("", "k") {
		t.Fatal("empty key must not match a real key")
	}
	if !KeyMatches("k-1", "k-1") {
		t.Fatal("equal non-empty keys must match")
	}
	if KeyMatches("k-1", "k-2") {
		t.Fatal("different keys must not match")
	}
	// Key equality is a caller assertion, not body equivalence: the
	// policy documents that a key hit returns the first request's
	// record without comparing bodies, so callers must not reuse a
	// key for a different request. That rule lives in the doc, and
	// this test pins the only thing the function promises: the
	// match predicate itself.
}
