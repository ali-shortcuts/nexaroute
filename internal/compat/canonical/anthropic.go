package canonical

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// Canonical IR -> Anthropic Messages encoder. It upholds the same structural
// invariants as translate.OpenAIToAnthropic (role alternation, user-first
// conversation, tool_result coalescing, max_tokens default, thinking budget
// rules), so Responses ingress and future canonical-routed traffic reach
// Anthropic upstreams with a valid envelope. Unsigned reasoning parts are
// dropped rather than replayed: Anthropic rejects thinking blocks without a
// signature, and fabricating them would poison the conversation.

// defaultAnthropicMaxTokens matches the relay convention when the caller
// omits max_tokens (Responses max_output_tokens is optional; Anthropic
// requires max_tokens).
const defaultAnthropicMaxTokens = 4096

// emptyAnthropicText replaces empty content, which the Anthropic API rejects.
const emptyAnthropicText = "..."

// ToAnthropicRequest encodes a canonical request into an Anthropic Messages
// request for the given upstream model.
func (r Request) ToAnthropicRequest(model string) core.AnthropicRequest {
	out := core.AnthropicRequest{
		Model: model, Stream: r.Stream,
		Temperature: r.Temperature, TopP: r.TopP,
		StopSequences: append([]string(nil), r.Stop...),
	}
	out.MaxTokens = r.MaxTokens
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultAnthropicMaxTokens
	}
	if r.System != "" {
		out.System, _ = json.Marshal(r.System)
	}
	appendSystem := func(text string) {
		if text == "" {
			return
		}
		if len(out.System) == 0 {
			out.System, _ = json.Marshal(text)
			return
		}
		var prev string
		_ = json.Unmarshal(out.System, &prev)
		combined := strings.TrimSpace(prev + "\n" + text)
		out.System, _ = json.Marshal(combined)
	}

	if budget, ok := canonicalThinkingBudget(r.Reasoning); ok {
		out.Thinking, _ = json.Marshal(map[string]any{"type": "enabled", "budget_tokens": budget})
		if out.MaxTokens <= budget {
			out.MaxTokens = budget + 1024
		}
	}

	type pendingMsg struct {
		role           string
		blocks         []map[string]any
		allToolResults bool
	}
	messages := []pendingMsg{}
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

	for _, m := range r.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			texts := []string{}
			for _, p := range m.Parts {
				if p.Kind == ContentText && p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
			appendSystem(strings.Join(texts, "\n"))
			continue
		case "user", "assistant", "tool":
		case "":
			role = "user"
		default:
			// Unknown roles never dead-end the request; they degrade to
			// user turns, matching the lenient canonical philosophy.
			role = "user"
		}
		if role == "tool" {
			results := []map[string]any{}
			for _, p := range m.Parts {
				if p.Kind != ContentToolResult || strings.TrimSpace(p.ToolUseID) == "" {
					continue
				}
				block := map[string]any{
					"type": "tool_result", "tool_use_id": p.ToolUseID,
					"content": p.ToolContent,
				}
				if p.ToolError {
					block["is_error"] = true
				}
				results = append(results, block)
			}
			if len(results) == 0 {
				continue
			}
			if n := len(messages); n > 0 && messages[n-1].role == "user" && messages[n-1].allToolResults {
				messages[n-1].blocks = append(messages[n-1].blocks, results...)
			} else {
				messages = append(messages, pendingMsg{role: "user", blocks: results, allToolResults: true})
			}
			continue
		}
		blocks := []map[string]any{}
		toolResults := []map[string]any{}
		for _, p := range m.Parts {
			switch p.Kind {
			case ContentText:
				if p.Text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				}
			case ContentImage:
				if src := canonicalImageToAnthSource(p.ImageURL, p.MediaType); src != nil {
					blocks = append(blocks, map[string]any{"type": "image", "source": src})
				}
			case ContentToolCall:
				if role != "assistant" || strings.TrimSpace(p.ToolName) == "" {
					continue
				}
				id := strings.TrimSpace(p.ToolCallID)
				if id == "" {
					id = fmt.Sprintf("toolu_canon_%d", len(messages))
				}
				input := p.ToolInput
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": id, "name": p.ToolName, "input": input,
				})
				assistantHasToolUse = true
			case ContentToolResult:
				if strings.TrimSpace(p.ToolUseID) == "" {
					continue
				}
				block := map[string]any{
					"type": "tool_result", "tool_use_id": p.ToolUseID,
					"content": p.ToolContent,
				}
				if p.ToolError {
					block["is_error"] = true
				}
				toolResults = append(toolResults, block)
			case ContentReasoning:
				// Dropped: unsigned thinking must never be replayed to an
				// Anthropic upstream (signature requirement).
			}
		}
		if role == "assistant" {
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": emptyAnthropicText})
			}
			flushInto("assistant", blocks)
			// Tool results embedded in an assistant turn belong to the
			// following user turn.
			for _, tr := range toolResults {
				flushInto("user", []map[string]any{tr})
			}
			continue
		}
		if len(toolResults) > 0 && len(blocks) == 0 {
			if n := len(messages); n > 0 && messages[n-1].role == "user" && messages[n-1].allToolResults {
				messages[n-1].blocks = append(messages[n-1].blocks, toolResults...)
			} else {
				messages = append(messages, pendingMsg{role: "user", blocks: toolResults, allToolResults: true})
			}
			continue
		}
		blocks = append(blocks, toolResults...)
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": emptyAnthropicText})
		}
		flushInto("user", blocks)
	}

	// Anthropic rejects thinking requests over histories that contain
	// tool_use without signed thinking blocks; drop the request instead of
	// letting every upstream call 400.
	if assistantHasToolUse && len(out.Thinking) > 0 {
		out.Thinking = nil
	}

	if len(messages) == 0 {
		messages = append(messages, pendingMsg{role: "user", blocks: []map[string]any{{"type": "text", "text": emptyAnthropicText}}})
	}
	if messages[0].role != "user" {
		messages = append([]pendingMsg{{role: "user", blocks: []map[string]any{{"type": "text", "text": emptyAnthropicText}}}}, messages...)
	}
	for _, p := range messages {
		raw, err := json.Marshal(p.blocks)
		if err != nil {
			continue
		}
		out.Messages = append(out.Messages, core.AnthMessage{Role: p.role, Content: raw})
	}

	for _, t := range r.Tools {
		schema := t.Parameters
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
		out.Tools = append(out.Tools, core.AnthTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	switch {
	case r.ToolChoice == "auto":
		out.ToolChoice = map[string]any{"type": "auto"}
	case r.ToolChoice == "required", r.ToolChoice == "any":
		out.ToolChoice = map[string]any{"type": "any"}
	case r.ToolChoice == "none":
		out.ToolChoice = map[string]any{"type": "none"}
	case strings.HasPrefix(r.ToolChoice, "named:"):
		if name := strings.TrimPrefix(r.ToolChoice, "named:"); name != "" {
			out.ToolChoice = map[string]any{"type": "tool", "name": name}
		}
	}
	if r.Metadata != nil {
		if user, _ := r.Metadata["user"].(string); strings.TrimSpace(user) != "" {
			out.Metadata, _ = json.Marshal(map[string]any{"user_id": strings.TrimSpace(user)})
		}
	}
	return out
}

func toolResultBlocks(in []map[string]any) []map[string]any {
	if len(in) == 0 {
		return nil
	}
	return in
}

// canonicalThinkingBudget maps normalized reasoning controls onto an
// Anthropic thinking budget, mirroring translate's conservative table.
func canonicalThinkingBudget(r *Reasoning) (int, bool) {
	if r == nil || !r.Enabled {
		return 0, false
	}
	if r.Budget > 0 {
		return r.Budget, true
	}
	switch strings.ToLower(strings.TrimSpace(r.Effort)) {
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

// canonicalImageToAnthSource converts a normalized image reference into an
// Anthropic image source. Base64 data URLs become base64 sources; remote
// URLs pass through as URL sources for providers that accept them.
func canonicalImageToAnthSource(imageURL, mediaType string) map[string]any {
	u := strings.TrimSpace(imageURL)
	if u == "" {
		return nil
	}
	if strings.HasPrefix(u, "data:") {
		rest := strings.TrimPrefix(u, "data:")
		parts := strings.SplitN(rest, ",", 2)
		if len(parts) != 2 || parts[1] == "" {
			return nil
		}
		if !strings.HasSuffix(parts[0], ";base64") {
			return map[string]any{"type": "url", "url": u}
		}
		mt := strings.TrimSuffix(parts[0], ";base64")
		if mt == "" {
			mt = mediaType
		}
		if mt == "" {
			mt = "image/png"
		}
		return map[string]any{"type": "base64", "media_type": canonicalMediaType(mt), "data": parts[1]}
	}
	return map[string]any{"type": "url", "url": u}
}

func canonicalMediaType(mt string) string {
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "image/jpg", "image/jpe", "image/jp2", "image/pipeg":
		return "image/jpeg"
	case "image/svg", "image/svgz":
		return "image/svg+xml"
	default:
		return mt
	}
}
