package taskintel

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFReasonSummaryListsSignals verifies the human summary names the
// fired classifier signals in order.
func TestAgentFReasonSummaryListsSignals(t *testing.T) {
	p := taskprofile.TaskProfile{
		Type: taskprofile.TaskCoding, Confidence: 0.95,
		ReasonCodes: []taskprofile.ReasonCode{
			taskprofile.ReasonVisionPresent, taskprofile.ReasonAudioPresent,
		},
	}
	r := Resolve(Input{Mode: ModeSuggest, Profile: p, TaskPools: map[string]string{"coding": "code-pool"}})
	if r.Outcome != OutcomeSuggested {
		t.Fatalf("outcome = %q, want suggested", r.Outcome)
	}
	if !strings.Contains(r.Summary, "vision_present+audio_present") {
		t.Fatalf("summary = %q, want it to list vision_present+audio_present", r.Summary)
	}
}

// TestAgentFReasonSummaryTruncates verifies a long signal list is truncated
// to the 160-character bound with an ellipsis marker.
func TestAgentFReasonSummaryTruncates(t *testing.T) {
	codes := make([]taskprofile.ReasonCode, 0, 20)
	for i := 0; i < 20; i++ {
		codes = append(codes, taskprofile.ReasonCode("signal_number_"+strings.Repeat("x", 10)+"_"+string(rune('a'+i))))
	}
	p := taskprofile.TaskProfile{Type: taskprofile.TaskCoding, Confidence: 0.95, ReasonCodes: codes}
	got := reasonSummary(p)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("summary = %q, want ellipsis-terminated truncation", got)
	}
	if len(got) > 160+len("…") {
		t.Fatalf("summary len = %d, want at most %d", len(got), 160+len("…"))
	}
}

// TestAgentFReasonSummaryNoSignals verifies a confident classification with
// no fired signals renders as "no signals" instead of an empty reason.
func TestAgentFReasonSummaryNoSignals(t *testing.T) {
	p := taskprofile.TaskProfile{Type: taskprofile.TaskCoding, Confidence: 0.95}
	if got := reasonSummary(p); got != "no signals" {
		t.Fatalf("reasonSummary = %q, want %q", got, "no signals")
	}
	r := Resolve(Input{Mode: ModeSuggest, Profile: p, TaskPools: map[string]string{"coding": "code-pool"}})
	if !strings.Contains(r.Summary, "no signals") {
		t.Fatalf("summary = %q, want it to report no signals", r.Summary)
	}
}
