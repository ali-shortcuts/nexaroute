package canonical

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// ---------- Anthropic Messages <-> Canonical ----------

// DecodeAnthropicRequest converts an Anthropic Messages request into the IR.
func DecodeAnthropicRequest(in core.AnthropicRequest, clientModel string) (Request, error) {
	out := Request{
		Model:           clientModel,
		MaxOutputTokens: in.MaxTokens,
		Stream:          in.Stream,
		ClientProtocol:  "anthropic",
	}
	if in.Temperature != nil {
		t := *in.Temperature
		out.Temperature = &t
	}
	if in.TopP != nil {
		p := *in.TopP
		out.TopP = &p
	}
	if in.TopK > 0 {
		k := float64(in.TopK)
		out.TopK = &k
	}
	out.Stop = append([]string(nil), in.StopSequences...)
	sys, err := decodeAnthropicSystem(in.System)
	if err != nil {
		return out, err
	}
	out.System = sys
	for _, m := range in.Messages {
		parts, err := decodeAnthropicContent(m.Content)
		if err != nil {
			return out, err
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != RoleUser && role != RoleAssistant {
			return out, fmt.Errorf("unsupported Anthropic message role %q", m.Role)
		}
		out.Messages = append(out.Messages, Message{Role: role, Parts: parts})
	}
	out.Tools = make([]ToolDef, 0, len(in.Tools))
	for _, t := range in.Tools {
		out.Tools = append(out.Tools, ToolDef{Name: t.Name, Description: t.Description, Parameters: marshalSchema(t.InputSchema)})
	}
	out.ToolChoice = decodeAnthropicToolChoice(in.ToolChoice)
	out.Reasoning = decodeAnthropicThinking(in.Thinking)
	if len(in.Metadata) > 0 {
		var meta map[string]any
		if json.Unmarshal(in.Metadata, &meta) == nil && len(meta) > 0 {
			out.Metadata = meta
		}
	}
	return out, nil
}

func marshalSchema(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

func decodeAnthropicSystem(raw json.RawMessage) ([]Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []Part{{Type: PartText, Text: text}}, nil
	}
	var blocks []core.AnthBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("invalid Anthropic system content: %w", err)
	}
	var parts []Part
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, Part{Type: PartText, Text: b.Text, CacheControl: anthCacheValue(b.CacheControl)})
		}
		// Non-text system blocks carry no canonical meaning; they
		// survive only on native passthrough.
	}
	return parts, nil
}

// anthCacheValue flattens an Anthropic cache_control block marker
// into the IR's string form: the TTL when one is set, otherwise
// the marker type ("ephemeral").
func anthCacheValue(cc *core.AnthCacheControl) string {
	if cc == nil {
		return ""
	}
	if cc.TTL != "" {
		return cc.TTL
	}
	if cc.Type != "" {
		return cc.Type
	}
	return "ephemeral"
}

// anthCacheBlock renders an IR marker back into wire form.
func anthCacheBlock(v string) map[string]any {
	out := map[string]any{"type": "ephemeral"}
	if v == "1h" || v == "5m" {
		out["ttl"] = v
	}
	return out
}

// anthToolInputArgs canonicalizes a decoded Anthropic tool_use input into
// the IR arguments string. Both Anthropic encoders (EncodeAnthropicResponse
// and encodeAnthBlocks) marshal arguments back through map[string]any with
// `input,omitempty`, so missing, null, and empty-object inputs are all
// indistinguishable on the wire — the input is simply omitted. If the
// decoder kept them distinct ("null" vs "{}"), decode(encode(decode(x)))
// would be unstable (FuzzResponseRoundTrip found exactly this: {} -> null).
// Collapse those shapes to "{}", the canonical form the encoder converges
// to, mirroring the unsigned-thinking fix: the decoder keeps exactly what
// survives the encoder. Non-empty inputs are already canonical (the decode
// unmarshal sorts keys and compacts), so they pass through verbatim.
func anthToolInputArgs(in map[string]any) string {
	if len(in) == 0 {
		return "{}"
	}
	out, err := json.Marshal(in)
	if err != nil {
		return "{}" // unreachable for JSON-decoded input; fail closed
	}
	return string(out)
}

func decodeAnthropicContent(raw json.RawMessage) ([]Part, error) {
	blocks, err := core.ParseAnthContent(raw)
	if err != nil {
		return nil, err
	}
	parts := make([]Part, 0, len(blocks))
	for _, m := range blocks {
		switch m.Type {
		case "text":
			parts = append(parts, Part{Type: PartText, Text: m.Text, CacheControl: anthCacheValue(m.CacheControl)})
		case "image":
			img := decodeAnthImage(m.Source)
			if img == nil {
				return nil, fmt.Errorf("anthropic image block has no usable source")
			}
			parts = append(parts, Part{Type: PartImage, Image: img})
		case "tool_use":
			// Nameless tool_use blocks are dropped at decode: the
			// Anthropic request encoder never emits them (it skips
			// nameless PartToolCall parts), so keeping one breaks
			// decode(encode(decode(x))) symmetry (#447, same class as
			// the OpenAI nameless drops in #403).
			if m.Name == "" {
				continue
			}
			parts = append(parts, Part{Type: PartToolCall, ToolCall: &ToolCall{ID: m.ID, Name: m.Name, Arguments: anthToolInputArgs(m.Input)}, CacheControl: anthCacheValue(m.CacheControl)})
		case "tool_result":
			parts = append(parts, Part{Type: PartToolResult, ToolResult: &ToolResult{
				ToolUseID: m.ToolUseID,
				Content:   anthToolResultText(m),
				IsError:   m.IsError,
			}, CacheControl: anthCacheValue(m.CacheControl)})
		case "thinking", "redacted_thinking":
			var tb struct {
				Thinking         string `json:"thinking"`
				Signature        string `json:"signature"`
				RedactedThinking string `json:"redacted_thinking"`
			}
			_ = json.Unmarshal(m.Content, &tb)
			if m.Type == "redacted_thinking" {
				parts = append(parts, Part{Type: PartThinking, Thinking: &Thinking{Text: tb.RedactedThinking, Signature: "redacted"}})
			} else {
				parts = append(parts, Part{Type: PartThinking, Thinking: &Thinking{Text: tb.Thinking, Signature: tb.Signature}})
			}
		default:
			// Fail closed rather than silently losing client content when an
			// ingress feature has no canonical representation.
			return nil, fmt.Errorf("unsupported Anthropic content block %q", m.Type)
		}
	}
	return parts, nil
}

func anthToolResultText(b core.AnthBlock) string {
	if len(b.Content) == 0 || string(b.Content) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	var blocks []map[string]any
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var sb strings.Builder
		for _, rb := range blocks {
			typ, _ := rb["type"].(string)
			if typ == "text" {
				if txt, _ := rb["text"].(string); txt != "" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(txt)
				}
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return string(b.Content)
}

func decodeAnthImage(src map[string]any) *Image {
	if src == nil {
		return nil
	}
	typ, _ := src["type"].(string)
	if typ == "base64" {
		mt, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if mt != "" && data != "" {
			return &Image{MediaType: mt, Data: data}
		}
	}
	if typ == "url" {
		if u, _ := src["url"].(string); u != "" {
			return &Image{URL: u}
		}
	}
	if u, _ := src["url"].(string); u != "" {
		return &Image{URL: u}
	}
	return nil
}

func decodeAnthropicToolChoice(v any) *ToolChoice {
	switch c := v.(type) {
	case string:
		switch c {
		case "auto", "none":
			return &ToolChoice{Mode: c}
		case "any", "required":
			return &ToolChoice{Mode: "required"}
		}
	case map[string]any:
		typ, _ := c["type"].(string)
		switch typ {
		case "auto":
			return &ToolChoice{Mode: "auto"}
		case "none":
			return &ToolChoice{Mode: "none"}
		case "any", "required":
			return &ToolChoice{Mode: "required"}
		case "tool":
			name, _ := c["name"].(string)
			if name != "" {
				return &ToolChoice{Mode: "tool", Name: name}
			}
		}
	}
	return nil
}

func decodeAnthropicThinking(raw json.RawMessage) *Reasoning {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var cfg struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	switch cfg.Type {
	case "enabled", "adaptive":
		effort := "medium"
		switch {
		case cfg.Type == "adaptive" && cfg.BudgetTokens <= 0:
			effort = "high"
		case cfg.BudgetTokens > 0 && cfg.BudgetTokens <= 2048:
			effort = "low"
		case cfg.BudgetTokens > 12288:
			effort = "high"
		}
		return &Reasoning{Effort: effort, BudgetTokens: cfg.BudgetTokens}
	default:
		return &Reasoning{Disabled: true}
	}
}

// EncodeAnthropicRequest builds an Anthropic Messages payload from the IR.
// It is the encoder used when routing canonical traffic to
// anthropic-compatible upstreams.
func EncodeAnthropicRequest(in Request, upstreamModel string) (core.AnthropicRequest, error) {
	out := core.AnthropicRequest{
		Model:         upstreamModel,
		MaxTokens:     in.MaxOutputTokens,
		Stream:        in.Stream,
		StopSequences: append([]string(nil), in.Stop...),
	}
	if in.MaxOutputTokens <= 0 {
		out.MaxTokens = 4096 // Anthropic requires an explicit budget.
	}
	if in.Temperature != nil {
		t := *in.Temperature
		out.Temperature = &t
	}
	if in.TopP != nil {
		p := *in.TopP
		out.TopP = &p
	}
	if in.TopK != nil {
		out.TopK = int(*in.TopK)
	}
	if len(in.System) > 0 {
		marked := false
		for _, p := range in.System {
			if p.CacheControl != "" {
				marked = true
				break
			}
		}
		if marked {
			// The joined-string system form cannot carry cache
			// markers; emit blocks so the markers survive.
			if blocks := encodeAnthBlocks(in.System); len(blocks) > 0 {
				out.System = json.RawMessage(mustJSON(blocks))
			}
		} else {
			var sb strings.Builder
			for _, p := range in.System {
				if p.Type == PartText && p.Text != "" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(p.Text)
				}
			}
			if sb.Len() > 0 {
				out.System = json.RawMessage(mustJSON(sb.String()))
			}
		}
	}
	for _, m := range in.Messages {
		blocks := encodeAnthBlocks(m.Parts)
		if len(blocks) == 0 {
			blocks = []map[string]any{{"type": "text", "text": ""}}
		}
		// Anthropic has no "tool" role: tool results travel as
		// tool_result blocks inside a USER turn (the IR keeps them in
		// dedicated RoleTool messages for OpenAI-shaped dialects).
		// Emitting role "tool" verbatim produced conversations the
		// real Anthropic API rejects with a 400 (M5-2 conformance).
		role := m.Role
		if role != RoleAssistant {
			role = RoleUser
		}
		out.Messages = append(out.Messages, core.AnthMessage{Role: role, Content: mustJSON(blocks)})
	}
	if len(in.Tools) > 0 {
		names := make([]string, 0, len(in.Tools))
		for _, t := range in.Tools {
			names = append(names, t.Name)
			schema := map[string]any{"type": "object"}
			if len(t.Parameters) > 0 {
				var parsed map[string]any
				if json.Unmarshal(t.Parameters, &parsed) == nil && parsed != nil {
					schema = parsed
				}
			}
			out.Tools = append(out.Tools, core.AnthTool{Name: t.Name, Description: t.Description, InputSchema: schema})
		}
		_ = names
	}
	if in.ToolChoice != nil {
		switch in.ToolChoice.Mode {
		case "auto":
			out.ToolChoice = map[string]any{"type": "auto"}
		case "none":
			out.ToolChoice = map[string]any{"type": "none"}
		case "required":
			out.ToolChoice = map[string]any{"type": "any"}
		case "tool":
			out.ToolChoice = map[string]any{"type": "tool", "name": in.ToolChoice.Name}
		}
	}
	if in.ParallelToolCalls != nil && !*in.ParallelToolCalls && in.ToolChoice != nil && in.ToolChoice.Mode != "none" {
		// Anthropic disables parallel calls inside tool_choice.
		tc, _ := out.ToolChoice.(map[string]any)
		if tc != nil {
			tc["disable_parallel_tool_use"] = true
		}
	}
	if in.Reasoning != nil && !in.Reasoning.Disabled {
		budget := in.Reasoning.BudgetTokens
		if budget <= 0 {
			switch in.Reasoning.Effort {
			case "low":
				budget = 2048
			case "high":
				budget = 16384
			default:
				budget = 8192
			}
		}
		if budget >= 1024 {
			// Anthropic requires max_tokens > thinking.budget_tokens.
			// The legacy translator raises the ceiling the same way; without
			// this the API rejects default and high-effort thinking requests.
			if out.MaxTokens <= budget {
				out.MaxTokens = budget + 1024
			}
			out.Thinking = json.RawMessage(mustJSON(map[string]any{"type": "enabled", "budget_tokens": budget}))
		}
	}
	return out, nil
}

func encodeAnthBlocks(parts []Part) []map[string]any {
	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		before := len(blocks)
		switch p.Type {
		case PartText:
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		case PartImage:
			if p.Image == nil {
				continue
			}
			if p.Image.URL != "" && !strings.HasPrefix(p.Image.URL, "data:") {
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": p.Image.URL}})
				continue
			}
			mt, data := p.Image.MediaType, p.Image.Data
			if strings.HasPrefix(p.Image.URL, "data:") {
				if parsed := parseDataURL(p.Image.URL); parsed != nil {
					mt, data = parsed.mediaType, parsed.data
				}
			}
			if mt == "" || data == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mt, "data": data}})
		case PartToolCall:
			if p.ToolCall == nil || p.ToolCall.Name == "" {
				continue
			}
			input := map[string]any{}
			if strings.TrimSpace(p.ToolCall.Arguments) != "" {
				_ = json.Unmarshal([]byte(p.ToolCall.Arguments), &input)
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": p.ToolCall.ID, "name": p.ToolCall.Name, "input": input})
		case PartToolResult:
			if p.ToolResult == nil || p.ToolResult.ToolUseID == "" {
				continue
			}
			block := map[string]any{"type": "tool_result", "tool_use_id": p.ToolResult.ToolUseID, "content": p.ToolResult.Content}
			if p.ToolResult.IsError {
				block["is_error"] = true
			}
			blocks = append(blocks, block)
		case PartThinking:
			// Unsigned thinking cannot be replayed to Anthropic upstreams.
			if p.Thinking != nil && p.Thinking.Signature != "" && p.Thinking.Signature != "redacted" {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": p.Thinking.Text, "signature": p.Thinking.Signature})
			}
		}
		// A cache marker rides the block this part produced (text,
		// image, tool_use, tool_result all accept cache_control).
		if len(blocks) > before && p.CacheControl != "" {
			blocks[len(blocks)-1]["cache_control"] = anthCacheBlock(p.CacheControl)
		}
	}
	return blocks
}

// DecodeAnthropicResponse converts a native Anthropic response body into the IR.
func DecodeAnthropicResponse(b []byte) (Response, error) {
	var in core.AnthResponse
	if err := json.Unmarshal(b, &in); err != nil {
		return Response{}, fmt.Errorf("invalid Anthropic response: %w", err)
	}
	out := Response{ID: in.ID, Model: in.Model, StopReason: StopEndTurn, Raw: append(json.RawMessage(nil), b...)}
	if in.StopReason != nil {
		out.StopReason = MapStopReason(*in.StopReason)
	}
	out.Usage = Usage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens,
		CacheReadTokens: in.Usage.CacheReadInputTokens, CacheWriteTokens: in.Usage.CacheCreationInputTokens}
	for _, c := range in.Content {
		switch c.Type {
		case "text":
			out.Blocks = append(out.Blocks, Block{Type: PartText, Text: c.Text})
		case "tool_use":
			// Normalize the input to the canonical form the encoder
			// converges to (see anthToolInputArgs): missing, null, and
			// empty-object inputs are indistinguishable on the wire once the
			// encoder's `input,omitempty` drops them.
			out.Blocks = append(out.Blocks, Block{Type: PartToolCall, ToolCall: &ToolCall{ID: c.ID, Name: c.Name, Arguments: anthToolInputArgs(c.Input)}})
		case "thinking":
			// Drop thinking the encoder cannot emit: EncodeAnthropicResponse
			// only surfaces signed thinking (unsigned would poison replay,
			// see COMPATIBILITY.md rule 2). Keeping an unsigned phantom here
			// while the encoder drops it breaks decode(encode(decode(x)))
			// stability (FuzzResponseRoundTrip #447: {"Content":
			// [{"tYpe":"thinking"}]} decoded to a thinking block that
			// re-decoded as text). This mirrors the nameless tool-call drop
			// (#403): the decoder drops exactly what no encoder emits.
			if c.Signature == "" || c.Signature == "redacted" {
				continue
			}
			out.Blocks = append(out.Blocks, Block{Type: PartThinking, Thinking: &Thinking{Text: c.Thinking, Signature: c.Signature}})
		case "redacted_thinking":
			// Redacted thinking carries Signature "redacted", which the
			// encoder never surfaces, so it is dropped here for the same
			// round-trip reason as unsigned thinking above.
			continue
		}
	}
	if out.Blocks == nil {
		out.Blocks = []Block{}
	}
	return out, nil
}

// EncodeAnthropicResponse builds an Anthropic Messages response from the IR.
func EncodeAnthropicResponse(in Response, requestedModel string) core.AnthResponse {
	stop := in.StopReason
	if stop == "" {
		stop = StopEndTurn
	}
	// StopStopSequence is emitted verbatim: downgrading it to max_tokens
	// (as was done before) claims a truncation that never happened — a
	// client may wrongly grow max_tokens and retry — and it breaks
	// decode(encode(decode(x))) stability (FuzzResponseRoundTrip #447).
	// The matched sequence value is not carried in the IR; a bare
	// stop_sequence reason with a null sequence is the honest encoding,
	// and it re-decodes to StopStopSequence.
	content := make([]core.AnthContentBlock, 0, len(in.Blocks))
	for _, b := range in.Blocks {
		switch b.Type {
		case PartText:
			content = append(content, core.AnthContentBlock{Type: "text", Text: b.Text})
		case PartToolCall:
			if b.ToolCall == nil {
				continue
			}
			input := map[string]any{}
			if strings.TrimSpace(b.ToolCall.Arguments) != "" {
				if err := json.Unmarshal([]byte(b.ToolCall.Arguments), &input); err != nil {
					input = map[string]any{"_raw": b.ToolCall.Arguments}
				}
			}
			content = append(content, core.AnthContentBlock{Type: "tool_use", ID: b.ToolCall.ID, Name: b.ToolCall.Name, Input: input})
		case PartThinking:
			// Unsigned thinking is not surfaced (would poison replay).
			if b.Thinking != nil && b.Thinking.Signature != "" && b.Thinking.Signature != "redacted" {
				content = append(content, core.AnthContentBlock{Type: "thinking", Thinking: b.Thinking.Text, Signature: b.Thinking.Signature})
			}
		}
	}
	if len(content) == 0 {
		content = append(content, core.AnthContentBlock{Type: "text", Text: ""})
	}
	model := in.Model
	if requestedModel != "" {
		model = requestedModel
	}
	return core.AnthResponse{
		ID: in.ID, Type: "message", Role: "assistant", Content: content, Model: model,
		StopReason: &stop, Usage: core.AnthUsage{
			InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens,
			CacheReadInputTokens: in.Usage.CacheReadTokens, CacheCreationInputTokens: in.Usage.CacheWriteTokens,
		},
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

type dataURLParts struct{ mediaType, data string }

func parseDataURL(u string) *dataURLParts {
	const prefix = "data:"
	if !strings.HasPrefix(u, prefix) {
		return nil
	}
	rest := u[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return nil
	}
	mt := strings.TrimSuffix(rest[:comma], ";base64")
	if mt == "" {
		mt = "application/octet-stream"
	}
	return &dataURLParts{mediaType: mt, data: rest[comma+1:]}
}
