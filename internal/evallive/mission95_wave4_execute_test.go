package evallive

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// A nil executor reports "not configured" instead of panicking on its
// counters: Execute must be safe to call on a zero executor reference.
func TestWave4ExecuteNilExecutorFailsClosed(t *testing.T) {
	var e *LiveEvaluationExecutor
	if _, err := e.Execute(context.Background(), eval.Case{ID: "c"}); err == nil || err.Error() != "live evaluation executor is not configured" {
		t.Fatalf("err = %v, want the not-configured error", err)
	}
}

// An adapter swapped to a non-isolated one after construction makes
// LiveComplete fail, and Execute propagates that executor failure as an
// error — never as a synthetic scored outcome.
func TestWave4ExecuteNonIsolatedAdapterIsExecutorError(t *testing.T) {
	srv := liveUpstream(t, nil, http.StatusOK, 0)
	twin := liveTwin(t, srv.URL)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), twin, []Prompt{{CaseID: "c1", Prompt: "hello <<ANSWER:yes>>"}}, 64)
	if err != nil {
		t.Fatal(err)
	}
	p := config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true}
	p.ApplyDefaults()
	production, err := providers.NewAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	exec.adapter = production
	out, err := exec.Execute(context.Background(), eval.Case{ID: "c1"})
	if err == nil || !strings.Contains(err.Error(), "not evaluation-isolated") {
		t.Fatalf("err = %v, want the evaluation-isolation refusal", err)
	}
	if out != (eval.Outcome{}) {
		t.Fatalf("out = %+v, want zero outcome on executor failure", out)
	}
	if exec.Calls() != 1 || exec.Failures() != 0 {
		t.Fatalf("calls=%d failures=%d, want 1/0: an executor failure is not a scored failure", exec.Calls(), exec.Failures())
	}
}

// A clock that runs backwards between the two reads clamps latency at zero
// instead of reporting a negative latency.
func TestWave4ExecuteBackwardsClockClampsLatency(t *testing.T) {
	srv := liveUpstream(t, nil, http.StatusOK, 0)
	twin := liveTwin(t, srv.URL)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), twin, []Prompt{{CaseID: "c1", Prompt: "hello <<ANSWER:yes>>"}}, 64)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC()
	earlier := later.Add(-time.Hour)
	calls := 0
	exec.now = func() time.Time {
		calls++
		if calls == 1 {
			return later
		}
		return earlier
	}
	out, err := exec.Execute(context.Background(), eval.Case{ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.LatencyMS != 0 {
		t.Fatalf("LatencyMS = %d, want 0 for a backwards clock", out.LatencyMS)
	}
	if out.Status != eval.OutcomeOK || out.Output != "yes" {
		t.Fatalf("out = %+v, want a successful outcome despite the clock", out)
	}
}

// A transport failure at the upstream is graded as upstream_transport
// evidence (not an executor error) and counts exactly once as a failure
// and once as an upstream call.
func TestWave4ExecuteTransportFailureGraded(t *testing.T) {
	srv := liveUpstream(t, nil, http.StatusOK, 0)
	twin := liveTwin(t, srv.URL)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), twin, []Prompt{{CaseID: "c1", Prompt: "hello <<ANSWER:yes>>"}}, 64)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	out, err := exec.Execute(context.Background(), eval.Case{ID: "c1"})
	if err != nil {
		t.Fatalf("transport failure must be evidence, not an error: %v", err)
	}
	if out.Status != eval.OutcomeError || out.ErrorType != "upstream_transport" {
		t.Fatalf("out = %+v, want status error with upstream_transport", out)
	}
	if exec.Calls() != 1 || exec.UpstreamCalls() != 1 || exec.Failures() != 1 || exec.Missing() != 0 {
		t.Fatalf("calls=%d upstream=%d failures=%d missing=%d, want 1/1/1/0", exec.Calls(), exec.UpstreamCalls(), exec.Failures(), exec.Missing())
	}
}
