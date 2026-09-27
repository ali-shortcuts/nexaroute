package policy

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func TestFromConfig_Valid(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:            "balanced",
		Name:          "Balanced",
		SelectionMode: "select_first",
		Weights: config.DecisionPolicyWeights{
			RouterBaseline: 1,
			Reliability:    1,
		},
		MinScoreDelta: 0.1,
	}
	pol, err := FromConfig(cfg)
	if err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if pol.ID != "balanced" {
		t.Fatalf("expected balanced id")
	}
}

func TestFromConfig_InvalidID(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:      "",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
	}
	_, err := FromConfig(cfg)
	if err == nil {
		t.Fatalf("expected error for empty id")
	}
}

func TestFromConfig_InvalidSelectionMode(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:            "test",
		SelectionMode: "rank",
		Weights:       config.DecisionPolicyWeights{RouterBaseline: 1},
	}
	_, err := FromConfig(cfg)
	if err == nil {
		t.Fatalf("expected error for invalid selection mode")
	}
}

func TestFromConfig_InvalidMinDelta(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:            "test",
		Weights:       config.DecisionPolicyWeights{RouterBaseline: 1},
		MinScoreDelta: 2.0,
	}
	_, err := FromConfig(cfg)
	if err == nil {
		t.Fatalf("expected error for invalid min delta")
	}
}

func TestFromConfig_NoPositiveWeight(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:      "test",
		Weights: config.DecisionPolicyWeights{},
	}
	_, err := FromConfig(cfg)
	if err == nil {
		t.Fatalf("expected error for no positive weight")
	}
}

func TestFromConfig_TaskOverrideCanonical(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:      "test",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
		TaskOverrides: map[string]config.DecisionPolicyWeights{
			"coding": {Latency: 1},
		},
	}
	_, err := FromConfig(cfg)
	if err != nil {
		t.Fatalf("expected valid canonical override, got %v", err)
	}
}

func TestFromConfig_TaskOverrideNonCanonical(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:      "test",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
		TaskOverrides: map[string]config.DecisionPolicyWeights{
			"not_a_task": {Latency: 1},
		},
	}
	_, err := FromConfig(cfg)
	if err == nil {
		t.Fatalf("expected error for non-canonical task")
	}
}

func TestFromConfig_TaskOverrideAllZeroRejected(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:      "test",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
		TaskOverrides: map[string]config.DecisionPolicyWeights{
			"coding": {},
		},
	}
	_, err := FromConfig(cfg)
	if err == nil {
		t.Fatalf("expected error for all-zero task override")
	}
}

func TestFromConfig_TaskOverrideCaseInsensitive(t *testing.T) {
	cfg := config.DecisionPolicyConfig{
		ID:      "test",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
		TaskOverrides: map[string]config.DecisionPolicyWeights{
			"CODING": {Latency: 1},
		},
	}
	pol, err := FromConfig(cfg)
	if err != nil {
		t.Fatalf("expected valid case-insensitive, got %v", err)
	}
	if _, ok := pol.TaskOverrides["coding"]; !ok {
		t.Fatalf("expected lowercased key coding")
	}
}

func TestResolveWeights(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1},
		TaskOverrides: map[string]Weights{
			"coding": {Latency: 1},
		},
	}
	w, taskAware := pol.ResolveWeights("coding")
	if !taskAware {
		t.Fatalf("expected task-aware")
	}
	if w.Latency != 1 {
		t.Fatalf("expected latency weight 1")
	}
	w2, taskAware2 := pol.ResolveWeights("simple_chat")
	if taskAware2 {
		t.Fatalf("should not be task-aware for missing override")
	}
	if w2.RouterBaseline != 1 {
		t.Fatalf("should fallback to base")
	}
}

func TestCanonicalTaskVocabularyParity(t *testing.T) {
	// Ensure policy canonicalTasks matches taskprofile.AllTaskTypes()
	allTypes := taskprofile.AllTaskTypes()
	if len(allTypes) != len(canonicalTasks) {
		t.Fatalf("canonical task count mismatch: policy has %d, taskprofile has %d", len(canonicalTasks), len(allTypes))
	}
	for _, tt := range allTypes {
		lower := string(tt)
		// lowercased already
		if _, ok := canonicalTasks[lower]; !ok {
			t.Fatalf("taskprofile type %q not in policy canonicalTasks", lower)
		}
	}
	// Ensure no extra unknown names are accepted — already tested via non-canonical test
	// Also test all 15 current types are accepted
	for _, tt := range allTypes {
		cfg := config.DecisionPolicyConfig{
			ID:      "test",
			Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
			TaskOverrides: map[string]config.DecisionPolicyWeights{
				string(tt): {Latency: 1},
			},
		}
		_, err := FromConfig(cfg)
		if err != nil {
			t.Fatalf("expected type %q to be accepted, got %v", tt, err)
		}
	}
}

func TestWeightsTotal(t *testing.T) {
	w := Weights{RouterBaseline: 1, Reliability: 2}
	if w.TotalWeight() != 3 {
		t.Fatalf("expected total 3, got %v", w.TotalWeight())
	}
}
