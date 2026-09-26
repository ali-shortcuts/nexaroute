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
		// Secondary requirement flags
		RequiresVision:           f.HasVision,
		RequiresReasoning:        f.HasReasoning,
		RequiresTools:            f.HasTools,
		RequiresStructuredOutput: f.StructuredOutput,
		RequiresLongContext:      f.EstimatedPromptTokens > 8000,
		ToolChoiceRequired:       f.ToolChoiceRequired,
	}

	reasons := make([]ReasonCode, 0, 16)

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
	if f.ToolChoiceRequired {
		reasons = append(reasons, ReasonToolChoiceRequired)
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
	if f.EstimatedPromptTokens > 8000 {
		reasons = append(reasons, ReasonLongContext)
	}
	if f.MessageCount > 3 {
		reasons = append(reasons, ReasonMultiTurn)
	} else if f.MessageCount <= 1 {
		reasons = append(reasons, ReasonSingleTurn)
	}
	// Simple chat signal
	if f.MessageCount <= 1 && !f.HasCodeBlock && !f.HasTools && !f.HasVision && !f.HasStackTrace && !f.HasDiff && !f.HasFilePath {
		reasons = append(reasons, ReasonSimpleChat)
	}

	reasons = dedupAndSortReasons(reasons)
	profile.ReasonCodes = reasons

	// Handle TooComplex -> UNKNOWN with low confidence
	if f.TooComplex {
		profile.Type = TaskUnknown
		profile.Complexity = ComplexityMedium
		profile.Confidence = 0.1
		return profile
	}

	profile.Type = classifyType(f)
	profile.Complexity = classifyComplexity(f)
	profile.Confidence = computeConfidence(f, profile.Type)

	return profile
}

func classifyType(f feature.RequestFeatures) TaskType {
	// Highest precedence: debugging with stack trace
	if f.HasStackTrace {
		return TaskDebugging
	}
	if f.HasDebugKeywords && (f.HasCodeBlock || f.HasCodeIdentifiers || f.HasFilePath) {
		return TaskDebugging
	}
	// Code edit: diff is strong signal
	if f.HasDiff {
		return TaskCodeEdit
	}
	if f.HasEditKeywords {
		// Strong edit signals: require code context
		// - code block or identifiers always sufficient
		// - file path sufficient only when not just a URL containing a path
		if f.HasCodeBlock || f.HasCodeIdentifiers {
			return TaskCodeEdit
		}
		if f.HasFilePath && !f.HasURL {
			return TaskCodeEdit
		}
	}
	// Repository analysis
	if f.HasRepoKeywords && (f.HasFilePath || f.HasCodeBlock) {
		return TaskRepositoryAnalysis
	}
	// Architecture reasoning
	if f.HasArchKeywords {
		return TaskArchitectureReasoning
	}
	// Coding
	if f.HasCodeBlock || f.HasCodeIdentifiers {
		return TaskCoding
	}
	// Data extraction
	if f.HasExtractionKeywords {
		return TaskDataExtraction
	}
	// Agentic task
	if f.HasAgentKeywords {
		return TaskAgenticTask
	}
	if f.HasTools && f.MessageCount > 3 {
		return TaskAgenticTask
	}
	// Tool use: tools present and required or tool choice required
	if f.HasTools && (f.ToolChoiceRequired || f.ToolCount > 0) {
		// Distinguish tool_use from agentic_task: agentic already handled
		// If single turn with tools, it's tool_use
		if f.MessageCount <= 2 {
			return TaskToolUse
		}
		// Multi-turn with tools but without agent keywords -> still tool_use if not agentic
		return TaskToolUse
	}
	// Vision: if vision present and no stronger code signals
	if f.HasVision && !f.HasCodeBlock && !f.HasCodeIdentifiers && !f.HasStackTrace && !f.HasDiff {
		return TaskVision
	}
	// Structured output as primary only when it's dominant and no other strong signals
	if f.StructuredOutput && !f.HasCodeBlock && !f.HasStackTrace && !f.HasDiff && !f.HasFilePath && !f.HasTools && !f.HasVision {
		return TaskStructuredOutput
	}
	// Long context as primary only when very long and no other strong signals
	if f.EstimatedPromptTokens > 8000 && !f.HasCodeBlock && !f.HasStackTrace && !f.HasDiff && !f.HasFilePath && !f.HasTools && !f.HasVision {
		return TaskLongContext
	}
	// Deep reasoning
	if f.HasReasoning && f.EstimatedPromptTokens > 2000 {
		return TaskDeepReasoning
	}
	// Simple chat: trivial/low, single turn, no code/tools/vision
	if f.MessageCount <= 1 && !f.HasCodeBlock && !f.HasCodeIdentifiers && !f.HasTools && !f.HasVision && !f.HasStackTrace && !f.HasDiff && !f.HasFilePath {
		// Further check: if estimated tokens small
		if f.EstimatedPromptTokens < 500 {
			return TaskSimpleChat
		}
		return TaskGeneral
	}
	// General fallback
	return TaskGeneral
}

func classifyComplexity(f feature.RequestFeatures) Complexity {
	score := f.EstimatedPromptTokens
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
	conf := 0.5

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
	case TaskCodeEdit:
		if f.HasDiff {
			conf += 0.4
		}
		if f.HasEditKeywords {
			conf += 0.15
		}
		if f.HasFilePath {
			conf += 0.1
		}
	case TaskRepositoryAnalysis:
		if f.HasRepoKeywords {
			conf += 0.3
		}
		if f.HasFilePath {
			conf += 0.2
		}
	case TaskArchitectureReasoning:
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
	case TaskDataExtraction:
		if f.HasExtractionKeywords {
			conf += 0.3
		}
	case TaskAgenticTask:
		if f.HasAgentKeywords {
			conf += 0.3
		}
		if f.HasTools {
			conf += 0.2
		}
		if f.MessageCount > 3 {
			conf += 0.1
		}
	case TaskToolUse:
		if f.HasTools {
			conf += 0.3
		}
		if f.ToolChoiceRequired {
			conf += 0.2
		}
		if f.ToolCount > 1 {
			conf += 0.1
		}
	case TaskVision:
		if f.HasVision {
			conf += 0.4
		}
		if f.VisionImageCount > 1 {
			conf += 0.1
		}
	case TaskDeepReasoning:
		if f.HasReasoning {
			conf += 0.3
		}
		if f.EstimatedPromptTokens > 2000 {
			conf += 0.2
		}
	case TaskSimpleChat:
		// High confidence if few signals
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
		if f.HasTools {
			signalCount++
		}
		if f.HasVision {
			signalCount++
		}
		if signalCount == 0 {
			conf = 0.85
		} else {
			conf = 0.4
		}
	case TaskGeneral:
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
	case TaskLongContext:
		if f.EstimatedPromptTokens > 8000 {
			conf += 0.3
		}
	case TaskStructuredOutput:
		if f.StructuredOutput {
			conf += 0.4
		}
	case TaskUnknown:
		conf = 0.1
	}

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
