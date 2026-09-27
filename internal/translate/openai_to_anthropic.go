package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

const (
	// defaultAnthropicMaxTokens matches the value used by mature relays when a
	// client omits max_tokens (OpenAI allows omission; Anthropic requires it).
	defaultAnthropicMaxTokens = 4096
	// emptyTextPlaceholder replaces empty content, which the Anthropic API
	// rejects ("text: String should have at least 1 character").
	emptyTextPlaceholder = "..."
)

// OpenAIToAnthropic converts an OpenAI Chat Completions request into an
// Anthropic Messages request. It enforces the two structural invariants that
// real Anthropic upstreams hard-reject and naive relays get wrong:
//
//  1. Strict role alternation — consecutive same-role messages (including the
//     user messages produced by OpenAI tool results) are merged into single
//     messages with multiple content blocks.
//  2. The first message must have role "user" — a placeholder user message is
//     prepended when a conversation opens with an assistant turn.
//
// Tool names are sanitized through a reversible NameMap (MCP-style names with
// dots/colons/length overflow), malformed tool arguments are preserved in a
// {"_raw": ...} object instead of failing the request, and thinking budgets
// derived from reasoning_effort keep the max_tokens > budget_tokens invariant.
func OpenAIToAnthropic(in core.OpenAIRequest, model string) (core.AnthropicRequest, *NameMap, error) {
	out := core.AnthropicRequest{Model: model, Stream: in.Stream, Temperature: in.Temperature, TopP: in.TopP}
	out.MaxTokens = in.MaxTokens
	if out.MaxTokens <= 0 {
		out.MaxTokens = in.MaxCompletionTokens
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultAnthropicMaxTokens
	}

	names := make([]string, 0, len(in.Tools))
	for _, t := range in.Tools {
		names = append(names, t.Function.Name)
	}
	nm := NewAnthropicNameMap(names)

	budget, thinkingEnabled := reasoningEffortToThinkingBudget(openAIEffort(in))
	if thinkingEnabled {
		out.Thinking, _ = json.Marshal(map[string]any{"type": "enabled", "budget_tokens": budget})
		if out.MaxTokens <= budget {
			// Anthropic requires max_tokens > thinking.budget_tokens; raise the
			// ceiling instead of dropping the caller's reasoning request.
			out.MaxTokens = budget + 1024
		}
	}

	type pendingMsg struct {
		role           string
		blocks         []map[string]any
		allToolResults bool
	}
	var messages []pendingMsg
	assistantHasToolUse := false

	flushInto := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(messages); n > 0 && messages[n-1].role == role {
			messages[n-1].blocks = append(messages[n-1].blocks, blocks...)
			return
		}
		messages = append(messages, pendingMsg{role: role, blocks: blocks})
	}

	for _, m := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			if err := validateOpenAIContentForAnthropic(m.Content, false); err != nil {
				return out, nm, fmt.Errorf("%s message content: %w", role, err)
			}
			var sysText strings.Builder
			for _, b := range openAIContentToAnthBlocks(m.Content) {
				if b["type"] == "text" {
					if sysText.Len() > 0 {
						sysText.WriteByte('\n')
					}
					sysText.WriteString(fmt.Sprint(b["text"]))
				}
			}
			if sysText.Len() > 0 {
				if out.System != nil && len(out.System) > 0 {
					var prev string
					_ = json.Unmarshal(out.System, &prev)
					out.System, _ = json.Marshal(strings.TrimSpace(prev + "\n" + sysText.String()))
				} else {
					out.System, _ = json.Marshal(sysText.String())
				}
			}
			continue
		case "user", "assistant", "tool":
		default:
			return out, nm, fmt.Errorf("unsupported OpenAI message role %q for Anthropic translation", m.Role)
		}
		if err := validateOpenAIContentForAnthropic(m.Content, role != "tool"); err != nil {
			return out, nm, fmt.Errorf("%s message content: %w", role, err)
		}
		blocks := openAIContentToAnthBlocks(m.Content)

		switch role {
		case "assistant":
			assistantBlocks := make([]map[string]any, 0, len(blocks)+len(m.ToolCalls))
			assistantBlocks = append(assistantBlocks, blocks...)
			for i, tc := range m.ToolCalls {
				if strings.TrimSpace(tc.Function.Name) == "" {
					return out, nm, fmt.Errorf("assistant tool call requires non-empty function name")
				}
				obj := map[string]any{}
				if strings.TrimSpace(tc.Function.Arguments) != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &obj); err != nil {
						// Preserve the malformed payload instead of rejecting
						// the whole conversation history.
						obj = map[string]any{"_raw": tc.Function.Arguments}
					}
				}
				id := strings.TrimSpace(tc.ID)
				if id == "" {
					id = fmt.Sprintf("toolu_openai_%d", len(messages)+i)
				}
				assistantBlocks = append(assistantBlocks, map[string]any{
					"type": "tool_use", "id": id, "name": nm.Forward(tc.Function.Name), "input": obj,
				})
				assistantHasToolUse = true
			}
			if len(assistantBlocks) == 0 {
				assistantBlocks = append(assistantBlocks, map[string]any{"type": "text", "text": emptyTextPlaceholder})
			}
			flushInto("assistant", assistantBlocks)
		case "tool":
			if strings.TrimSpace(m.ToolCallID) == "" {
				return out, nm, fmt.Errorf("tool message requires non-empty tool_call_id")
			}
			// Parallel OpenAI tool results arrive as consecutive tool messages;
			// they must coalesce into ONE user message with multiple
			// tool_result blocks or Anthropic rejects the alternation.
			toolResults := []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     normalizeToolResultContent(m.Content),
			}}
			if n := len(messages); n > 0 && messages[n-1].role == "user" && messages[n-1].allToolResults {
				messages[n-1].blocks = append(messages[n-1].blocks, toolResults...)
			} else {
				messages = append(messages, pendingMsg{role: "user", blocks: toolResults, allToolResults: true})
			}
		case "user":
			if len(blocks) == 0 {
				blocks = []map[string]any{{"type": "text", "text": emptyTextPlaceholder}}
			}
			flushInto("user", blocks)
		}
	}

	// Anthropic rejects "Expected thinking or redacted_thinking, but found
	// tool_use": when reasoning is requested but the replayed assistant turns
	// contain tool_use without any signed thinking blocks, drop the thinking
	// request instead of letting every upstream call 400.
	if thinkingEnabled && assistantHasToolUse {
		out.Thinking = nil
	}

	if len(messages) == 0 {
		return out, nm, fmt.Errorf("no messages after translation")
	}
	// Anthropic requires the first message to be a user turn.
	if messages[0].role != "user" {
		messages = append([]pendingMsg{{role: "user", blocks: []map[string]any{{"type": "text", "text": emptyTextPlaceholder}}}}, messages...)
	}
	for _, p := range messages {
		raw, err := json.Marshal(p.blocks)
		if err != nil {
			return out, nm, fmt.Errorf("translate OpenAI message content: %w", err)
		}
		out.Messages = append(out.Messages, core.AnthMessage{Role: p.role, Content: raw})
	}

	for _, t := range in.Tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object"}
		} else if _, ok := schema["type"]; !ok {
			clone := make(map[string]any, len(schema)+1)
			for k, v := range schema {
				clone[k] = v
			}
			clone["type"] = "object"
			schema = clone
		}
		out.Tools = append(out.Tools, core.AnthTool{Name: nm.Forward(t.Function.Name), Description: t.Function.Description, InputSchema: schema})
	}
	applyOpenAIToolChoiceToAnthropic(&out, in.ToolChoice, nm)
	applyOpenAIStopToAnthropic(&out, in.Stop)
	if strings.TrimSpace(in.User) != "" {
		if out.Metadata == nil || len(out.Metadata) == 0 {
			out.Metadata, _ = json.Marshal(map[string]any{"user_id": strings.TrimSpace(in.User)})
		}
	}
	return out, nm, nil
}

// openAIEffort resolves the reasoning effort from either reasoning_effort or a
// structured reasoning {effort: ...} object (some clients use either shape).
func openAIEffort(in core.OpenAIRequest) string {
	effort := strings.ToLower(strings.TrimSpace(in.ReasoningEffort))
	if effort != "" {
		return effort
	}
	if len(in.Reasoning) > 0 && string(in.Reasoning) != "null" {
		var cfg struct {
			Effort  string `json:"effort"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.Unmarshal(in.Reasoning, &cfg); err == nil {
			if cfg.Enabled != nil && !*cfg.Enabled {
				return "none"
			}
			return strings.ToLower(strings.TrimSpace(cfg.Effort))
		}
	}
	return ""
}

// reasoningEffortToThinkingBudget maps OpenAI reasoning effort levels onto
// conservative Anthropic thinking budgets.
func reasoningEffortToThinkingBudget(effort string) (int, bool) {
	switch effort {
	case "none", "off", "disabled", "":
		return 0, false
	case "minimal", "low":
		return 2048, true
	case "medium":
		return 8192, true
	case "high":
		return 16384, true
	case "xhigh", "max":
		return 32768, true
	default:
		return 8192, true
	}
}

func applyOpenAIToolChoiceToAnthropic(out *core.AnthropicRequest, choice any, names *NameMap) {
	switch v := choice.(type) {
	case string:
		switch v {
		case "required":
			out.ToolChoice = map[string]any{"type": "any"}
		case "none":
			out.ToolChoice = map[string]any{"type": "none"}
		case "auto":
			out.ToolChoice = map[string]any{"type": "auto"}
		}
	case map[string]any:
		if f, ok := v["function"].(map[string]any); ok {
			if name, _ := f["name"].(string); name != "" {
				if names != nil {
					name = names.Forward(name)
				}
				out.ToolChoice = map[string]any{"type": "tool", "name": name}
			}
		}
	}
}

// applyOpenAIStopToAnthropic carries stop sequences across the protocol
// boundary; dropping them would make stop-controlled clients misbehave.
func applyOpenAIStopToAnthropic(out *core.AnthropicRequest, stop any) {
	switch v := stop.(type) {
	case string:
		if v != "" {
			out.StopSequences = []string{v}
		}
	case []string:
		if len(v) > 0 {
			out.StopSequences = append([]string(nil), v...)
		}
	case []any:
		var seqs []string
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				seqs = append(seqs, s)
			}
		}
		if len(seqs) > 0 {
			out.StopSequences = seqs
		}
	}
}

func validateOpenAIContentForAnthropic(content any, allowImage bool) error {
	validatePart := func(m map[string]any) error {
		typ, ok := m["type"].(string)
		if !ok || strings.TrimSpace(typ) == "" {
			return fmt.Errorf("content part type is required")
		}
		switch typ {
		case "text", "input_text":
			if _, ok := m["text"].(string); !ok {
				return fmt.Errorf("%s content part requires string text", typ)
			}
		case "image_url":
			if !allowImage {
				return fmt.Errorf("image_url is not supported in this message role")
			}
			if openAIImageToAnthSource(m["image_url"]) == nil {
				return fmt.Errorf("image_url content part has no usable URL")
			}
		default:
			return fmt.Errorf("unsupported OpenAI content part type %q for Anthropic translation", typ)
		}
		return nil
	}

	switch v := content.(type) {
	case nil, string:
		return nil
	case []any:
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("content array contains a non-object part")
			}
			if err := validatePart(m); err != nil {
				return err
			}
		}
		return nil
	case []map[string]any:
		for _, m := range v {
			if err := validatePart(m); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported OpenAI content value type %T", content)
	}
}

func openAIContentToAnthBlocks(content any) []map[string]any {
	out := []map[string]any{}
	switch v := content.(type) {
	case string:
		if v != "" {
			out = append(out, map[string]any{"type": "text", "text": v})
		}
	case []any:
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text":
				txt, _ := m["text"].(string)
				if txt != "" {
					out = append(out, map[string]any{"type": "text", "text": txt})
				}
			case "image_url":
				if src := openAIImageToAnthSource(m["image_url"]); src != nil {
					out = append(out, map[string]any{"type": "image", "source": src})
				}
			}
		}
	case []map[string]any:
		for _, m := range v {
			for _, b := range openAIContentToAnthBlocks([]any{m}) {
				out = append(out, b)
			}
		}
	case nil:
	default:
		if b, err := json.Marshal(v); err == nil {
			out = append(out, map[string]any{"type": "text", "text": string(b)})
		}
	}
	return out
}

func openAIImageToAnthSource(v any) map[string]any {
	var u string
	switch x := v.(type) {
	case string:
		u = x
	case map[string]any:
		u, _ = x["url"].(string)
	}
	u = strings.TrimSpace(u)
	if u == "" {
		return nil
	}
	if strings.HasPrefix(u, "data:") {
		rest := strings.TrimPrefix(u, "data:")
		parts := strings.SplitN(rest, ",", 2)
		if len(parts) == 2 {
			mt := strings.TrimSuffix(parts[0], ";base64")
			if strings.HasSuffix(parts[0], ";base64") {
				return map[string]any{"type": "base64", "media_type": normalizeImageMediaType(mt), "data": parts[1]}
			}
			// Inline non-base64 data URLs (svg+xml, plain text) cannot be
			// represented as an Anthropic base64 source; keep them as URL
			// sources so upstream providers that support them still work.
			return map[string]any{"type": "url", "url": u}
		}
		return nil
	}
	return map[string]any{"type": "url", "url": u}
}

// normalizeImageMediaType maps common non-standard image MIME types onto the
// set the Anthropic API accepts (png/jpeg/gif/webp).
func normalizeImageMediaType(mt string) string {
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch mt {
	case "image/jpg":
		return "image/jpeg"
	case "image/jpe", "image/jp2", "image/pipeg":
		return "image/jpeg"
	case "image/svg", "image/svgz":
		return "image/svg+xml"
	default:
		return mt
	}
}

func normalizeToolResultContent(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// AnthropicResponseToOpenAI converts an Anthropic Messages response into an
// OpenAI Chat Completions response. Thinking text is surfaced through the
// widely-adopted reasoning_content field (safe: OpenAI clients never replay it
// to an Anthropic upstream with signature requirements).
func AnthropicResponseToOpenAI(in core.AnthResponse, requestedModel string, nm *NameMap) core.OpenAIResponse {
	msg := core.OpenAIMessage{Role: "assistant"}
	text := ""
	reasoning := ""
	calls := []core.OpenAIToolCall{}
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			text += b.Text
		case "tool_use":
			arg, _ := json.Marshal(b.Input)
			calls = append(calls, core.OpenAIToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: core.OpenAIFunctionCall{Name: nm.Reverse(b.Name), Arguments: string(arg)},
			})
		case "thinking":
			if b.Thinking != "" {
				if reasoning != "" {
					reasoning += "\n"
				}
				reasoning += b.Thinking
			}
		}
	}
	if text != "" || len(calls) == 0 {
		msg.Content = text
	}
	if reasoning != "" {
		msg.ReasoningContent = reasoning
	}
	msg.ToolCalls = calls
	finish := "stop"
	if in.StopReason != nil {
		switch *in.StopReason {
		case "tool_use":
			finish = "tool_calls"
		case "max_tokens":
			finish = "length"
		case "refusal":
			finish = "content_filter"
		}
	}
	usage := core.OpenAIUsage{
		PromptTokens:     in.Usage.InputTokens,
		CompletionTokens: in.Usage.OutputTokens,
		TotalTokens:      in.Usage.InputTokens + in.Usage.OutputTokens,
	}
	if in.Usage.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &core.OpenAIUsageDetail{CachedTokens: in.Usage.CacheReadInputTokens}
	}
	return core.OpenAIResponse{
		ID:      in.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   requestedModel,
		Choices: []core.OpenAIChoice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage:   usage,
	}
}
