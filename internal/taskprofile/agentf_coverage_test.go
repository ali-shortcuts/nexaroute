package taskprofile

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFAudioPresentReason verifies audio input is recorded as a
// classification reason.
func TestAgentFAudioPresentReason(t *testing.T) {
	a := NewAnalyzer()
	got := a.Analyze(feature.RequestFeatures{HasAudio: true, AudioCount: 1})
	found := false
	for _, rc := range got.ReasonCodes {
		if rc == ReasonAudioPresent {
			found = true
		}
	}
	if !found {
		t.Fatalf("reason codes = %v, want audio_present", got.ReasonCodes)
	}
	// Audio alone does not imply vision or tools.
	if got.HasVision || got.HasTools {
		t.Fatalf("profile = %+v, audio must not imply vision/tools", got)
	}
}
