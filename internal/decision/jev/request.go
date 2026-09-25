package jev

import (
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

// Jev model-route request per official docs: https://www.jevai.org/docs
// POST /api/v1/decisions/model-route
// Required: task, candidates (at least two, each with id and description)
// Optional: priorities, constraints, stakes
// Body capped 32 KiB

type JevCandidate struct {
	ID          string `json:"id"`          // opaque c0,c1
	Description string `json:"description"` // factual metadata only
	// Optional fields per docs: cost, latency, etc. We include as needed but bounded
	Cost    string `json:"cost,omitempty"`
	Latency string `json:"latency,omitempty"`
}

type JevRequest struct {
	Task        string         `json:"task"`       // synthetic task summary, max 1024
	Candidates  []JevCandidate `json:"candidates"` // at least 2, opaque IDs
	Priorities  []string       `json:"priorities,omitempty"`
	Constraints []string       `json:"constraints,omitempty"`
	// Stakes omitted in Phase F
}

// Validate checks request bounds before sending
func (r *JevRequest) Validate() error {
	if len(r.Task) == 0 {
		return remote.NewError(remote.ErrInvalidConfig, "task required")
	}
	if len(r.Task) > remote.MaxTaskLength {
		return remote.NewError(remote.ErrInvalidConfig, "task too long")
	}
	if len(r.Candidates) < 2 {
		return remote.NewError(remote.ErrInvalidConfig, "at least 2 candidates required")
	}
	if len(r.Candidates) > remote.MaxCandidates {
		return remote.NewError(remote.ErrInvalidConfig, "too many candidates")
	}
	for _, c := range r.Candidates {
		if c.ID == "" {
			return remote.NewError(remote.ErrInvalidConfig, "candidate id required")
		}
		if c.Description == "" {
			return remote.NewError(remote.ErrInvalidConfig, "candidate description required")
		}
		if len(c.Description) > remote.MaxCandidateDescriptionLength {
			return remote.NewError(remote.ErrInvalidConfig, "candidate description too long")
		}
	}
	if len(r.Priorities) > remote.MaxPriorities {
		return remote.NewError(remote.ErrInvalidConfig, "too many priorities")
	}
	if len(r.Constraints) > remote.MaxConstraints {
		return remote.NewError(remote.ErrInvalidConfig, "too many constraints")
	}
	return nil
}
