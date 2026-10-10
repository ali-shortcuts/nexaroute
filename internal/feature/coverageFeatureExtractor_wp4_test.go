package feature

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoverageFeatureExtractorMalformedAndEmptyRequests(t *testing.T) {
	ext := NewExtractor()
	for _, raw := range [][]byte{nil, []byte("{"), []byte(`[1,2]`), []byte(`null`)} {
		f := ext.Extract(raw, ExtractOptions{Model: "  model-x  ", Streaming: true, MaxOutputTokens: 24})
		if f.Protocol != ProtocolUnknown || f.ModelRequested != "model-x" || !f.Streaming || f.MaxOutputTokens != 24 {
			t.Fatalf("base options not preserved for malformed/empty JSON %q: %+v", raw, f)
		}
		if f.HasReasoning || f.HasTools || f.ToolChoicePresent || f.HasVision || f.TotalChars != 0 {
			t.Fatalf("malformed/empty input unexpectedly produced request features: %+v", f)
		}
	}

	f := ext.Extract([]byte(`{}`), ExtractOptions{Protocol: ProtocolOpenAI, MaxOutputTokens: 37, ReasoningKeys: []string{"thinking"}})
	if f.Protocol != ProtocolOpenAI || f.EstimatedPromptTokens != defaultEstimatedTokens || f.EstimatedTotalTokens != defaultEstimatedTokens+37 {
		t.Fatalf("empty object should use bounded default token estimate: %+v", f)
	}
	if f.HasSystemPrompt || f.HasReasoning || f.RelevantTextLength != 0 || f.RelevantTruncated {
		t.Fatalf("empty object should not imply content or capabilities: %+v", f)
	}

	longModel := strings.Repeat("m", maxModelIDLen+20)
	f = ext.Extract([]byte(`{}`), ExtractOptions{Model: longModel})
	if len(f.ModelRequested) != maxModelIDLen {
		t.Fatalf("model id was not bounded: got %d bytes", len(f.ModelRequested))
	}
}

func TestCoverageFeatureExtractorToolChoiceHintsAndShapes(t *testing.T) {
	tests := []struct {
		name     string
		choice   string
		present  bool
		required bool
	}{
		{name: "unknown string", choice: `"future-mode"`, present: true},
		{name: "required substring", choice: `"please-required-now"`, present: true, required: true},
		{name: "any spelling", choice: `"ANY"`, present: true, required: true},
		{name: "none string", choice: `"none"`, present: true},
		{name: "forced function object", choice: `{"type":"function","name":"lookup"}`, present: true, required: true},
		{name: "unknown named object", choice: `{"type":"future","name":"lookup"}`, present: true, required: true},
		{name: "function key without type", choice: `{"function":{"name":"lookup"}}`, present: true, required: true},
		{name: "empty choice object", choice: `{}`, present: true},
		{name: "scalar choice", choice: `true`, present: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewExtractor().Extract([]byte(`{"tool_choice":`+tc.choice+`,"messages":[]}`), ExtractOptions{})
			if f.ToolChoicePresent != tc.present || f.ToolChoice != tc.present || f.ToolChoiceRequired != tc.required {
				t.Fatalf("tool_choice %s: got present=%v alias=%v required=%v", tc.choice, f.ToolChoicePresent, f.ToolChoice, f.ToolChoiceRequired)
			}
		})
	}

	falseValue := false
	f := NewExtractor().Extract([]byte(`{"messages":[],"tool_choice":"required"}`), ExtractOptions{ToolChoiceRequiredHint: &falseValue})
	if !f.ToolChoicePresent || f.ToolChoiceRequired {
		t.Fatalf("explicit false required hint must override body while preserving presence: %+v", f)
	}
	f = NewExtractor().Extract([]byte(`{"messages":[]}`), ExtractOptions{ToolChoiceHint: true})
	if !f.ToolChoicePresent || f.ToolChoiceRequired {
		t.Fatalf("presence-only hint should mark optional tool choice: %+v", f)
	}
	trueValue := true
	f = NewExtractor().Extract([]byte(`{"messages":[]}`), ExtractOptions{ToolChoiceRequiredHint: &trueValue})
	if !f.ToolChoicePresent || !f.ToolChoiceRequired {
		t.Fatalf("required hint should imply presence: %+v", f)
	}
}

func TestCoverageFeatureExtractorCountsHintsAndMultimodalShape(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"developer","content":[{"type":"input_image","image_url":{"url":"https://img.invalid/a"}},{"type":"tool_result","text":"tool result"}]},
			{"role":"user","content":"hello"}
		],
		"tools":[{},{}],"tool_choice":"auto","text":{"format":{"type":"json_schema"}},
		"Thinking":true,"metadata":{"thinking":true}
	}`)
	f := NewExtractor().Extract(raw, ExtractOptions{
		Protocol: ProtocolResponses, VisionType: "input_image", ReasoningKeys: []string{"THINKING"},
		ContentFields: []string{"messages"}, MaxOutputTokens: 60,
	})
	if !f.HasVision || f.VisionImageCount != 1 || !f.HasImageURL || !f.HasToolResult {
		t.Fatalf("multimodal/tool-result shape was not reflected: %+v", f)
	}
	if f.MessageCount != 2 || !f.HasSystemPrompt || f.ToolCount != 2 || !f.HasTools {
		t.Fatalf("message, developer prompt, or actual tool counts are wrong: %+v", f)
	}
	if !f.HasReasoning || !f.StructuredOutput || !f.ToolChoicePresent || f.ToolChoiceRequired {
		t.Fatalf("reasoning/structured-output/tool-choice signals are wrong: %+v", f)
	}
	if f.TotalChars == 0 || f.EstimatedPromptTokens < defaultEstimatedTokens || f.EstimatedTotalTokens != f.EstimatedPromptTokens+60 {
		t.Fatalf("token/character estimates are inconsistent: %+v", f)
	}

	f = NewExtractor().Extract([]byte(`{"tools":[1,2,3],"tool_choice":"required","system":"body system"}`), ExtractOptions{
		Protocol: ProtocolOpenAI, ContentFields: []string{"missing"}, ToolCountHint: 500,
		HasSystemPromptHint: func() *bool { v := false; return &v }(),
	})
	if f.ToolCount != maxToolCount || !f.HasTools || !f.ToolChoiceRequired || f.HasSystemPrompt {
		t.Fatalf("hints should cap tools and control system prompt / required choice: %+v", f)
	}
	if f.EstimatedPromptTokens != defaultEstimatedTokens || f.EstimatedTotalTokens != defaultEstimatedTokens {
		t.Fatalf("absent content should use default estimate even with non-content metadata: %+v", f)
	}

	f = NewExtractor().Extract([]byte(`{"tools":"not-an-array","messages":[]}`), ExtractOptions{ToolCountHint: -1})
	if f.ToolCount != 0 || f.HasTools {
		t.Fatalf("invalid tool shape or nonpositive hint should not fabricate tools: %+v", f)
	}
}

func TestCoverageFeatureExtractorOpenAIRelevantContentAndPrivacy(t *testing.T) {
	raw := []byte(`{
		"system":"See https://docs.invalid/guide",
		"instructions":"Use the repository branch and keep the component stable.",
		"messages":[
			null,
			{"role":"tool","content":"debug this crash src/secret.go"},
			{"role":"function","content":"refactor internal/secret.go"},
			{"role":"assistant","content":["plain text",{"type":"TEXT","text":"Update the component and inspect src/main.go"},{"type":"input_text","text":"func handle() {}"},{"type":"output_text","input_text":"class Widget {}"},{"type":"image_url","image_url":{"url":"https://private.invalid/refactor"}}],"tool_calls":[{"function":{"arguments":"panic: goroutine debug"}}]},
			{"role":"user","content":42}
		]
	}`)
	f := NewExtractor().Extract(raw, ExtractOptions{Protocol: ProtocolUnknown, ContentFields: []string{"messages"}})
	if !f.HasURL || !f.HasRepoKeywords || !f.HasArchKeywords || !f.HasEditKeywords || !f.HasCodeIdentifiers || !f.HasFilePath {
		t.Fatalf("relevant top-level/system and text-part semantics missing: %+v", f)
	}
	if f.HasDebugKeywords || f.HasStackTrace || f.HasCodeBlock {
		t.Fatalf("tool/function results or tool-call payload leaked into lexical features: %+v", f)
	}
	if f.RelevantTextLength == 0 || f.Protocol != ProtocolUnknown {
		t.Fatalf("default OpenAI collector did not retain relevant text/protocol: %+v", f)
	}
}

func TestCoverageFeatureExtractorAnthropicAndResponsesContentShapes(t *testing.T) {
	anthropic := []byte(`{
		"system":[{"type":"text","text":"Design the system architecture."},{"type":"image","text":"ignore src/no.go"},"not-a-block"],
		"messages":[null,{"content":"Summarize the key points as table."},{"content":["skip",{"type":"text","text":"See lib/worker.py"},{"type":"tool_use","name":"ignore","input":"panic: goroutine"},{"type":"tool_result","content":"debug crash"}]}]
	}`)
	a := NewExtractor().Extract(anthropic, ExtractOptions{Protocol: ProtocolAnthropic})
	if !a.HasArchKeywords || !a.HasExtractionKeywords || !a.HasFilePath {
		t.Fatalf("Anthropic string/text-block input signals missing: %+v", a)
	}
	if a.HasDebugKeywords || a.HasCodeBlock {
		t.Fatalf("Anthropic tool/image blocks should not be lexical prompt text: %+v", a)
	}

	responses := []byte(`{
		"instructions":["Refactor the API",{"text":"See pkg/api.rs"},7],
		"input":["plain input",{"type":"input_text","text":"first block"},{"type":"image","text":"ignore debug crash"},
			{"content":"Update the handler"},
			{"content":[{"type":"text","text":"function run() {}"},{"type":"output_text","text":"diff --git a/a b/b"},{"type":"input_image","text":"ignore traceback"},{"type":"tool_call","text":"ignore error"}]},
			{"content":23}]
	}`)
	r := NewExtractor().Extract(responses, ExtractOptions{Protocol: ProtocolResponses})
	if !r.HasEditKeywords || !r.HasFilePath || !r.HasCodeIdentifiers || !r.HasDiff {
		t.Fatalf("Responses instructions/input text shapes not collected: %+v", r)
	}
	if r.HasDebugKeywords || r.HasStackTrace || r.RelevantTextLength == 0 {
		t.Fatalf("Responses non-text payload leaked or text was lost: %+v", r)
	}
}

func TestCoverageFeatureExtractorLexicalEvidenceAndTruncation(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		check func(RequestFeatures) bool
	}{
		{"fenced and inline code", "```go\nfunc x() {}\n``` and `y`", func(f RequestFeatures) bool {
			return f.HasCodeBlock && f.CodeBlockCount == 1 && f.HasInlineCode && f.HasCodeIdentifiers
		}},
		{"unpaired fence still counts", "```", func(f RequestFeatures) bool { return f.HasCodeBlock && f.CodeBlockCount == 1 }},
		{"stack trace source location", "panic: failed\ngoroutine 1 at /main.go:12", func(f RequestFeatures) bool { return f.HasStackTrace && f.HasDebugKeywords }},
		{"diff hunk", "@@ -1 +1 @@", func(f RequestFeatures) bool { return f.HasDiff }},
		{"unified diff", "--- old\n+++ new", func(f RequestFeatures) bool { return f.HasDiff }},
		{"debug phrase without code", "Please debug this for me", func(f RequestFeatures) bool { return f.HasDebugKeywords }},
		{"explicit json output extraction", "Return structured JSON output", func(f RequestFeatures) bool { return f.HasExtractionKeywords }},
		{"design document architecture", "Write a design document", func(f RequestFeatures) bool { return f.HasArchKeywords }},
		{"agent and repo terms", "Use an autonomous agent and commit the branch", func(f RequestFeatures) bool { return f.HasAgentKeywords && f.HasRepoKeywords }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": tc.text}}})
			if err != nil {
				t.Fatal(err)
			}
			f := NewExtractor().Extract(raw, ExtractOptions{})
			if !tc.check(f) {
				t.Fatalf("expected semantic lexical signal for %q: %+v", tc.text, f)
			}
		})
	}

	large := strings.Repeat("a", maxRelevantTextBytes+100)
	f := NewExtractor().Extract([]byte(`{"messages":[{"role":"user","content":"`+large+`"}]}`), ExtractOptions{})
	if !f.RelevantTruncated || f.RelevantTextLength != maxRelevantTextBytes {
		t.Fatalf("relevant text should be byte-bounded: len=%d truncated=%v", f.RelevantTextLength, f.RelevantTruncated)
	}

	tooLarge := strings.Repeat("null,", 100001)
	tooLarge = strings.TrimSuffix(tooLarge, ",")
	f = NewExtractor().Extract([]byte(`{"payload":[`+tooLarge+`]}`), ExtractOptions{ContentFields: []string{"payload"}})
	if !f.TooComplex {
		t.Fatalf("oversized content node list should be rejected by complexity guard: %+v", f)
	}
}
