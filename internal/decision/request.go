package decision

import (
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// CandidateCapabilities is a privacy-safe subset of deployment capabilities.
type CandidateCapabilities struct {
	Streaming bool `json:"streaming,omitempty"`
	Tools     bool `json:"tools,omitempty"`
	Vision    bool `json:"vision,omitempty"`
	Reasoning bool `json:"reasoning,omitempty"`
}

// Candidate is a privacy-safe snapshot of an eligible deployment.
// It contains only routing-relevant identifiers, not secrets or raw prompts.
// Extended in Phase E with pool, health, cost, and ranking signals.
type Candidate struct {
	ID         string  `json:"id"`
	ProviderID string  `json:"provider_id"`
	Model      string  `json:"model,omitempty"`
	Priority   int     `json:"priority"`
	Weight     float64 `json:"weight"`

	// Phase E: pool / fallback boundary
	PoolID      string `json:"pool_id,omitempty"`
	PoolOrdinal int    `json:"pool_ordinal"`

	// Phase E: router baseline and health
	RouterScore      float64               `json:"router_score"`
	HealthStatus     string                `json:"health_status,omitempty"`
	EWMALatencyMS    float64               `json:"ewma_latency_ms,omitempty"`
	EWMATTFTMS       float64               `json:"ewma_ttft_ms,omitempty"`
	EWMAFailureRate  float64               `json:"ewma_failure_rate,omitempty"`
	Successes        int64                 `json:"successes,omitempty"`
	Failures         int64                 `json:"failures,omitempty"`
	CapacityPressure float64               `json:"capacity_pressure,omitempty"`
	EstimatedCostUSD float64               `json:"estimated_cost_usd,omitempty"`
	PriceKnown       bool                  `json:"price_known,omitempty"`
	ContextWindow    int                   `json:"context_window,omitempty"`
	Capabilities     CandidateCapabilities `json:"capabilities,omitempty"`
	OriginalRank     int                   `json:"original_rank"`
}

// Constraints placeholder for Phase E policy constraints.
// Phase D: empty, but struct exists for forward compatibility.
type Constraints struct{}

// DecisionRequest is the input to a DecisionProvider.
// It MUST NOT contain raw prompts, user content, tool results, API keys,
// headers, or any PII. Only RequestFeatures + TaskProfile + candidate IDs.
type DecisionRequest struct {
	// Task intelligence (privacy-safe)
	TaskProfile TaskProfile `json:"task_profile"`
	Features    Features    `json:"features"`

	// Eligible set snapshot (authoritative)
	Candidates []Candidate `json:"candidates"`

	// Routing context (IDs only, no secrets)
	VirtualEndpointID string `json:"virtual_endpoint_id,omitempty"`
	RouteProfileID    string `json:"route_profile_id,omitempty"`
	CandidatePoolID   string `json:"candidate_pool_id,omitempty"`

	// Phase E: affinity and policy routing (privacy-safe, ID only)
	PinnedCandidateID string `json:"pinned_candidate_id,omitempty"`
	PolicyID          string `json:"policy_id,omitempty"`

	// Phase E: context window signals (estimated tokens, no raw content)
	EstimatedInputTokens int `json:"estimated_input_tokens,omitempty"`
	MaxOutputTokens      int `json:"max_output_tokens,omitempty"`
	MinContextWindow     int `json:"min_context_window,omitempty"`

	// Policy / budget
	Constraints Constraints `json:"constraints"`
	Budget      Budget      `json:"budget"`

	// Request correlation (no PII)
	RequestID string `json:"request_id,omitempty"`
}

// TaskProfile is a local alias for taskprofile.TaskProfile to avoid
// importing cycles in some contexts, but we use the upstream type directly.
// We define type aliases for JSON clarity.

type TaskProfile = taskprofile.TaskProfile
type Features = feature.RequestFeatures

// CloneCandidates returns a deep copy of candidates slice.
func CloneCandidates(in []Candidate) []Candidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]Candidate, len(in))
	copy(out, in)
	return out
}
