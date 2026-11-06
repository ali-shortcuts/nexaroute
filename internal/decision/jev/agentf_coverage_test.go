package jev

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFTaskSummaryTruncates verifies a task summary built from an
// unbounded task-type label is truncated to MaxTaskLength, never leaking an
// unbounded string into the request.
func TestAgentFTaskSummaryTruncates(t *testing.T) {
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: taskprofile.TaskType(strings.Repeat("x", 5000))},
		Candidates:  []decision.Candidate{{ID: "a"}, {ID: "b"}},
	}
	got := BuildTaskSummary(req)
	if len(got) != remote.MaxTaskLength {
		t.Fatalf("BuildTaskSummary len = %d, want %d (truncated)", len(got), remote.MaxTaskLength)
	}
	if !strings.HasPrefix(got, "task_type=") {
		t.Fatalf("truncated summary lost its prefix: %q", got[:32])
	}
}

// TestAgentFDecideTooManyCandidates verifies a request with more candidates
// than the Jev request bound fails validation and abstains fail-open with the
// invalid-response reason, instead of sending an oversized request.
func TestAgentFDecideTooManyCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when request validation fails")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := NewProvider(ProviderConfig{
		ID: "jev-main", Type: "jev", Enabled: true, APIKey: "test-key",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cands := make([]decision.Candidate, 0, remote.MaxCandidates+1)
	for i := 0; i <= remote.MaxCandidates; i++ {
		cands = append(cands, decision.Candidate{ID: "c" + string(rune('a'+i%26)) + "-" + itoaTest(i), OriginalRank: i})
	}
	res, err := p.Decide(context.Background(), decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "coding"},
		Features:    feature.RequestFeatures{},
		Candidates:  cands,
	})
	if err != nil {
		t.Fatalf("Decide must not return an error, got %v", err)
	}
	if res.Action != decision.ActionAbstain || !res.Abstained {
		t.Fatalf("result = %+v, want ABSTAIN fail-open", res)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonExternalInvalidResponse {
			found = true
		}
	}
	if !found {
		t.Fatalf("reason codes %v must include EXTERNAL_INVALID_RESPONSE", res.ReasonCodes)
	}
	if _, err := BuildJevRequest(decision.DecisionRequest{Candidates: cands}, BuildOpaqueMapping(cands)); err == nil {
		t.Fatal("BuildJevRequest must reject more than MaxCandidates candidates")
	}
}

// TestAgentFRequestSizeBound documents the bound that makes the request-too-
// large branches unreachable: with the maximum 100 candidates, each carrying
// the longest possible description buckets, the marshalled request stays
// below MaxRequestBodyBytes. BuildJevRequest.Validate caps candidates at
// MaxCandidates, so no larger request can be constructed through it.
func TestAgentFRequestSizeBound(t *testing.T) {
	// Longest possible candidate description: max-width context window,
	// unknown buckets, all four capabilities.
	cands := make([]decision.Candidate, 0, remote.MaxCandidates)
	for i := 0; i < remote.MaxCandidates; i++ {
		cands = append(cands, decision.Candidate{
			ID:               "candidate-" + itoaTest(i),
			ContextWindow:    math.MaxInt64,
			EWMALatencyMS:    math.NaN(),
			EstimatedCostUSD: math.NaN(),
			PriceKnown:       false,
			Successes:        0,
			Failures:         0,
			EWMAFailureRate:  math.NaN(),
			Capabilities: decision.CandidateCapabilities{
				Tools: true, Vision: true, Reasoning: true, Streaming: true,
			},
			OriginalRank: i,
		})
	}
	for _, c := range cands {
		if d := BuildCandidateDescription(c, 0); len(d) > remote.MaxCandidateDescriptionLength {
			t.Fatalf("candidate description len %d exceeds bound %d", len(d), remote.MaxCandidateDescriptionLength)
		}
	}
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: taskprofile.TaskType(strings.Repeat("y", 5000))},
		Candidates:  cands,
	}
	jr, err := BuildJevRequest(req, BuildOpaqueMapping(cands))
	if err != nil {
		t.Fatalf("BuildJevRequest: %v", err)
	}
	b, err := json.Marshal(jr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(b) > remote.MaxRequestBodyBytes {
		t.Fatalf("max request is %d bytes, exceeds MaxRequestBodyBytes %d", len(b), remote.MaxRequestBodyBytes)
	}
	t.Logf("max 100-candidate request: %d bytes (bound %d)", len(b), remote.MaxRequestBodyBytes)
}

// TestAgentFParseRejectsOverflowingConfidence pins the property that no
// NaN/Inf confidence can smuggle through response parsing: encoding/json
// rejects overflowing numbers at unmarshal time, so the NaN/Inf guard in
// ParseAndValidate is defense-in-depth that JSON input can never trigger.
func TestAgentFParseRejectsOverflowingConfidence(t *testing.T) {
	allowed := map[string]struct{}{"c0": {}, "c1": {}}
	for _, body := range []string{
		`{"code":0,"data":{"decision":"c0","confidence":1e999}}`,
		`{"code":0,"data":{"decision":"c0","confidence":NaN}}`,
		`{"code":0,"data":{"decision":"c0","confidence":Infinity}}`,
	} {
		if _, err := ParseAndValidate([]byte(body), allowed); err == nil {
			t.Fatalf("body %s must be rejected", body)
		}
	}
	// A tiny underflowing value decodes to 0 and is accepted as a valid
	// finite confidence, never NaN.
	got, err := ParseAndValidate([]byte(`{"code":0,"data":{"decision":"c0","confidence":1e-999}}`), allowed)
	if err != nil {
		t.Fatalf("underflowing confidence must parse: %v", err)
	}
	if got.Confidence != 0 {
		t.Fatalf("confidence = %v, want 0", got.Confidence)
	}
}

func itoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
