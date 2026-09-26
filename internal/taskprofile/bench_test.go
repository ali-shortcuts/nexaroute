package taskprofile

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
)

func BenchmarkAnalyzer_SimpleChat(b *testing.B) {
	feat := feature.RequestFeatures{
		EstimatedPromptTokens: 100,
		MessageCount:          1,
	}
	a := NewAnalyzer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Analyze(feat)
	}
}

func BenchmarkAnalyzer_Coding(b *testing.B) {
	feat := feature.RequestFeatures{
		HasCodeBlock:          true,
		HasFilePath:           true,
		HasCodeIdentifiers:    true,
		EstimatedPromptTokens: 1000,
		MessageCount:          2,
	}
	a := NewAnalyzer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Analyze(feat)
	}
}

func BenchmarkAnalyzer_ToolHeavy(b *testing.B) {
	feat := feature.RequestFeatures{
		HasTools:              true,
		ToolCount:             10,
		ToolChoiceRequired:    true,
		HasAgentKeywords:      true,
		MessageCount:          5,
		EstimatedPromptTokens: 2000,
	}
	a := NewAnalyzer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Analyze(feat)
	}
}

func BenchmarkAnalyzer_Complex(b *testing.B) {
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
		HasEditKeywords:       true,
		HasDebugKeywords:      true,
		HasRepoKeywords:       true,
		HasArchKeywords:       true,
		HasAgentKeywords:      true,
		HasExtractionKeywords: true,
		HasCodeIdentifiers:    true,
		HasURL:                true,
		HasDiff:               true,
	}
	a := NewAnalyzer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Analyze(feat)
	}
}
