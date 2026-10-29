package events

import (
	"testing"
	"time"
)

// Wave-3 lifecycle coverage: Summarize's merge of a second event into
// an earlier snapshot — request-ID filling, parent/endpoint/pool
// attribution, earlier StartedAt, token and cost accumulation, and
// attempt-field updates for success, failover and cancellation. The
// CorrelationID-fill branch is unreachable through Summarize (every
// event carries a CorrelationID from creation) and is not faked here.
// Note: only route_attempt carries PhaseAttempt; later events update
// the open attempt through lastStartedAttempt instead of appending.

func wave3SummarizeOne(t *testing.T, events []Event) RequestLifecycle {
	t.Helper()
	got := Summarize(events, SummarizeOptions{})
	if len(got) != 1 {
		t.Fatalf("Summarize returned %d lifecycles, want 1", len(got))
	}
	return got[0]
}

func TestWave3MergeFillsRequestIDAndAttribution(t *testing.T) {
	events := []Event{
		{Time: time.Unix(1000, 0), RequestID: "req-1", CorrelationID: "corr-1", Kind: "request_accepted", Phase: "request", Deployment: "dep"},
		{Time: time.Unix(900, 0), CorrelationID: "corr-1", Kind: "route_resolved", Phase: "request", Deployment: "dep",
			ParentRequestID: "parent-1", VirtualEndpoint: "pub-model", Pool: "pool-a",
			InputTokens: 11, OutputTokens: 22, CostBasis: "usage"},
	}
	sum := wave3SummarizeOne(t, events)
	if sum.RequestID != "req-1" {
		t.Fatalf("request id = %q, want req-1 (second event must not overwrite)", sum.RequestID)
	}
	if sum.ParentRequestID != "parent-1" || sum.VirtualEndpoint != "pub-model" || sum.Pool != "pool-a" {
		t.Fatalf("attribution = %q/%q/%q, want parent-1/pub-model/pool-a",
			sum.ParentRequestID, sum.VirtualEndpoint, sum.Pool)
	}
	if sum.InputTokens != 11 || sum.OutputTokens != 22 || sum.CostBasis != "usage" {
		t.Fatalf("tokens/cost = %d/%d/%q, want 11/22/usage", sum.InputTokens, sum.OutputTokens, sum.CostBasis)
	}
	if !sum.StartedAt.Equal(time.Unix(900, 0)) {
		t.Fatalf("startedAt = %v, want the earlier event time", sum.StartedAt)
	}
}

func TestWave3MergeFillsMissingRequestIDFromLaterEvent(t *testing.T) {
	events := []Event{
		{Time: time.Unix(1000, 0), CorrelationID: "corr-2", Kind: "request_accepted", Phase: "request", Deployment: "dep"},
		{Time: time.Unix(1001, 0), RequestID: "req-2", CorrelationID: "corr-2", Kind: "route_resolved", Phase: "request", Deployment: "dep"},
	}
	sum := wave3SummarizeOne(t, events)
	if sum.RequestID != "req-2" {
		t.Fatalf("request id = %q, want req-2 filled from the later event", sum.RequestID)
	}
}

func TestWave3MergeUpdatesOpenAttemptFields(t *testing.T) {
	events := []Event{
		{Time: time.Unix(1000, 0), CorrelationID: "corr-3", Kind: "route_attempt", Phase: PhaseAttempt, Deployment: "dep", Provider: "p1"},
		{Time: time.Unix(1001, 0), CorrelationID: "corr-3", Kind: "route_ok", Deployment: "dep", Provider: "p1",
			LatencyMS: 42, StatusCode: 200, TTFTMS: 7,
			Retryable: true, RetryAfterMS: 250, FailoverClass: "retry", StreamCommitted: true},
	}
	sum := wave3SummarizeOne(t, events)
	if len(sum.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(sum.Attempts))
	}
	a := sum.Attempts[0]
	if a.LatencyMS != 42 || a.StatusCode != 200 || a.TTFTMS != 7 {
		t.Fatalf("attempt metrics = %d/%d/%d, want 42/200/7", a.LatencyMS, a.StatusCode, a.TTFTMS)
	}
	if !a.Retryable || a.RetryAfterMS != 250 || a.FailoverClass != "retry" || !a.Committed {
		t.Fatalf("attempt fields = %+v, want retryable/250/retry/committed", a)
	}
	if sum.Terminal != TerminalSucceeded || sum.State != "succeeded" {
		t.Fatalf("route_ok must terminate as succeeded: %q/%q", sum.Terminal, sum.State)
	}
}

func TestWave3MergeClientDisconnectCancels(t *testing.T) {
	events := []Event{
		{Time: time.Unix(1000, 0), CorrelationID: "corr-4", Kind: "route_attempt", Phase: PhaseAttempt, Deployment: "dep", Provider: "p1"},
		{Time: time.Unix(1001, 0), CorrelationID: "corr-4", Kind: "client_disconnect", Deployment: "dep", Provider: "p1"},
	}
	sum := wave3SummarizeOne(t, events)
	if sum.Terminal != TerminalCancelled || sum.State != "cancelled" {
		t.Fatalf("client_disconnect must terminate as cancelled: %q/%q", sum.Terminal, sum.State)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Outcome != TerminalCancelled {
		t.Fatalf("attempt outcome = %+v, want cancelled", sum.Attempts)
	}
}

func TestWave3MergeFailoverCountsAndClassifiesAttempt(t *testing.T) {
	events := []Event{
		{Time: time.Unix(1000, 0), CorrelationID: "corr-5", Kind: "route_attempt", Phase: PhaseAttempt, Deployment: "dep", Provider: "p1"},
		{Time: time.Unix(1001, 0), CorrelationID: "corr-5", Kind: "failover", Deployment: "dep", Provider: "p1", FailoverClass: "quota"},
	}
	sum := wave3SummarizeOne(t, events)
	if sum.FailoverCount != 1 {
		t.Fatalf("failover count = %d, want 1", sum.FailoverCount)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].FailoverClass != "quota" {
		t.Fatalf("attempt failover class = %+v, want quota", sum.Attempts)
	}
}
