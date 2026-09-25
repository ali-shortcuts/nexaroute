package taskprofile

import (
	"sort"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
)

// Analyzer is stateless, deterministic, no global locks.
// It consumes only RequestFeatures and produces TaskProfile.
type Analyzer struct{}

func NewAnalyzer() *Analyzer { return &Analyzer{} }

// Analyze classifies the request based on features.
// Deterministic: same features => same profile.
// No raw prompt is accessed.
func (a *Analyzer) Analyze(f feature.RequestFeatures) TaskProfile {
	profile := TaskProfile{
		EstimatedContextTokens: f.EstimatedPromptTokens,
		HasVision:              f.HasVision,
		HasReasoning:           f.HasReasoning,
		HasTools:               f.HasTools,
		ToolCount:              f.ToolCount,
		ImageCount:             f.VisionImageCount,
		MessageCount:           f.MessageCount,
	}

	reasons := make([]ReasonCode, 0, 12)

	// Collect reason codes from features (bounded, deterministic order)
	if f.HasVision {
		reasons = append(reasons, ReasonVisionPresent)
		if f.VisionImageCount > 3 {
			reasons = append(reasons, ReasonHighImageCount)
		}
	}
	if f.HasReasoning {
		reasons = append(reasons, ReasonReasoningRequested)
	}
	if f.HasTools {
		reasons = append(reasons, ReasonToolsPresent)
		if f.ToolCount > 5 {
			reasons = append(reasons, ReasonComplexTools)
		}
	}
	if f.StructuredOutput {
		reasons = append(reasons, ReasonStructuredOutput)
	}
	if f.HasCodeBlock {
		reasons = append(reasons, ReasonCodeBlock)
	}
	if f.HasInlineCode {
		reasons = append(reasons, ReasonInlineCode)
	}
	if f.HasStackTrace {
		reasons = append(reasons, ReasonStackTrace)
	}
	if f.HasDiff {
		reasons = append(reasons, ReasonDiffPresent)
	}
	if f.HasFilePath {
		reasons = append(reasons, ReasonFilePath)
	}
	if f.HasURL {
		reasons = append(reasons, ReasonURLPresent)
	}
	if f.HasEditKeywords {
		reasons = append(reasons, ReasonEditKeyword)
	}
	if f.HasDebugKeywords {
		reasons = append(reasons, ReasonDebugKeyword)
	}
	if f.HasRepoKeywords {
		reasons = append(reasons, ReasonRepoKeyword)
	}
	if f.HasArchKeywords {
		reasons = append(reasons, ReasonArchKeyword)
	}
	if f.HasAgentKeywords {
		reasons = append(reasons, ReasonAgentKeyword)
	}
	if f.HasExtractionKeywords {
		reasons = append(reasons, ReasonExtractionKeyword)
	}
	if f.HasCodeIdentifiers {
		reasons = append(reasons, ReasonCodeIdentifiers)
	}
	if f.HasSystemPrompt {
		reasons = append(reasons, ReasonSystemPrompt)
	}
	if f.Streaming {
		reasons = append(reasons, ReasonStreaming)
	}
	// Context length signals
	if f.EstimatedPromptTokens > 8000 {
		reasons = append(reasons, ReasonLongContext)
	}
	if f.MessageCount > 3 {
		reasons = append(reasons, ReasonMultiTurn)
	} else if f.MessageCount <= 1 {
		reasons = append(reasons, ReasonSingleTurn)
	}

	// Deduplicate and sort for determinism
	reasons = dedupAndSortReasons(reasons)
	profile.ReasonCodes = reasons

	// Task type precedence (deterministic)
	// 1. Debugging: stack trace OR (debug keywords + code)
	// 2. Editing: diff OR (edit keywords + (code or file path))
	// 3. Repo: repo keywords + file path or code
	// 4. Architecture: arch keywords
	// 5. Coding: code block or code identifiers or file path with code context
	// 6. Extraction: extraction keywords
	// 7. Agent: agent keywords OR (tools + multi-turn)
	// 8. Vision: vision present and no stronger code signals
	// 9. Reasoning: reasoning requested and high complexity
	// 10. General fallback
	profile.Type = classifyType(f)

	// Complexity based on tokens, message count, tools, images
	profile.Complexity = classifyComplexity(f)

	// Confidence based on number of matching signals
	profile.Confidence = computeConfidence(f, profile.Type)

	return profile
}

func classifyType(f feature.RequestFeatures) TaskType {
	// Debugging has highest precedence when stack trace present
	if f.HasStackTrace {
		return TaskDebugging
	}
	if f.HasDebugKeywords && (f.HasCodeBlock || f.HasCodeIdentifiers || f.HasFilePath) {
		return TaskDebugging
	}
	// Editing: diff is strong signal
	if f.HasDiff {
		return TaskEditing
	}
	if f.HasEditKeywords && (f.HasCodeBlock || f.HasFilePath || f.HasCodeIdentifiers) {
		return TaskEditing
	}
	// Repo
	if f.HasRepoKeywords && (f.HasFilePath || f.HasCodeBlock) {
		return TaskRepo
	}
	// Architecture
	if f.HasArchKeywords {
		return TaskArchitecture
	}
	// Coding
	if f.HasCodeBlock || f.HasCodeIdentifiers {
		return TaskCoding
	}
	if f.HasFilePath && (f.HasEditKeywords || f.HasDebugKeywords || f.HasRepoKeywords) {
		// file path alone not enough for coding unless combined; but if file path + code identifiers already handled
		// fallback to general if only file path
	}
	// Extraction
	if f.HasExtractionKeywords {
		return TaskExtraction
	}
	// Agent
	if f.HasAgentKeywords {
		return TaskAgent
	}
	if f.HasTools && f.MessageCount > 3 {
		return TaskAgent
	}
	// Vision: if vision present and no code signals, classify as vision
	if f.HasVision && !f.HasCodeBlock && !f.HasCodeIdentifiers && !f.HasStackTrace && !f.HasDiff {
		return TaskVision
	}
	// Reasoning: if reasoning requested and complexity high or code present
	if f.HasReasoning && f.EstimatedPromptTokens > 2000 {
		return TaskReasoning
	}
	// General fallback
	return TaskGeneral
}

func classifyComplexity(f feature.RequestFeatures) Complexity {
	tokens := f.EstimatedPromptTokens
	// Adjust for tools and images
	score := tokens
	score += f.ToolCount * 200
	score += f.VisionImageCount * 500
	score += f.MessageCount * 50
	if f.HasCodeBlock {
		score += 500
	}
	if f.HasDiff {
		score += 800
	}
	if f.HasStackTrace {
		score += 600
	}

	switch {
	case score < 200:
		return ComplexityTrivial
	case score < 1000:
		return ComplexityLow
	case score < 4000:
		return ComplexityMedium
	case score < 16000:
		return ComplexityHigh
	default:
		return ComplexityVeryHigh
	}
}

func computeConfidence(f feature.RequestFeatures, t TaskType) float64 {
	// Base confidence
	conf := 0.5

	// Increase based on matching signals for the chosen type
	switch t {
	case TaskDebugging:
		if f.HasStackTrace {
			conf += 0.4
		}
		if f.HasDebugKeywords {
			conf += 0.1
		}
		if f.HasCodeBlock {
			conf += 0.1
		}
	case TaskEditing:
		if f.HasDiff {
			conf += 0.4
		}
		if f.HasEditKeywords {
			conf += 0.15
		}
		if f.HasFilePath {
			conf += 0.1
		}
	case TaskRepo:
		if f.HasRepoKeywords {
			conf += 0.3
		}
		if f.HasFilePath {
			conf += 0.2
		}
	case TaskArchitecture:
		if f.HasArchKeywords {
			conf += 0.4
		}
	case TaskCoding:
		if f.HasCodeBlock {
			conf += 0.3
		}
		if f.HasCodeIdentifiers {
			conf += 0.2
		}
		if f.HasFilePath {
			conf += 0.1
		}
	case TaskExtraction:
		if f.HasExtractionKeywords {
			conf += 0.3
		}
	case TaskAgent:
		if f.HasAgentKeywords {
			conf += 0.3
		}
		if f.HasTools {
			conf += 0.2
		}
		if f.MessageCount > 3 {
			conf += 0.1
		}
	case TaskVision:
		if f.HasVision {
			conf += 0.4
		}
		if f.VisionImageCount > 1 {
			conf += 0.1
		}
	case TaskReasoning:
		if f.HasReasoning {
			conf += 0.3
		}
		if f.EstimatedPromptTokens > 2000 {
			conf += 0.2
		}
	case TaskGeneral:
		// General is low confidence if many signals present, high if few
		signalCount := 0
		if f.HasCodeBlock {
			signalCount++
		}
		if f.HasStackTrace {
			signalCount++
		}
		if f.HasDiff {
			signalCount++
		}
		if f.HasFilePath {
			signalCount++
		}
		if f.HasEditKeywords {
			signalCount++
		}
		if f.HasDebugKeywords {
			signalCount++
		}
		if f.HasRepoKeywords {
			signalCount++
		}
		if f.HasArchKeywords {
			signalCount++
		}
		if f.HasAgentKeywords {
			signalCount++
		}
		if f.HasExtractionKeywords {
			signalCount++
		}
		if signalCount == 0 {
			conf = 0.8
		} else {
			conf = 0.5 - float64(signalCount)*0.05
			if conf < 0.3 {
				conf = 0.3
			}
		}
	}

	// Adjust for TooComplex: lower confidence
	if f.TooComplex {
		conf -= 0.1
	}
	if f.RelevantTruncated {
		conf -= 0.05
	}

	if conf > 1.0 {
		conf = 1.0
	}
	if conf < 0.1 {
		conf = 0.1
	}
	return conf
}

func dedupAndSortReasons(in []ReasonCode) []ReasonCode {
	if len(in) == 0 {
		return in
	}
	seen := make(map[ReasonCode]struct{}, len(in))
	for _, r := range in {
		seen[r] = struct{}{}
	}
	out := make([]ReasonCode, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
