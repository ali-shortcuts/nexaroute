package taskprofile

type TaskType string

const (
	TaskCoding       TaskType = "coding"
	TaskDebugging    TaskType = "debugging"
	TaskEditing      TaskType = "editing"
	TaskRepo         TaskType = "repo"
	TaskArchitecture TaskType = "architecture"
	TaskExtraction   TaskType = "extraction"
	TaskAgent        TaskType = "agent"
	TaskVision       TaskType = "vision"
	TaskReasoning    TaskType = "reasoning"
	TaskGeneral      TaskType = "general"
	TaskUnknown      TaskType = "unknown"
)

func (t TaskType) Valid() bool {
	switch t {
	case TaskCoding, TaskDebugging, TaskEditing, TaskRepo, TaskArchitecture, TaskExtraction, TaskAgent, TaskVision, TaskReasoning, TaskGeneral, TaskUnknown:
		return true
	default:
		return false
	}
}

type Complexity string

const (
	ComplexityTrivial  Complexity = "trivial"
	ComplexityLow      Complexity = "low"
	ComplexityMedium   Complexity = "medium"
	ComplexityHigh     Complexity = "high"
	ComplexityVeryHigh Complexity = "very_high"
)

func (c Complexity) Valid() bool {
	switch c {
	case ComplexityTrivial, ComplexityLow, ComplexityMedium, ComplexityHigh, ComplexityVeryHigh:
		return true
	default:
		return false
	}
}

type ReasonCode string

const (
	ReasonVisionPresent      ReasonCode = "vision_present"
	ReasonReasoningRequested ReasonCode = "reasoning_requested"
	ReasonToolsPresent       ReasonCode = "tools_present"
	ReasonStructuredOutput   ReasonCode = "structured_output"
	ReasonCodeBlock          ReasonCode = "code_block"
	ReasonInlineCode         ReasonCode = "inline_code"
	ReasonStackTrace         ReasonCode = "stack_trace"
	ReasonDiffPresent        ReasonCode = "diff_present"
	ReasonFilePath           ReasonCode = "file_path"
	ReasonURLPresent         ReasonCode = "url_present"
	ReasonEditKeyword        ReasonCode = "edit_keyword"
	ReasonDebugKeyword       ReasonCode = "debug_keyword"
	ReasonRepoKeyword        ReasonCode = "repo_keyword"
	ReasonArchKeyword        ReasonCode = "arch_keyword"
	ReasonAgentKeyword       ReasonCode = "agent_keyword"
	ReasonExtractionKeyword  ReasonCode = "extraction_keyword"
	ReasonCodeIdentifiers    ReasonCode = "code_identifiers"
	ReasonLongContext        ReasonCode = "long_context"
	ReasonMultiTurn          ReasonCode = "multi_turn"
	ReasonSingleTurn         ReasonCode = "single_turn"
	ReasonComplexTools       ReasonCode = "complex_tools"
	ReasonSystemPrompt       ReasonCode = "system_prompt"
	ReasonStreaming          ReasonCode = "streaming"
	ReasonHighImageCount     ReasonCode = "high_image_count"
)
