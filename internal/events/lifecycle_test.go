package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ev(seq uint64, kind string, mut func(*Event)) Event {
	e := Event{Seq: seq, Kind: kind, Time: time.Unix(0, int64(seq))}
	if mut != nil {
		mut(&e)
	}
	return e
}

func TestSummarizeBuildsSingleRequestLifecycle(t *testing.T) {
	rid := "req-1"
	list := []Event{
		ev(1, "request_accepted", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseAccepted
			e.RequestedModel = "claude-public"
			e.Adapter = "anthropic"
			e.Streaming = true
		}),
		ev(2, "route_resolved", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseResolved
			e.PublicModel = "claude-public"
			e.ResolvedModel = "upstream-physical"
			e.RouteProfile = "prof-1"
		}),
		ev(3, "route_attempt", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseAttempt
			e.Attempt = 1
			e.AttemptKind = "primary"
			e.Deployment = "dep-a"
			e.Provider = "prov-a"
		}),
		ev(4, "stream_committed", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseCommitted
			e.TTFTMS = 42
		}),
		ev(5, "route_ok", func(e *Event) {
			e.RequestID = rid
			e.Deployment = "dep-a"
			e.LatencyMS = 120
			e.StatusCode = 200
		}),
		ev(6, "request_finished", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseFinished
			e.Terminal = TerminalSucceeded
			e.StatusCode = 200
		}),
	}
	got := Summarize(list, SummarizeOptions{Now: time.Unix(0, 1000), StaleAfter: time.Minute})
	if len(got) != 1 {
		t.Fatalf("got %d lifecycles want 1", len(got))
	}
	lc := got[0]
	if lc.State != TerminalSucceeded || lc.Terminal != TerminalSucceeded {
		t.Fatalf("state=%q terminal=%q", lc.State, lc.Terminal)
	}
	if lc.RequestedModel != "claude-public" || lc.ResolvedModel != "upstream-physical" || lc.RouteProfile != "prof-1" {
		t.Fatalf("route identity not merged: %+v", lc)
	}
	if lc.AttemptCount != 1 || len(lc.Attempts) != 1 || lc.Attempts[0].Outcome != TerminalSucceeded {
		t.Fatalf("attempts=%+v", lc.Attempts)
	}
	if lc.TTFTMS != 42 || !lc.Streaming || lc.LatencyMS != 120 {
		t.Fatalf("timings not merged: %+v", lc)
	}
}

func TestSummarizeFailoverShowsFailedThenBackupInOrder(t *testing.T) {
	rid := "req-failover"
	list := []Event{
		ev(1, "request_accepted", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAccepted }),
		ev(2, "route_attempt", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseAttempt
			e.Attempt = 1
			e.Deployment = "dep-a"
			e.Provider = "prov-a"
		}),
		ev(3, "route_fail", func(e *Event) {
			e.RequestID = rid
			e.Deployment = "dep-a"
			e.ErrorType = "provider_timeout"
			e.LatencyMS = 30
			e.StatusCode = 429
		}),
		ev(4, "failover", func(e *Event) { e.RequestID = rid; e.Deployment = "dep-a" }),
		ev(5, "route_attempt", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseAttempt
			e.Attempt = 2
			e.Deployment = "dep-b"
			e.Provider = "prov-b"
		}),
		ev(6, "route_ok", func(e *Event) { e.RequestID = rid; e.Deployment = "dep-b"; e.LatencyMS = 55 }),
		ev(7, "request_finished", func(e *Event) { e.RequestID = rid; e.Phase = PhaseFinished; e.Terminal = TerminalSucceeded }),
	}
	got := Summarize(list, SummarizeOptions{Now: time.Unix(0, 1000)})
	if len(got) != 1 {
		t.Fatalf("got %d lifecycles", len(got))
	}
	lc := got[0]
	if len(lc.Attempts) != 2 {
		t.Fatalf("attempts=%+v", lc.Attempts)
	}
	if lc.Attempts[0].Deployment != "dep-a" || lc.Attempts[0].Outcome != TerminalFailed {
		t.Fatalf("first attempt not failed: %+v", lc.Attempts[0])
	}
	if lc.Attempts[1].Deployment != "dep-b" || lc.Attempts[1].Outcome != TerminalSucceeded {
		t.Fatalf("backup attempt not succeeded: %+v", lc.Attempts[1])
	}
	if lc.FailoverCount != 1 {
		t.Fatalf("failover count=%d want 1", lc.FailoverCount)
	}
	if lc.Attempts[0].ErrorType != "provider_timeout" || lc.Attempts[0].StatusCode != 429 {
		t.Fatalf("failure metadata lost: %+v", lc.Attempts[0])
	}
}

func TestSummarizeHandlesDuplicateAndOutOfOrderEvents(t *testing.T) {
	rid := "req-dup"
	list := []Event{
		ev(3, "route_ok", func(e *Event) { e.RequestID = rid; e.Deployment = "dep-a" }),
		ev(1, "request_accepted", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAccepted; e.RequestedModel = "m" }),
		ev(2, "route_attempt", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAttempt; e.Attempt = 1; e.Deployment = "dep-a" }),
		// Replayed duplicates (as after an SSE resume) must be idempotent.
		ev(1, "request_accepted", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAccepted; e.RequestedModel = "m" }),
		ev(3, "route_ok", func(e *Event) { e.RequestID = rid; e.Deployment = "dep-a" }),
	}
	got := Summarize(list, SummarizeOptions{Now: time.Unix(0, 100)})
	if len(got) != 1 {
		t.Fatalf("got %d lifecycles from duplicates", len(got))
	}
	lc := got[0]
	if len(lc.Attempts) != 1 {
		t.Fatalf("duplicate attempt created: %+v", lc.Attempts)
	}
	if lc.State != TerminalSucceeded || lc.RequestedModel != "m" {
		t.Fatalf("duplicate replay corrupted lifecycle: %+v", lc)
	}
}

func TestSummarizeMarksStaleButNotLive(t *testing.T) {
	rid := "req-stale"
	base := time.Unix(1000, 0)
	list := []Event{{Seq: 1, Time: base, RequestID: rid, Kind: "request_accepted", Phase: PhaseAccepted}}
	stale := Summarize(list, SummarizeOptions{Now: base.Add(10 * time.Minute), StaleAfter: 5 * time.Minute})
	if len(stale) != 1 || stale[0].State != StateStale {
		t.Fatalf("expected stale, got %+v", stale)
	}
	active := Summarize(list, SummarizeOptions{Now: base.Add(time.Second), StaleAfter: 5 * time.Minute})
	if len(active) != 1 || active[0].State != StateActive {
		t.Fatalf("expected active, got %+v", active)
	}
}

func TestSummarizeIgnoresUnrelatedEvents(t *testing.T) {
	list := []Event{
		{Seq: 1, Kind: "model_healthy", Deployment: "dep-a", Message: "deployment healthy"},
		{Seq: 2, Kind: "probe_result", Deployment: "dep-a"},
	}
	if got := Summarize(list, SummarizeOptions{}); len(got) != 0 {
		t.Fatalf("unrelated events formed lifecycles: %+v", got)
	}
}

func TestSummarizeNeverExposesMessageOrContent(t *testing.T) {
	rid := "req-priv"
	secret := "Bearer sk-super-secret-token"
	list := []Event{
		ev(1, "request_accepted", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAccepted; e.Message = secret; e.RequestedModel = "m" }),
		ev(2, "request_finished", func(e *Event) {
			e.RequestID = rid
			e.Phase = PhaseFinished
			e.Terminal = TerminalSucceeded
			e.Message = secret
		}),
	}
	got := Summarize(list, SummarizeOptions{})
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-super-secret-token") || strings.Contains(string(raw), "Bearer") {
		t.Fatalf("lifecycle leaked message content: %s", raw)
	}
}

func TestSummarizeBoundsReturnedRequests(t *testing.T) {
	var list []Event
	for i := 0; i < 50; i++ {
		rid := "r" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		list = append(list, ev(uint64(i+1), "request_accepted", func(e *Event) { e.RequestID = rid; e.Phase = PhaseAccepted }))
	}
	got := Summarize(list, SummarizeOptions{MaxRequests: 5})
	if len(got) != 5 {
		t.Fatalf("got %d want 5", len(got))
	}
}
