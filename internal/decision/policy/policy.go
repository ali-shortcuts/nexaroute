package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
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
	// Quality weights the per-task scorecard evidence component
	// (#510 item 5), carried on candidates as TaskQuality with its
	// sample count. Zero disables the component.
	Quality float64 `json:"quality,omitempty"`
}

// Policy is the deterministic policy configuration.
// ID must be unique, weights validated, task overrides canonical.
type Policy struct {
	ID            string             `json:"id"`
	Name          string             `json:"name,omitempty"`
	Version       string             `json:"version"`        // explicit operator label, or derived content fingerprint
	SelectionMode string             `json:"selection_mode"` // only select_first in Phase E
	Weights       Weights            `json:"weights"`
	TaskOverrides map[string]Weights `json:"task_overrides,omitempty"`
	MinScoreDelta float64            `json:"min_score_delta"`
	// MinObservations gates reliability evidence: below this many success+failure
	// samples the reliability component decays toward the neutral baseline so a
	// single sample cannot flap the route. Zero disables the gate.
	MinObservations int `json:"min_observations,omitempty"`
}

// VersionPrefix marks a version string that was derived from policy content
// rather than declared by the operator, so traces can tell them apart.
const VersionPrefix = "auto:"

// deriveVersion returns a stable, deterministic fingerprint of the effective
// policy definition. Identical policy content always yields the identical
// version; any weight, override or threshold change yields a new one.
func deriveVersion(p Policy) string {
	canonical := struct {
		ID              string             `json:"id"`
		SelectionMode   string             `json:"selection_mode"`
		Weights         Weights            `json:"weights"`
		TaskOverrides   map[string]Weights `json:"task_overrides,omitempty"`
		MinScoreDelta   float64            `json:"min_score_delta"`
		MinObservations int                `json:"min_observations,omitempty"`
	}{
		ID:              p.ID,
		SelectionMode:   p.SelectionMode,
		Weights:         p.Weights,
		TaskOverrides:   p.TaskOverrides,
		MinScoreDelta:   p.MinScoreDelta,
		MinObservations: p.MinObservations,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return VersionPrefix + "unavailable"
	}
	sum := sha256.Sum256(raw)
	return VersionPrefix + hex.EncodeToString(sum[:8])
}

// canonical task types (must match taskprofile.AllTaskTypes lowercased)
var canonicalTasks = map[string]struct{}{
	"simple_chat": {}, "coding": {}, "code_edit": {}, "debugging": {},
	"repository_analysis": {}, "architecture_reasoning": {}, "deep_reasoning": {},
	"tool_use": {}, "agentic_task": {}, "long_context": {}, "vision": {},
	"structured_output": {}, "data_extraction": {}, "general": {}, "background": {}, "unknown": {},
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
		ctx + ".quality":         w.Quality,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1_000_000 {
			return fmt.Errorf("%s must be finite and between 0 and 1000000", name)
		}
	}
	return nil
}

func hasPositive(w Weights) bool {
	return w.RouterBaseline > 0 || w.Reliability > 0 || w.Latency > 0 || w.TTFT > 0 || w.Capacity > 0 || w.Cost > 0 || w.Context > 0 || w.Quality > 0
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
		Quality:        c.Weights.Quality,
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
			Quality:        vw.Quality,
		}
		if err := validateWeights(ww, fmt.Sprintf("policy %q task_overrides[%q]", id, nk)); err != nil {
			return Policy{}, err
		}
		if !hasPositive(ww) {
			return Policy{}, fmt.Errorf("policy %q task_overrides[%q] must have at least one positive weight", id, nk)
		}
		overrides[nk] = ww
	}
	if c.MinObservations < 0 {
		return Policy{}, fmt.Errorf("policy %q min_observations must not be negative", id)
	}
	p := Policy{
		ID:              id,
		Name:            strings.TrimSpace(c.Name),
		Version:         strings.TrimSpace(c.Version),
		SelectionMode:   sel,
		Weights:         w,
		TaskOverrides:   overrides,
		MinScoreDelta:   c.MinScoreDelta,
		MinObservations: c.MinObservations,
	}
	if p.Version == "" {
		p.Version = deriveVersion(p)
	}
	return p, nil
}

// EvidenceFor returns the [0,1] confidence in a deployment's reliability
// evidence given its success+failure sample count. A zero gate means full
// confidence (historical behavior). Below the gate the value scales linearly,
// so one sample out of N contributes proportionally less.
func (p Policy) EvidenceFor(c decision.Candidate) float64 {
	if p.MinObservations <= 0 {
		return 1
	}
	n := c.Successes + c.Failures
	if n <= 0 {
		return 0
	}
	if n >= int64(p.MinObservations) {
		return 1
	}
	return float64(n) / float64(p.MinObservations)
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
	sum := w.RouterBaseline + w.Reliability + w.Latency + w.TTFT + w.Capacity + w.Cost + w.Context + w.Quality
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return 0
	}
	return sum
}
