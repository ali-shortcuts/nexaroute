package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// TestAgentDParentRetryAfterOverflowReturnsFallback covers the overflow guard
// in retryAfterDuration (routing_helpers.go). A Retry-After of 9,999,999,999s
// would overflow time.Duration(seconds) if multiplied naively; with no max
// configured the function must fall back to 30s. (The guard's inner
// `return max` is genuinely unreachable: reaching it needs sec > 9223372036
// while sec < maxSeconds, but maxSeconds can never exceed 9223372036 because
// a time.Duration tops out at ~292 years — verified against the arithmetic,
// so no test forces it.)
func TestAgentDParentRetryAfterOverflowReturnsFallback(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "9999999999")
	// With no max configured the same overflowing value falls back to 30s.
	if got := retryAfterDuration(h, 0); got != 30*time.Second {
		t.Fatalf("retryAfterDuration with overflowing Retry-After and no max = %v, want 30s", got)
	}
	// Sanity: an ordinary value below the cap is still honoured.
	h.Set("Retry-After", "5")
	if got := retryAfterDuration(h, time.Minute); got != 5*time.Second {
		t.Fatalf("retryAfterDuration with Retry-After 5 = %v, want 5s", got)
	}
}
