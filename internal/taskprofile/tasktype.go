package taskprofile

type TaskType string

const (
	// Primary task types matching spec vocabulary
	TaskSimpleChat            TaskType = "simple_chat"
	TaskCoding                TaskType = "coding"
	TaskCodeEdit              TaskType = "code_edit"
	TaskDebugging             TaskType = "debugging"
	TaskRepositoryAnalysis    TaskType = "repository_analysis"
	TaskArchitectureReasoning TaskType = "architecture_reasoning"
	TaskDeepReasoning         TaskType = "deep_reasoning"
	TaskToolUse               TaskType = "tool_use"
	TaskAgenticTask           TaskType = "agentic_task"
	TaskLongContext           TaskType = "long_context"
	TaskVision                TaskType = "vision"
	TaskStructuredOutput      TaskType = "structured_output"
	TaskDataExtraction        TaskType = "data_extraction"
	TaskGeneral               TaskType = "general"
	TaskUnknown               TaskType = "unknown"

	// Backward compatibility aliases for previous vocabulary
	TaskCodingAlias    = TaskCoding
	TaskDebuggingAlias = TaskDebugging
	TaskEditing        = TaskCodeEdit // old "editing" -> code_edit
	TaskRepo           = TaskRepositoryAnalysis
	TaskArchitecture   = TaskArchitectureReasoning
	TaskExtraction     = TaskDataExtraction
	TaskAgent          = TaskAgenticTask
	TaskReasoning      = TaskDeepReasoning
	TaskVisionAlias    = TaskVision
)

// AllTaskTypes returns the canonical list of task types for metrics cardinality checks
func AllTaskTypes() []TaskType {
	return []TaskType{
		TaskSimpleChat,
		TaskCoding,
		TaskCodeEdit,
		TaskDebugging,
		TaskRepositoryAnalysis,
		TaskArchitectureReasoning,
		TaskDeepReasoning,
		TaskToolUse,
		TaskAgenticTask,
		TaskLongContext,
		TaskVision,
		TaskStructuredOutput,
		TaskDataExtraction,
		TaskGeneral,
		TaskUnknown,
	}
}

func (t TaskType) Valid() bool {
	switch t {
	case TaskSimpleChat, TaskCoding, TaskCodeEdit, TaskDebugging, TaskRepositoryAnalysis, TaskArchitectureReasoning, TaskDeepReasoning, TaskToolUse, TaskAgenticTask, TaskLongContext, TaskVision, TaskStructuredOutput, TaskDataExtraction, TaskGeneral, TaskUnknown:
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

func AllComplexities() []Complexity {
	return []Complexity{
		ComplexityTrivial,
		ComplexityLow,
		ComplexityMedium,
		ComplexityHigh,
		ComplexityVeryHigh,
	}
}

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
	ReasonToolChoiceRequired ReasonCode = "tool_choice_required"
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
	ReasonSimpleChat         ReasonCode = "simple_chat"
)
