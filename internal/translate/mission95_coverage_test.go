package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func mission95AnthBlocks(blocks []map[string]any) json.RawMessage {
	raw, err := json.Marshal(blocks)
	if err != nil {
		panic(err)
	}
	return raw
}

func mission95AnthTextMsg(role, text string) core.AnthMessage {
	return core.AnthMessage{Role: role, Content: mission95AnthBlocks([]map[string]any{{"type": "text", "text": text}})}
}

// --- Anthropic -> OpenAI ------------------------------------------------------

func TestMission95AnthropicSingleStopAndEmptyAndBadContent(t *testing.T) {
	base := core.AnthropicRequest{Model: "x", MaxTokens: 10,
		Messages: []core.AnthMessage{{Role: "user", Content: mission95AnthBlocks([]map[string]any{{"type": "text", "text": "hi"}})}}}
	in := base
	in.StopSequences = []string{"STOP"}
	out, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := out.Stop.(string); !ok || s != "STOP" {
		t.Fatalf("single stop=%#v want string STOP", out.Stop)
	}

	// An empty block array yields an empty message, not an error.
	in = base
	in.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[]`)}}
	out, _, err = AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Content != "" {
		t.Fatalf("empty blocks -> messages=%#v want one empty message", out.Messages)
	}

	// Malformed block JSON fails the translation.
	in = base
	in.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`{"bad`)}}
	if _, _, err := AnthropicToOpenAI(in, "backend"); err == nil {
		t.Fatal("malformed content must fail translation")
	}
}

func TestMission95AnthropicAssistantTextPlusToolCall(t *testing.T) {
	content := mission95AnthBlocks([]map[string]any{
		{"type": "text", "text": "calling"},
		{"type": "tool_use", "id": "tu-1", "name": "shell", "input": map[string]any{"cmd": "ls"}},
	})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 10,
		Messages: []core.AnthMessage{{Role: "assistant", Content: content}}}
	out, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(out.Messages))
	}
	parts, ok := out.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 1 || parts[0]["text"] != "calling" {
		t.Fatalf("assistant content=%#v want text part 'calling'", out.Messages[0].Content)
	}
	if len(out.Messages[0].ToolCalls) != 1 || out.Messages[0].ToolCalls[0].ID != "tu-1" {
		t.Fatalf("assistant tool calls=%#v want one tu-1", out.Messages[0].ToolCalls)
	}
}

func TestMission95AnthropicToolResultTrailingMultiText(t *testing.T) {
	content := mission95AnthBlocks([]map[string]any{
		{"type": "tool_result", "tool_use_id": "tu-1", "content": "done"},
		{"type": "text", "text": "first"},
		{"type": "text", "text": "second"},
	})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 10,
		Messages: []core.AnthMessage{{Role: "user", Content: content}}}
	out, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages=%d want 2 (tool + trailing user text)", len(out.Messages))
	}
	if out.Messages[0].Role != "tool" || out.Messages[0].ToolCallID != "tu-1" {
		t.Fatalf("first message=%#v want tool tu-1", out.Messages[0])
	}
	parts, ok := out.Messages[1].Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("trailing content=%#v want 2 text parts", out.Messages[1].Content)
	}
}

// Note: the tool-input marshal error in AnthropicToOpenAI is not reachable
// through the public API — tool inputs arrive as JSON-decoded blocks, and
// every JSON-decodable value re-marshals. It is a defensive branch only,
// so no test fabricates it here.

func TestMission95ToolResultContentShapes(t *testing.T) {
	cases := []struct {
		name string
		b    core.AnthBlock
		want any
	}{
		{"empty error", core.AnthBlock{IsError: true}, "[tool error] (empty result)"},
		{"empty plain", core.AnthBlock{}, ""},
		{"null error", core.AnthBlock{Content: json.RawMessage(`null`), IsError: true}, "[tool error] (empty result)"},
		{"string error", core.AnthBlock{Content: json.RawMessage(`"boom"`), IsError: true}, "[tool error] boom"},
		{"string plain", core.AnthBlock{Content: json.RawMessage(`"ok"`)}, "ok"},
		{"generic number", core.AnthBlock{Content: json.RawMessage(`42`)}, float64(42)},
		{"generic number error", core.AnthBlock{Content: json.RawMessage(`42`), IsError: true}, "[tool error] 42"},
		{"invalid json error", core.AnthBlock{Content: json.RawMessage(`{bad`), IsError: true}, "[tool error] {bad"},
		{"invalid json plain", core.AnthBlock{Content: json.RawMessage(`{bad`)}, "{bad"},
	}
	for _, tc := range cases {
		got := toolResultToOpenAIContent(tc.b)
		if got != tc.want {
			t.Fatalf("%s: got %#v want %#v", tc.name, got, tc.want)
		}
	}
	// Consecutive text blocks merge with a newline, and unknown block types
	// are preserved as encoded JSON text merged into the same text part.
	merged := toolResultToOpenAIContent(core.AnthBlock{Content: json.RawMessage(
		`[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"custom","x":1}]`)})
	parts, ok := merged.([]map[string]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("merged parts=%#v want 1 merged part", merged)
	}
	if parts[0]["text"] != "one\ntwo\n{\"type\":\"custom\",\"x\":1}" {
		t.Fatalf("merged text=%q want one\\ntwo\\nencoded-custom", parts[0]["text"])
	}
	// An image block inside a tool result becomes an image_url part.
	withImage := toolResultToOpenAIContent(core.AnthBlock{Content: json.RawMessage(
		`[{"type":"image","source":{"type":"url","url":"https://example.com/i.png"}}]`)})
	imgParts, ok := withImage.([]map[string]any)
	if !ok || len(imgParts) != 1 || imgParts[0]["type"] != "image_url" {
		t.Fatalf("image parts=%#v want one image_url part", withImage)
	}
}

func TestMission95AnthropicToolChoiceThinkingMetadata(t *testing.T) {
	base := core.AnthropicRequest{Model: "x", MaxTokens: 10,
		Messages: []core.AnthMessage{{Role: "user", Content: mission95AnthBlocks([]map[string]any{{"type": "text", "text": "hi"}})}}}
	// Tool choice "any" with parallel use disabled.
	in := base
	in.ToolChoice = map[string]any{"type": "any", "disable_parallel_tool_use": true}
	out, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice != "required" {
		t.Fatalf("tool choice=%#v want required", out.ToolChoice)
	}
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Fatalf("parallel tool calls=%v want false pointer", out.ParallelToolCalls)
	}

	thinking := []struct {
		raw  string
		want string
	}{
		{`{"type":"enabled","budget_tokens":0}`, "medium"},
		{`{"type":"enabled","budget_tokens":1024}`, "low"},
		{`{"type":"enabled","budget_tokens":20000}`, "high"},
		{`{"type":"adaptive"}`, "high"},
		{`{"type":"disabled","budget_tokens":99999}`, ""},
		{`{"type":"weird","budget_tokens":99999}`, ""},
		{`{"type":`, ""},
	}
	for _, tc := range thinking {
		in := base
		in.Thinking = json.RawMessage(tc.raw)
		out, _, err := AnthropicToOpenAI(in, "backend")
		if err != nil {
			t.Fatal(err)
		}
		if out.ReasoningEffort != tc.want {
			t.Fatalf("thinking %s -> effort %q want %q", tc.raw, out.ReasoningEffort, tc.want)
		}
	}

	// Metadata: a nested user_id map resolves to the plain id string;
	// malformed metadata is ignored rather than failing the request.
	in = base
	in.Metadata = json.RawMessage(`{"user_id":{"id":"  u-1  "}}`)
	out, _, err = AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if out.User != "u-1" {
		t.Fatalf("metadata user=%q want u-1", out.User)
	}
	in = base
	in.Metadata = json.RawMessage(`{"user_id":`)
	out, _, err = AnthropicToOpenAI(in, "backend")
	if err != nil || out.User != "" {
		t.Fatalf("malformed metadata -> err=%v user=%q want no error and empty user", err, out.User)
	}
}

func TestMission95AnthropicSystemShapes(t *testing.T) {
	base := core.AnthropicRequest{Model: "x", MaxTokens: 10,
		Messages: []core.AnthMessage{{Role: "user", Content: mission95AnthBlocks([]map[string]any{{"type": "text", "text": "hi"}})}}}
	// Plain-string system prompt.
	in := base
	in.System = json.RawMessage(`"be helpful"`)
	out, _, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) < 1 || out.Messages[0].Role != "system" || out.Messages[0].Content != "be helpful" {
		t.Fatalf("string system -> messages=%#v", out.Messages)
	}
	// Multiple text blocks join with a newline.
	in = base
	in.System = json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`)
	out, _, err = AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Content != "first\nsecond" {
		t.Fatalf("multi-block system=%q want first\\nsecond", out.Messages[0].Content)
	}
	// A non-text system block is rejected.
	in = base
	in.System = json.RawMessage(`[{"type":"image","text":"x"}]`)
	if _, _, err := AnthropicToOpenAI(in, "backend"); err == nil {
		t.Fatal("non-text system block must be rejected")
	}
}

func TestMission95AnthImageURLShapes(t *testing.T) {
	if got := anthImageURL(nil); got != "" {
		t.Fatalf("nil source -> %q want empty", got)
	}
	if got := anthImageURL(map[string]any{"type": "url", "url": "https://example.com/a.png"}); got != "https://example.com/a.png" {
		t.Fatalf("url source -> %q", got)
	}
	if got := anthImageURL(map[string]any{"type": "file", "url": "https://example.com/b.png"}); got != "https://example.com/b.png" {
		t.Fatalf("fallback url field -> %q", got)
	}
	if got := anthImageURL(map[string]any{"type": "base64", "media_type": "", "data": "AAAA"}); got != "" {
		t.Fatalf("base64 without media type -> %q want empty", got)
	}
}

func TestMission95OpenAIResponseToAnthropicEdges(t *testing.T) {
	// No choices at all: an empty text block keeps the response well-formed.
	resp, err := OpenAIResponseToAnthropic(core.OpenAIResponse{ID: "r1", Usage: core.OpenAIUsage{PromptTokens: 3, CompletionTokens: 4}}, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "" {
		t.Fatalf("empty response content=%#v want one empty text block", resp.Content)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("usage=%+v want 3/4", resp.Usage)
	}
	// Cached tokens propagate; an unmapped finish reason becomes end_turn.
	reason := "stop"
	resp, err = OpenAIResponseToAnthropic(core.OpenAIResponse{
		ID:      "r2",
		Choices: []core.OpenAIChoice{{Message: core.OpenAIMessage{Content: "hi"}, FinishReason: &reason}},
		Usage:   core.OpenAIUsage{PromptTokens: 1, CompletionTokens: 2, PromptTokensDetails: &core.OpenAIUsageDetail{CachedTokens: 7}},
	}, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason == nil || *resp.StopReason != "end_turn" {
		t.Fatalf("stop reason=%v want end_turn", resp.StopReason)
	}
	if resp.Usage.CacheReadInputTokens != 7 {
		t.Fatalf("cache read=%d want 7", resp.Usage.CacheReadInputTokens)
	}
	// Non-map parts in a response content array are skipped.
	blocks := openAIResponseContentToAnthBlocks([]any{"junk", map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}})
	if len(blocks) != 1 || blocks[0].Text != "a\nb" {
		t.Fatalf("response blocks=%#v want one merged text block a\\nb", blocks)
	}
}

// --- NameMap ------------------------------------------------------------------

func TestMission95NameMapEdges(t *testing.T) {
	// Blank names are skipped; a map with no renames is nil.
	if nm := NewOpenAINameMap([]string{"", "  ", "plain"}); nm != nil {
		t.Fatal("all-safe names must yield a nil map")
	}
	var nilMap *NameMap
	if got := nilMap.Forward("x"); got != "x" {
		t.Fatalf("nil Forward -> %q want x", got)
	}
	if got := nilMap.Reverse("x"); got != "x" {
		t.Fatalf("nil Reverse -> %q want x", got)
	}
	// Two names that sanitize identically are deduplicated with a suffix.
	nm := NewOpenAINameMap([]string{"a.b", "a:b"})
	if nm == nil {
		t.Fatal("renaming map expected")
	}
	if got := nm.Forward("a.b"); got != "a_b" {
		t.Fatalf("first sanitized=%q want a_b", got)
	}
	if got := nm.Forward("a:b"); got != "a_b-2" {
		t.Fatalf("second sanitized=%q want a_b-2", got)
	}
	if got := nm.Reverse("a_b-2"); got != "a:b" {
		t.Fatalf("reverse=%q want a:b", got)
	}
	if got := nm.Reverse("unmapped"); got != "unmapped" {
		t.Fatalf("reverse unmapped=%q want passthrough", got)
	}
	// Helper-level edges: empty sanitization, non-positive max length, and
	// suffix truncation that must make room for the suffix.
	if got := sanitizeToolName("", 64); got != "tool" {
		t.Fatalf("sanitize empty=%q want tool", got)
	}
	if got := truncateWithSuffix("abcdef", 0, ""); got != "abcdef" {
		t.Fatalf("truncate maxLen 0=%q want passthrough", got)
	}
	if got := truncateWithSuffix("abcdefghijklmnopqrst", 3, ""); got != "abc" {
		t.Fatalf("truncate tiny maxLen=%q want abc", got)
	}
	if got := truncateWithSuffix("abcdefghij", 5, "-2"); got != "abc-2" {
		t.Fatalf("truncate with suffix=%q want abc-2", got)
	}
	if got := truncateWithSuffix("ab", 10, "-2"); got != "ab-2" {
		t.Fatalf("truncate short with suffix=%q want ab-2", got)
	}
	// A very long name is truncated with a hash disambiguator, staying in
	// bounds and round-tripping through Reverse.
	long := strings.Repeat("n", 200)
	nm2 := NewAnthropicNameMap([]string{long})
	if nm2 == nil {
		t.Fatal("long name map expected")
	}
	fwd := nm2.Forward(long)
	if len(fwd) > 128 || fwd == long {
		t.Fatalf("long forward len=%d want <=128 and changed", len(fwd))
	}
	if got := nm2.Reverse(fwd); got != long {
		t.Fatal("long name must round-trip through Reverse")
	}
}

// --- OpenAI -> Anthropic -------------------------------------------------------

func TestMission95OpenAISystemMerging(t *testing.T) {
	// Two text parts in one system message join with a newline, and a
	// second system message appends to the first.
	in := core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{
		{Role: "system", Content: []any{map[string]any{"type": "text", "text": "first"}, map[string]any{"type": "text", "text": "second"}}},
		{Role: "system", Content: "third"},
		{Role: "user", Content: "hi"},
	}}
	out, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	var sys string
	if err := json.Unmarshal(out.System, &sys); err != nil {
		t.Fatal(err)
	}
	if sys != "first\nsecond\nthird" {
		t.Fatalf("system=%q want first\\nsecond\\nthird", sys)
	}
}

func TestMission95OpenAIToolCallIDAndEmptyAssistant(t *testing.T) {
	// An empty tool-call id is generated deterministically.
	in := core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{
		{Role: "assistant", ToolCalls: []core.OpenAIToolCall{{Function: core.OpenAIFunctionCall{Name: "shell", Arguments: "{}"}}}},
		{Role: "user", Content: "hi"},
	}}
	out, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	blocks := decodeMessages(t, out.Messages[1].Content)
	if len(blocks) != 1 || !strings.HasPrefix(blocks[0]["id"].(string), "toolu_openai_") {
		t.Fatalf("generated tool id blocks=%#v", blocks)
	}
	// An assistant turn with neither text nor tool calls gets the
	// placeholder so Anthropic's non-empty-content rule holds.
	in = core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{
		{Role: "assistant", Content: ""},
		{Role: "user", Content: "hi"},
	}}
	out, _, err = OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	blocks = decodeMessages(t, out.Messages[1].Content)
	if len(blocks) != 1 || blocks[0]["text"] != "..." {
		t.Fatalf("empty assistant blocks=%#v want placeholder", blocks)
	}
}

func TestMission95OpenAIReasoningEffortLevels(t *testing.T) {
	user := core.OpenAIMessage{Role: "user", Content: "hi"}
	cases := []struct {
		effort string
		budget int
		maxTok int
	}{
		{"low", 2048, 4096},
		{"medium", 8192, 8192 + 1024},
		{"max", 32768, 32768 + 1024},
		{"weird-level", 8192, 8192 + 1024},
	}
	for _, tc := range cases {
		in := core.OpenAIRequest{Model: "x", ReasoningEffort: tc.effort, Messages: []core.OpenAIMessage{user}}
		out, _, err := OpenAIToAnthropic(in, "backend")
		if err != nil {
			t.Fatal(err)
		}
		var th struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}
		if err := json.Unmarshal(out.Thinking, &th); err != nil {
			t.Fatalf("%s: thinking decode: %v", tc.effort, err)
		}
		if th.Type != "enabled" || th.BudgetTokens != tc.budget {
			t.Fatalf("%s: thinking=%+v want budget %d", tc.effort, th, tc.budget)
		}
		if out.MaxTokens != tc.maxTok {
			t.Fatalf("%s: max tokens=%d want %d (must exceed budget)", tc.effort, out.MaxTokens, tc.maxTok)
		}
	}
	// Reasoning objects: an explicit effort, a disabled flag, and malformed
	// JSON that must not enable thinking.
	obj := []struct {
		raw      string
		thinking bool
	}{
		{`{"effort":"HIGH"}`, true},
		{`{"enabled":false,"effort":"high"}`, false},
		{`{"effort":`, false},
	}
	for _, tc := range obj {
		in := core.OpenAIRequest{Model: "x", Reasoning: json.RawMessage(tc.raw), Messages: []core.OpenAIMessage{user}}
		out, _, err := OpenAIToAnthropic(in, "backend")
		if err != nil {
			t.Fatal(err)
		}
		if (len(out.Thinking) > 0) != tc.thinking {
			t.Fatalf("reasoning %s -> thinking present=%v want %v", tc.raw, len(out.Thinking) > 0, tc.thinking)
		}
	}
}

func TestMission95OpenAIStopAndValidation(t *testing.T) {
	user := func(content any) core.OpenAIMessage { return core.OpenAIMessage{Role: "user", Content: content} }
	// []string stop sequences carry across.
	in := core.OpenAIRequest{Model: "x", MaxTokens: 8, Stop: []string{"a", "b"}, Messages: []core.OpenAIMessage{user("hi")}}
	out, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.StopSequences) != 2 || out.StopSequences[0] != "a" || out.StopSequences[1] != "b" {
		t.Fatalf("stop sequences=%v want [a b]", out.StopSequences)
	}
	// Content validation failures: missing part type, non-string text,
	// image without a usable URL, and an unsupported part type.
	bad := []struct {
		name    string
		content any
	}{
		{"missing type", []any{map[string]any{"text": "hi"}}},
		{"non-string text", []any{map[string]any{"type": "text", "text": 7}}},
		{"image no url", []any{map[string]any{"type": "image_url", "image_url": ""}}},
		{"unsupported part", []any{map[string]any{"type": "audio", "text": "x"}}},
		{"non-object part", []any{"just-a-string"}},
		{"map form unsupported", []map[string]any{{"type": "audio"}}},
	}
	for _, tc := range bad {
		in := core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{user(tc.content)}}
		if _, _, err := OpenAIToAnthropic(in, "backend"); err == nil {
			t.Fatalf("%s: invalid content must be rejected", tc.name)
		}
	}
	// []map[string]any content is accepted and translated.
	in = core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{user([]map[string]any{{"type": "text", "text": "map-hi"}})}}
	out, _, err = OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	blocks := decodeMessages(t, out.Messages[0].Content)
	if len(blocks) != 1 || blocks[0]["text"] != "map-hi" {
		t.Fatalf("map content blocks=%#v want map-hi", blocks)
	}
}

func TestMission95OpenAIContentBlockHelpers(t *testing.T) {
	// Non-object parts are skipped; empty text parts produce no block.
	blocks := openAIContentToAnthBlocks([]any{"junk", map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text", "text": "hi"}})
	if len(blocks) != 1 || blocks[0]["text"] != "hi" {
		t.Fatalf("content blocks=%#v want single hi block", blocks)
	}
	// An arbitrary JSON value becomes a JSON text block.
	blocks = openAIContentToAnthBlocks(map[string]any{"a": 1})
	if len(blocks) != 1 || blocks[0]["text"] != `{"a":1}` {
		t.Fatalf("generic content blocks=%#v want JSON text", blocks)
	}
	// Image source shapes.
	if got := openAIImageToAnthSource("https://example.com/a.png"); got == nil || got["url"] != "https://example.com/a.png" {
		t.Fatalf("string image source=%#v", got)
	}
	if got := openAIImageToAnthSource(""); got != nil {
		t.Fatalf("empty image source=%#v want nil", got)
	}
	if got := openAIImageToAnthSource(map[string]any{}); got != nil {
		t.Fatalf("map without url=%#v want nil", got)
	}
	if got := openAIImageToAnthSource("data:image/svg+xml,abc"); got == nil || got["type"] != "url" {
		t.Fatalf("non-base64 data url=%#v want url source", got)
	}
	if got := openAIImageToAnthSource("data:image/png"); got != nil {
		t.Fatalf("data url without comma=%#v want nil", got)
	}
	if got := openAIImageToAnthSource("data:image/jpg;base64,AAAA"); got == nil || got["media_type"] != "image/jpeg" {
		t.Fatalf("jpg data url=%#v want jpeg media type", got)
	}
	// Media type normalization.
	if got := normalizeImageMediaType("IMAGE/JPG"); got != "image/jpeg" {
		t.Fatalf("jpg normalize=%q", got)
	}
	if got := normalizeImageMediaType("image/jpe"); got != "image/jpeg" {
		t.Fatalf("jpe normalize=%q", got)
	}
	if got := normalizeImageMediaType("image/svgz"); got != "image/svg+xml" {
		t.Fatalf("svgz normalize=%q", got)
	}
	if got := normalizeImageMediaType("image/png"); got != "image/png" {
		t.Fatalf("png normalize=%q (passthrough)", got)
	}
	// Tool result content: nil becomes an empty string, anything else
	// passes through untouched.
	if got := normalizeToolResultContent(nil); got != "" {
		t.Fatalf("nil tool result=%#v want empty string", got)
	}
	if got := normalizeToolResultContent("x"); got != "x" {
		t.Fatalf("tool result passthrough=%#v", got)
	}
}

func TestMission95AnthropicResponseThinkingAndFinish(t *testing.T) {
	// Two thinking blocks join with a newline into reasoning_content.
	in := core.AnthResponse{ID: "a1", Content: []core.AnthContentBlock{
		{Type: "thinking", Thinking: "step one"},
		{Type: "thinking", Thinking: "step two"},
		{Type: "text", Text: "done"},
	}}
	out := AnthropicResponseToOpenAI(in, "m", nil)
	if len(out.Choices) != 1 {
		t.Fatalf("choices=%d want 1", len(out.Choices))
	}
	if out.Choices[0].Message.ReasoningContent != "step one\nstep two" {
		t.Fatalf("reasoning=%q want joined thinking", out.Choices[0].Message.ReasoningContent)
	}
	// Finish mapping for max_tokens and refusal.
	for _, tc := range []struct {
		stop string
		want string
	}{
		{"max_tokens", "length"},
		{"refusal", "content_filter"},
	} {
		sr := tc.stop
		in := core.AnthResponse{ID: "a1", StopReason: &sr, Content: []core.AnthContentBlock{{Type: "text", Text: "x"}}}
		out := AnthropicResponseToOpenAI(in, "m", nil)
		if out.Choices[0].FinishReason == nil || *out.Choices[0].FinishReason != tc.want {
			t.Fatalf("stop %s -> finish %v want %s", tc.stop, out.Choices[0].FinishReason, tc.want)
		}
	}
}

func TestMission95FinishReasonToAnthropicStop(t *testing.T) {
	cases := map[string]string{
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"length":         "max_tokens",
		"content_filter": "refusal",
		"stop_sequence":  "stop_sequence",
		"stop":           "end_turn",
		"":               "end_turn",
	}
	for in, want := range cases {
		if got := finishReasonToAnthropicStop(in); got != want {
			t.Fatalf("finishReasonToAnthropicStop(%q)=%q want %q", in, got, want)
		}
	}
}
