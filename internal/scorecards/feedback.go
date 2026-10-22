package scorecards

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Feedback loop (Phase 19, #510 item 5): production outcomes are
// aggregated per (task_type, deployment_id) into scorecards with
// exponential decay, so stale glory fades on its own and thin data
// stays visibly thin (decayed effective sample counts drive
// ConfidenceFromSamples — nothing is rounded up to certainty).
//
// The tracker is the producer seam: callers report outcomes via
// Observe; Card/Cards build provenance-carrying scorecards
// (production_telemetry) that can be upserted into the Registry
// alongside evaluation-derived cards.

const DefaultFeedbackHalfLife = 24 * time.Hour

// FeedbackOutcome is one observed request outcome for a
// (task, deployment) pair.
type FeedbackOutcome struct {
	Success   bool
	LatencyMS float64
	CostUSD   float64
	HasCost   bool
}

type feedbackAgg struct {
	taskType     string
	deploymentID string
	succ, fail   float64
	latencySum   float64
	latencyN     float64
	costSum      float64
	costN        float64
	last         time.Time
}

// FeedbackTracker aggregates outcomes with exponential decay.
// The zero value is not usable; construct with NewFeedbackTracker.
type FeedbackTracker struct {
	mu              sync.Mutex
	halfLife        time.Duration
	latencyTargetMS float64
	agg             map[string]*feedbackAgg
}

// NewFeedbackTracker builds a tracker. A non-positive half-life
// selects DefaultFeedbackHalfLife. latencyTargetMS follows the
// codebase rule that latency is only scored against an explicit
// target: with no target the latency dimension is omitted from
// built cards rather than scored against an invented budget.
func NewFeedbackTracker(halfLife time.Duration, latencyTargetMS float64) *FeedbackTracker {
	if halfLife <= 0 {
		halfLife = DefaultFeedbackHalfLife
	}
	return &FeedbackTracker{
		halfLife:        halfLife,
		latencyTargetMS: latencyTargetMS,
		agg:             map[string]*feedbackAgg{},
	}
}

func (t *FeedbackTracker) decayLocked(a *feedbackAgg, at time.Time) {
	if a.last.IsZero() {
		a.last = at
		return
	}
	if !at.After(a.last) {
		return
	}
	f := math.Pow(0.5, at.Sub(a.last).Seconds()/t.halfLife.Seconds())
	a.succ *= f
	a.fail *= f
	a.latencySum *= f
	a.latencyN *= f
	a.costSum *= f
	a.costN *= f
	a.last = at
}

// Observe records one outcome. Invalid identifiers are ignored:
// feedback must never be able to break the request path.
func (t *FeedbackTracker) Observe(taskType, deploymentID string, o FeedbackOutcome, at time.Time) {
	if !validIdentifier(taskType, 32) || !validIdentifier(deploymentID, MaxStringBytes) {
		return
	}
	if o.LatencyMS < 0 || math.IsNaN(o.LatencyMS) || math.IsInf(o.LatencyMS, 0) {
		o.LatencyMS = 0
	}
	if !o.HasCost || o.CostUSD < 0 || math.IsNaN(o.CostUSD) || math.IsInf(o.CostUSD, 0) {
		o.HasCost = false
		o.CostUSD = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := registryKey(taskType, deploymentID)
	a, ok := t.agg[key]
	if !ok {
		a = &feedbackAgg{taskType: taskType, deploymentID: deploymentID}
		t.agg[key] = a
	}
	t.decayLocked(a, at)
	if o.Success {
		a.succ++
	} else {
		a.fail++
	}
	if o.LatencyMS > 0 {
		a.latencySum += o.LatencyMS
		a.latencyN++
	}
	if o.HasCost {
		a.costSum += o.CostUSD
		a.costN++
	}
}

func (t *FeedbackTracker) cardLocked(a *feedbackAgg, at time.Time) (Scorecard, bool) {
	t.decayLocked(a, at)
	n := a.succ + a.fail
	if n <= 0 {
		return Scorecard{}, false
	}
	effective := int(n + 0.5)
	rate := a.succ / n
	values := map[Dimension]Value{
		DimAvailability: {
			Score: rate, Raw: rate, Unit: DimAvailability.Unit(),
			SampleCount: effective, Provenance: ProvenanceProductionTelemetry,
			Confidence:  ConfidenceFromSamples(effective),
			EvaluatedAt: at, Source: "task-feedback",
		},
	}
	if t.latencyTargetMS > 0 && a.latencyN > 0 {
		mean := a.latencySum / a.latencyN
		score := 1 - mean/t.latencyTargetMS
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		values[DimLatencyMS] = Value{
			Score: score, Raw: mean, Unit: DimLatencyMS.Unit(),
			SampleCount: int(a.latencyN + 0.5), Provenance: ProvenanceProductionTelemetry,
			Confidence:  ConfidenceFromSamples(int(a.latencyN)),
			EvaluatedAt: at, Source: "task-feedback",
		}
	}
	var notes []string
	if a.costN > 0 {
		notes = append(notes, fmt.Sprintf("mean_cost_usd=%.6f (decayed over %.1f effective cost samples)", a.costSum/a.costN, a.costN))
	}
	return Scorecard{
		DeploymentID: a.deploymentID,
		TaskType:     a.taskType,
		Version:      1,
		GeneratedAt:  at,
		Values:       values,
		Notes:        notes,
	}, true
}

// Card builds the current scorecard for one (task, deployment)
// pair, applying decay as of `at`. ok is false when no effective
// evidence remains.
func (t *FeedbackTracker) Card(taskType, deploymentID string, at time.Time) (Scorecard, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.agg[registryKey(taskType, deploymentID)]
	if !ok {
		return Scorecard{}, false
	}
	return t.cardLocked(a, at)
}
