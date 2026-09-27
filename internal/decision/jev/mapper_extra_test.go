package jev

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// TestBuildPrioritiesIsBoundedAndCanonical covers the documented task-type to
// priority mapping: every canonical task type (and unknown input) must produce a
// bounded, known priority list, because it becomes part of the external request.
func TestBuildPrioritiesIsBoundedAndCanonical(t *testing.T) {
	allowed := map[string]bool{"latency": true, "cost": true, "reliability": true, "context": true}
	for _, tt := range taskprofile.AllTaskTypes() {
		got := BuildPriorities(string(tt))
		if len(got) == 0 || len(got) > remote.MaxPriorities {
			t.Fatalf("task type %s produced %d priorities", tt, len(got))
		}
		seen := map[string]bool{}
		for _, p := range got {
			if !allowed[p] {
				t.Fatalf("task type %s produced unknown priority %q", tt, p)
			}
			if seen[p] {
				t.Fatalf("task type %s produced duplicate priority %q", tt, p)
			}
			seen[p] = true
		}
	}
	// Unknown, empty and oddly cased input all fall back to the same default, so
	// an unrecognised task type cannot change what is sent upstream.
	def := BuildPriorities("")
	for _, raw := range []string{"  ", "UNKNOWN", "not-a-task", "Coding"} {
		got := BuildPriorities(raw)
		if strings.Join(got, ",") != strings.Join(def, ",") && raw != "Coding" {
			t.Fatalf("BuildPriorities(%q) = %v, want the default %v", raw, got, def)
		}
	}
	if got := BuildPriorities("Coding"); got[0] != "reliability" {
		t.Fatalf("coding must prioritise reliability, got %v", got)
	}
}

// TestJevRequestValidateBounds closes the request-side bounds: the mapper must
// refuse to build a request that exceeds the documented external caps instead of
// sending it and hoping the remote rejects it.
func TestJevRequestValidateBounds(t *testing.T) {
	base := func() JevRequest {
		return JevRequest{
			Task: "task=x;",
			Candidates: []JevCandidate{
				{ID: "c0", Description: "context_window=8192"},
				{ID: "c1", Description: "context_window=4096"},
			},
		}
	}
	valid := base()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	mutations := []struct {
		name   string
		mutate func(r *JevRequest)
	}{
		{"empty task", func(r *JevRequest) { r.Task = "" }},
		{"task too long", func(r *JevRequest) { r.Task = strings.Repeat("t", remote.MaxTaskLength+1) }},
		{"one candidate", func(r *JevRequest) { r.Candidates = r.Candidates[:1] }},
		{"empty candidate id", func(r *JevRequest) { r.Candidates[0].ID = "" }},
		{"empty description", func(r *JevRequest) { r.Candidates[0].Description = "" }},
		{"description too long", func(r *JevRequest) {
			r.Candidates[0].Description = strings.Repeat("d", remote.MaxCandidateDescriptionLength+1)
		}},
		{"too many priorities", func(r *JevRequest) {
			r.Priorities = make([]string, remote.MaxPriorities+1)
		}},
		{"too many constraints", func(r *JevRequest) {
			r.Constraints = make([]string, remote.MaxConstraints+1)
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("expected the request to be rejected")
			}
		})
	}
}

// TestBuildJevRequestRejectsOversizedCandidateSet proves the mapper refuses a
// candidate set larger than the external cap rather than truncating it silently,
// which would hide eligible candidates from the decision.
func TestBuildJevRequestRejectsOversizedCandidateSet(t *testing.T) {
	cands := make([]decision.Candidate, 0, remote.MaxCandidates+1)
	for i := 0; i <= remote.MaxCandidates; i++ {
		cands = append(cands, decision.Candidate{ID: "p/m" + string(rune('a'+i%26)) + string(rune('0'+i/26))})
	}
	req := decision.DecisionRequest{Candidates: cands}
	if _, err := BuildJevRequest(req, BuildOpaqueMapping(cands)); err == nil {
		t.Fatal("a candidate set above the external cap must be rejected")
	}
}

// TestMapperOutputStaysInsideRemoteBounds pins the privacy/bounds contract of the
// two summary builders: bounded, metadata-only strings.
func TestMapperOutputStaysInsideRemoteBounds(t *testing.T) {
	cands := []decision.Candidate{{
		ID: "p1/m1", ContextWindow: 200000, Successes: 10, Failures: 1,
		EWMALatencyMS: 120, EstimatedCostUSD: 0.002, PriceKnown: true,
		Capabilities: decision.CandidateCapabilities{Tools: true, Vision: true, Reasoning: true, Streaming: true},
	}}
	req := decision.DecisionRequest{
		Candidates:           cands,
		Features:             feature.RequestFeatures{EstimatedTotalTokens: 200000},
		EstimatedInputTokens: 9000,
		MaxOutputTokens:      4096,
		MinContextWindow:     200000,
		TaskProfile:          taskprofile.TaskProfile{Type: taskprofile.TaskCoding},
	}
	summary := BuildTaskSummary(req)
	if len(summary) > remote.MaxTaskLength {
		t.Fatalf("task summary = %d bytes, cap %d", len(summary), remote.MaxTaskLength)
	}
	if strings.Contains(summary, "p1/m1") {
		t.Fatalf("task summary leaked a physical id: %s", summary)
	}
	desc := BuildCandidateDescription(cands[0], 200000)
	if len(desc) > remote.MaxCandidateDescriptionLength {
		t.Fatalf("description = %d bytes, cap %d", len(desc), remote.MaxCandidateDescriptionLength)
	}
	if strings.Contains(desc, "p1/m1") {
		t.Fatalf("candidate description leaked a physical id: %s", desc)
	}
}
