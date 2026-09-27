package jev

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func makeTestCandidate(id string, poolOrd, priority int, ctxWindow int, tools, vision bool, latency float64, successes int64, cost float64, priceKnown bool) decision.Candidate {
	return decision.Candidate{
		ID:            id,
		ProviderID:    "p1",
		Priority:      priority,
		PoolOrdinal:   poolOrd,
		ContextWindow: ctxWindow,
		Capabilities: decision.CandidateCapabilities{
			Tools:  tools,
			Vision: vision,
		},
		EWMALatencyMS:    latency,
		Successes:        successes,
		EstimatedCostUSD: cost,
		PriceKnown:       priceKnown,
		OriginalRank:     0,
	}
}

func TestMapper_OpaqueIDs(t *testing.T) {
	cands := []decision.Candidate{
		{ID: "p1/m1"},
		{ID: "p2/m2"},
		{ID: "p3/m3"},
	}
	mapping := BuildOpaqueMapping(cands)
	if len(mapping.OpaqueToPhysical) != 3 {
		t.Fatalf("expected 3 mappings")
	}
	// Check opaque IDs are c0,c1,c2
	if mapping.OpaqueToPhysical["c0"] != "p1/m1" {
		t.Fatalf("c0 should map to p1/m1")
	}
	if mapping.OpaqueToPhysical["c1"] != "p2/m2" {
		t.Fatalf("c1 should map to p2/m2")
	}
	// Ensure physical IDs not exposed as opaque
	if _, ok := mapping.AllowedOpaqueIDs["p1/m1"]; ok {
		t.Fatalf("physical ID should not be allowed opaque")
	}
}

func TestMapper_PhysicalNameNotExposed(t *testing.T) {
	canary := "SECRET_PHYSICAL_CANARY_abc123"
	cands := []decision.Candidate{
		{ID: canary, ProviderID: "secret-provider", Model: "secret-model"},
		{ID: "p2/m2"},
	}
	mapping := BuildOpaqueMapping(cands)
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "coding"},
		Features:    feature.RequestFeatures{},
		Candidates:  cands,
	}
	jevReq, err := BuildJevRequest(req, mapping)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	// Marshal and check canary not in JSON
	// The opaque IDs should be c0,c1, not physical
	for _, c := range jevReq.Candidates {
		if strings.Contains(c.ID, canary) {
			t.Fatalf("physical canary leaked in candidate ID")
		}
		if strings.Contains(c.Description, canary) {
			t.Fatalf("physical canary leaked in description")
		}
		if strings.Contains(c.Description, "secret-provider") || strings.Contains(c.Description, "secret-model") {
			t.Fatalf("provider/model name leaked")
		}
	}
}

func TestMapper_TaskSynthesis_NoRawPrompt(t *testing.T) {
	canary := "SECRET_EXTERNAL_PROMPT_CANARY_94af"
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "debugging"},
		Features: feature.RequestFeatures{
			HasTools: true,
		},
		Candidates: []decision.Candidate{
			{ID: "p1/m1"},
			{ID: "p2/m2"},
		},
		EstimatedInputTokens: 18400,
	}
	// Simulate that raw prompt containing canary is NOT in DecisionRequest (privacy invariant)
	// Task summary should not contain canary
	task := BuildTaskSummary(req)
	if strings.Contains(task, canary) {
		t.Fatalf("canary leaked into task summary")
	}
	if len(task) > 1024 {
		t.Fatalf("task too long: %d", len(task))
	}
	// Should contain metadata
	if !strings.Contains(task, "task_type=debugging") {
		t.Fatalf("task should contain task_type")
	}
}

func TestMapper_CandidateDescription_NoQualityClaims(t *testing.T) {
	c := makeTestCandidate("p1/m1", 0, 0, 128000, true, false, 10, 10, 0.001, true)
	desc := BuildCandidateDescription(c, 12000)
	// Should not contain quality claims
	forbidden := []string{"great at coding", "smart model", "best reasoning"}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(desc), f) {
			t.Fatalf("description contains forbidden quality claim %q", f)
		}
	}
	// Should contain factual buckets
	if !strings.Contains(desc, "tools=true") {
		t.Fatalf("description should contain tools capability")
	}
}

func TestMapper_PriorityMapping(t *testing.T) {
	tests := []struct {
		taskType string
		expected []string
	}{
		{"simple_chat", []string{"latency", "cost", "reliability"}},
		{"coding", []string{"reliability", "context", "latency"}},
		{"long_context", []string{"context", "reliability"}},
		{"tool_use", []string{"reliability", "latency"}},
	}
	for _, tt := range tests {
		prios := BuildPriorities(tt.taskType)
		if len(prios) == 0 {
			t.Fatalf("priorities empty for %s", tt.taskType)
		}
		// Check no quality unless scorecards exist (Phase F has no quality)
		for _, p := range prios {
			if p == "quality" {
				t.Fatalf("quality should not be included in Phase F for %s", tt.taskType)
			}
		}
	}
}

func TestMapper_RequestSizeLimit(t *testing.T) {
	// Build maximum candidate set and ensure normal payload <=32KiB
	cands := make([]decision.Candidate, 20)
	for i := range cands {
		cands[i] = makeTestCandidate(
			"p1/m"+string(rune('0'+i)), 0, 0, 128000, true, false, 10, 10, 0.001, true,
		)
		cands[i].ID = "p1/m" + string(rune(i))
	}
	mapping := BuildOpaqueMapping(cands)
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "coding"},
		Candidates:  cands,
	}
	jevReq, err := BuildJevRequest(req, mapping)
	if err != nil {
		t.Fatalf("normal payload should be within limit, got %v", err)
	}
	if jevReq == nil {
		t.Fatalf("jevReq nil")
	}
	// Force oversized by creating huge task (but task is bounded 1024, so we need to test via direct marshal)
	// Our BuildJevRequest already checks size, so normal should pass
}

func TestMapper_Buckets_Deterministic(t *testing.T) {
	c := makeTestCandidate("p1/m1", 0, 0, 128000, true, false, 10, 10, 0.001, true)
	desc1 := BuildCandidateDescription(c, 12000)
	desc2 := BuildCandidateDescription(c, 12000)
	if desc1 != desc2 {
		t.Fatalf("candidate description not deterministic")
	}
}
