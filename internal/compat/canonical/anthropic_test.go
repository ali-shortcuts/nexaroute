package canonical

import (
	"encoding/json"
	"testing"
)

func anthBlocks(t *testing.T, raw json.RawMessage) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("message content is not blocks: %v", err)
	}
	return out
}

func TestToAnthropicRequestAlternation(t *testing.T) {
	in := Request{
		Model: "m",
		Messages: []Message{
			{Role: "assistant", Parts: []ContentPart{{Kind: ContentText, Text: "hi"}}},
			{Role: "user", Parts: []ContentPart{{Kind: ContentText, Text: "a"}}},
			{Role: "user", Parts: []ContentPart{{Kind: ContentText, Text: "b"}}},
			{Role: "assistant", Parts: []ContentPart{{Kind: ContentText, Text: "c"}}},
		},
	}
	out := in.ToAnthropicRequest("up")
	if out.Model != "up" {
		t.Fatalf("model = %q", out.Model)
	}
	if out.MaxTokens != defaultAnthropicMaxTokens {
		t.Fatalf("max_tokens = %d", out.MaxTokens)
	}
	roles := []string{}
	for _, m := range out.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "user", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v", roles)
		}
	}
	merged := anthBlocks(t, out.Messages[2].Content)
	if len(merged) != 2 || merged[0]["text"] != "a" || merged[1]["text"] != "b" {
		t.Fatalf("merged user blocks = %v", merged)
	}
	placeholder := anthBlocks(t, out.Messages[0].Content)
	if len(placeholder) != 1 || placeholder[0]["text"] != emptyAnthropicText {
		t.Fatalf("first message = %v", placeholder)
	}
}

func TestToAnthropicRequestToolsAndChoice(t *testing.T) {
	in := Request{
		Model:     "m",
		MaxTokens: 64,
		Messages:  []Message{{Role: "user", Parts: []ContentPart{{Kind: ContentText, Text: "hi"}}}},
		Tools: []ToolDef{
			{Name: "a", Description: "A", Parameters: map[string]any{"properties": map[string]any{}}},
			{Name: "b"},
		},
		ToolChoice: "required",
	}
	out := in.ToAnthropicRequest("up")
	if out.MaxTokens != 64 {
		t.Fatalf("max_tokens = %d", out.MaxTokens)
	}
	if len(out.Tools) != 2 || out.Tools[0].Name != "a" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	if out.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("schema type not filled: %v", out.Tools[0].InputSchema)
	}
	if out.Tools[1].InputSchema["type"] != "object" {
		t.Fatalf("default schema missing: %v", out.Tools[1].InputSchema)
	}
	tc, _ := out.ToolChoice.(map[string]any)
	if tc["type"] != "any" {
		t.Fatalf("tool_choice = %v", out.ToolChoice)
	}
	for choice, want := range map[string]string{"auto": "auto", "none": "none", "named:x": "tool", "": ""} {
		in.ToolChoice = choice
		got := in.ToAnthropicRequest("up").ToolChoice
		if want == "" {
			if got != nil {
				t.Fatalf("choice %q: got %v", choice, got)
			}
			continue
		}
		m, _ := got.(map[string]any)
		if m["type"] != want {
			t.Fatalf("choice %q: got %v", choice, got)
		}
		if want == "tool" && m["name"] != "x" {
			t.Fatalf("named choice: got %v", got)
		}
	}
}

func TestToAnthropicRequestThinking(t *testing.T) {
	mk := func() Request {
		return Request{
			Model:     "m",
			MaxTokens: 100,
			Reasoning: &Reasoning{Enabled: true, Effort: "high"},
			Messages:  []Message{{Role: "user", Parts: []ContentPart{{Kind: ContentText, Text: "hi"}}}},
		}
	}
	out := mk().ToAnthropicRequest("up")
	var th struct {
		Type   string `json:"type"`
		Budget int    `json:"budget_tokens"`
	}
	if err := json.Unmarshal(out.Thinking, &th); err != nil || th.Type != "enabled" || th.Budget != 16384 {
		t.Fatalf("thinking = %s err=%v", out.Thinking, err)
	}
	if out.MaxTokens != 16384+1024 {
		t.Fatalf("max_tokens should exceed budget: %d", out.MaxTokens)
	}
	// Explicit budget wins over effort mapping.
	explicit := mk()
	explicit.Reasoning = &Reasoning{Enabled: true, Budget: 3000}
	explicit.MaxTokens = 8192
	out2 := explicit.ToAnthropicRequest("up")
	if err := json.Unmarshal(out2.Thinking, &th); err != nil || th.Budget != 3000 {
		t.Fatalf("explicit thinking = %s", out2.Thinking)
	}
	if out2.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d", out2.MaxTokens)
	}
	// Thinking is dropped over tool_use history (unsigned replay 400s).
	withTools := mk()
	withTools.MaxTokens = 20000
	withTools.Messages = append(withTools.Messages, Message{Role: "assistant", Parts: []ContentPart{
		{Kind: ContentToolCall, ToolCallID: "t1", ToolName: "a", ToolInput: map[string]any{}},
	}})
	out3 := withTools.ToAnthropicRequest("up")
	if len(out3.Thinking) != 0 {
		t.Fatalf("thinking should be dropped over tool_use: %s", out3.Thinking)
	}
}

func TestToAnthropicRequestImages(t *testing.T) {
	in := Request{
		Model: "m",
		Messages: []Message{{Role: "user", Parts: []ContentPart{
			{Kind: ContentImage, ImageURL: "data:image/jpeg;base64,QUJD"},
			{Kind: ContentImage, ImageURL: "https://example.com/x.png"},
			{Kind: ContentImage, ImageURL: "data:oops"},
		}}},
	}
	out := in.ToAnthropicRequest("up")
	blocks := anthBlocks(t, out.Messages[0].Content)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v", blocks)
	}
	src, _ := blocks[0]["source"].(map[string]any)
	if blocks[0]["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/jpeg" || src["data"] != "QUJD" {
		t.Fatalf("base64 source = %v", blocks[0])
	}
	src2, _ := blocks[1]["source"].(map[string]any)
	if src2["type"] != "url" || src2["url"] != "https://example.com/x.png" {
		t.Fatalf("url source = %v", blocks[1])
	}
}

func TestToAnthropicRequestSystemAndToolResults(t *testing.T) {
	in := Request{
		Model:  "m",
		System: "base",
		Messages: []Message{
			{Role: "system", Parts: []ContentPart{{Kind: ContentText, Text: "extra"}}},
			{Role: "assistant", Parts: []ContentPart{
				{Kind: ContentToolCall, ToolCallID: "t1", ToolName: "a", ToolInput: map[string]any{"x": 1}},
			}},
			{Role: "tool", Parts: []ContentPart{
				{Kind: ContentToolResult, ToolUseID: "t1", ToolContent: "ok"},
				{Kind: ContentToolResult, ToolUseID: "t2", ToolContent: "bad", ToolError: true},
			}},
			{Role: "tool", Parts: []ContentPart{
				{Kind: ContentToolResult, ToolUseID: "t3", ToolContent: "more"},
			}},
			{Role: "weird", Parts: []ContentPart{{Kind: ContentText, Text: "falls back to user"}}},
			{Role: "assistant", Parts: []ContentPart{{Kind: ContentReasoning, Text: "dropped"}}},
		},
	}
	out := in.ToAnthropicRequest("up")
	var sys string
	if err := json.Unmarshal(out.System, &sys); err != nil || sys != "base\nextra" {
		t.Fatalf("system = %s err=%v", out.System, err)
	}
	if len(out.Messages) != 4 {
		t.Fatalf("messages = %d", len(out.Messages))
	}
	first := anthBlocks(t, out.Messages[0].Content)
	if out.Messages[0].Role != "user" || len(first) != 1 || first[0]["text"] != emptyAnthropicText {
		t.Fatalf("first = %s %v", out.Messages[0].Role, first)
	}
	use := anthBlocks(t, out.Messages[1].Content)
	if out.Messages[1].Role != "assistant" || len(use) != 1 || use[0]["type"] != "tool_use" || use[0]["name"] != "a" {
		t.Fatalf("tool_use = %v", use)
	}
	results := anthBlocks(t, out.Messages[2].Content)
	if out.Messages[2].Role != "user" || len(results) != 4 {
		t.Fatalf("tool results = %v", results)
	}
	if results[0]["tool_use_id"] != "t1" || results[1]["is_error"] != true || results[2]["tool_use_id"] != "t3" {
		t.Fatalf("tool results = %v", results)
	}
	if results[3]["text"] != "falls back to user" {
		t.Fatalf("merged user text = %v", results[3])
	}
	last := anthBlocks(t, out.Messages[3].Content)
	if out.Messages[3].Role != "assistant" || len(last) != 1 || last[0]["text"] != emptyAnthropicText {
		t.Fatalf("last = %s %v (reasoning must be dropped)", out.Messages[3].Role, last)
	}
}
