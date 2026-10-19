package providers

import (
	"net/http"
	"testing"
	"time"
)

// #435: quota evidence must age out on a hard bound, not only on
// window rollover. A provider that sends remaining-* headers
// without reset-* headers (or with a far-future reset) would
// otherwise let an observation steer routing and reporting
// forever. Older than quotaEvidenceMaxAge, the evidence reports
// as unknown (-1); fresh evidence is untouched.
func TestQuotaEvidenceAgesOutWithoutReset(t *testing.T) {
	a := &httpAdapter{}
	a.requestLimit.Store(-1)
	a.remainingRequests.Store(-1)
	a.tokenLimit.Store(-1)
	a.remainingTokens.Store(-1)

	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "100")
	h.Set("x-ratelimit-remaining-requests", "3")
	h.Set("x-ratelimit-limit-tokens", "5000")
	h.Set("x-ratelimit-remaining-tokens", "40")
	// Deliberately NO reset headers: rollover invalidation can
	// never fire for this provider.
	a.observeRateLimitHeaders(h, 1)

	st := a.Stats()
	if st.RemainingRequests != 3 || st.RemainingTokens != 40 {
		t.Fatalf("fresh evidence = %d/%d, want 3/40", st.RemainingRequests, st.RemainingTokens)
	}

	// Age the observations past the bound.
	old := time.Now().Add(-2 * quotaEvidenceMaxAge).Unix()
	a.requestRemainObservedUnix.Store(old)
	a.tokenRemainObservedUnix.Store(old)
	st = a.Stats()
	if st.RemainingRequests != -1 || st.RemainingTokens != -1 {
		t.Fatalf("aged evidence = %d/%d, want -1/-1 (unknown)", st.RemainingRequests, st.RemainingTokens)
	}
	if st.EffectiveRemainingRequests != -1 || st.EffectiveRemainingTokens != -1 {
		t.Fatalf("aged effective evidence = %d/%d, want -1/-1", st.EffectiveRemainingRequests, st.EffectiveRemainingTokens)
	}
	// Limits are configuration-shaped facts from the same
	// headers; they are not aged (only remaining evidence is).
	if st.RequestLimit != 100 || st.TokenLimit != 5000 {
		t.Fatalf("limits = %d/%d, want 100/5000 (limits do not age)", st.RequestLimit, st.TokenLimit)
	}
}

// Aging is per-resource: a fresh token observation must not
// resurrect stale request evidence, and vice versa.
func TestQuotaEvidenceAgingIsPerResource(t *testing.T) {
	a := &httpAdapter{}
	a.requestLimit.Store(-1)
	a.remainingRequests.Store(-1)
	a.tokenLimit.Store(-1)
	a.remainingTokens.Store(-1)

	h := http.Header{}
	h.Set("x-ratelimit-remaining-requests", "3")
	h.Set("x-ratelimit-remaining-tokens", "40")
	a.observeRateLimitHeaders(h, 1)

	a.tokenRemainObservedUnix.Store(time.Now().Add(-2 * quotaEvidenceMaxAge).Unix())
	st := a.Stats()
	if st.RemainingRequests != 3 {
		t.Fatalf("request evidence = %d, want 3 (fresh)", st.RemainingRequests)
	}
	if st.RemainingTokens != -1 {
		t.Fatalf("token evidence = %d, want -1 (aged)", st.RemainingTokens)
	}
}
