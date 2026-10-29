// Package retrysafety classifies upstream failures for retry decisions and
// states the gateway's idempotency policy in one place.
//
// It never claims exactly-once provider execution. No provider dialect
// shipped today has a verified idempotency mechanism in the conformance
// manifest (every dialect's idempotency dimension is UNTESTED), so the
// gateway forwards no client idempotency key to any upstream and generates
// no provider idempotency key. A retry of a stateless generation request
// is a new provider execution that may duplicate provider-side work or
// cost; it is safe for the client only while no response bytes have been
// committed, and only inside the configured attempt/deadline budget.
//
// The one place where duplicate execution is prevented by durable state
// is the video plane, not this package: video job creation is keyed by a
// caller-supplied idempotency key in the job store (concurrent creates
// with one key produce exactly one job), job claims have a single lease
// winner, and cost settlement is idempotent for an identical amount and
// an error for a conflicting amount. See internal/video/queue and
// internal/video/cost for those guards; this package's DurableSideEffect
// flag is how callers tell Decide that such a replay must not happen.
package retrysafety

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"time"
)

// SentState records how far a request is known to have travelled.
// The HTTP client does not expose whether request bytes left the
// process, so anything dispatched that did not return a provider
// response is SentUnknown, never optimistically SentNotSent.
type SentState int

const (
	// SentNotSent is proven pre-dispatch: the request was refused
	// before dispatch (dial refused, DNS failure, credential pool
	// exhausted) or was never built. Only this state is provably
	// execution-free.
	SentNotSent SentState = iota
	// SentUnknown is the default for a dispatched request that
	// produced no provider response: timeout, reset, or disconnect.
	// The provider may or may not have executed it.
	SentUnknown
	// SentConfirmed means a provider response (any status) was received.
	SentConfirmed
)

// ExecutionState records whether provider-side execution is known.
type ExecutionState int

const (
	// ExecutionNone is proven: the provider rejected the request
	// before execution (authentication/validation failure) or the
	// request was never sent.
	ExecutionNone ExecutionState = iota
	// ExecutionUnknown means execution cannot be ruled out.
	ExecutionUnknown
	// ExecutionConfirmed means the provider produced output (a
	// response body, stream bytes, or a tool result).
	ExecutionConfirmed
)

// IdempotencySupport describes a provider's verified idempotency
// mechanism. Verified is deliberately unreachable from this package's
// current data: it may only be returned after a conformance-manifest
// promotion backed by fixture or live evidence AND an adapter change
// that actually forwards the key for that dialect.
type IdempotencySupport int

const (
	// IdempotencyNone: no verified mechanism for this dialect.
	IdempotencyNone IdempotencySupport = iota
	// IdempotencyUnverified: a mechanism is claimed by the provider
	// but has not been verified by a gateway test or live canary.
	IdempotencyUnverified
	// IdempotencyVerified: a mechanism is verified by evidence and
	// wired into the adapter. No dialect reaches this state today.
	IdempotencyVerified
)

// SupportForDialect returns the verified idempotency support for a
// provider dialect. Every dialect shipped today returns IdempotencyNone
// because the conformance manifest records idempotency as UNTESTED for
// all of them. An unknown dialect also returns None: absence of
// evidence is never treated as support.
func SupportForDialect(_ string) IdempotencySupport {
	return IdempotencyNone
}

// ForwardClientKey reports the gateway's default forwarding decision
// for a client-supplied idempotency key: never, for every dialect.
// The one exception is operator-explicit, not gateway-verified: an
// operator may list the header in a provider's forward_headers, and
// the adapter then forwards exactly what the client sent. That is
// the operator's assertion about their specific provider, not a
// gateway claim, and no dialect's conformance entry verifies it.
// A static provider header carrying an idempotency key is rejected
// at config validation: one fixed key for every request would let a
// provider dedupe distinct requests.
func ForwardClientKey(_ string) bool {
	return false
}

// GenerateProviderKey reports whether the gateway generates a provider
// idempotency key. Today: never. A generated key without a verified
// provider mechanism would be a label, not a guarantee, and a reused
// generated key across distinct requests would be actively harmful.
func GenerateProviderKey() (string, bool) {
	return "", false
}

// BodyEqualityProvesDuplicate reports whether two requests with
// identical bodies may be treated as duplicates. Never: identical
// bodies are distinct requests unless a caller-supplied idempotency
// key (for durable operations) or an explicit opt-in cache (for
// stateless generation, cache-disabled by default, never for streams)
// says otherwise. Body equality is not identity.
func BodyEqualityProvesDuplicate() bool {
	return false
}

// KeyMatches reports whether two caller-supplied idempotency keys
// identify the same durable request. Only non-empty equal keys match;
// an empty key is the absence of an identifier, never a match, and
// key equality is a caller assertion about the request, not proof
// that two bodies are equivalent. Callers must not reuse a key for
// a different request: durable stores return the first job for a
// key hit without comparing bodies.
func KeyMatches(a, b string) bool {
	return a != "" && b != "" && a == b
}

// ClassifyTransportError maps a dispatch error to its sent state.
// Dial refusals and DNS failures are pre-dispatch by construction
// (no bytes reached a provider). Credential-pool exhaustion and
// semaphore cancellation before dispatch are likewise pre-dispatch.
// Every other dispatch error is SentUnknown: the request may have
// been sent, and the provider may have executed it. The conservative
// fallback is deliberate: over-recording ambiguity is safe, while
// claiming "not sent" without proof is how duplicates are created.
func ClassifyTransportError(err error) SentState {
	if err == nil {
		return SentNotSent
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return SentNotSent
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "all configured provider credentials are cooling down"):
		return SentNotSent
		// Deliberately absent: "context canceled". Cancellation may
		// arrive after dispatch, so it is never proof of not-sent;
		// callers check Outcome.Cancelled separately and never
		// replay a cancelled request regardless of SentState.
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.ECONNREFUSED {
		return SentNotSent
	}
	return SentUnknown
}

// Outcome is everything a retry decision needs, and nothing it does
// not. It carries no prompt, response content, credential, or
// upstream error text: decisions and their records stay metadata-only.
type Outcome struct {
	Sent SentState
	// Execution is the provider-execution state for this attempt.
	Execution ExecutionState
	// ResponseCommitted is true once any response bytes were written
	// to the client (headers or stream body). A committed stream
	// must never be replayed, whatever else is true.
	ResponseCommitted bool
	// Cancelled is true when the client or gateway context ended
	// the request. A cancelled request is never replayed.
	Cancelled bool
	// Retryable is true only for error classes that may succeed on
	// another attempt (transport, timeout, 429, 5xx, auth/credential
	// rejection to another deployment). Caller errors (400/422
	// validation) are not retryable: they will fail everywhere.
	Retryable bool
	// DurableSideEffect is true for operations whose replay can
	// duplicate a durable effect (a video job submission, a cost
	// settlement, an external tool execution). For those, any
	// execution state other than ExecutionNone forbids replay.
	DurableSideEffect bool
	// AttemptsUsed counts upstream attempts already spent for this
	// request, including hedged legs. MaxAttempts is the configured
	// routing.max_attempts bound (default 4, hard cap 64).
	AttemptsUsed int
	MaxAttempts  int
	// Deadline is the request's overall deadline; zero means no
	// deadline is configured. A retry past it is never started.
	Deadline time.Time
}

// Decision is the verdict of Decide.
type Decision struct {
	// Allow is true only when a replay may be started.
	Allow bool
	// Reason is a fixed, metadata-only explanation. It never
	// contains prompt, response, or error text.
	Reason string
	// Ambiguous is true when a replay may duplicate provider-side
	// work or cost. An allowed ambiguous replay must be recorded
	// (bus event kind "ambiguous_outcome") before it is started.
	Ambiguous bool
}

// Decide applies the retry-safety policy in a fixed order. The order
// matters: cancellation and commitment are absolute bars that no
// retry budget, error class, or idempotency claim can override.
func Decide(o Outcome, now time.Time) Decision {
	if o.Cancelled {
		return Decision{Reason: "cancelled: never replay a cancelled request"}
	}
	if o.ResponseCommitted {
		return Decision{Reason: "response committed: never replay after client-visible output"}
	}
	if !o.Retryable {
		return Decision{Reason: "error class is not retryable"}
	}
	if o.MaxAttempts <= 0 || o.AttemptsUsed >= o.MaxAttempts {
		return Decision{Reason: "retry budget exhausted"}
	}
	if !o.Deadline.IsZero() && !now.Before(o.Deadline) {
		return Decision{Reason: "retry deadline exceeded"}
	}
	if o.DurableSideEffect && o.Execution != ExecutionNone {
		return Decision{
			Reason:    "durable operation with unknown or confirmed execution must not be replayed; record as ambiguous and resolve manually",
			Ambiguous: true,
		}
	}
	if o.Execution == ExecutionUnknown || o.Sent == SentUnknown {
		return Decision{
			Allow:     true,
			Reason:    "pre-commit ambiguous replay for a stateless request; provider may have executed the first attempt; record as ambiguous_outcome",
			Ambiguous: true,
		}
	}
	return Decision{Allow: true, Reason: "pre-commit safe replay: request was not sent or was rejected before execution"}
}

// TimeoutBeforeSend is the outcome of a failure proven to precede
// dispatch (dial refused, DNS failure). Safe to retry within budget.
func TimeoutBeforeSend(attemptsUsed, maxAttempts int) Outcome {
	return Outcome{Sent: SentNotSent, Execution: ExecutionNone, Retryable: true, AttemptsUsed: attemptsUsed, MaxAttempts: maxAttempts}
}

// TimeoutAfterSend is the outcome of a gateway or provider timeout
// after dispatch. The provider may have executed the request; a
// replay is an ambiguous pre-commit replay for stateless requests
// and is forbidden for durable operations.
func TimeoutAfterSend(attemptsUsed, maxAttempts int, durable bool) Outcome {
	return Outcome{Sent: SentUnknown, Execution: ExecutionUnknown, Retryable: true, DurableSideEffect: durable, AttemptsUsed: attemptsUsed, MaxAttempts: maxAttempts}
}

// Disconnect is the outcome of a transport disconnect after dispatch
// (reset, EOF mid-response before commit). Same ambiguity as a
// timeout after send.
func Disconnect(attemptsUsed, maxAttempts int, durable bool) Outcome {
	return Outcome{Sent: SentUnknown, Execution: ExecutionUnknown, Retryable: true, DurableSideEffect: durable, AttemptsUsed: attemptsUsed, MaxAttempts: maxAttempts}
}

// StatusOutcome maps a received upstream HTTP status to an outcome.
// The mapping is the gateway's conservative reading, not a provider
// guarantee: 401/403/404 reject before model execution, while 429
// and 5xx leave execution unknown (a rate limit or server error
// may arrive after admission). Caller validation errors are not
// retryable at all.
func StatusOutcome(status, attemptsUsed, maxAttempts int) Outcome {
	o := Outcome{Sent: SentConfirmed, AttemptsUsed: attemptsUsed, MaxAttempts: maxAttempts}
	switch {
	case status == 400 || status == 422:
		o.Execution = ExecutionNone
		o.Retryable = false
	case status == 401 || status == 403 || status == 404:
		o.Execution = ExecutionNone
		o.Retryable = true
	case status == 429 || status >= 500 || status == 408:
		o.Execution = ExecutionUnknown
		o.Retryable = true
	default:
		o.Execution = ExecutionUnknown
		o.Retryable = false
	}
	return o
}

// PartialStream returns the outcome for a stream failure. Once any
// bytes are committed, replay is forbidden even though the stream
// is incomplete: the client already owns the prefix and a second
// provider's continuation would corrupt it.
func PartialStream(committed bool, attemptsUsed, maxAttempts int) Outcome {
	if committed {
		return Outcome{Sent: SentConfirmed, Execution: ExecutionConfirmed, ResponseCommitted: true, Retryable: true, AttemptsUsed: attemptsUsed, MaxAttempts: maxAttempts}
	}
	return Outcome{Sent: SentConfirmed, Execution: ExecutionConfirmed, Retryable: true, AttemptsUsed: attemptsUsed, MaxAttempts: maxAttempts}
}
