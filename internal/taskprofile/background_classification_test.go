package taskprofile

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
)

// #421 item 3: background chores classify as TaskBackground so
// the task-intelligence layer can steer them to the route's
// background pool — but any real work signal vetoes the class.
func TestAnalyzerBackgroundClassification(t *testing.T) {
	a := NewAnalyzer()
	cases := []struct {
		name string
		feat feature.RequestFeatures
		want TaskType
	}{
		{"title chore", feature.RequestFeatures{HasBackgroundKeywords: true, MessageCount: 1, EstimatedPromptTokens: 120}, TaskBackground},
		{"conversation summary", feature.RequestFeatures{HasBackgroundKeywords: true, MessageCount: 6, EstimatedPromptTokens: 3000}, TaskBackground},
		{"code vetoes background", feature.RequestFeatures{HasBackgroundKeywords: true, HasCodeBlock: true, MessageCount: 1}, TaskCoding},
		{"tools veto background", feature.RequestFeatures{HasBackgroundKeywords: true, HasTools: true, ToolCount: 1, MessageCount: 1}, TaskToolUse},
		{"stack trace vetoes background", feature.RequestFeatures{HasBackgroundKeywords: true, HasStackTrace: true}, TaskDebugging},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := a.Analyze(tc.feat)
			if p.Type != tc.want {
				t.Fatalf("type = %s, want %s (profile %+v)", p.Type, tc.want, p)
			}
		})
	}
	// The reason code records WHY, for the decision trace.
	p := a.Analyze(feature.RequestFeatures{HasBackgroundKeywords: true, MessageCount: 1})
	found := false
	for _, rc := range p.ReasonCodes {
		if rc == ReasonBackgroundChore {
			found = true
		}
	}
	if !found {
		t.Fatalf("reason codes %v missing background_chore", p.ReasonCodes)
	}
	if !TaskBackground.Valid() {
		t.Fatal("TaskBackground must be a valid task type (pins/overrides/pools key on it)")
	}
}
