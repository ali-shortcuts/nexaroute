package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeMessages(t *testing.T, raw json.RawMessage) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	return blocks
}

func TestAnthropicToOpenAITool(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{{"type": "text", "text": "hi"}})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 10, Messages: []core.AnthMessage{{Role: "user", Content: content}}, Tools: []core.AnthTool{{Name: "shell", InputSchema: map[string]any{"type": "object"}}}}
	o, nm, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if nm != nil {
		t.Fatal("no name map expected for protocol-legal names")
	}
	if o.Model != "backend" || len(o.Tools) != 1 {
		t.Fatalf("bad output %#v", o)
	}
}

func TestAnthropicImageToOpenAIDataURL(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}}})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 8, Messages: []core.AnthMessage{{Role: "user", Content: content}}}
	o, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := o.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("bad parts %#v", o.Messages[0].Content)
	}
	iu := parts[0]["image_url"].(map[string]any)["url"]
	if iu != "data:image/png;base64,AAAA" {
		t.Fatalf("bad data url %v", iu)
	}
}

func TestOpenAIImageURLToAnthropic(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{{Role: "user", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}}}}}}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	blocks := decodeMessages(t, a.Messages[0].Content)
	src := blocks[0]["source"].(map[string]any)
	if src["type"] != "url" {
		t.Fatalf("bad source %#v", src)
	}
}

// OpenAI tool calls with malformed JSON arguments are preserved in a
// {"_raw": ...} object instead of failing the whole conversation.
func TestOpenAIToAnthropicPreservesMalformedToolArguments(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{{
			Role: "assistant",
			ToolCalls: []core.OpenAIToolCall{{
				ID:       "call-1",
				Function: core.OpenAIFunctionCall{Name: "shell", Arguments: "{bad"},
			}},
		}},
	}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	// An assistant-only history gets a user placeholder prepended (Anthropic
	// requires the first message to be a user turn), so the tool_use turn is
	// the second message.
	if len(a.Messages) != 2 || a.Messages[0].Role != "user" || a.Messages[1].Role != "assistant" {
		t.Fatalf("unexpected messages: %#v", a.Messages)
	}
	blocks := decodeMessages(t, a.Messages[1].Content)
	if blocks[0]["type"] != "tool_use" {
		t.Fatalf("expected tool_use block, got %#v", blocks[0])
	}
	input := blocks[0]["input"].(map[string]any)
	if input["_raw"] != "{bad" {
		t.Fatalf("malformed arguments must be preserved via _raw, got %#v", input)
	}
}

func TestOpenAIResponseToAnthropicPreservesMalformedToolArguments(t *testing.T) {
	finish := "tool_calls"
	in := core.OpenAIResponse{
		ID: "x",
		Choices: []core.OpenAIChoice{{
			Message: core.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []core.OpenAIToolCall{{
					ID:       "call-1",
					Function: core.OpenAIFunctionCall{Name: "shell", Arguments: "{bad"},
				}},
			},
			FinishReason: &finish,
		}},
	}
	resp, err := OpenAIResponseToAnthropic(in, "m", nil)
	if err != nil {
		t.Fatalf("malformed arguments must not fail the response translation: %v", err)
	}
	var found bool
	for _, b := range resp.Content {
		if b.Type == "tool_use" && b.Input["_raw"] == "{bad" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected _raw preservation, got %#v", resp.Content)
	}
}

func TestOpenAIToAnthropicMapsDeveloperRoleToSystem(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{
			{Role: "developer", Content: "developer policy"},
			{Role: "user", Content: "hello"},
		},
	}
	got, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	var system string
	if err := json.Unmarshal(got.System, &system); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, "developer policy") {
		t.Fatalf("developer message not mapped into Anthropic system context: %q", system)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("unexpected translated messages: %#v", got.Messages)
	}
}

func TestOpenAIToAnthropicRejectsInvalidRolesAndToolIdentity(t *testing.T) {
	cases := []core.OpenAIRequest{
		{Messages: []core.OpenAIMessage{{Role: "nonsense", Content: "x"}}},
		{Messages: []core.OpenAIMessage{{Role: "tool", Content: "x"}}},
		{Messages: []core.OpenAIMessage{{Role: "assistant", ToolCalls: []core.OpenAIToolCall{{Function: core.OpenAIFunctionCall{Arguments: "{}"}}}}}},
	}
	for i, in := range cases {
		if _, _, err := OpenAIToAnthropic(in, "m"); err == nil {
			t.Fatalf("case %d should have failed", i)
		}
	}
}

func TestAnthropicToOpenAIRejectsInvalidToolStructure(t *testing.T) {
	cases := []core.AnthropicRequest{
		{Messages: []core.AnthMessage{{Role: "invalid", Content: json.RawMessage(`"x"`)}}},
		{Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"tool_use","id":"x","name":"f","input":{}}]`)}}},
		{Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","content":"ok"}]`)}}},
	}
	for i, in := range cases {
		if _, _, err := AnthropicToOpenAI(in, "m"); err == nil {
			t.Fatalf("case %d should have failed", i)
		}
	}
}

func TestOpenAIToAnthropicRejectsUnsupportedContentParts(t *testing.T) {
	cases := []core.OpenAIRequest{
		{Messages: []core.OpenAIMessage{{Role: "user", Content: []any{map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "x"}}}}}},
		{Messages: []core.OpenAIMessage{{Role: "user", Content: []any{"not-an-object"}}}},
		{Messages: []core.OpenAIMessage{{Role: "user", Content: map[string]any{"type": "text", "text": "x"}}}},
		{Messages: []core.OpenAIMessage{{Role: "system", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/x.png"}}}}}},
	}
	for i, in := range cases {
		if _, _, err := OpenAIToAnthropic(in, "m"); err == nil {
			t.Fatalf("case %d should reject unsupported content instead of dropping it", i)
		}
	}
}

func TestAnthropicToOpenAIRejectsInvalidImageAndSystem(t *testing.T) {
	cases := []core.AnthropicRequest{
		{System: json.RawMessage(`{"type":"not-valid-system"}`), Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}},
		{Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"","data":""}}]`)}}},
	}
	for i, in := range cases {
		if _, _, err := AnthropicToOpenAI(in, "m"); err == nil {
			t.Fatalf("case %d should have failed", i)
		}
	}
}

// --- New behavior coverage -------------------------------------------------

// Claude Code extended-thinking sessions replay assistant turns containing
// thinking blocks. The OpenAI upstream cannot accept them, so translation must
// drop them and keep the conversation alive.
func TestAnthropicToOpenAIDropsThinkingBlocks(t *testing.T) {
	content := json.RawMessage(`[
		{"type":"thinking","thinking":"internal scratchpad","signature":"sig"},
		{"type":"redacted_thinking","data":"xxx"},
		{"type":"text","text":"final answer"}
	]`)
	in := core.AnthropicRequest{Model: "x", MaxTokens: 10, Messages: []core.AnthMessage{{Role: "assistant", Content: content}}}
	o, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatalf("thinking history must not fail translation: %v", err)
	}
	msg := o.Messages[0]
	txt, _ := msg.Content.(string)
	if txt != "final answer" {
		t.Fatalf("expected only text to survive, got %#v", msg.Content)
	}
}

// Unknown Anthropic blocks are rejected on cross-protocol translation rather
// than silently disappearing and changing the caller's message semantics.
func TestAnthropicToOpenAIRejectsUnknownBlockTypes(t *testing.T) {
	content := json.RawMessage(`[
		{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}},
		{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]},
		{"type":"text","text":"answer"}
	]`)
	in := core.AnthropicRequest{Model: "x", MaxTokens: 10, Messages: []core.AnthMessage{{Role: "assistant", Content: content}}}
	if _, _, err := AnthropicToOpenAI(in, "backend"); err == nil {
		t.Fatal("unknown block types must not be silently dropped")
	}
}

// Consecutive OpenAI messages with the same role must merge into one Anthropic
// message: the Anthropic API enforces strict alternation.
func TestOpenAIToAnthropicMergesConsecutiveSameRole(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{
			{Role: "user", Content: "first"},
			{Role: "user", Content: "second"},
			{Role: "assistant", Content: "a1"},
			{Role: "assistant", Content: "a2"},
			{Role: "user", Content: "third"},
		},
	}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 3 {
		t.Fatalf("expected 3 merged messages, got %d: %#v", len(a.Messages), a.Messages)
	}
	roles := []string{a.Messages[0].Role, a.Messages[1].Role, a.Messages[2].Role}
	if roles[0] != "user" || roles[1] != "assistant" || roles[2] != "user" {
		t.Fatalf("bad role sequence %#v", roles)
	}
	blocks := decodeMessages(t, a.Messages[0].Content)
	joined := blocks[0]["text"].(string) + " " + blocks[1]["text"].(string)
	if joined != "first second" {
		t.Fatalf("merged content wrong: %q", joined)
	}
}

// Parallel OpenAI tool results must coalesce into ONE user message with
// multiple tool_result blocks.
func TestOpenAIToAnthropicMergesParallelToolResults(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{
			{Role: "user", Content: "run both"},
			{Role: "assistant", ToolCalls: []core.OpenAIToolCall{
				{ID: "call-a", Function: core.OpenAIFunctionCall{Name: "f", Arguments: "{}"}},
				{ID: "call-b", Function: core.OpenAIFunctionCall{Name: "g", Arguments: "{}"}},
			}},
			{Role: "tool", ToolCallID: "call-a", Content: "result a"},
			{Role: "tool", ToolCallID: "call-b", Content: "result b"},
		},
	}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, merged user), got %d", len(a.Messages))
	}
	blocks := decodeMessages(t, a.Messages[2].Content)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %#v", blocks)
	}
	if blocks[0]["type"] != "tool_result" || blocks[1]["type"] != "tool_result" {
		t.Fatalf("bad blocks %#v", blocks)
	}
	if blocks[0]["tool_use_id"] != "call-a" || blocks[1]["tool_use_id"] != "call-b" {
		t.Fatalf("tool_use_id pairing wrong: %#v", blocks)
	}
}

// A conversation that opens with an assistant turn needs a user placeholder,
// because Anthropic requires the first message to be a user turn.
func TestOpenAIToAnthropicPrependsUserForAssistantFirstHistory(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{
			{Role: "assistant", Content: "hi there"},
			{Role: "user", Content: "hello"},
		},
	}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 3 || a.Messages[0].Role != "user" {
		t.Fatalf("expected prepended user message, got %#v", a.Messages)
	}
}

// Empty user content must be replaced with a placeholder, never an empty text
// block (Anthropic rejects empty text).
func TestOpenAIToAnthropicPlaceholderForEmptyContent(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: ""}}}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	blocks := decodeMessages(t, a.Messages[0].Content)
	if blocks[0]["text"] != "..." {
		t.Fatalf("expected placeholder, got %#v", blocks)
	}
}

// OpenAI stop sequences must map onto Anthropic stop_sequences.
func TestOpenAIToAnthropicMapsStopSequences(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}, Stop: []any{"END", "STOP"}}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.StopSequences) != 2 || a.StopSequences[0] != "END" || a.StopSequences[1] != "STOP" {
		t.Fatalf("stop sequences lost: %#v", a.StopSequences)
	}
	single := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}, Stop: "END"}
	a2, _, err := OpenAIToAnthropic(single, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(a2.StopSequences) != 1 || a2.StopSequences[0] != "END" {
		t.Fatalf("single stop lost: %#v", a2.StopSequences)
	}
}

func TestOpenAIToAnthropicDefaultMaxTokens(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if a.MaxTokens != 4096 {
		t.Fatalf("expected default max_tokens 4096, got %d", a.MaxTokens)
	}
	alt := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}, MaxCompletionTokens: 256}
	a2, _, err := OpenAIToAnthropic(alt, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if a2.MaxTokens != 256 {
		t.Fatalf("max_completion_tokens must be honored, got %d", a2.MaxTokens)
	}
}

func TestOpenAIToAnthropicReasoningEffortToThinking(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "think"}}, ReasoningEffort: "high"}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(a.Thinking, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["type"] != "enabled" {
		t.Fatalf("expected enabled thinking, got %#v", cfg)
	}
	if int(cfg["budget_tokens"].(float64)) != 16384 {
		t.Fatalf("bad budget: %#v", cfg)
	}

	// Reasoning over a tool-using conversation must drop thinking rather than
	// trigger Anthropic's "Expected thinking ... but found tool_use" 400.
	toolConv := core.OpenAIRequest{
		Model:           "x",
		Messages:        []core.OpenAIMessage{{Role: "user", Content: "hi"}, {Role: "assistant", ToolCalls: []core.OpenAIToolCall{{ID: "t", Function: core.OpenAIFunctionCall{Name: "f", Arguments: "{}"}}}}, {Role: "tool", ToolCallID: "t", Content: "ok"}},
		ReasoningEffort: "high",
	}
	a2, _, err := OpenAIToAnthropic(toolConv, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if a2.Thinking != nil {
		t.Fatalf("thinking must be dropped for tool-using histories without signed blocks, got %s", a2.Thinking)
	}
}

// The max_tokens > thinking.budget_tokens invariant must hold after translation.
func TestOpenAIToAnthropicThinkingBudgetInvariant(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}, ReasoningEffort: "high"}
	in.MaxTokens = 2000
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if a.MaxTokens <= 16384 {
		t.Fatalf("max_tokens must be raised above budget: %d", a.MaxTokens)
	}
}

func TestOpenAIToAnthropicToolChoiceMatrix(t *testing.T) {
	cases := []struct {
		choice any
		want   string // JSON of tool_choice
	}{
		{"auto", `{"type":"auto"}`},
		{"none", `{"type":"none"}`},
		{"required", `{"type":"any"}`},
		{map[string]any{"function": map[string]any{"name": "f"}}, `{"name":"f","type":"tool"}`},
	}
	for _, c := range cases {
		in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}, ToolChoice: c.choice}
		a, _, err := OpenAIToAnthropic(in, "backend")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(a.ToolChoice)
		if string(got) != c.want {
			t.Fatalf("tool_choice %v: want %s got %s", c.choice, c.want, got)
		}
	}
}

func TestNamedToolChoiceUsesSanitizedName(t *testing.T) {
	anthContent := json.RawMessage(`"run it"`)
	anth := core.AnthropicRequest{
		Model: "client", MaxTokens: 12,
		Messages: []core.AnthMessage{{Role: "user", Content: anthContent}},
		Tools: []core.AnthTool{{Name: "server.run", InputSchema: map[string]any{"type": "object"}}},
		ToolChoice: map[string]any{"type": "tool", "name": "server.run"},
	}
	openReq, _, err := AnthropicToOpenAI(anth, "physical")
	if err != nil {
		t.Fatal(err)
	}
	toolChoice, ok := openReq.ToolChoice.(map[string]any)
	if !ok {
		t.Fatalf("tool_choice=%#v", openReq.ToolChoice)
	}
	fn, _ := toolChoice["function"].(map[string]any)
	if fn["name"] != "server_run" {
		t.Fatalf("named Anthropic choice did not use sanitized OpenAI tool name: %#v", toolChoice)
	}

	openReqIn := core.OpenAIRequest{
		Model: "client", Messages: []core.OpenAIMessage{{Role: "user", Content: "go"}},
		Tools: []core.OpenAITool{{Type: "function", Function: core.OpenAIFunction{Name: "server.run", Parameters: map[string]any{"type": "object"}}}},
		ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "server.run"}},
	}
	anthReq, _, err := OpenAIToAnthropic(openReqIn, "physical")
	if err != nil {
		t.Fatal(err)
	}
	choice, ok := anthReq.ToolChoice.(map[string]any)
	if !ok || choice["name"] != "server_run" {
		t.Fatalf("named OpenAI choice did not use sanitized Anthropic tool name: %#v", anthReq.ToolChoice)
	}
}

func TestAnthropicToOpenAIToolChoiceMatrix(t *testing.T) {
	content, _ := json.Marshal("hi")
	mk := func(choice any) core.AnthropicRequest {
		return core.AnthropicRequest{Model: "x", MaxTokens: 8, Messages: []core.AnthMessage{{Role: "user", Content: content}}, ToolChoice: choice}
	}
	cases := []struct {
		choice any
		want   string
	}{
		{map[string]any{"type": "auto"}, `"auto"`},
		{map[string]any{"type": "any"}, `"required"`},
		{map[string]any{"type": "none"}, `"none"`},
		{map[string]any{"type": "tool", "name": "f"}, `{"function":{"name":"f"},"type":"function"}`},
		{"any", `"required"`},
		{"none", `"none"`},
	}
	for _, c := range cases {
		o, _, err := AnthropicToOpenAI(mk(c.choice), "backend")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(o.ToolChoice)
		if string(got) != c.want {
			t.Fatalf("tool_choice %v: want %s got %s", c.choice, c.want, got)
		}
	}
	// disable_parallel_tool_use maps onto parallel_tool_calls=false
	o, _, err := AnthropicToOpenAI(mk(map[string]any{"type": "auto", "disable_parallel_tool_use": true}), "backend")
	if err != nil {
		t.Fatal(err)
	}
	if o.ParallelToolCalls == nil || *o.ParallelToolCalls {
		t.Fatalf("disable_parallel_tool_use not mapped: %#v", o.ParallelToolCalls)
	}
}

// Tool names carrying characters outside [a-zA-Z0-9_-] or exceeding the
// OpenAI 64-char limit are renamed reversibly.
func TestToolNameMappingRoundTrip(t *testing.T) {
	long := strings.Repeat("m", 80) + ".end"
	names := []string{"server.tool", "weird:name/slash", long, "clean_name"}
	in := core.AnthropicRequest{
		Model:     "x",
		MaxTokens: 8,
		Messages:  []core.AnthMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:     []core.AnthTool{{Name: names[0]}, {Name: names[1]}, {Name: names[2]}, {Name: names[3]}},
	}
	o, nm, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if nm == nil {
		t.Fatal("expected a name map for non-conforming names")
	}
	if o.Tools[3].Function.Name != "clean_name" {
		t.Fatalf("clean name must pass through untouched, got %q", o.Tools[3].Function.Name)
	}
	seen := map[string]bool{}
	for _, tool := range o.Tools {
		name := tool.Function.Name
		if len(name) > 64 {
			t.Fatalf("sanitized name too long: %q", name)
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				t.Fatalf("sanitized name has illegal char: %q", name)
			}
		}
		if seen[name] {
			t.Fatalf("duplicate sanitized name: %q", name)
		}
		seen[name] = true
	}
	if nm.Reverse(o.Tools[0].Function.Name) != names[0] {
		t.Fatalf("reverse mapping failed for dots: %q", o.Tools[0].Function.Name)
	}
	if nm.Reverse(o.Tools[2].Function.Name) != long {
		t.Fatalf("reverse mapping failed for long name")
	}
}

func TestAnthropicNameMapLengthLimit(t *testing.T) {
	long := strings.Repeat("x", 200)
	in := core.OpenAIRequest{
		Model:    "x",
		Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools:    []core.OpenAITool{{Type: "function", Function: core.OpenAIFunction{Name: long}}},
	}
	a, nm, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	sanitized := a.Tools[0].Name
	if len(sanitized) > 128 {
		t.Fatalf("anthropic name too long: %d", len(sanitized))
	}
	if nm.Reverse(sanitized) != long {
		t.Fatal("reverse map must restore the original long name")
	}
}

// tool_result content normalization: block arrays become text+image parts and
// is_error keeps its failure signal.
func TestAnthropicToOpenAIToolResultNormalization(t *testing.T) {
	content := json.RawMessage(`[
		{"type":"tool_result","tool_use_id":"call-a","content":[
			{"type":"text","text":"line1"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}}
		]},
		{"type":"tool_result","tool_use_id":"call-b","content":"plain","is_error":true},
		{"type":"text","text":"continue"}
	]`)
	in := core.AnthropicRequest{Model: "x", MaxTokens: 8, Messages: []core.AnthMessage{{Role: "user", Content: content}}}
	o, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	// Expect: tool(a) -> tool(b) -> user(text)
	if len(o.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %#v", o.Messages)
	}
	if o.Messages[0].Role != "tool" || o.Messages[1].Role != "tool" || o.Messages[2].Role != "user" {
		t.Fatalf("bad order: %#v", o.Messages)
	}
	parts, ok := o.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("tool result parts wrong: %#v", o.Messages[0].Content)
	}
	if parts[0]["text"] != "line1" {
		t.Fatalf("text part lost: %#v", parts)
	}
	if b, errStr := o.Messages[1].Content.(string); !errStr || !strings.HasPrefix(b, "[tool error] ") || !strings.Contains(b, "plain") {
		t.Fatalf("is_error prefix missing: %#v", o.Messages[1].Content)
	}
}

func TestAnthropicToOpenAIMapsThinkingEffortAndMetadata(t *testing.T) {
	in := core.AnthropicRequest{
		Model:     "x",
		MaxTokens: 8,
		Messages:  []core.AnthMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Thinking:  json.RawMessage(`{"type":"enabled","budget_tokens":9000}`),
		Metadata:  json.RawMessage(`{"user_id":"user-42"}`),
	}
	o, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if o.ReasoningEffort != "medium" {
		t.Fatalf("budget 9000 should map to medium, got %q", o.ReasoningEffort)
	}
	if o.User != "user-42" {
		t.Fatalf("metadata.user_id must map to user, got %q", o.User)
	}
}

func TestAnthropicToOpenAIStopSequencesPassThrough(t *testing.T) {
	in := core.AnthropicRequest{Model: "x", MaxTokens: 8, Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}, StopSequences: []string{"A", "B"}}
	o, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	stop, ok := o.Stop.([]string)
	if !ok || len(stop) != 2 {
		t.Fatalf("stop sequences lost: %#v", o.Stop)
	}
}

// Anthropic responses: thinking text maps onto reasoning_content (safe
// direction), tool names reverse-map, cache tokens surface in usage.
func TestAnthropicResponseToOpenAIThinkingAndNames(t *testing.T) {
	nm := &NameMap{fwd: map[string]string{"server_tool": "server.tool"}, rev: map[string]string{"server.tool": "server_tool"}}
	in := core.AnthResponse{
		ID:    "msg_1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude",
		Content: []core.AnthContentBlock{
			{Type: "thinking", Thinking: "step one"},
			{Type: "text", Text: "final"},
			{Type: "tool_use", ID: "tu1", Name: "server.tool", Input: map[string]any{"a": 1}},
		},
		StopReason: strPtr("tool_use"),
		Usage:      core.AnthUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 4},
	}
	out := AnthropicResponseToOpenAI(in, "client-model", nm)
	if out.Choices[0].Message.ReasoningContent != "step one" {
		t.Fatalf("reasoning_content lost: %#v", out.Choices[0].Message)
	}
	tc := out.Choices[0].Message.ToolCalls[0]
	if tc.Function.Name != "server_tool" {
		t.Fatalf("tool name not reversed: %q", tc.Function.Name)
	}
	if out.Choices[0].Message.Content != "final" {
		t.Fatalf("text lost: %#v", out.Choices[0].Message)
	}
	if *out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish wrong: %v", *out.Choices[0].FinishReason)
	}
	if out.Usage.PromptTokensDetails == nil || out.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Fatalf("cache tokens not surfaced: %#v", out.Usage)
	}
}

func TestOpenAIResponseToAnthropicContentVariants(t *testing.T) {
	// content as array of parts (some providers do this)
	parts := []any{map[string]any{"type": "text", "text": "alpha"}, map[string]any{"type": "text", "text": "beta"}}
	in := core.OpenAIResponse{ID: "x", Choices: []core.OpenAIChoice{{Message: core.OpenAIMessage{Role: "assistant", Content: parts}}}}
	resp, err := OpenAIResponseToAnthropic(in, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "alpha\nbeta" {
		t.Fatalf("content array not flattened: %#v", resp.Content)
	}

	// reasoning_content from reasoning models must NOT fabricate an unsigned
	// thinking block (poisons Anthropic replay).
	in2 := core.OpenAIResponse{ID: "y", Choices: []core.OpenAIChoice{{Message: core.OpenAIMessage{Role: "assistant", Content: "ok", ReasoningContent: "secret"}}}}
	resp2, err := OpenAIResponseToAnthropic(in2, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range resp2.Content {
		if b.Type == "thinking" {
			t.Fatal("unsigned thinking blocks must never be fabricated")
		}
	}
}

func TestFinishReasonMatrix(t *testing.T) {
	cases := map[string]string{
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"length":         "max_tokens",
		"content_filter": "refusal",
		"stop":           "end_turn",
		"weird":          "end_turn",
	}
	for k, want := range cases {
		if got := finishReasonToAnthropicStop(k); got != want {
			t.Fatalf("finish %q: want %q got %q", k, want, got)
		}
	}
}

func TestImageMediaTypeNormalization(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpg;base64,QQ=="}},
	}}}}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	blocks := decodeMessages(t, a.Messages[0].Content)
	src := blocks[0]["source"].(map[string]any)
	if src["media_type"] != "image/jpeg" {
		t.Fatalf("image/jpg must normalize to image/jpeg, got %v", src["media_type"])
	}
}

func TestOpenAIToAnthropicUserToMetadata(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}, User: "u-1"}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(a.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["user_id"] != "u-1" {
		t.Fatalf("user must map to metadata.user_id, got %#v", meta)
	}
}

func TestOpenAIToAnthropicToolsInputSchemaDefault(t *testing.T) {
	in := core.OpenAIRequest{
		Model:    "x",
		Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools:    []core.OpenAITool{{Type: "function", Function: core.OpenAIFunction{Name: "f"}}, {Type: "function", Function: core.OpenAIFunction{Name: "g", Parameters: map[string]any{"properties": map[string]any{}}}}},
	}
	a, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if a.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("missing input_schema must default to object, got %#v", a.Tools[0].InputSchema)
	}
	if a.Tools[1].InputSchema["type"] != "object" {
		t.Fatalf("schema missing type must gain object, got %#v", a.Tools[1].InputSchema)
	}
}

func strPtr(s string) *string { return &s }
