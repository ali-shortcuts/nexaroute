package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Canonical stream event model. See spec section 13.
// Provider SSE dialects decode into these events; client encoders render them
// into the client's expected format. Router code never touches SSE bytes.
type Kind string

const (
	KindStart          Kind = "start"
	KindTextDelta      Kind = "text_delta"
	KindReasoningDelta Kind = "reasoning_delta"
	KindToolCallStart  Kind = "tool_call_start"
	KindToolCallDelta  Kind = "tool_call_delta"
	KindToolCallEnd    Kind = "tool_call_end"
	KindUsage          Kind = "usage"
	KindEnd            Kind = "end"
	KindError          Kind = "error"
)

// Event is one normalized stream event.
type Event struct {
	Kind Kind `json:"kind"`
	// Text for text/reasoning deltas and error messages.
	Text string `json:"text,omitempty"`
	// Tool call correlation.
	ToolIndex int    `json:"tool_index,omitempty"`
	ToolID    string `json:"tool_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolArgs  string `json:"tool_args,omitempty"`
	// Usage totals (set on KindUsage / terminal KindEnd).
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// Finish reason on KindEnd (stop, length, tool_calls, ...).
	FinishReason string `json:"finish_reason,omitempty"`
}

// Decoder incrementally parses one provider's SSE bytes into events.
type Decoder interface {
	// Feed consumes bytes and returns newly completed events.
	Feed(p []byte) ([]Event, error)
	// Finish validates terminal state at EOF.
	Finish() ([]Event, error)
}

const maxLineBytes = 8 << 20

// OpenAIDecoder parses OpenAI Chat Completions SSE (data: {...} / [DONE]).
type OpenAIDecoder struct {
	line     []byte
	sawEnd   bool
	finish   string
	in, out  int
	usageSet bool
}

func (d *OpenAIDecoder) Feed(p []byte) ([]Event, error) {
	events := []Event{}
	for len(p) > 0 {
		n := bytes.IndexByte(p, '\n')
		if n < 0 {
			if len(d.line)+len(p) > maxLineBytes {
				return events, fmt.Errorf("openai SSE line exceeds %d bytes", maxLineBytes)
			}
			d.line = append(d.line, p...)
			return events, nil
		}
		if len(d.line)+n > maxLineBytes {
			return events, fmt.Errorf("openai SSE line exceeds %d bytes", maxLineBytes)
		}
		d.line = append(d.line, p[:n]...)
		evs, err := d.processLine()
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
		d.line = d.line[:0]
		p = p[n+1:]
	}
	return events, nil
}

func (d *OpenAIDecoder) processLine() ([]Event, error) {
	line := bytes.TrimSpace(d.line)
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return nil, nil
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 {
		return nil, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		d.sawEnd = true
		out := []Event{}
		if d.usageSet {
			out = append(out, Event{Kind: KindUsage, InputTokens: d.in, OutputTokens: d.out})
		}
		out = append(out, Event{Kind: KindEnd, FinishReason: d.finishOr("stop")})
		return out, nil
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("invalid OpenAI SSE JSON: %w", err)
	}
	if bytes.Contains(data, []byte(`"error"`)) {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(data, &env); err == nil {
			if raw := env["error"]; len(raw) > 0 && string(raw) != "null" {
				return []Event{{Kind: KindError, Text: "openai SSE error event"}}, fmt.Errorf("openai SSE error event")
			}
		}
	}
	events := []Event{}
	if chunk.Usage != nil {
		d.in, d.out = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		d.usageSet = true
	}
	for _, ch := range chunk.Choices {
		switch c := ch.Delta.Content.(type) {
		case string:
			if c != "" {
				events = append(events, Event{Kind: KindTextDelta, Text: c})
			}
		case []any:
			for _, part := range c {
				m, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := m["text"].(string); t != "" {
					events = append(events, Event{Kind: KindTextDelta, Text: t})
				}
			}
		}
		if ch.Delta.ReasoningContent != "" {
			events = append(events, Event{Kind: KindReasoningDelta, Text: ch.Delta.ReasoningContent})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if tc.ID != "" || tc.Function.Name != "" {
				events = append(events, Event{Kind: KindToolCallStart, ToolIndex: tc.Index, ToolID: tc.ID, ToolName: tc.Function.Name})
			}
			if tc.Function.Arguments != "" {
				events = append(events, Event{Kind: KindToolCallDelta, ToolIndex: tc.Index, ToolID: tc.ID, ToolArgs: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			d.finish = *ch.FinishReason
		}
	}
	return events, nil
}

func (d *OpenAIDecoder) finishOr(def string) string {
	if d.finish != "" {
		return d.finish
	}
	return def
}

func (d *OpenAIDecoder) Finish() ([]Event, error) {
	if len(d.line) > 0 {
		if _, err := d.processLine(); err != nil {
			return nil, err
		}
		d.line = nil
	}
	if !d.sawEnd {
		return []Event{{Kind: KindError, Text: "upstream stream ended without [DONE]"}}, fmt.Errorf("openai stream ended without [DONE]")
	}
	return nil, nil
}

// AnthropicDecoder parses Anthropic Messages SSE event frames.
type AnthropicDecoder struct {
	sawStop  bool
	finish   string
	in, out  int
	usageSet bool
	buf      []byte
}

func (d *AnthropicDecoder) Feed(p []byte) ([]Event, error) {
	// Anthropic frames span multiple lines (event: + data:); accumulate by
	// blank-line-delimited blocks. A trailing block without its terminator
	// is buffered until more bytes arrive or Finish runs.
	if len(d.buf)+len(p) > maxLineBytes*4 {
		return nil, fmt.Errorf("anthropic SSE buffer exceeds limit")
	}
	d.buf = append(d.buf, p...)
	events := []Event{}
	for {
		n := bytes.Index(d.buf, []byte("\n\n"))
		if n < 0 {
			return events, nil
		}
		block := d.buf[:n]
		rest := make([]byte, len(d.buf)-(n+2))
		copy(rest, d.buf[n+2:])
		d.buf = rest
		evs, err := d.processBlock(block)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
	}
}

func (d *AnthropicDecoder) processBlock(block []byte) ([]Event, error) {
	lines := bytes.Split(block, []byte("\n"))
	data := ""
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if bytes.HasPrefix(ln, []byte("data:")) {
			data = strings.TrimSpace(string(ln[len("data:"):]))
		}
	}
	if data == "" {
		return nil, nil
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return nil, fmt.Errorf("invalid Anthropic SSE JSON: %w", err)
	}
	typ, _ := env["type"].(string)
	switch typ {
	case "message_start":
		if msg, _ := env["message"].(map[string]any); msg != nil {
			if u, _ := msg["usage"].(map[string]any); u != nil {
				d.observeUsage(u)
			}
		}
		return []Event{{Kind: KindStart}}, nil
	case "content_block_start":
		idx := intNumber(env["index"])
		cb, _ := env["content_block"].(map[string]any)
		if cb != nil && cb["type"] == "tool_use" {
			name, _ := cb["name"].(string)
			id, _ := cb["id"].(string)
			return []Event{{Kind: KindToolCallStart, ToolIndex: idx, ToolID: id, ToolName: name}}, nil
		}
		return nil, nil
	case "content_block_delta":
		idx := intNumber(env["index"])
		delta, _ := env["delta"].(map[string]any)
		if delta == nil {
			return nil, nil
		}
		switch delta["type"] {
		case "text_delta":
			if t, _ := delta["text"].(string); t != "" {
				return []Event{{Kind: KindTextDelta, Text: t}}, nil
			}
		case "input_json_delta":
			if part, _ := delta["partial_json"].(string); part != "" {
				return []Event{{Kind: KindToolCallDelta, ToolIndex: idx, ToolArgs: part}}, nil
			}
		case "thinking_delta":
			if t, _ := delta["thinking"].(string); t != "" {
				return []Event{{Kind: KindReasoningDelta, Text: t}}, nil
			}
		}
		return nil, nil
	case "content_block_stop":
		idx := intNumber(env["index"])
		return []Event{{Kind: KindToolCallEnd, ToolIndex: idx}}, nil
	case "message_delta":
		if del, _ := env["delta"].(map[string]any); del != nil {
			if sr, _ := del["stop_reason"].(string); sr != "" {
				d.finish = sr
			}
		}
		if u, _ := env["usage"].(map[string]any); u != nil {
			d.observeUsage(u)
		}
		return nil, nil
	case "message_stop":
		d.sawStop = true
		out := []Event{}
		if d.usageSet {
			out = append(out, Event{Kind: KindUsage, InputTokens: d.in, OutputTokens: d.out})
		}
		out = append(out, Event{Kind: KindEnd, FinishReason: d.finishOr()})
		return out, nil
	case "error":
		return []Event{{Kind: KindError, Text: "anthropic SSE error event"}}, fmt.Errorf("anthropic SSE error event")
	case "ping":
		return nil, nil
	default:
		return nil, nil
	}
}

func (d *AnthropicDecoder) observeUsage(u map[string]any) {
	if v, ok := intNumberOK(u["input_tokens"]); ok && v >= 0 {
		d.in = v
		d.usageSet = true
	}
	if v, ok := intNumberOK(u["output_tokens"]); ok && v >= 0 {
		d.out = v
		d.usageSet = true
	}
}

func (d *AnthropicDecoder) finishOr() string {
	if d.finish != "" {
		switch d.finish {
		case "tool_use":
			return "tool_calls"
		case "max_tokens":
			return "length"
		default:
			return d.finish
		}
	}
	return "stop"
}

func (d *AnthropicDecoder) Finish() ([]Event, error) {
	events := []Event{}
	if len(bytes.TrimSpace(d.buf)) > 0 {
		evs, err := d.processBlock(d.buf)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
		d.buf = nil
	}
	if !d.sawStop {
		return events, fmt.Errorf("anthropic stream ended without message_stop")
	}
	return events, nil
}

func intNumber(v any) int {
	n, _ := intNumberOK(v)
	return n
}

func intNumberOK(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case json.Number:
		n, err := x.Int64()
		return int(n), err == nil
	}
	return 0, false
}
