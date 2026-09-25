package decision

import (
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// Candidate is a privacy-safe snapshot of an eligible deployment.
// It contains only routing-relevant identifiers, not secrets or raw prompts.
type Candidate struct {
	ID         string  `json:"id"`
	ProviderID string  `json:"provider_id"`
	Model      string  `json:"model,omitempty"`
	Priority   int     `json:"priority"`
	Weight     float64 `json:"weight"`
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
