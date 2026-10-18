package httpapi

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Phase 19 feedback loop (#510 item 5, producer wiring): real
// request outcomes in the protocol handlers land in the feedback
// tracker keyed by the classified task — successes through the
// shared success funnel, failures through the failure paths.

func TestTaskFeedbackProducer_SuccessAndFailureSamples(t *testing.T) {
	// Success: the coding request completes on the fake upstream.
	var calls atomic.Int64
	up := taskIntelUpstream(t, &calls, "code")
	defer up.Close()
	cfg := taskIntelTestConfig(up.URL, up.URL, "suggest", "")
	srv := testGateway(t, cfg)
	rr := taskIntelPost(t, srv, "fb-prod-ok", taskIntelCodingBody(t), nil)
	if rr.Code != 200 {
		t.Fatalf("coding request status=%d body=%s", rr.Code, rr.Body.String())
	}
	plane := srv.evaluationSnapshot()
	card, ok := plane.Feedback().Card("coding", "codeprov/m", time.Now())
	if !ok {
		t.Fatal("no feedback card after a completed coding request")
	}
	v := card.Values[scorecards.DimAvailability]
	if v.Score != 1 || v.SampleCount < 1 {
		t.Fatalf("success card value = %+v, want availability 1 with samples", v)
	}
	if card.TaskType != "coding" {
		t.Fatalf("card task = %q, want coding", card.TaskType)
	}

	// Failure: the coding pool points at a dead upstream, so the
	// attempt fails and a failure sample is recorded.
	cfg2 := taskIntelTestConfig("http://127.0.0.1:1", "http://127.0.0.1:1", "suggest", "")
	srv2 := testGateway(t, cfg2)
	rr2 := taskIntelPost(t, srv2, "fb-prod-fail", taskIntelCodingBody(t), nil)
	if rr2.Code == 200 {
		t.Fatalf("dead-upstream request unexpectedly succeeded: %s", rr2.Body.String())
	}
	card2, ok := srv2.evaluationSnapshot().Feedback().Card("coding", "codeprov/m", time.Now())
	if !ok {
		t.Fatal("no feedback card after a failed coding request")
	}
	v2 := card2.Values[scorecards.DimAvailability]
	if v2.Score != 0 || v2.SampleCount < 1 {
		t.Fatalf("failure card value = %+v, want availability 0 with samples", v2)
	}
}
