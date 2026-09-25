package jev

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestTaskSummaryMetadataOnly(t *testing.T) {
	f := decision.RequestFeatures{
		Tools: true, Reasoning: true, StructuredOut: true,
		EstimatedContextTokens: 18400, EstimatedInputTokens: 18000, MaxOutputTokens: 400,
	}
	s := TaskSummary(f)
	if len(s) == 0 || len(s) > MaxTaskSummaryBytes {
		t.Fatalf("len=%d", len(s))
	}
	for _, want := range []string{"task_type=agentic_task", "complexity=high", "tools=true", "reasoning=true", "structured_output=true", "estimated_context_tokens=18400"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary=%q missing %q", s, want)
		}
	}
}

func TestDeriveTaskKindCoverage(t *testing.T) {
	cases := []struct {
		f    decision.RequestFeatures
		want decision.TaskKind
	}{
		{decision.RequestFeatures{Vision: true, Tools: true, Reasoning: true}, decision.TaskVision},
		{decision.RequestFeatures{Tools: true, Reasoning: true}, decision.TaskAgentic},
		{decision.RequestFeatures{Tools: true}, decision.TaskToolUse},
		{decision.RequestFeatures{EstimatedContextTokens: 64000}, decision.TaskLongContext},
		{decision.RequestFeatures{Reasoning: true}, decision.TaskDebugging},
		{decision.RequestFeatures{EstimatedContextTokens: 100}, decision.TaskSimpleChat},
		{decision.RequestFeatures{Streaming: true, EstimatedContextTokens: 9000}, decision.TaskGeneral},
	}
	for _, c := range cases {
		if got := decision.DeriveTaskKind(c.f); got != c.want {
			t.Fatalf("features=%+v kind=%q want %q", c.f, got, c.want)
		}
	}
}

func TestPrioritiesNeverClaimQuality(t *testing.T) {
	for _, k := range decision.AllTaskKinds {
		for _, p := range PrioritiesForKind(k) {
			if p == "quality" {
				t.Fatalf("kind %q priorities must not claim quality", k)
			}
		}
		if len(PrioritiesForKind(k)) == 0 || len(PrioritiesForKind(k)) > 4 {
			t.Fatalf("kind %q priorities unbounded: %v", k, PrioritiesForKind(k))
		}
	}
	// Spot-check the documented mapping.
	if got := PrioritiesForKind(decision.TaskLongContext); strings.Join(got, ",") != "context,reliability" {
		t.Fatalf("long_context=%v", got)
	}
}

func TestBucketsDeterministic(t *testing.T) {
	if LatencyBucket(0) != "unknown" || LatencyBucket(100) != "low" || LatencyBucket(1000) != "medium" || LatencyBucket(5000) != "high" {
		t.Fatal("latency buckets wrong")
	}
	if ReliabilityBucket(0, 0) != "unknown" || ReliabilityBucket(0.01, 10) != "higher" ||
		ReliabilityBucket(0.1, 10) != "medium" || ReliabilityBucket(0.5, 10) != "lower" {
		t.Fatal("reliability buckets wrong")
	}
	if HeadroomBucket(0, 100) != "unknown" || HeadroomBucket(1000, 0) != "unknown" ||
		HeadroomBucket(10000, 1000) != "high" || HeadroomBucket(10000, 7000) != "medium" ||
		HeadroomBucket(10000, 9500) != "low" {
		t.Fatal("headroom buckets wrong")
	}
	c := decision.Candidate{HasCost: true, InputCostPerMTok: 1, OutputCostPerMTok: 4}
	if CostBucket(c, 1000, 1000) != "low" {
		t.Fatalf("cost=%q", CostBucket(c, 1000, 1000))
	}
	if CostBucket(c, 1000, 0) != "unknown" {
		t.Fatal("missing output ceiling must yield unknown cost, never invented")
	}
	if CostBucket(decision.Candidate{}, 1000, 1000) != "unknown" {
		t.Fatal("unpriced deployment must yield unknown cost")
	}
}

func TestDescribeCandidateNeverLeaksIdentity(t *testing.T) {
	const canary = "SECRET_PHYSICAL_NAME_CANARY_77aa"
	c := decision.Candidate{
		ID: canary + "/model-x", ContextWindow: 200000,
		Tools: true, Reasoning: true, Streaming: true,
		EWMALatencyMS: 120, EWMAFailureRate: 0.01, Observations: 50,
	}
	d := DescribeCandidate(c, 5000)
	if strings.Contains(d, canary) || strings.Contains(d, "model-x") {
		t.Fatalf("description leaks identity: %q", d)
	}
	for _, want := range []string{"ctx=200000", "tools=true", "reliability=higher", "latency=low", "headroom=high"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description=%q missing %q", d, want)
		}
	}
	if len(d) > MaxDescriptionBytes {
		t.Fatalf("len=%d", len(d))
	}
}

func TestBuildMappingOpaqueAndBounded(t *testing.T) {
	cands := []decision.Candidate{{ID: "p1/model-a"}, {ID: "p2/model-b"}, {ID: "p3/model-c"}}
	m, err := BuildMapping(cands, decision.RequestFeatures{EstimatedContextTokens: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.OpaqueToPhysical) != 3 || m.OpaqueToPhysical["c1"] != "p2/model-b" {
		t.Fatalf("map=%v", m.OpaqueToPhysical)
	}
	body := string(m.Body)
	for _, physical := range []string{"p1/model-a", "p2/model-b", "p3/model-c"} {
		if strings.Contains(body, physical) {
			t.Fatalf("body leaks physical ID %q: %s", physical, body)
		}
	}
	for _, opaque := range []string{`"c0"`, `"c1"`, `"c2"`} {
		if !strings.Contains(body, opaque) {
			t.Fatalf("body missing %s: %s", opaque, body)
		}
	}
	if len(m.Body) > 32*1024 {
		t.Fatalf("len=%d", len(m.Body))
	}
}

func TestBuildMappingRequiresTwo(t *testing.T) {
	if _, err := BuildMapping([]decision.Candidate{{ID: "A"}}, decision.RequestFeatures{}); err == nil {
		t.Fatal("single candidate must abstain, not map")
	} else if _, ok := err.(abstainSignal); !ok {
		t.Fatalf("want abstain signal, got %T", err)
	}
}

func TestBuildMappingMaxMetadataSetFits32KiB(t *testing.T) {
	// 100 fully-described candidates must fit comfortably.
	cands := make([]decision.Candidate, 0, 100)
	for i := 0; i < 100; i++ {
		cands = append(cands, decision.Candidate{
			ID: "p/m", ContextWindow: 1000000, Tools: true, Vision: true,
			Reasoning: true, Streaming: true, EWMALatencyMS: 99999,
			EWMAFailureRate: 0.99, Observations: 1 << 40,
			HasCost: true, InputCostPerMTok: 100000, OutputCostPerMTok: 100000,
		})
	}
	m, err := BuildMapping(cands, decision.RequestFeatures{EstimatedContextTokens: 1 << 30, EstimatedInputTokens: 1 << 29, MaxOutputTokens: 1 << 29})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Body) > 32*1024 {
		t.Fatalf("100-candidate body=%d bytes, exceeds 32 KiB", len(m.Body))
	}
}

func TestBuildMappingOversizeFailsWithoutBody(t *testing.T) {
	cands := make([]decision.Candidate, 0, MaxCandidates)
	for i := 0; i < MaxCandidates; i++ {
		cands = append(cands, decision.Candidate{ID: "p/m", ContextWindow: 1000000, Tools: true, Vision: true, Reasoning: true, Streaming: true})
	}
	_, err := BuildMapping(cands, decision.RequestFeatures{})
	if err == nil {
		t.Fatal("512-candidate body must exceed 32 KiB and fail")
	}
	coded, ok := err.(interface{ DecisionReason() decision.ReasonCode })
	if !ok || coded.DecisionReason() != decision.ReasonExternalRequestTooLarge {
		t.Fatalf("want EXTERNAL_REQUEST_TOO_LARGE, got %v", err)
	}
}
