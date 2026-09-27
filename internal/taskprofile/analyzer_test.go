package taskprofile

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
)

func TestAnalyzer_Deterministic(t *testing.T) {
	feat := feature.RequestFeatures{
		HasCodeBlock:          true,
		HasFilePath:           true,
		HasEditKeywords:       true,
		EstimatedPromptTokens: 1000,
		MessageCount:          2,
	}
	a := NewAnalyzer()
	p1 := a.Analyze(feat)
	p2 := a.Analyze(feat)
	if p1.Type != p2.Type || p1.Complexity != p2.Complexity || p1.Confidence != p2.Confidence {
		t.Fatalf("non-deterministic: %v vs %v", p1, p2)
	}
	if len(p1.ReasonCodes) != len(p2.ReasonCodes) {
		t.Fatalf("reason codes length mismatch")
	}
	for i := range p1.ReasonCodes {
		if p1.ReasonCodes[i] != p2.ReasonCodes[i] {
			t.Fatalf("reason codes mismatch")
		}
	}
}

func TestAnalyzer_TaskTypes(t *testing.T) {
	tests := []struct {
		name     string
		feat     feature.RequestFeatures
		expected TaskType
	}{
		{
			name:     "debugging stack trace",
			feat:     feature.RequestFeatures{HasStackTrace: true},
			expected: TaskDebugging,
		},
		{
			name:     "debugging debug+code",
			feat:     feature.RequestFeatures{HasDebugKeywords: true, HasCodeBlock: true},
			expected: TaskDebugging,
		},
		{
			name:     "code_edit diff",
			feat:     feature.RequestFeatures{HasDiff: true},
			expected: TaskCodeEdit,
		},
		{
			name:     "code_edit edit+code",
			feat:     feature.RequestFeatures{HasEditKeywords: true, HasCodeBlock: true},
			expected: TaskCodeEdit,
		},
		{
			name:     "repository_analysis",
			feat:     feature.RequestFeatures{HasRepoKeywords: true, HasFilePath: true},
			expected: TaskRepositoryAnalysis,
		},
		{
			name:     "architecture_reasoning",
			feat:     feature.RequestFeatures{HasArchKeywords: true},
			expected: TaskArchitectureReasoning,
		},
		{
			name:     "coding code block",
			feat:     feature.RequestFeatures{HasCodeBlock: true},
			expected: TaskCoding,
		},
		{
			name:     "coding identifiers",
			feat:     feature.RequestFeatures{HasCodeIdentifiers: true},
			expected: TaskCoding,
		},
		{
			name:     "data_extraction",
			feat:     feature.RequestFeatures{HasExtractionKeywords: true},
			expected: TaskDataExtraction,
		},
		{
			name:     "agentic_task keywords",
			feat:     feature.RequestFeatures{HasAgentKeywords: true},
			expected: TaskAgenticTask,
		},
		{
			name:     "agentic_task tools multi-turn",
			feat:     feature.RequestFeatures{HasTools: true, ToolCount: 2, MessageCount: 5},
			expected: TaskAgenticTask,
		},
		{
			name:     "tool_use single turn",
			feat:     feature.RequestFeatures{HasTools: true, ToolCount: 1, MessageCount: 1},
			expected: TaskToolUse,
		},
		{
			name:     "tool_use required",
			feat:     feature.RequestFeatures{HasTools: true, ToolChoiceRequired: true, MessageCount: 1},
			expected: TaskToolUse,
		},
		{
			name:     "vision",
			feat:     feature.RequestFeatures{HasVision: true},
			expected: TaskVision,
		},
		{
			name:     "simple_chat",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 100, MessageCount: 1},
			expected: TaskSimpleChat,
		},
		{
			name:     "deep_reasoning",
			feat:     feature.RequestFeatures{HasReasoning: true, EstimatedPromptTokens: 3000, MessageCount: 1},
			expected: TaskDeepReasoning,
		},
		{
			name:     "structured_output",
			feat:     feature.RequestFeatures{StructuredOutput: true, EstimatedPromptTokens: 100, MessageCount: 1},
			expected: TaskStructuredOutput,
		},
		{
			name:     "long_context",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 9000, MessageCount: 1},
			expected: TaskLongContext,
		},
		{
			name:     "general fallback",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 600, MessageCount: 2},
			expected: TaskGeneral,
		},
		{
			name:     "unknown too complex",
			feat:     feature.RequestFeatures{TooComplex: true},
			expected: TaskUnknown,
		},
	}
	a := NewAnalyzer()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := a.Analyze(tc.feat)
			if p.Type != tc.expected {
				t.Fatalf("expected %s got %s", tc.expected, p.Type)
			}
			if !p.Valid() {
				t.Fatalf("invalid profile: %+v", p)
			}
		})
	}
}

func TestAnalyzer_Complexity(t *testing.T) {
	tests := []struct {
		name     string
		feat     feature.RequestFeatures
		expected Complexity
	}{
		{
			name:     "trivial",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 10},
			expected: ComplexityTrivial,
		},
		{
			name:     "low",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 500},
			expected: ComplexityLow,
		},
		{
			name:     "medium",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 2000},
			expected: ComplexityMedium,
		},
		{
			name:     "high",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 5000},
			expected: ComplexityHigh,
		},
		{
			name:     "very high",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 20000},
			expected: ComplexityVeryHigh,
		},
		{
			name:     "high via tools",
			feat:     feature.RequestFeatures{EstimatedPromptTokens: 100, ToolCount: 10},
			expected: ComplexityMedium,
		},
	}
	a := NewAnalyzer()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := a.Analyze(tc.feat)
			if p.Complexity != tc.expected {
				t.Fatalf("expected %s got %s (score based)", tc.expected, p.Complexity)
			}
		})
	}
}

func TestAnalyzer_Confidence(t *testing.T) {
	a := NewAnalyzer()
	feat := feature.RequestFeatures{HasStackTrace: true, HasCodeBlock: true, HasDebugKeywords: true}
	p := a.Analyze(feat)
	if p.Confidence < 0.8 {
		t.Fatalf("expected high confidence for strong signals, got %f", p.Confidence)
	}
	feat2 := feature.RequestFeatures{EstimatedPromptTokens: 100, MessageCount: 1}
	p2 := a.Analyze(feat2)
	if p2.Confidence < 0.5 {
		t.Fatalf("expected reasonable confidence for simple_chat, got %f", p2.Confidence)
	}
	if p2.Confidence > 1.0 || p2.Confidence < 0.0 {
		t.Fatalf("confidence out of range %f", p2.Confidence)
	}
}

func TestAnalyzer_ReasonCodes(t *testing.T) {
	feat := feature.RequestFeatures{
		HasVision:             true,
		VisionImageCount:      4,
		HasTools:              true,
		ToolCount:             6,
		HasCodeBlock:          true,
		HasStackTrace:         true,
		HasFilePath:           true,
		HasSystemPrompt:       true,
		Streaming:             true,
		EstimatedPromptTokens: 9000,
		MessageCount:          5,
		ToolChoiceRequired:    true,
	}
	a := NewAnalyzer()
	p := a.Analyze(feat)
	expected := map[ReasonCode]bool{
		ReasonVisionPresent:      true,
		ReasonHighImageCount:     true,
		ReasonToolsPresent:       true,
		ReasonComplexTools:       true,
		ReasonToolChoiceRequired: true,
		ReasonCodeBlock:          true,
		ReasonStackTrace:         true,
		ReasonFilePath:           true,
		ReasonSystemPrompt:       true,
		ReasonStreaming:          true,
		ReasonLongContext:        true,
		ReasonMultiTurn:          true,
	}
	for rc := range expected {
		found := false
		for _, actual := range p.ReasonCodes {
			if actual == rc {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected reason code %s not found in %v", rc, p.ReasonCodes)
		}
	}
	for i := 1; i < len(p.ReasonCodes); i++ {
		if p.ReasonCodes[i-1] >= p.ReasonCodes[i] {
			t.Fatalf("reason codes not sorted/deduped: %v", p.ReasonCodes)
		}
	}
}

func TestAnalyzer_NoRawPromptAccess(t *testing.T) {
	a := NewAnalyzer()
	feat := feature.RequestFeatures{}
	p := a.Analyze(feat)
	if p.Type == "" {
		t.Fatalf("expected type")
	}
}

func TestAnalyzer_SecondaryRequirements(t *testing.T) {
	a := NewAnalyzer()
	feat := feature.RequestFeatures{
		HasVision:             true,
		HasReasoning:          true,
		HasTools:              true,
		StructuredOutput:      true,
		EstimatedPromptTokens: 9000,
		ToolChoiceRequired:    true,
	}
	p := a.Analyze(feat)
	if !p.RequiresVision {
		t.Fatalf("expected RequiresVision")
	}
	if !p.RequiresReasoning {
		t.Fatalf("expected RequiresReasoning")
	}
	if !p.RequiresTools {
		t.Fatalf("expected RequiresTools")
	}
	if !p.RequiresStructuredOutput {
		t.Fatalf("expected RequiresStructuredOutput")
	}
	if !p.RequiresLongContext {
		t.Fatalf("expected RequiresLongContext")
	}
	if !p.ToolChoiceRequired {
		t.Fatalf("expected ToolChoiceRequired")
	}
}
