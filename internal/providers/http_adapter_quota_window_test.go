package providers

import (
	"net/http"
	"testing"
	"time"
)

// Quota evidence is valid only for the provider window it was observed in.
// When a later response advances a resource's reset (the window rolled over)
// without carrying a fresh remaining value for that resource, the previous
// window's remaining must be discarded as unknown — never paired with the new
// window's future reset, where remaining==0 would pose as fresh proof of
// exhaustion and pin the provider at maximum routing pressure for a whole
// new window.
func TestQuotaWindowRolloverWithoutFreshRemainingDiscardsStaleEvidence(t *testing.T) {
	a := &httpAdapter{}
	a.requestLimit.Store(-1)
	a.remainingRequests.Store(-1)
	a.tokenLimit.Store(-1)
	a.remainingTokens.Store(-1)

	r1 := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	r2 := time.Now().Add(70 * time.Minute).UTC().Truncate(time.Second)

	first := http.Header{}
	first.Set("x-ratelimit-limit-requests", "100")
	first.Set("x-ratelimit-remaining-requests", "0")
	first.Set("x-ratelimit-reset-requests", r1.Format(time.RFC3339))
	first.Set("x-ratelimit-limit-tokens", "5000")
	first.Set("x-ratelimit-remaining-tokens", "0")
	first.Set("x-ratelimit-reset-tokens", r1.Format(time.RFC3339))
	a.observeRateLimitHeaders(first, 1)

	st := a.Stats()
	if st.RemainingRequests != 0 || st.RemainingTokens != 0 {
		t.Fatalf("initial quota evidence not recorded: %+v", st)
	}

	// The window rolls over: a later response reports only the new resets
	// (partial headers are common, e.g. on 429s) and no remaining values.
	rollover := http.Header{}
	rollover.Set("x-ratelimit-reset-requests", r2.Format(time.RFC3339))
	rollover.Set("x-ratelimit-reset-tokens", r2.Format(time.RFC3339))
	a.observeRateLimitHeaders(rollover, 2)

	st = a.Stats()
	if st.RemainingRequests != -1 || st.EffectiveRemainingRequests != -1 {
		t.Fatalf("stale request evidence survived window rollover: %+v", st)
	}
	if st.RemainingTokens != -1 || st.EffectiveRemainingTokens != -1 {
		t.Fatalf("stale token evidence survived window rollover: %+v", st)
	}
	// The resets themselves and the limits are facts and stay tracked.
	if st.RequestResetUnix != r2.Unix() || st.TokenResetUnix != r2.Unix() {
		t.Fatalf("advanced resets were not kept: %+v", st)
	}
	if st.RequestLimit != 100 || st.TokenLimit != 5000 {
		t.Fatalf("limits must survive rollover: %+v", st)
	}
}

// A rollover response that DOES carry a fresh remaining value replaces the
// evidence for the new window; nothing is discarded.
func TestQuotaWindowRolloverWithFreshRemainingKeepsEvidence(t *testing.T) {
	a := &httpAdapter{}
	a.remainingRequests.Store(-1)
	a.remainingTokens.Store(-1)

	r1 := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	r2 := time.Now().Add(70 * time.Minute).UTC().Truncate(time.Second)

	first := http.Header{}
	first.Set("x-ratelimit-remaining-requests", "0")
	first.Set("x-ratelimit-reset-requests", r1.Format(time.RFC3339))
	a.observeRateLimitHeaders(first, 1)

	rollover := http.Header{}
	rollover.Set("x-ratelimit-remaining-requests", "77")
	rollover.Set("x-ratelimit-reset-requests", r2.Format(time.RFC3339))
	a.observeRateLimitHeaders(rollover, 2)

	st := a.Stats()
	if st.RemainingRequests != 77 || st.EffectiveRemainingRequests != 77 || st.RequestResetUnix != r2.Unix() {
		t.Fatalf("fresh rollover evidence not kept: %+v", st)
	}
}

// A reset-only response for the SAME window (reset unchanged) is not a
// rollover and must not discard the window's remaining evidence.
func TestQuotaSameWindowResetOnlyKeepsRemaining(t *testing.T) {
	a := &httpAdapter{}
	a.remainingRequests.Store(-1)
	a.remainingTokens.Store(-1)

	r1 := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)

	first := http.Header{}
	first.Set("x-ratelimit-remaining-requests", "3")
	first.Set("x-ratelimit-reset-requests", r1.Format(time.RFC3339))
	a.observeRateLimitHeaders(first, 1)

	same := http.Header{}
	same.Set("x-ratelimit-reset-requests", r1.Format(time.RFC3339))
	a.observeRateLimitHeaders(same, 2)

	st := a.Stats()
	if st.RemainingRequests != 3 {
		t.Fatalf("same-window reset-only response discarded evidence: %+v", st)
	}
}

// An out-of-order reset-only response from an older dispatch must not
// discard remaining evidence recorded by a newer dispatch.
func TestQuotaRolloverDoesNotDiscardNewerRemainingFromLaterDispatch(t *testing.T) {
	a := &httpAdapter{}
	a.remainingRequests.Store(-1)
	a.remainingTokens.Store(-1)

	r1 := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	r2 := time.Now().Add(70 * time.Minute).UTC().Truncate(time.Second)

	first := http.Header{}
	first.Set("x-ratelimit-remaining-requests", "0")
	first.Set("x-ratelimit-reset-requests", r1.Format(time.RFC3339))
	a.observeRateLimitHeaders(first, 1)

	// A newer dispatch reports fresh remaining without a reset.
	newer := http.Header{}
	newer.Set("x-ratelimit-remaining-requests", "55")
	a.observeRateLimitHeaders(newer, 3)

	// An older in-flight dispatch completes late, carrying only an
	// advanced reset. Its evidence is older than the seq-3 remaining and
	// must not invalidate it.
	late := http.Header{}
	late.Set("x-ratelimit-reset-requests", r2.Format(time.RFC3339))
	a.observeRateLimitHeaders(late, 2)

	st := a.Stats()
	if st.RemainingRequests != 55 {
		t.Fatalf("older dispatch discarded newer remaining evidence: %+v", st)
	}
}

// Once a window's reset has passed, a remaining value that was observed
// during that window is stale evidence: Stats must report it as unknown
// rather than presenting the dead window's value as current headroom.
func TestQuotaExpiredWindowEvidenceIsUnknownInStats(t *testing.T) {
	a := &httpAdapter{}
	now := time.Now().Unix()
	reset := now - 60
	a.remainingRequests.Store(0)
	a.requestResetUnix.Store(reset)
	a.requestRemainObservedUnix.Store(reset - 30)
	a.remainingTokens.Store(0)
	a.tokenResetUnix.Store(reset)
	a.tokenRemainObservedUnix.Store(reset - 30)

	st := a.Stats()
	if st.RemainingRequests != -1 || st.EffectiveRemainingRequests != -1 {
		t.Fatalf("expired request-window evidence not unknown: %+v", st)
	}
	if st.RemainingTokens != -1 || st.EffectiveRemainingTokens != -1 {
		t.Fatalf("expired token-window evidence not unknown: %+v", st)
	}
	// The reset timestamps themselves remain recorded facts.
	if st.RequestResetUnix != reset || st.TokenResetUnix != reset {
		t.Fatalf("reset facts must be preserved: %+v", st)
	}
}

// A remaining value observed AFTER the previous window's reset passed is
// evidence for the current window (its own reset may simply not have been
// reported yet) and must not be expired.
func TestQuotaRemainingObservedAfterResetStaysKnown(t *testing.T) {
	a := &httpAdapter{}
	now := time.Now().Unix()
	a.remainingRequests.Store(42)
	a.requestResetUnix.Store(now - 60)
	a.requestRemainObservedUnix.Store(now)

	st := a.Stats()
	if st.RemainingRequests != 42 || st.EffectiveRemainingRequests != 42 {
		t.Fatalf("post-window remaining evidence was wrongly expired: %+v", st)
	}
}

// Rollover of one resource must not touch the sibling resource's evidence.
func TestQuotaWindowRolloverDiscardsOnlyRolledResource(t *testing.T) {
	a := &httpAdapter{}
	a.remainingRequests.Store(-1)
	a.remainingTokens.Store(-1)

	r1 := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	r2 := time.Now().Add(70 * time.Minute).UTC().Truncate(time.Second)

	first := http.Header{}
	first.Set("x-ratelimit-remaining-requests", "0")
	first.Set("x-ratelimit-reset-requests", r1.Format(time.RFC3339))
	first.Set("x-ratelimit-remaining-tokens", "250")
	first.Set("x-ratelimit-reset-tokens", r1.Format(time.RFC3339))
	a.observeRateLimitHeaders(first, 1)

	rollover := http.Header{}
	rollover.Set("x-ratelimit-reset-requests", r2.Format(time.RFC3339))
	a.observeRateLimitHeaders(rollover, 2)

	st := a.Stats()
	if st.RemainingRequests != -1 {
		t.Fatalf("rolled request evidence not discarded: %+v", st)
	}
	if st.RemainingTokens != 250 || st.TokenResetUnix != r1.Unix() {
		t.Fatalf("sibling token evidence must be untouched: %+v", st)
	}
}
