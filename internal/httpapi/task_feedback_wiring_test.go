package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Phase 19 feedback loop (#510 item 5): evidence resolution order
// at the decision boundary, and an end-to-end proof that
// scorecard-backed preferences shift routing for the task they
// describe — and only that task.

func TestTaskQualityEvidenceResolutionOrder(t *testing.T) {
	cfg := taskIntelTestConfig("http://127.0.0.1:1", "http://127.0.0.1:2", "apply", "")
	srv := testGateway(t, cfg)
	plane := srv.evaluationSnapshot()
	now := time.Now()

	// Tier 3 only: deployment-wide coding card.
	reg := plane.Registry()
	_, err := reg.Upsert(scorecards.Scorecard{
		DeploymentID: "codeprov/m", Version: 1, GeneratedAt: now,
		Values: map[scorecards.Dimension]scorecards.Value{
			scorecards.DimCoding: {Score: 0.5, Provenance: scorecards.ProvenanceEvaluation, SampleCount: 10, Source: "test", SuiteVersion: "v1", EvaluatedAt: now},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if score, samples, ok := srv.taskQualityEvidence("coding", "codeprov/m"); !ok || score != 0.5 || samples != 10 {
		t.Fatalf("tier3 evidence = %v/%d/%v, want 0.5/10/true", score, samples, ok)
	}
	// Tier 2: a task-scoped card beats the deployment-wide one.
	_, err = reg.Upsert(scorecards.Scorecard{
		DeploymentID: "codeprov/m", TaskType: "coding", Version: 1, GeneratedAt: now,
		Values: map[scorecards.Dimension]scorecards.Value{
			scorecards.DimCoding: {Score: 0.9, Provenance: scorecards.ProvenanceEvaluation, SampleCount: 20, Source: "test", SuiteVersion: "v1", EvaluatedAt: now},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if score, samples, ok := srv.taskQualityEvidence("coding", "codeprov/m"); !ok || score != 0.9 || samples != 20 {
		t.Fatalf("tier2 evidence = %v/%d/%v, want 0.9/20/true", score, samples, ok)
	}
	// Tier 1: production feedback beats both.
	for i := 0; i < 40; i++ {
		plane.Feedback().Observe("coding", "codeprov/m", scorecards.FeedbackOutcome{Success: true}, now)
	}
	if score, samples, ok := srv.taskQualityEvidence("coding", "codeprov/m"); !ok || score != 1 || samples != 40 {
		t.Fatalf("tier1 evidence = %v/%d/%v, want 1/40/true", score, samples, ok)
	}
	// No evidence anywhere: ok=false, nothing substituted.
	if _, _, ok := srv.taskQualityEvidence("debugging", "ctxprov/m"); ok {
		t.Fatal("debugging evidence must be absent")
	}
	// A task with no dimension and no feedback: absent.
	if _, _, ok := srv.taskQualityEvidence("vision", "codeprov/m"); ok {
		t.Fatal("vision evidence must be absent")
	}
}

func TestTaskFeedbackShiftsRoutingEndToEnd(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeUpstream(t, &hitsA, false)
	defer upA.Close()
	upB := makeUpstream(t, &hitsB, false)
	defer upB.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "qp"
	cfg.Decision.TimeoutMS = 50
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{{
		ID: "qp", SelectionMode: "select_first",
		Weights:       config.DecisionPolicyWeights{RouterBaseline: 1},
		TaskOverrides: map[string]config.DecisionPolicyWeights{"coding": {RouterBaseline: 1, Quality: 4}},
	}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
	}
	srv := testGateway(t, cfg)
	plane := srv.evaluationSnapshot()
	now := time.Now()
	for i := 0; i < 40; i++ {
		plane.Feedback().Observe("coding", "p2/m2", scorecards.FeedbackOutcome{Success: true, LatencyMS: 50}, now)
		plane.Feedback().Observe("coding", "p1/m1", scorecards.FeedbackOutcome{Success: false, LatencyMS: 50}, now)
	}

	post := func(id, body string) string {
		req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", id)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", id, rr.Code, rr.Body.String())
		}
		return rr.Header().Get("X-Gateway-Deployment")
	}

	// Plain chat first: no override, no quality weight — baseline order.
	// (Order matters: after a successful request the router's own
	// live scores move, which is pre-existing gateway behavior
	// unrelated to the quality component.)
	plain := `{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`
	if got := post("fb-plain", plain); got != "p1/m1" {
		t.Fatalf("plain deployment = %q, want p1/m1 (baseline order preserved)", got)
	}
	// Coding request: feedback evidence flips the route to p2/m2.
	codingBody := strings.Replace(taskIntelCodingBody(t), `"task-model"`, `"nexa-code"`, 1)
	if got := post("fb-coding", codingBody); got != "p2/m2" {
		t.Fatalf("coding deployment = %q, want p2/m2 (feedback-backed preference)", got)
	}
}
