package policy

import (
	"fmt"
	"math"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Weights holds multi-objective weights for policy scoring.
// Each weight must be finite, non-negative, and at least one positive.
type Weights struct {
	RouterBaseline float64 `json:"router_baseline"`
	Reliability    float64 `json:"reliability"`
	Latency        float64 `json:"latency"`
	TTFT           float64 `json:"ttft"`
	Capacity       float64 `json:"capacity"`
	Cost           float64 `json:"cost"`
	Context        float64 `json:"context"`
}

// Policy is the deterministic policy configuration.
// ID must be unique, weights validated, task overrides canonical.
type Policy struct {
	ID            string             `json:"id"`
	Name          string             `json:"name,omitempty"`
	SelectionMode string             `json:"selection_mode"` // only select_first in Phase E
	Weights       Weights            `json:"weights"`
	TaskOverrides map[string]Weights `json:"task_overrides,omitempty"`
	MinScoreDelta float64            `json:"min_score_delta"`
}

// canonical task types (must match taskprofile.AllTaskTypes lowercased)
var canonicalTasks = map[string]struct{}{
	"simple_chat": {}, "coding": {}, "code_edit": {}, "debugging": {},
	"repository_analysis": {}, "architecture_reasoning": {}, "deep_reasoning": {},
	"tool_use": {}, "agentic_task": {}, "long_context": {}, "vision": {},
	"structured_output": {}, "data_extraction": {}, "general": {}, "unknown": {},
}

func validateWeights(w Weights, ctx string) error {
	for name, v := range map[string]float64{
		ctx + ".router_baseline": w.RouterBaseline,
		ctx + ".reliability":     w.Reliability,
		ctx + ".latency":         w.Latency,
		ctx + ".ttft":            w.TTFT,
		ctx + ".capacity":        w.Capacity,
		ctx + ".cost":            w.Cost,
		ctx + ".context":         w.Context,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1_000_000 {
			return fmt.Errorf("%s must be finite and between 0 and 1000000", name)
		}
	}
	return nil
}

func hasPositive(w Weights) bool {
	return w.RouterBaseline > 0 || w.Reliability > 0 || w.Latency > 0 || w.TTFT > 0 || w.Capacity > 0 || w.Cost > 0 || w.Context > 0
}

// FromConfig converts config.DecisionPolicyConfig to Policy with validation.
func FromConfig(c config.DecisionPolicyConfig) (Policy, error) {
	id := strings.TrimSpace(c.ID)
	if id == "" {
		return Policy{}, fmt.Errorf("policy id is required")
	}
	sel := strings.TrimSpace(strings.ToLower(c.SelectionMode))
	if sel == "" {
		sel = "select_first"
	}
	if sel != "select_first" {
		return Policy{}, fmt.Errorf("policy %q selection_mode must be select_first", id)
	}
	if math.IsNaN(c.MinScoreDelta) || math.IsInf(c.MinScoreDelta, 0) || c.MinScoreDelta < 0 || c.MinScoreDelta > 1 {
		return Policy{}, fmt.Errorf("policy %q min_score_delta must be finite and between 0 and 1", id)
	}
	w := Weights{
		RouterBaseline: c.Weights.RouterBaseline,
		Reliability:    c.Weights.Reliability,
		Latency:        c.Weights.Latency,
		TTFT:           c.Weights.TTFT,
		Capacity:       c.Weights.Capacity,
		Cost:           c.Weights.Cost,
		Context:        c.Weights.Context,
	}
	if err := validateWeights(w, fmt.Sprintf("policy %q weights", id)); err != nil {
		return Policy{}, err
	}
	if !hasPositive(w) {
		return Policy{}, fmt.Errorf("policy %q must have at least one positive weight", id)
	}
	overrides := map[string]Weights{}
	for k, vw := range c.TaskOverrides {
		nk := strings.TrimSpace(strings.ToLower(k))
		if nk == "" {
			continue
		}
		if _, ok := canonicalTasks[nk]; !ok {
			return Policy{}, fmt.Errorf("policy %q task override %q is not canonical", id, nk)
		}
		ww := Weights{
			RouterBaseline: vw.RouterBaseline,
			Reliability:    vw.Reliability,
			Latency:        vw.Latency,
			TTFT:           vw.TTFT,
			Capacity:       vw.Capacity,
			Cost:           vw.Cost,
			Context:        vw.Context,
		}
		if err := validateWeights(ww, fmt.Sprintf("policy %q task_overrides[%q]", id, nk)); err != nil {
			return Policy{}, err
		}
		if !hasPositive(ww) {
			return Policy{}, fmt.Errorf("policy %q task_overrides[%q] must have at least one positive weight", id, nk)
		}
		overrides[nk] = ww
	}
	return Policy{
		ID:            id,
		Name:          strings.TrimSpace(c.Name),
		SelectionMode: sel,
		Weights:       w,
		TaskOverrides: overrides,
		MinScoreDelta: c.MinScoreDelta,
	}, nil
}

// ResolveWeights returns effective weights for given task type and whether task-aware override was used.
func (p Policy) ResolveWeights(taskType string) (Weights, bool) {
	tt := strings.TrimSpace(strings.ToLower(taskType))
	if tt == "" {
		return p.Weights, false
	}
	if ow, ok := p.TaskOverrides[tt]; ok {
		// If override has at least one positive, use it; otherwise fallback to base
		if hasPositive(ow) {
			return ow, true
		}
		// If override is all zero, treat as explicit zero override? For safety, fallback to base
		// But we consider it as task-aware if key exists
		// If all zero, we still return base to avoid division by zero
		// However we still signal task-aware if key existed
		// Check if override is non-zero? We already checked hasPositive, so if not positive, fallback
		// But we still want to indicate task-aware? No, because we didn't use it.
	}
	return p.Weights, false
}

// TotalWeight returns sum of positive weights.
func (w Weights) TotalWeight() float64 {
	sum := w.RouterBaseline + w.Reliability + w.Latency + w.TTFT + w.Capacity + w.Cost + w.Context
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return 0
	}
	return sum
}
