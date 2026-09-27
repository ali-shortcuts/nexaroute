package jev

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

// Opaque mapping: c0 -> physical ID
type OpaqueMapping struct {
	OpaqueToPhysical map[string]string // c0 -> p1/m1
	PhysicalToOpaque map[string]string // p1/m1 -> c0
	AllowedOpaqueIDs map[string]struct{}
}

// BuildOpaqueMapping creates ephemeral mapping for request-local use, never persisted
func BuildOpaqueMapping(candidates []decision.Candidate) OpaqueMapping {
	opaqueToPhysical := make(map[string]string, len(candidates))
	physicalToOpaque := make(map[string]string, len(candidates))
	allowed := make(map[string]struct{}, len(candidates))
	for i, c := range candidates {
		opaque := fmt.Sprintf("c%d", i)
		opaqueToPhysical[opaque] = c.ID
		physicalToOpaque[c.ID] = opaque
		allowed[opaque] = struct{}{}
	}
	return OpaqueMapping{
		OpaqueToPhysical: opaqueToPhysical,
		PhysicalToOpaque: physicalToOpaque,
		AllowedOpaqueIDs: allowed,
	}
}

// Buckets for operational metadata (deterministic, bounded, no fake quality)
func latencyBucket(ms float64, successes int64) string {
	if successes == 0 || ms <= 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return "unknown"
	}
	if ms < 50 {
		return "low"
	}
	if ms < 200 {
		return "medium"
	}
	return "high"
}

func costBucket(cost float64, known bool) string {
	if !known || math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return "unknown"
	}
	if cost < 0.001 {
		return "low"
	}
	if cost < 0.01 {
		return "medium"
	}
	return "high"
}

func reliabilityBucket(successes, failures int64, failureRate float64) string {
	total := successes + failures
	if total == 0 {
		return "unknown"
	}
	if math.IsNaN(failureRate) || math.IsInf(failureRate, 0) {
		return "unknown"
	}
	// lower failure rate = higher reliability
	if failureRate < 0.1 {
		return "higher"
	}
	if failureRate < 0.3 {
		return "medium"
	}
	return "lower"
}

func contextHeadroomBucket(window, required int) string {
	if window <= 0 || required <= 0 {
		return "unknown"
	}
	if window < required {
		return "low"
	}
	headroom := float64(window-required) / float64(window)
	if headroom > 0.7 {
		return "high"
	}
	if headroom > 0.3 {
		return "medium"
	}
	return "low"
}

// BuildTaskSummary constructs synthetic task summary from TaskProfile + Features, bounded 1024, no raw user content
func BuildTaskSummary(req decision.DecisionRequest) string {
	var sb strings.Builder
	// Use only derived metadata
	sb.WriteString(fmt.Sprintf("task_type=%s;", strings.ToLower(string(req.TaskProfile.Type))))
	// Complexity from task profile (string type)
	complexity := strings.ToLower(string(req.TaskProfile.Complexity))
	if complexity == "" {
		complexity = "unknown"
	}
	// If unknown, derive from features if available
	if complexity == "unknown" {
		if req.Features.EstimatedTotalTokens > 10000 {
			complexity = "high"
		} else if req.Features.EstimatedTotalTokens > 2000 {
			complexity = "medium"
		} else {
			complexity = "low"
		}
	}
	sb.WriteString(fmt.Sprintf("complexity=%s;", complexity))
	sb.WriteString(fmt.Sprintf("tools=%t;", req.Features.HasTools))
	sb.WriteString(fmt.Sprintf("vision=%t;", req.Features.HasVision))
	sb.WriteString(fmt.Sprintf("reasoning=%t;", req.Features.HasReasoning))
	sb.WriteString(fmt.Sprintf("structured_output=%t;", req.Features.StructuredOutput))
	sb.WriteString(fmt.Sprintf("estimated_context_tokens=%d;", req.EstimatedInputTokens+req.MaxOutputTokens))
	sb.WriteString(fmt.Sprintf("min_context_window=%d;", req.MinContextWindow))
	sb.WriteString(fmt.Sprintf("candidate_count=%d;", len(req.Candidates)))
	// No raw prompt, no system prompt, no transcript, no source code, no files, no tool results, no headers

	out := sb.String()
	if len(out) > remote.MaxTaskLength {
		out = out[:remote.MaxTaskLength]
	}
	return out
}

// BuildCandidateDescription creates factual metadata only description, no quality claims, no provider/model names
func BuildCandidateDescription(c decision.Candidate, required int) string {
	// Use buckets, not raw IDs
	latency := latencyBucket(c.EWMALatencyMS, c.Successes)
	cost := costBucket(c.EstimatedCostUSD, c.PriceKnown)
	reliability := reliabilityBucket(c.Successes, c.Failures, c.EWMAFailureRate)
	headroom := contextHeadroomBucket(c.ContextWindow, required)

	// Capabilities factual
	caps := []string{}
	if c.Capabilities.Tools {
		caps = append(caps, "tools")
	}
	if c.Capabilities.Vision {
		caps = append(caps, "vision")
	}
	if c.Capabilities.Reasoning {
		caps = append(caps, "reasoning")
	}
	if c.Capabilities.Streaming {
		caps = append(caps, "streaming")
	}
	if len(caps) == 0 {
		caps = append(caps, "none")
	}

	desc := fmt.Sprintf("context_window=%d; headroom=%s; tools=%t; vision=%t; reasoning=%t; streaming=%t; latency=%s; cost=%s; reliability=%s; caps=%s",
		c.ContextWindow, headroom, c.Capabilities.Tools, c.Capabilities.Vision, c.Capabilities.Reasoning, c.Capabilities.Streaming,
		latency, cost, reliability, strings.Join(caps, ","))

	if len(desc) > remote.MaxCandidateDescriptionLength {
		desc = desc[:remote.MaxCandidateDescriptionLength]
	}
	return desc
}

// BuildPriorities deterministically maps TaskProfile to Jev priorities (bounded, documented, no fake quality)
// Based on spec conceptual mapping, but defined as bounded documented mapping
func BuildPriorities(taskType string) []string {
	taskType = strings.ToLower(strings.TrimSpace(taskType))
	switch taskType {
	case "simple_chat":
		return []string{"latency", "cost", "reliability"}
	case "coding":
		return []string{"reliability", "context", "latency"}
	case "code_edit":
		return []string{"reliability", "context", "latency"}
	case "debugging":
		return []string{"reliability", "context", "latency"}
	case "repository_analysis":
		return []string{"context", "reliability"}
	case "architecture_reasoning":
		return []string{"reliability", "context"}
	case "deep_reasoning":
		return []string{"reliability", "context", "latency"}
	case "tool_use":
		return []string{"reliability", "latency"}
	case "agentic_task":
		return []string{"reliability", "latency", "context"}
	case "long_context":
		return []string{"context", "reliability"}
	case "vision":
		return []string{"reliability", "latency"}
	case "structured_output":
		return []string{"reliability", "latency"}
	case "data_extraction":
		return []string{"reliability", "context"}
	case "general", "unknown", "":
		return []string{"reliability", "latency", "cost"}
	default:
		return []string{"reliability", "latency", "cost"}
	}
}

// BuildConstraints returns safe constraints explaining candidate set already passed eligibility
func BuildConstraints() []string {
	return []string{
		"candidates already passed hard eligibility (protocol, capability, context window, health)",
		"select exactly one supplied candidate",
	}
}

// BuildJevRequest creates Jev request from DecisionRequest and opaque mapping, with size check
func BuildJevRequest(req decision.DecisionRequest, mapping OpaqueMapping) (*JevRequest, error) {
	required := req.MinContextWindow
	if required <= 0 {
		required = req.EstimatedInputTokens + req.MaxOutputTokens
	}

	task := BuildTaskSummary(req)
	priorities := BuildPriorities(string(req.TaskProfile.Type))
	constraints := BuildConstraints()

	candidates := make([]JevCandidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		opaque, ok := mapping.PhysicalToOpaque[c.ID]
		if !ok {
			continue // should not happen
		}
		desc := BuildCandidateDescription(c, required)
		jc := JevCandidate{
			ID:          opaque,
			Description: desc,
			Cost:        costBucket(c.EstimatedCostUSD, c.PriceKnown),
			Latency:     latencyBucket(c.EWMALatencyMS, c.Successes),
		}
		candidates = append(candidates, jc)
	}

	// Sort candidates by opaque ID for determinism
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})

	jevReq := &JevRequest{
		Task:        task,
		Candidates:  candidates,
		Priorities:  priorities,
		Constraints: constraints,
	}

	if err := jevReq.Validate(); err != nil {
		return nil, err
	}

	// Measure bytes
	b, err := json.Marshal(jevReq)
	if err != nil {
		return nil, remote.NewError(remote.ErrInvalidResponse, "failed to marshal")
	}
	if len(b) > remote.MaxRequestBodyBytes {
		return nil, &remote.Error{Kind: remote.ErrRequestTooLarge, Message: fmt.Sprintf("request %d > %d", len(b), remote.MaxRequestBodyBytes)}
	}

	return jevReq, nil
}
