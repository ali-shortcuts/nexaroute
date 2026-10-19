package taskprofile

import "github.com/ali-shortcuts/nexaroute/internal/feature"

// Rule is one row of the inspectable task-classification decision table.
//
// The table mirrors the precedence chain implemented by Analyze, in order:
// the first rule whose condition holds wins. It exists so an operator can
// read exactly which signal combination produces which task class without
// reading the classifier source.
//
// Each rule carries a Probe: a canonical RequestFeatures value that triggers
// exactly that rule. The probe set is executable documentation —
// TestRuleTable_ProbesClassify keeps table and code in lockstep, so the
// table can never silently drift from the implementation.
type Rule struct {
	// Name is the stable rule identifier (e.g. "stack-trace->debugging").
	Name string
	// When is the human-readable condition, for the operator.
	When string
	// Signals lists the feature flags this rule examines.
	Signals []string
	// Produces is the task class when the condition holds.
	Produces TaskType
	// Probe is the canonical feature set that triggers this rule.
	Probe feature.RequestFeatures
}

// RuleTable returns the ordered decision table for RuleVersion.
// Rules are evaluated top-down; the first matching rule wins.
func RuleTable() []Rule {
	return []Rule{
		{
			Name:     "too-complex->unknown",
			When:     "the request JSON exceeded the bounded inspection budget",
			Signals:  []string{"too_complex"},
			Produces: TaskUnknown,
			Probe:    feature.RequestFeatures{TooComplex: true},
		},
		{
			Name:     "stack-trace->debugging",
			When:     "a stack trace is present",
			Signals:  []string{"has_stack_trace"},
			Produces: TaskDebugging,
			Probe:    feature.RequestFeatures{HasStackTrace: true},
		},
		{
			Name:     "debug-keywords+code->debugging",
			When:     "debug keywords appear together with code (block, identifiers, or file path)",
			Signals:  []string{"has_debug_keywords", "has_code_block", "has_code_identifiers", "has_file_path"},
			Produces: TaskDebugging,
			Probe: feature.RequestFeatures{
				HasDebugKeywords: true, HasCodeBlock: true,
			},
		},
		{
			Name:     "diff->code-edit",
			When:     "a unified diff is present",
			Signals:  []string{"has_diff"},
			Produces: TaskCodeEdit,
			Probe:    feature.RequestFeatures{HasDiff: true},
		},
		{
			Name:     "edit-keywords+code->code-edit",
			When:     "edit keywords appear with code context (block, identifiers, or a non-URL file path)",
			Signals:  []string{"has_edit_keywords", "has_code_block", "has_code_identifiers", "has_file_path", "has_url"},
			Produces: TaskCodeEdit,
			Probe: feature.RequestFeatures{
				HasEditKeywords: true, HasCodeIdentifiers: true,
			},
		},
		{
			Name:     "repo-keywords->repository-analysis",
			When:     "repository keywords appear with a file path or code block",
			Signals:  []string{"has_repo_keywords", "has_file_path", "has_code_block"},
			Produces: TaskRepositoryAnalysis,
			Probe: feature.RequestFeatures{
				HasRepoKeywords: true, HasFilePath: true,
			},
		},
		{
			Name:     "arch-keywords->architecture-reasoning",
			When:     "architecture keywords are present",
			Signals:  []string{"has_arch_keywords"},
			Produces: TaskArchitectureReasoning,
			Probe:    feature.RequestFeatures{HasArchKeywords: true},
		},
		{
			Name:     "code->coding",
			When:     "a code block or code identifiers are present",
			Signals:  []string{"has_code_block", "has_code_identifiers"},
			Produces: TaskCoding,
			Probe:    feature.RequestFeatures{HasCodeBlock: true},
		},
		{
			Name:     "extraction-keywords->data-extraction",
			When:     "extraction keywords are present and no stronger code signal fired",
			Signals:  []string{"has_extraction_keywords"},
			Produces: TaskDataExtraction,
			Probe:    feature.RequestFeatures{HasExtractionKeywords: true},
		},
		{
			Name:     "agent-keywords->agentic-task",
			When:     "agentic keywords are present, or tools are used across a multi-turn session",
			Signals:  []string{"has_agent_keywords", "has_tools", "message_count"},
			Produces: TaskAgenticTask,
			Probe:    feature.RequestFeatures{HasAgentKeywords: true},
		},
		{
			Name:     "tools->tool-use",
			When:     "tools are present/required in a short session without agentic signals",
			Signals:  []string{"has_tools", "tool_count", "tool_choice_required", "message_count"},
			Produces: TaskToolUse,
			Probe: feature.RequestFeatures{
				HasTools: true, ToolCount: 2, MessageCount: 1,
			},
		},
		{
			Name:     "vision->vision",
			When:     "image parts are present and no stronger code signal fired",
			Signals:  []string{"has_vision", "vision_image_count"},
			Produces: TaskVision,
			Probe: feature.RequestFeatures{
				HasVision: true, VisionImageCount: 1,
			},
		},
		{
			Name:     "structured-output->structured-output",
			When:     "structured output was requested and no stronger signal fired",
			Signals:  []string{"structured_output"},
			Produces: TaskStructuredOutput,
			Probe:    feature.RequestFeatures{StructuredOutput: true},
		},
		{
			Name:     "long-context->long-context",
			When:     "estimated prompt exceeds 8000 tokens and no stronger signal fired",
			Signals:  []string{"estimated_prompt_tokens"},
			Produces: TaskLongContext,
			Probe:    feature.RequestFeatures{EstimatedPromptTokens: 9000},
		},
		{
			Name:     "reasoning->deep-reasoning",
			When:     "reasoning was requested on a substantial prompt (>2000 tokens)",
			Signals:  []string{"has_reasoning", "estimated_prompt_tokens"},
			Produces: TaskDeepReasoning,
			Probe: feature.RequestFeatures{
				HasReasoning: true, EstimatedPromptTokens: 3000,
			},
		},
		{
			Name:     "trivial->simple-chat",
			When:     "single turn, small prompt, no code/tools/vision",
			Signals:  []string{"message_count", "estimated_prompt_tokens"},
			Produces: TaskSimpleChat,
			Probe: feature.RequestFeatures{
				MessageCount: 1, EstimatedPromptTokens: 100,
			},
		},
		{
			Name:     "background-chore->background",
			When:     "background keyword signal with no code/tools/stack-trace/diff/vision veto (#421 item 3)",
			Signals:  []string{"HasBackgroundKeywords"},
			Produces: TaskBackground,
			Probe: feature.RequestFeatures{
				HasBackgroundKeywords: true, MessageCount: 1, EstimatedPromptTokens: 120,
			},
		},
		{
			Name:     "fallback->general",
			When:     "no rule above matched",
			Signals:  []string{},
			Produces: TaskGeneral,
			Probe: feature.RequestFeatures{
				MessageCount: 2, EstimatedPromptTokens: 1000,
			},
		},
	}
}
