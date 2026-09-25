// Package jev implements one optional external DecisionProvider for the Jev
// model-routing API (https://www.jevai.org/api/v1/decisions/model-route).
//
// The adapter is metadata-only and opaque by construction: it sends a
// synthetic task summary plus factual candidate metadata addressed to
// ephemeral per-request IDs (c0, c1, ...), and maps the returned choice back
// to the physical deployment locally. Jev-specific JSON stays inside this
// package; the generic decision.DecisionRequest never carries Jev fields.
package jev

import (
	"sort"
	"strconv"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

const (
	// MaxTaskSummaryBytes tightly bounds the synthetic task string (§22).
	MaxTaskSummaryBytes = 1024
	// MaxCandidates caps the per-request choice set as defense-in-depth; the
	// 32 KiB body measurement is the governing limit and trips first.
	MaxCandidates = 512
	// MaxDescriptionBytes bounds one candidate description.
	MaxDescriptionBytes = 512
)

// Opaque IDs are per-request, never persisted, and carry no physical meaning.
func opaqueID(i int) string { return "c" + strconv.Itoa(i) }

// PrioritiesForKind is the single bounded, documented mapping from task kind
// to Jev priority hints. "quality" is deliberately absent: Phase F has no
// empirical quality scorecard, so no quality semantics are claimed.
func PrioritiesForKind(k decision.TaskKind) []string {
	switch k {
	case decision.TaskSimpleChat:
		return []string{"latency", "cost", "reliability"}
	case decision.TaskCoding, decision.TaskDebugging:
		return []string{"reliability", "context", "latency"}
	case decision.TaskLongContext:
		return []string{"context", "reliability"}
	case decision.TaskToolUse:
		return []string{"reliability", "latency"}
	case decision.TaskAgentic:
		return []string{"reliability", "latency", "context"}
	case decision.TaskVision:
		return []string{"reliability", "latency"}
	default:
		return []string{"reliability", "latency", "cost"}
	}
}

// ComplexityForFeatures derives a coarse, honest complexity bucket from
// metadata alone (never from task content).
func ComplexityForFeatures(f decision.RequestFeatures) string {
	tokens := f.EstimatedContextTokens
	if tokens < 0 {
		tokens = 0
	}
	if tokens >= decision.LongContextThresholdTokens || (f.Tools && f.Reasoning) {
		return "high"
	}
	if tokens >= decision.SimpleChatThresholdTokens || f.Tools || f.Reasoning || f.Vision {
		return "medium"
	}
	return "low"
}

// TaskSummary builds the synthetic metadata-only task string. It contains no
// raw user text — only the bounded task kind, complexity bucket, capability
// needs, and token estimate.
func TaskSummary(f decision.RequestFeatures) string {
	kind := decision.DeriveTaskKind(f)
	tokens := f.EstimatedContextTokens
	if tokens < 0 {
		tokens = 0
	}
	var b strings.Builder
	b.Grow(192)
	b.WriteString("task_type=")
	b.WriteString(string(kind))
	b.WriteString(";complexity=")
	b.WriteString(ComplexityForFeatures(f))
	b.WriteString(";tools=")
	b.WriteString(strconv.FormatBool(f.Tools))
	b.WriteString(";vision=")
	b.WriteString(strconv.FormatBool(f.Vision))
	b.WriteString(";reasoning=")
	b.WriteString(strconv.FormatBool(f.Reasoning))
	b.WriteString(";structured_output=")
	b.WriteString(strconv.FormatBool(f.StructuredOut))
	b.WriteString(";estimated_context_tokens=")
	b.WriteString(strconv.Itoa(tokens))
	return b.String()
}

// Buckets are deterministic and factual. Thresholds are documented here so
// reviewers can audit them; none claim model quality.

// LatencyBucket maps measured response-header latency to low/medium/high.
func LatencyBucket(ewmaMS float64) string {
	if ewmaMS <= 0 {
		return "unknown"
	}
	switch {
	case ewmaMS < 500:
		return "low"
	case ewmaMS < 2000:
		return "medium"
	default:
		return "high"
	}
}

// ReliabilityBucket maps the recency-weighted failure rate to
// lower/medium/higher. Deployments with no observations are "unknown" —
// never assumed good or bad.
func ReliabilityBucket(failureRate float64, observations int64) string {
	if observations <= 0 {
		return "unknown"
	}
	if failureRate < 0 {
		failureRate = 0
	}
	switch {
	case failureRate < 0.05:
		return "higher"
	case failureRate < 0.25:
		return "medium"
	default:
		return "lower"
	}
}

// HeadroomBucket maps advertised context window against the estimated request
// size. Unknown windows or unknown estimates yield "unknown" (never filtered,
// never assumed).
func HeadroomBucket(contextWindow, estimatedTokens int) string {
	if contextWindow <= 0 || estimatedTokens <= 0 {
		return "unknown"
	}
	ratio := float64(estimatedTokens) / float64(contextWindow)
	switch {
	case ratio < 0.5:
		return "high"
	case ratio < 0.85:
		return "medium"
	default:
		return "low"
	}
}

// estimatedRequestCostUSD prices a request deterministically from configured
// per-model pricing plus the request's split token estimates. ok=false when
// pricing is unconfigured or the output ceiling is unknown: unknown pricing
// is never treated as free and no output estimate is ever invented.
func estimatedRequestCostUSD(c decision.Candidate, inputTokens, outputTokens int) (cost float64, ok bool) {
	if !c.HasCost || inputTokens <= 0 || outputTokens <= 0 {
		return 0, false
	}
	if c.InputCostPerMTok <= 0 && c.OutputCostPerMTok <= 0 {
		return 0, false
	}
	cost = (float64(inputTokens)*c.InputCostPerMTok + float64(outputTokens)*c.OutputCostPerMTok) / 1_000_000
	if cost < 0 {
		return 0, false
	}
	return cost, true
}

// CostBucket maps the estimated request cost to low/medium/high with absolute
// USD thresholds. Unknown pricing or unknown output ceiling yields "unknown".
func CostBucket(c decision.Candidate, inputTokens, outputTokens int) string {
	cost, ok := estimatedRequestCostUSD(c, inputTokens, outputTokens)
	if !ok {
		return "unknown"
	}
	switch {
	case cost < 0.01:
		return "low"
	case cost < 0.25:
		return "medium"
	default:
		return "high"
	}
}

// DescribeCandidate renders factual metadata only: context window,
// capability flags, and operational buckets. It never includes provider IDs,
// model names, deployment IDs, credentials, or quality prose.
func DescribeCandidate(c decision.Candidate, estimatedTokens int) string {
	var b strings.Builder
	b.Grow(200)
	b.WriteString("ctx=")
	if c.ContextWindow > 0 {
		b.WriteString(strconv.Itoa(c.ContextWindow))
	} else {
		b.WriteString("unknown")
	}
	b.WriteString(" tools=")
	b.WriteString(strconv.FormatBool(c.Tools))
	b.WriteString(" vision=")
	b.WriteString(strconv.FormatBool(c.Vision))
	b.WriteString(" reasoning=")
	b.WriteString(strconv.FormatBool(c.Reasoning))
	b.WriteString(" streaming=")
	b.WriteString(strconv.FormatBool(c.Streaming))
	b.WriteString(" reliability=")
	b.WriteString(ReliabilityBucket(c.EWMAFailureRate, c.Observations))
	b.WriteString(" latency=")
	b.WriteString(LatencyBucket(c.EWMALatencyMS))
	b.WriteString(" headroom=")
	b.WriteString(HeadroomBucket(c.ContextWindow, estimatedTokens))
	return b.String()
}

// ConstraintsForRequest returns the fixed, safe constraint hints. They are
// explanatory only: the NexaRoute validator remains the security authority
// and no text constraint is trusted for safety.
func ConstraintsForRequest() []string {
	return []string{"Candidates are pre-filtered eligible deployments; select exactly one of the supplied candidate ids."}
}

// Mapping is the per-request opaque translation: the serialized Jev body plus
// the request-local opaqueID->physicalID table. The table never leaves the
// process and is never persisted, logged, or emitted to events/metrics.
type Mapping struct {
	OpaqueToPhysical map[string]string
	Body             []byte
	TaskKind         decision.TaskKind
}

// BuildMapping translates allowed primary candidates plus metadata-only
// features into a Jev model-route body. Candidates keep their input order so
// opaque IDs are deterministic (c0 for the first allowed candidate, ...).
// Fewer than two candidates yields an abstain signal (not an error): there is
// nothing to choose between. Bound violations yield a typed remote error;
// callers must fail open without sending.
func BuildMapping(cands []decision.Candidate, f decision.RequestFeatures) (Mapping, error) {
	if len(cands) < 2 {
		return Mapping{}, errAbstainNotMappable()
	}
	if len(cands) > MaxCandidates {
		return Mapping{}, errRequestTooLarge()
	}
	task := TaskSummary(f)
	if len(task) == 0 || len(task) > MaxTaskSummaryBytes {
		return Mapping{}, errRequestTooLarge()
	}
	tokens := f.EstimatedContextTokens
	if tokens < 0 {
		tokens = 0
	}
	wire := requestWire{
		Task:        task,
		Candidates:  make([]candidateWire, 0, len(cands)),
		Priorities:  PrioritiesForKind(decision.DeriveTaskKind(f)),
		Constraints: ConstraintsForRequest(),
	}
	opaque := make(map[string]string, len(cands))
	for i, c := range cands {
		if c.ID == "" {
			return Mapping{}, errRequestTooLarge()
		}
		id := opaqueID(i)
		opaque[id] = c.ID
		desc := DescribeCandidate(c, tokens)
		if len(desc) > MaxDescriptionBytes {
			desc = desc[:MaxDescriptionBytes]
		}
		cw := candidateWire{ID: id, Description: desc}
		if b := CostBucket(c, f.EstimatedInputTokens, f.MaxOutputTokens); b != "unknown" {
			cw.Cost = b
		}
		if b := LatencyBucket(c.EWMALatencyMS); b != "unknown" {
			cw.Latency = b
		}
		wire.Candidates = append(wire.Candidates, cw)
	}
	body, err := marshalRequest(wire)
	if err != nil {
		return Mapping{}, err
	}
	return Mapping{OpaqueToPhysical: opaque, Body: body, TaskKind: decision.DeriveTaskKind(f)}, nil
}

// SortedOpaqueIDs returns opaque IDs in deterministic c0..cn order. Test and
// debug helper only: production paths never log these alongside the physical
// map.
func SortedOpaqueIDs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PhysicalFor maps an opaque choice back to its physical deployment ID.
func PhysicalFor(opaque map[string]string, choice string) (string, bool) {
	id, ok := opaque[choice]
	return id, ok
}
