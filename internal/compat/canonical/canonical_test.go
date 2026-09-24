package canonical

import (
	"encoding/json"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func TestAnthropicRoundTrip(t *testing.T) {
	in := core.AnthropicRequest{
		Model:     "m",
		MaxTokens: 16,
		System:    json.RawMessage(`"Be helpful"`),
		Messages: []core.AnthMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"Hi"}]`)},
		},
		Tools: []core.AnthTool{{Name: "get_time", InputSchema: map[string]any{"type": "object"}}},
	}
	canon, err := FromAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if canon.System != "Be helpful" || !canon.HasTools() || len(canon.Messages) != 2 {
		t.Fatalf("decode wrong: %+v", canon)
	}
	req := canon.Requirements()
	found := false
	for _, r := range req.Required {
		if r == CapTools {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools must be required: %+v", req)
	}
	// Encode to OpenAI and decode back.
	openai := canon.ToOpenAIRequest("m2")
	if len(openai.Tools) != 1 || openai.Tools[0].Function.Name != "get_time" {
		t.Fatalf("tool lost: %+v", openai.Tools)
	}
	back, err := FromOpenAIRequest(openai)
	if err != nil {
		t.Fatal(err)
	}
	if !back.HasTools() || back.System != "Be helpful" {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestOpenAIToolResultMapping(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "m",
		Messages: []core.OpenAIMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []core.OpenAIToolCall{{ID: "c1", Type: "function", Function: core.OpenAIFunctionCall{Name: "t", Arguments: `{"a":1}`}}}},
			{Role: "tool", ToolCallID: "c1", Content: "done"},
		},
		Tools: []core.OpenAITool{{Type: "function", Function: core.OpenAIFunction{Name: "t"}}},
	}
	canon, err := FromOpenAIRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	hasCall, hasResult := false, false
	for _, m := range canon.Messages {
		if m.HasToolCalls() {
			hasCall = true
		}
		if m.HasToolResults() {
			hasResult = true
		}
	}
	if !hasCall || !hasResult {
		t.Fatalf("tool loop parts missing: %+v", canon.Messages)
	}
}

func TestRequirementsOptionalSampling(t *testing.T) {
	temp := 0.7
	r := Request{
		Model: "m", Temperature: &temp,
		Messages: []Message{{Role: "user", Parts: []ContentPart{{Kind: ContentText, Text: "hi"}}}},
	}
	req := r.Requirements()
	for _, need := range req.Required {
		if need == CapTemperature {
			t.Fatalf("temperature must be optional, not required: %+v", req)
		}
	}
	found := false
	for _, o := range req.Optional {
		if o == CapTemperature {
			found = true
		}
	}
	if !found {
		t.Fatalf("temperature must be optional: %+v", req)
	}
}

func TestClaudeCodeRequirements(t *testing.T) {
	agent := RequirementsForClaudeCode(true, true)
	has := func(list []CapabilityName, want CapabilityName) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	if !has(agent.Required, CapTools) || !has(agent.Required, CapStreaming) {
		t.Fatalf("agent mode must require tools+streaming: %+v", agent)
	}
	chat := RequirementsForClaudeCode(false, false)
	if has(chat.Required, CapTools) {
		t.Fatalf("basic chat must not require tools: %+v", chat)
	}
}

func TestFromResponses(t *testing.T) {
	o := core.OpenAIResponse{
		Model:   "m",
		Choices: []core.OpenAIChoice{{Message: core.OpenAIMessage{Content: "hello"}}},
		Usage:   core.OpenAIUsage{PromptTokens: 3, CompletionTokens: 5},
	}
	resp := FromOpenAIResponse(o)
	if resp.Text != "hello" || resp.InputTokens != 3 || resp.OutputTokens != 5 {
		t.Fatalf("wrong: %+v", resp)
	}
	a := core.AnthResponse{
		Content: []core.AnthContentBlock{{Type: "text", Text: "hi"}},
		Usage:   core.AnthUsage{InputTokens: 2, OutputTokens: 4},
	}
	resp = FromAnthropicResponse(a)
	if resp.Text != "hi" || resp.InputTokens != 2 {
		t.Fatalf("wrong: %+v", resp)
	}
}

func TestGeminiConverters(t *testing.T) {
	r := Request{
		Model: "m", System: "sys",
		Messages: []Message{{Role: "user", Parts: []ContentPart{{Kind: ContentText, Text: "hi"}}}},
		Tools:    []ToolDef{{Name: "t"}},
	}
	g := r.ToGeminiRequest()
	if g.SystemInstruction == nil || len(g.Contents) != 1 || len(g.Tools) != 1 {
		t.Fatalf("wrong gemini request: %+v", g)
	}
	resp, err := FromGeminiResponse(GeminiResponse{
		Candidates: []GeminiCandidate{{Content: GeminiContent{Parts: []GeminiPart{{Text: "yo"}}}, FinishReason: "STOP"}},
		Usage:      GeminiUsage{PromptTokenCount: 1, CandidatesTokenCount: 2},
	})
	if err != nil || resp.Text != "yo" || resp.StopReason != "stop" {
		t.Fatalf("wrong gemini response: %+v %v", resp, err)
	}
	if _, err := FromGeminiResponse(GeminiResponse{}); err == nil {
		t.Fatalf("empty candidates must error")
	}
}
