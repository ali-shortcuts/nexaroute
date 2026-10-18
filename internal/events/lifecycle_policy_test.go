package events

import "testing"

func TestSummarizeRecordsAppliedPolicyVersion(t *testing.T) {
	rid := "req-policy"
	list := []Event{
		ev(1, "request_accepted", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAccepted }),
		ev(2, "decision_ok", func(e *Event) {
			e.RequestID = rid
			e.DecisionPolicyID = "balanced"
			e.DecisionPolicyVersion = "2026-10-18.1"
			e.DecisionSelected = "dep-b"
		}),
		ev(3, "request_finished", func(e *Event) { e.RequestID = rid; e.Phase = PhaseFinished; e.Terminal = TerminalSucceeded }),
	}
	got := Summarize(list, SummarizeOptions{})
	if len(got) != 1 {
		t.Fatalf("got %d lifecycles, want 1", len(got))
	}
	lc := got[0]
	if lc.DecisionPolicyID != "balanced" {
		t.Fatalf("policy id = %q, want balanced", lc.DecisionPolicyID)
	}
	if lc.DecisionPolicyVersion != "2026-10-18.1" {
		t.Fatalf("policy version = %q, want 2026-10-18.1", lc.DecisionPolicyVersion)
	}
}

func TestSummarizeClassifiesFailoverTrigger(t *testing.T) {
	rid := "req-failover"
	list := []Event{
		ev(1, "request_accepted", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAccepted }),
		ev(2, "route_attempt", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseAttempt
			e.Deployment = "dep-a"
			e.AttemptKind = "primary"
		}),
		ev(3, "route_fail", func(e *Event) {
			e.RequestID = rid
			e.Deployment = "dep-a"
			e.ErrorType = "provider_rate_limited"
			e.StatusCode = 429
		}),
		ev(4, "failover", func(e *Event) {
			e.RequestID = rid
			e.Deployment = "dep-a"
			e.FailoverClass = "provider_rate_limited"
		}),
		ev(5, "request_finished", func(e *Event) { e.RequestID = rid; e.Phase = PhaseFinished; e.Terminal = TerminalSucceeded }),
	}
	got := Summarize(list, SummarizeOptions{})
	if len(got) != 1 {
		t.Fatalf("got %d lifecycles, want 1", len(got))
	}
	lc := got[0]
	if lc.FailoverCount != 1 {
		t.Fatalf("failover count = %d, want 1", lc.FailoverCount)
	}
	if lc.FailoverClass != "provider_rate_limited" {
		t.Fatalf("lifecycle failover class = %q, want provider_rate_limited", lc.FailoverClass)
	}
	if len(lc.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(lc.Attempts))
	}
	if lc.Attempts[0].FailoverClass != "provider_rate_limited" {
		t.Fatalf("attempt failover class = %q, want provider_rate_limited", lc.Attempts[0].FailoverClass)
	}
}
