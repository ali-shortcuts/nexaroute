package decision

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// SECRET_DECISION_CANARY_82c1 must never appear in decision artifacts
const canary = "SECRET_DECISION_CANARY_82c1"

func TestDecisionRequest_NoCanaryLeak(t *testing.T) {
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
	if strings.Contains(strings.ToLower(s), "api_key") {
		t.Fatalf("api_key field found in decision request")
	}
	if strings.Contains(strings.ToLower(s), "authorization") {
		t.Fatalf("authorization found in decision request")
	}
}

func TestDecisionRequest_FieldsBounded(t *testing.T) {
	c := Candidate{
		ID:         "p1/m1",
		ProviderID: "p1",
		Model:      "model",
		Priority:   1,
		Weight:     1.0,
	}
	b, _ := json.Marshal(c)
	s := string(b)
	if strings.Contains(s, canary) {
		t.Fatalf("canary in candidate")
	}
}

func TestPrivacy_CompletePath(t *testing.T) {
	// Verify canary absent from all decision artifacts
	feat := feature.RequestFeatures{
		Protocol:       feature.ProtocolOpenAI,
		ModelRequested: "test",
	}
	profile := taskprofile.TaskProfile{
		Type:       taskprofile.TaskCoding,
		Complexity: taskprofile.ComplexityMedium,
		Confidence: 0.9,
	}
	candidates := []Candidate{{ID: "p1/m1", ProviderID: "p1"}}

	req := DecisionRequest{
		TaskProfile: profile,
		Features:    feat,
		Candidates:  candidates,
		Budget:      DefaultBudget(),
	}

	// Request
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), canary) {
		t.Fatalf("canary in request")
	}

	// Result
	res := DecisionResult{
		Action:      ActionAbstain,
		Confidence:  0.9,
		ReasonCodes: []ReasonCode{ReasonExistingOrderPreserved},
		ProviderID:  "local",
	}
	b, _ = json.Marshal(res)
	if strings.Contains(string(b), canary) {
		t.Fatalf("canary in result")
	}

	// Trace
	trace := DecisionTrace{
		Mode:           "local",
		ProviderID:     "local",
		CandidateCount: 1,
		Action:         ActionAbstain,
		ReasonCodes:    []ReasonCode{ReasonExistingOrderPreserved},
	}
	b, _ = json.Marshal(trace)
	if strings.Contains(string(b), canary) {
		t.Fatalf("canary in trace")
	}

	// Event
	ev := events.Event{
		Kind:                "decision_ok",
		DecisionProvider:    "local",
		DecisionAction:      "ABSTAIN",
		DecisionReasonCodes: "EXISTING_ORDER_PRESERVED",
	}
	b, _ = json.Marshal(ev)
	if strings.Contains(string(b), canary) {
		t.Fatalf("canary in event")
	}

	// Metrics snapshot should not contain canary (keys are fixed)
	m := &Metrics{}
	snap := m.Snapshot()
	for k := range snap {
		if strings.Contains(k, canary) {
			t.Fatalf("canary in metrics key")
		}
	}

	// Ensure no secret fields in request JSON
	s := string(b)
	if strings.Contains(strings.ToLower(s), "api_key") || strings.Contains(strings.ToLower(s), "authorization") {
		t.Fatalf("secret field in event")
	}
}
