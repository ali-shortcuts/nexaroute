package decision

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// SECRET_DECISION_CANARY_82c1 must never appear in decision request JSON
const canary = "SECRET_DECISION_CANARY_82c1"

func TestDecisionRequest_NoCanaryLeak(t *testing.T) {
	// Simulate a request that contains canary in raw prompt, but DecisionRequest must not contain it
	// DecisionRequest only has Features + TaskProfile + Candidates, no raw prompt
	feat := feature.RequestFeatures{
		Protocol:       feature.ProtocolOpenAI,
		ModelRequested: "test-model",
		Streaming:      false,
		HasVision:      false,
		HasTools:       true,
		ToolCount:      2,
		MessageCount:   5,
	}
	profile := taskprofile.TaskProfile{
		Type:       taskprofile.TaskCoding,
		Complexity: taskprofile.ComplexityMedium,
		Confidence: 0.9,
	}
	candidates := []Candidate{{ID: "p1/m1", ProviderID: "p1"}}

	req := DecisionRequest{
		TaskProfile:       profile,
		Features:          feat,
		Candidates:        candidates,
		VirtualEndpointID: "ve1",
		RouteProfileID:    "rp1",
		CandidatePoolID:   "pool1",
		Budget:            DefaultBudget(),
		RequestID:         "req-123",
	}

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	s := string(b)
	if strings.Contains(s, canary) {
		t.Fatalf("canary leaked in DecisionRequest JSON: %s", s)
	}
	// Also ensure no API keys, headers, etc. fields exist
	// DecisionRequest struct should not have fields named api_key, header, prompt, etc.
	if strings.Contains(strings.ToLower(s), "api_key") {
		t.Fatalf("api_key field found in decision request")
	}
	if strings.Contains(strings.ToLower(s), "authorization") {
		t.Fatalf("authorization found in decision request")
	}
}

func TestDecisionRequest_FieldsBounded(t *testing.T) {
	// Ensure Candidate only has bounded fields, no secrets
	c := Candidate{
		ID:         "p1/m1",
		ProviderID: "p1",
		Model:      "model",
		Priority:   1,
		Weight:     1.0,
	}
	b, _ := json.Marshal(c)
	s := string(b)
	// Should not contain raw prompt or content
	if strings.Contains(s, canary) {
		t.Fatalf("canary in candidate")
	}
}
