package taskprofile

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
)

func TestCoveragePassReasonSignalsInlineCodeAndURL(t *testing.T) {
	a := NewAnalyzer()
	inline := a.Analyze(feature.RequestFeatures{HasInlineCode: true, MessageCount: 2, EstimatedPromptTokens: 600})
	found := false
	for _, rc := range inline.ReasonCodes {
		if rc == ReasonInlineCode {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("inline code reason missing: %v", inline.ReasonCodes)
	}

	withURL := a.Analyze(feature.RequestFeatures{HasURL: true, HasFilePath: true, MessageCount: 2, EstimatedPromptTokens: 600})
	foundURL := false
	for _, rc := range withURL.ReasonCodes {
		if rc == ReasonURLPresent {
			foundURL = true
			break
		}
	}
	if !foundURL {
		t.Fatalf("url reason missing: %v", withURL.ReasonCodes)
	}
}

func TestCoveragePassClassifyEditFilePathVariants(t *testing.T) {
	a := NewAnalyzer()
	tests := []struct {
		name     string
		feat     feature.RequestFeatures
		expected TaskType
	}{
		{
			name:     "edit plus file path without url is code edit",
			feat:     feature.RequestFeatures{HasEditKeywords: true, HasFilePath: true, MessageCount: 2, EstimatedPromptTokens: 600},
			expected: TaskCodeEdit,
		},
		{
			name:     "edit plus file path with url is not code edit",
			feat:     feature.RequestFeatures{HasEditKeywords: true, HasFilePath: true, HasURL: true, MessageCount: 2, EstimatedPromptTokens: 600},
			expected: TaskGeneral,
		},
		{
			name:     "multi-turn tool use without agent keywords",
			feat:     feature.RequestFeatures{HasTools: true, ToolCount: 1, MessageCount: 3, EstimatedPromptTokens: 600},
			expected: TaskToolUse,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.Analyze(tc.feat)
			if got.Type != tc.expected {
				t.Fatalf("type=%s want %s (profile=%+v)", got.Type, tc.expected, got)
			}
			if !got.Valid() {
				t.Fatalf("invalid profile: %+v", got)
			}
		})
	}
}

func TestCoveragePassConfidenceArms(t *testing.T) {
	a := NewAnalyzer()

	coding := a.Analyze(feature.RequestFeatures{HasCodeBlock: true, HasFilePath: true, MessageCount: 2, EstimatedPromptTokens: 600})
	if coding.Type != TaskCoding {
		t.Fatalf("coding type=%s", coding.Type)
	}
	if coding.Confidence < 0.89 || coding.Confidence > 1.0 {
		t.Fatalf("coding with file path confidence=%v, want ~0.9", coding.Confidence)
	}

	toolUse := a.Analyze(feature.RequestFeatures{HasTools: true, ToolCount: 3, ToolChoiceRequired: true, MessageCount: 1, EstimatedPromptTokens: 100})
	if toolUse.Type != TaskToolUse {
		t.Fatalf("tool use type=%s", toolUse.Type)
	}
	if toolUse.Confidence < 1.0 {
		t.Fatalf("tool use confidence=%v, want clamped 1.0", toolUse.Confidence)
	}

	vision := a.Analyze(feature.RequestFeatures{HasVision: true, VisionImageCount: 2, MessageCount: 1, EstimatedPromptTokens: 100})
	if vision.Type != TaskVision {
		t.Fatalf("vision type=%s", vision.Type)
	}
	if vision.Confidence != 1.0 {
		t.Fatalf("vision confidence=%v, want 1.0 (0.5+0.4+0.1)", vision.Confidence)
	}

	longCtx := a.Analyze(feature.RequestFeatures{EstimatedPromptTokens: 9000, MessageCount: 1})
	if longCtx.Type != TaskLongContext {
		t.Fatalf("long context type=%s", longCtx.Type)
	}
	if longCtx.Confidence != 0.8 {
		t.Fatalf("long context confidence=%v, want 0.8", longCtx.Confidence)
	}

	structured := a.Analyze(feature.RequestFeatures{StructuredOutput: true, EstimatedPromptTokens: 100, MessageCount: 1})
	if structured.Type != TaskStructuredOutput {
		t.Fatalf("structured type=%s", structured.Type)
	}
	if structured.Confidence != 0.9 {
		t.Fatalf("structured confidence=%v, want 0.9", structured.Confidence)
	}

	if got := computeConfidence(feature.RequestFeatures{}, TaskUnknown); got != 0.1 {
		t.Fatalf("unknown confidence=%v, want 0.1", got)
	}

	// SimpleChat with a contradictory signal can only be constructed by calling
	// computeConfidence directly; classifyType never returns SimpleChat with
	// signals set. The branch is exercised here to pin the documented fallback.
	if got := computeConfidence(feature.RequestFeatures{HasCodeBlock: true, HasStackTrace: true, HasDiff: true, HasFilePath: true, HasTools: true, HasVision: true}, TaskSimpleChat); got != 0.4 {
		t.Fatalf("simple chat with signals confidence=%v, want 0.4", got)
	}
	if got := computeConfidence(feature.RequestFeatures{}, TaskSimpleChat); got != 0.85 {
		t.Fatalf("simple chat without signals confidence=%v, want 0.85", got)
	}

	generalClean := computeConfidence(feature.RequestFeatures{}, TaskGeneral)
	if generalClean != 0.8 {
		t.Fatalf("general without signals confidence=%v, want 0.8", generalClean)
	}
	generalBusy := computeConfidence(feature.RequestFeatures{
		HasCodeBlock: true, HasStackTrace: true, HasDiff: true, HasFilePath: true,
		HasEditKeywords: true, HasDebugKeywords: true, HasRepoKeywords: true,
		HasArchKeywords: true, HasAgentKeywords: true, HasExtractionKeywords: true,
	}, TaskGeneral)
	if generalBusy != 0.3 {
		t.Fatalf("general saturated signals confidence=%v, want floored 0.3", generalBusy)
	}

	truncated := computeConfidence(feature.RequestFeatures{RelevantTruncated: true}, TaskDataExtraction)
	plain := computeConfidence(feature.RequestFeatures{}, TaskDataExtraction)
	if truncated != plain-0.05 {
		t.Fatalf("truncated=%v plain=%v, want exactly 0.05 penalty", truncated, plain)
	}
	complexPenalty := computeConfidence(feature.RequestFeatures{TooComplex: true}, TaskDataExtraction)
	if complexPenalty != plain-0.1 {
		t.Fatalf("too-complex=%v plain=%v, want exactly 0.10 penalty", complexPenalty, plain)
	}
	floored := computeConfidence(feature.RequestFeatures{TooComplex: true, RelevantTruncated: true}, TaskUnknown)
	if floored != 0.1 {
		t.Fatalf("unknown over-penalized confidence=%v, want floored 0.1", floored)
	}
}

func TestCoveragePassValidRejectsMalformedProfiles(t *testing.T) {
	tests := []struct {
		name    string
		profile TaskProfile
		valid   bool
	}{
		{"bad type", TaskProfile{Type: "nope", Complexity: ComplexityLow, Confidence: 0.5}, false},
		{"bad complexity", TaskProfile{Type: TaskCoding, Complexity: "nope", Confidence: 0.5}, false},
		{"confidence above range", TaskProfile{Type: TaskCoding, Complexity: ComplexityLow, Confidence: 1.5}, false},
		{"confidence below range", TaskProfile{Type: TaskCoding, Complexity: ComplexityLow, Confidence: -0.1}, false},
		{"well formed", TaskProfile{Type: TaskCoding, Complexity: ComplexityLow, Confidence: 0.5}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.profile.Valid(); got != tc.valid {
				t.Fatalf("Valid()=%v want %v for %+v", got, tc.valid, tc.profile)
			}
		})
	}
}

func TestCoveragePassTaskTypeAndComplexityEnumerations(t *testing.T) {
	types := AllTaskTypes()
	if len(types) != 15 {
		t.Fatalf("AllTaskTypes=%d, want 15", len(types))
	}
	seen := map[TaskType]bool{}
	for _, typ := range types {
		if !typ.Valid() {
			t.Fatalf("enumerated type %q is not valid", typ)
		}
		if seen[typ] {
			t.Fatalf("duplicate type %q", typ)
		}
		seen[typ] = true
	}
	if (TaskType("nope")).Valid() {
		t.Fatal("bogus task type reported valid")
	}

	complexities := AllComplexities()
	if len(complexities) != 5 {
		t.Fatalf("AllComplexities=%d, want 5", len(complexities))
	}
	for _, c := range complexities {
		if !c.Valid() {
			t.Fatalf("enumerated complexity %q is not valid", c)
		}
	}
	if (Complexity("nope")).Valid() {
		t.Fatal("bogus complexity reported valid")
	}
}
