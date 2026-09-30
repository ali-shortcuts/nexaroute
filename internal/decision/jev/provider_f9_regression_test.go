package jev

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// TestF9_DeadMetadataNormalizationRemoved is a regression test for audit
// finding F9: the dead `_ = strings.ToLower("metadata_only")` init expression
// was removed with no behavior change. Metadata-only enforcement lives in
// NewProvider validation, Decide gating, and the metadata-only mappers
// (BuildTaskSummary/BuildCandidateDescription), not in any init side effect.
func TestF9_DeadMetadataNormalizationRemoved(t *testing.T) {
	// Empty privacy mode defaults to metadata_only.
	p, err := NewProvider(ProviderConfig{
		ID:      "jev-main",
		Type:    "jev",
		Enabled: true,
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatalf("NewProvider with empty privacy mode should succeed: %v", err)
	}
	p.mu.RLock()
	mode := p.privacyMode
	p.mu.RUnlock()
	if mode != "metadata_only" {
		t.Fatalf("expected default privacy_mode metadata_only, got %q", mode)
	}

	// Non-metadata_only mode is still rejected at construction.
	if _, err := NewProvider(ProviderConfig{
		ID:          "jev-main",
		Type:        "jev",
		Enabled:     true,
		APIKey:      "test-key",
		PrivacyMode: "full",
	}); err == nil {
		t.Fatalf("expected error for non-metadata_only privacy mode")
	}

	// Built request remains metadata-only and deterministic: no raw
	// candidate IDs or provider/model details leak into task/candidates.
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "coding"},
		Features:    feature.RequestFeatures{},
		Candidates: []decision.Candidate{
			{ID: "secret-provider/secret-model", PoolOrdinal: 1, Priority: 2, OriginalRank: 0},
			{ID: "p2/m2", PoolOrdinal: 0, Priority: 0, OriginalRank: 1},
		},
	}
	jevReq, mapping, err := p.testBuildRequest(req)
	if err != nil {
		t.Fatalf("testBuildRequest failed: %v", err)
	}
	if jevReq.Task == "" {
		t.Fatalf("expected non-empty metadata task summary")
	}
	for _, c := range jevReq.Candidates {
		if strings.Contains(c.ID, "secret-provider") || strings.Contains(c.Description, "secret-provider") {
			t.Fatalf("raw candidate ID leaked into request: %+v", c)
		}
	}
	// Opaque mapping still covers every candidate deterministically.
	if len(mapping.OpaqueToPhysical) != len(req.Candidates) {
		t.Fatalf("expected %d opaque mappings, got %d", len(req.Candidates), len(mapping.OpaqueToPhysical))
	}
}
