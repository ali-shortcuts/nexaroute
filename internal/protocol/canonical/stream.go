package canonical

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---------- Canonical stream event model ----------
//
// Every upstream SSE dialect decodes into these events; every client dialect
// encodes from them. Provider-specific SSE logic must never leak into the
// router or handlers.

// Stream event types.
const (
	StreamStart     = "start"
	StreamText      = "text"
	StreamThinking  = "thinking"
	StreamToolStart = "tool_start"
	StreamToolDelta = "tool_delta"
	StreamToolEnd   = "tool_end"
	StreamUsage     = "usage"
	StreamEnd       = "end"
	StreamError     = "error"
)

// StreamEvent is one canonical stream event.
type StreamEvent struct {
	Type string `json:"type"`

	Text       string `json:"text,omitempty"`
	ToolIndex  int    `json:"tool_index,omitempty"`
	ToolID     string `json:"tool_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ArgsDelta  string `json:"args_delta,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	ErrorMsg   string `json:"error_msg,omitempty"`
}

// ---------- SSE reader ----------

// SSEReader reads text/event-stream frames incrementally. It is dialect
// agnostic: it only splits frames and hands `data:` payloads to callers.
type SSEReader struct {
	scanner    *bufio.Scanner
	done       bool
	eventName  string
	dataBuffer []string
}

// NewSSEReader wraps an upstream body.
func NewSSEReader(r io.Reader) *SSEReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return &SSEReader{scanner: sc}
}

// Next returns the next SSE event's name and data payload. done is true when
// the stream ended cleanly. Unknown event types and comments are skipped.
func (sr *SSEReader) Next() (name, data string, done bool, err error) {
	for !sr.done && sr.scanner.Scan() {
		line := sr.scanner.Text()
		switch {
		case line == "":
			// End of one SSE event block.
			if len(sr.dataBuffer) > 0 {
				payload := strings.Join(sr.dataBuffer, "\n")
				sr.dataBuffer = sr.dataBuffer[:0]
				evName := sr.eventName
				sr.eventName = ""
				return evName, payload, false, nil
			}
			sr.eventName = ""
			continue
		case strings.HasPrefix(line, ":"):
			continue // comment / keep-alive
		case strings.HasPrefix(line, "event:"):
			sr.eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			sr.dataBuffer = append(sr.dataBuffer, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
			continue
		}
	}
	if err := sr.scanner.Err(); err != nil {
		if err == io.EOF {
			sr.done = true
			return "", "", true, nil
		}
		sr.done = true
		return "", "", false, err
	}
	sr.done = true
	// Flush a trailing block that lacked the final blank line.
	if len(sr.dataBuffer) > 0 {
		payload := strings.Join(sr.dataBuffer, "\n")
		sr.dataBuffer = sr.dataBuffer[:0]
		return sr.eventName, payload, false, nil
	}
	return "", "", true, nil
}

// ---------- Upstream decoders (SSE -> canonical events) ----------

// DecodeOpenAIStreamChunk decodes one OpenAI Chat Completions SSE data
// payload. It returns the canonical events and whether the stream terminated.
func DecodeOpenAIStreamChunk(data string) ([]StreamEvent, bool, error) {
	d := strings.TrimSpace(data)
	if d == "" {
		return nil, false, nil
	}
	if d == "[DONE]" {
		return nil, true, nil
	}
	var chunk struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role             string         `json:"role"`
				Content          any            `json:"content"`
				ReasoningContent string         `json:"reasoning_content"`
				Reasoning        string         `json:"reasoning"`
				ToolCalls        []coreToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *coreUsage `json:"usage"`
		Error *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(d), &chunk); err != nil {
		return nil, false, fmt.Errorf("invalid OpenAI SSE chunk: %w", err)
	}
	if chunk.Error != nil {
		code := chunk.Error.Type
		if c, ok := chunk.Error.Code.(string); ok && c != "" {
			code = c
		}
		return []StreamEvent{{Type: StreamError, ErrorCode: code, ErrorMsg: chunk.Error.Message}}, true, nil
	}
	var events []StreamEvent
	if chunk.Usage != nil && (chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0) {
		u := &Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		if chunk.Usage.PromptTokensDetails != nil {
			u.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		if chunk.Usage.CompletionTokensDetails != nil {
			u.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
		events = append(events, StreamEvent{Type: StreamUsage, Usage: u})
	}
	for _, ch := range chunk.Choices {
		d := &ch.Delta
		if d.Content != nil {
			switch v := d.Content.(type) {
			case string:
				if v != "" {
					events = append(events, StreamEvent{Type: StreamText, Text: v})
				}
			case []any:
				for _, raw := range v {
					if m, ok := raw.(map[string]any); ok {
						typ, _ := m["type"].(string)
						if typ == "text" || typ == "output_text" {
							if txt, _ := m["text"].(string); txt != "" {
								events = append(events, StreamEvent{Type: StreamText, Text: txt})
							}
						}
					}
				}
			}
		}
		if rc := d.ReasoningContent; rc == "" {
			if d.Reasoning != "" {
				events = append(events, StreamEvent{Type: StreamThinking, Text: d.Reasoning})
			}
		} else {
			events = append(events, StreamEvent{Type: StreamThinking, Text: rc})
		}
		for _, tc := range d.ToolCalls {
			if tc.ID != "" || tc.Function.Name != "" {
				events = append(events, StreamEvent{Type: StreamToolStart, ToolIndex: tc.Index, ToolID: tc.ID, ToolName: tc.Function.Name})
			}
			if tc.Function.Arguments != "" {
				events = append(events, StreamEvent{Type: StreamToolDelta, ToolIndex: tc.Index, ArgsDelta: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			events = append(events, StreamEvent{Type: StreamEnd, StopReason: MapStopReason(*ch.FinishReason)})
		}
	}
	return events, false, nil
}

type coreToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type coreUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// DecodeAnthropicStreamEvent decodes one Anthropic Messages SSE event payload
// (already split from its event: name) into canonical events.
func DecodeAnthropicStreamEvent(eventName, data string) ([]StreamEvent, error) {
	d := strings.TrimSpace(data)
	if d == "" {
		return nil, nil
	}
	var env struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(d), &env); err != nil {
		return nil, fmt.Errorf("invalid Anthropic SSE event: %w", err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(env.Error, &e)
		return []StreamEvent{{Type: StreamError, ErrorCode: e.Type, ErrorMsg: e.Message}}, nil
	}
	switch env.Type {
	case "message_start":
		var m struct {
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		_ = json.Unmarshal([]byte(d), &m)
		ev := StreamEvent{Type: StreamStart}
		if m.Message.Usage.InputTokens > 0 {
			ev.Usage = &Usage{InputTokens: m.Message.Usage.InputTokens}
		}
		return []StreamEvent{ev}, nil
	case "content_block_start":
		var cb struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type     string `json:"type"`
				ID       string `json:"id"`
				Name     string `json:"name"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(d), &cb); err != nil {
			return nil, fmt.Errorf("invalid Anthropic content_block_start: %w", err)
		}
		switch cb.ContentBlock.Type {
		case "text":
			if cb.ContentBlock.Text != "" {
				return []StreamEvent{{Type: StreamText, Text: cb.ContentBlock.Text}}, nil
			}
		case "thinking":
			if cb.ContentBlock.Thinking != "" {
				return []StreamEvent{{Type: StreamThinking, Text: cb.ContentBlock.Thinking}}, nil
			}
		case "tool_use":
			return []StreamEvent{{Type: StreamToolStart, ToolIndex: cb.Index, ToolID: cb.ContentBlock.ID, ToolName: cb.ContentBlock.Name}}, nil
		}
		return nil, nil
	case "content_block_delta":
		var dv struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(d), &dv); err != nil {
			return nil, fmt.Errorf("invalid Anthropic content_block_delta: %w", err)
		}
		switch dv.Delta.Type {
		case "text_delta":
			if dv.Delta.Text != "" {
				return []StreamEvent{{Type: StreamText, Text: dv.Delta.Text}}, nil
			}
		case "thinking_delta":
			if dv.Delta.Thinking != "" {
				return []StreamEvent{{Type: StreamThinking, Text: dv.Delta.Thinking}}, nil
			}
		case "input_json_delta":
			if dv.Delta.PartialJSON != "" {
				return []StreamEvent{{Type: StreamToolDelta, ToolIndex: dv.Index, ArgsDelta: dv.Delta.PartialJSON}}, nil
			}
		}
		return nil, nil
	case "content_block_stop":
		var st struct {
			Index int `json:"index"`
		}
		_ = json.Unmarshal([]byte(d), &st)
		return []StreamEvent{{Type: StreamToolEnd, ToolIndex: st.Index}}, nil
	case "message_delta":
		var md struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(d), &md); err != nil {
			return nil, fmt.Errorf("invalid Anthropic message_delta: %w", err)
		}
		var events []StreamEvent
		if md.Usage.OutputTokens > 0 || md.Usage.InputTokens > 0 {
			events = append(events, StreamEvent{Type: StreamUsage, Usage: &Usage{
				InputTokens: md.Usage.InputTokens, OutputTokens: md.Usage.OutputTokens,
			}})
		}
		if md.Delta.StopReason != "" {
			events = append(events, StreamEvent{Type: StreamEnd, StopReason: MapStopReason(md.Delta.StopReason)})
		}
		return events, nil
	case "message_stop":
		return []StreamEvent{{Type: StreamEnd, StopReason: StopEndTurn}}, nil
	case "ping", "content_block_start_empty":
		return nil, nil
	case "error":
		// Handled above via Error envelope; fall through defensively.
		return []StreamEvent{{Type: StreamError, ErrorMsg: "anthropic stream error event"}}, nil
	default:
		return nil, nil
	}
}

// DecodeGeminiStreamChunk decodes one Gemini streamGenerateContent SSE data
// payload into canonical events.
func DecodeGeminiStreamChunk(data string) ([]StreamEvent, bool, error) {
	d := strings.TrimSpace(data)
	if d == "" {
		return nil, false, nil
	}
	// DecodeGeminiResponse intentionally gives non-stream responses a default
	// end_turn stop reason even when finishReason is omitted. Streaming cannot
	// use that default to infer termination: ordinary intermediate Gemini
	// chunks omit finishReason. Inspect the raw envelope separately.
	var raw GeminiResponse_
	if err := json.Unmarshal([]byte(d), &raw); err != nil {
		return nil, false, fmt.Errorf("invalid Gemini stream chunk: %w", err)
	}
	terminal := raw.PromptFeedback != nil && raw.PromptFeedback.BlockReason != ""
	if len(raw.Candidates) > 0 && strings.TrimSpace(raw.Candidates[0].FinishReason) != "" {
		terminal = true
	}
	resp, err := DecodeGeminiResponse([]byte(d))
	if err != nil {
		return nil, false, err
	}
	var events []StreamEvent
	for _, b := range resp.Blocks {
		switch b.Type {
		case PartText:
			events = append(events, StreamEvent{Type: StreamText, Text: b.Text})
		case PartThinking:
			events = append(events, StreamEvent{Type: StreamThinking, Text: b.Thinking.Text})
		case PartToolCall:
			events = append(events, StreamEvent{Type: StreamToolStart, ToolName: b.ToolCall.Name, ToolID: b.ToolCall.ID})
			events = append(events, StreamEvent{Type: StreamToolDelta, ArgsDelta: b.ToolCall.Arguments})
			events = append(events, StreamEvent{Type: StreamToolEnd})
		}
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		u := resp.Usage
		events = append(events, StreamEvent{Type: StreamUsage, Usage: &u})
	}
	if terminal {
		events = append(events, StreamEvent{Type: StreamEnd, StopReason: resp.StopReason})
	}
	return events, terminal, nil
}

// ---------- Client encoders (canonical events -> SSE) ----------

// StreamEmitter encodes canonical events into a client's SSE dialect.
type StreamEmitter interface {
	Emit(ev StreamEvent) error
	// Finish flushes any protocol-required tail frames. Idempotent.
	Finish() error
}

// NewStreamEmitter returns the emitter for a client protocol family.
func NewStreamEmitter(protocol string, w http.ResponseWriter, model, requestID string) StreamEmitter {
	switch protocol {
	case "anthropic":
		return NewAnthropicEmitter(w, model, requestID)
	case "openai_responses":
		return NewResponsesEmitter(w, model)
	default:
		return NewOpenAIEmitter(w, model)
	}
}

// ---------- Anthropic client emitter ----------

type anthropicEmitter struct {
	w          http.ResponseWriter
	fl         http.Flusher
	model      string
	messageID  string
	writeErr   error
	started    bool
	blocks     map[int]int // upstream tool index -> anthropic content block index
	nextIndex  int
	textOpen   bool
	textIndex  int
	thinkOpen  bool
	thinkIndex int
	openBlocks map[int]bool
	usage      Usage
	usageSeen  bool
	stopReason string
	finished   bool
}

// NewAnthropicEmitter streams canonical events as Anthropic Messages SSE.
func NewAnthropicEmitter(w http.ResponseWriter, model, requestID string) StreamEmitter {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	e := &anthropicEmitter{
		w: w, fl: fl, model: model, messageID: uniqueMessageID(requestID),
		blocks: map[int]int{}, openBlocks: map[int]bool{},
		textIndex: -1, thinkIndex: -1,
	}
	e.frame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.messageID, "type": "message", "role": "assistant", "content": []any{},
			"model": model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		},
	})
	e.frame("ping", map[string]any{"type": "ping"})
	return e
}

func (e *anthropicEmitter) frame(name string, v any) {
	if e.writeErr != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		e.writeErr = err
		return
	}
	if name != "" {
		_, e.writeErr = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, b)
	} else {
		_, e.writeErr = fmt.Fprintf(e.w, "data: %s\n\n", b)
	}
	if e.writeErr == nil && e.fl != nil {
		e.fl.Flush()
	}
}

func (e *anthropicEmitter) openBlock(index int, block map[string]any) {
	e.frame("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
	e.openBlocks[index] = true
}

func (e *anthropicEmitter) closeBlock(index int) {
	if e.openBlocks[index] {
		e.frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		e.openBlocks[index] = false
	}
}

func (e *anthropicEmitter) Emit(ev StreamEvent) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	switch ev.Type {
	case StreamStart:
		if ev.Usage != nil && ev.Usage.InputTokens > 0 {
			e.usage.InputTokens = ev.Usage.InputTokens
			e.usageSeen = true
		}
	case StreamText:
		if !e.textOpen {
			if e.thinkOpen {
				e.closeBlock(e.thinkIndex)
				e.thinkOpen = false
				e.thinkIndex = -1
			}
			e.textOpen = true
			e.textIndex = e.nextIndex
			e.openBlock(e.textIndex, map[string]any{"type": "text", "text": ""})
			e.nextIndex++
		}
		e.frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.textIndex, "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
	case StreamThinking:
		if !e.thinkOpen {
			if e.textOpen {
				e.closeBlock(e.textIndex)
				e.textOpen = false
				e.textIndex = -1
			}
			e.thinkOpen = true
			e.thinkIndex = e.nextIndex
			e.openBlock(e.thinkIndex, map[string]any{"type": "thinking", "thinking": ""})
			e.nextIndex++
		}
		e.frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.thinkIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text}})
	case StreamToolStart:
		if e.thinkOpen {
			e.closeBlock(e.thinkIndex)
			e.thinkOpen = false
			e.thinkIndex = -1
		}
		if e.textOpen {
			e.closeBlock(e.textIndex)
			e.textOpen = false
			e.textIndex = -1
		}
		idx := e.nextIndex
		e.nextIndex++
		toolIdx := ev.ToolIndex
		e.blocks[toolIdx] = idx
		id := ev.ToolID
		if id == "" {
			id = fmt.Sprintf("toolu_%d", idx)
		}
		e.openBlock(idx, map[string]any{"type": "tool_use", "id": id, "name": ev.ToolName, "input": map[string]any{}})
	case StreamToolDelta:
		idx, ok := e.blocks[ev.ToolIndex]
		if !ok {
			// A delta arrived without a start (some upstreams do this).
			e.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: ev.ToolIndex, ToolName: ev.ToolName})
			idx = e.blocks[ev.ToolIndex]
		}
		e.frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgsDelta}})
	case StreamToolEnd:
		if idx, ok := e.blocks[ev.ToolIndex]; ok {
			e.closeBlock(idx)
		}
	case StreamUsage:
		if ev.Usage != nil {
			if ev.Usage.InputTokens > 0 {
				e.usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.OutputTokens > 0 {
				e.usage.OutputTokens = ev.Usage.OutputTokens
			}
			e.usageSeen = true
		}
	case StreamEnd:
		if ev.StopReason != "" && ev.StopReason != StopStopSequence {
			e.stopReason = ev.StopReason
		} else if ev.StopReason == StopStopSequence {
			e.stopReason = StopMaxTokens
		}
	case StreamError:
		e.frame("error", map[string]any{"type": "error", "error": map[string]any{"type": mapErrorType(ev.ErrorCode), "message": ev.ErrorMsg}})
	}
	return e.writeErr
}

func (e *anthropicEmitter) Finish() error {
	if e.finished || e.writeErr != nil {
		return e.writeErr
	}
	e.finished = true
	if e.thinkOpen {
		e.closeBlock(e.thinkIndex)
		e.thinkOpen = false
		e.thinkIndex = -1
	}
	if e.textOpen {
		e.closeBlock(e.textIndex)
		e.textOpen = false
		e.textIndex = -1
	}
	for idx, open := range e.openBlocks {
		if open {
			e.closeBlock(idx)
		}
	}
	stop := e.stopReason
	if stop == "" {
		stop = StopEndTurn
	}
	usage := map[string]int{"output_tokens": e.usage.OutputTokens}
	if e.usageSeen || e.usage.InputTokens > 0 {
		usage["input_tokens"] = e.usage.InputTokens
	}
	e.frame("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": usage})
	e.frame("message_stop", map[string]any{"type": "message_stop"})
	return e.writeErr
}

func mapErrorType(code string) string {
	switch strings.ToLower(code) {
	case "overloaded_error":
		return "overloaded_error"
	case "rate_limit_error":
		return "rate_limit_error"
	case "invalid_request_error":
		return "invalid_request_error"
	case "authentication_error":
		return "authentication_error"
	default:
		return "api_error"
	}
}

func uniqueMessageID(requestID string) string {
	base := strings.ReplaceAll(requestID, "-", "")
	if len(base) > 16 {
		base = base[:16]
	}
	const hexDigits = "0123456789abcdef"
	var sb strings.Builder
	sb.WriteString("msg_")
	sb.WriteString(pad(fmt.Sprintf("%x", messageClock()), 12))
	if base != "" {
		sb.WriteString("_")
		for i := 0; i < len(base); i++ {
			c := base[i]
			if !strings.ContainsRune(hexDigits, rune(c)) {
				c = hexDigits[int(c)%16]
			}
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func pad(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// ---------- OpenAI client emitter ----------

type openAIEmitter struct {
	w          http.ResponseWriter
	fl         http.Flusher
	model      string
	id         string
	created    int64
	writeErr   error
	toolArgs   map[int]*strings.Builder
	toolNames  map[int]string
	toolIDs    map[int]string
	emittedAny bool
	finish     string
	usage      *Usage
	finished   bool
}

// NewOpenAIEmitter streams canonical events as OpenAI Chat Completions SSE.
func NewOpenAIEmitter(w http.ResponseWriter, model string) StreamEmitter {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	e := &openAIEmitter{
		w: w, fl: fl, model: model, created: timeNow(),
		id:        fmt.Sprintf("chatcmpl-%d", messageClock()),
		toolArgs:  map[int]*strings.Builder{},
		toolNames: map[int]string{},
		toolIDs:   map[int]string{},
	}
	return e
}

func (e *openAIEmitter) chunk(payload map[string]any) {
	if e.writeErr != nil {
		return
	}
	payload["id"] = e.id
	payload["object"] = "chat.completion.chunk"
	payload["created"] = e.created
	payload["model"] = e.model
	b, err := json.Marshal(payload)
	if err != nil {
		e.writeErr = err
		return
	}
	_, e.writeErr = fmt.Fprintf(e.w, "data: %s\n\n", b)
	if e.writeErr == nil && e.fl != nil {
		e.fl.Flush()
	}
}

func (e *openAIEmitter) Emit(ev StreamEvent) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	switch ev.Type {
	case StreamStart:
		e.chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}})
	case StreamText:
		e.emittedAny = true
		e.chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ev.Text}, "finish_reason": nil}}})
	case StreamThinking:
		e.chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": ev.Text}, "finish_reason": nil}}})
	case StreamToolStart:
		e.emittedAny = true
		name := ev.ToolName
		if ev.ToolIndex >= 0 {
			e.toolNames[ev.ToolIndex] = name
			e.toolIDs[ev.ToolIndex] = ev.ToolID
			if _, ok := e.toolArgs[ev.ToolIndex]; !ok {
				e.toolArgs[ev.ToolIndex] = &strings.Builder{}
			}
		}
		id := ev.ToolID
		if id == "" {
			id = fmt.Sprintf("call_%d", ev.ToolIndex)
		}
		e.chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": ev.ToolIndex, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}}, "finish_reason": nil}}})
	case StreamToolDelta:
		buf, ok := e.toolArgs[ev.ToolIndex]
		if !ok {
			buf = &strings.Builder{}
			e.toolArgs[ev.ToolIndex] = buf
			e.toolIDs[ev.ToolIndex] = fmt.Sprintf("call_%d", ev.ToolIndex)
		}
		buf.WriteString(ev.ArgsDelta)
		e.emittedAny = true
		e.chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": ev.ToolIndex, "function": map[string]any{"arguments": ev.ArgsDelta}}},
		}, "finish_reason": nil}}})
	case StreamToolEnd:
	case StreamUsage:
		e.usage = ev.Usage
	case StreamEnd:
		e.finish = openAIFinish(ev.StopReason)
	case StreamError:
		// Surface as a terminal chunk carrying the error object.
		e.chunk(map[string]any{"choices": []any{}, "error": map[string]any{"message": ev.ErrorMsg, "type": ev.ErrorCode}})
	}
	return e.writeErr
}

func (e *openAIEmitter) Finish() error {
	if e.finished || e.writeErr != nil {
		return e.writeErr
	}
	e.finished = true
	if !e.emittedAny {
		e.chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}})
	}
	final := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": e.finish}}}
	if e.usage != nil {
		final["usage"] = map[string]any{
			"prompt_tokens": e.usage.InputTokens, "completion_tokens": e.usage.OutputTokens,
			"total_tokens":              e.usage.InputTokens + e.usage.OutputTokens,
			"prompt_tokens_details":     map[string]any{"cached_tokens": e.usage.CacheReadTokens},
			"completion_tokens_details": map[string]any{"reasoning_tokens": e.usage.ReasoningTokens},
		}
	}
	e.chunk(final)
	if e.writeErr == nil {
		_, e.writeErr = fmt.Fprint(e.w, "data: [DONE]\n\n")
		if e.writeErr == nil && e.fl != nil {
			e.fl.Flush()
		}
	}
	return e.writeErr
}

func openAIFinish(stop string) string {
	switch stop {
	case StopToolUse:
		return "tool_calls"
	case StopMaxTokens:
		return "length"
	case StopRefusal:
		return "content_filter"
	case StopStopSequence:
		return "stop"
	default:
		return "stop"
	}
}

// ---------- Responses client emitter ----------

type responsesEmitter struct {
	w          http.ResponseWriter
	fl         http.Flusher
	model      string
	id         string
	writeErr   error
	toolIdx    map[int]string
	toolOutIdx map[int]int
	nextOutIdx int
	itemSeq    int
	usage      *Usage
	stop       string
	finished   bool
}

// NewResponsesEmitter streams canonical events as OpenAI Responses SSE.
func NewResponsesEmitter(w http.ResponseWriter, model string) StreamEmitter {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	e := &responsesEmitter{
		w: w, fl: fl, model: model, id: fmt.Sprintf("resp_%d", messageClock()),
		toolIdx: map[int]string{}, toolOutIdx: map[int]int{}, nextOutIdx: 1,
	}
	created := float64(timeNow())
	e.event("response.created", map[string]any{"type": "response.created", "response": map[string]any{
		"id": e.id, "object": "response", "status": "in_progress", "model": model, "output": []any{}, "created_at": created,
	}})
	e.event("response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{
		"id": e.id, "object": "response", "status": "in_progress", "model": model, "output": []any{}, "created_at": created,
	}})
	return e
}

func (e *responsesEmitter) event(name string, payload map[string]any) {
	if e.writeErr != nil {
		return
	}
	payload["sequence_number"] = e.itemSeq
	e.itemSeq++
	b, err := json.Marshal(payload)
	if err != nil {
		e.writeErr = err
		return
	}
	_, e.writeErr = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, b)
	if e.writeErr == nil && e.fl != nil {
		e.fl.Flush()
	}
}

func (e *responsesEmitter) Emit(ev StreamEvent) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	switch ev.Type {
	case StreamText:
		e.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": "msg_0", "output_index": 0,
			"content_index": 0, "delta": ev.Text,
		})
	case StreamThinking:
		e.event("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": "rs_0", "output_index": 0,
			"summary_index": 0, "delta": ev.Text,
		})
	case StreamToolStart:
		callID := ev.ToolID
		if callID == "" {
			callID = fmt.Sprintf("call_%d", ev.ToolIndex)
		}
		outIdx := e.nextOutIdx
		e.nextOutIdx++
		e.toolIdx[ev.ToolIndex] = callID
		e.toolOutIdx[ev.ToolIndex] = outIdx
		e.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": outIdx,
			"item": map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": ev.ToolName, "arguments": "", "status": "in_progress"},
		})
	case StreamToolDelta:
		callID, ok := e.toolIdx[ev.ToolIndex]
		outIdx, outOK := e.toolOutIdx[ev.ToolIndex]
		if !ok {
			callID = fmt.Sprintf("call_%d", ev.ToolIndex)
			e.toolIdx[ev.ToolIndex] = callID
		}
		if !outOK {
			outIdx = e.nextOutIdx
			e.nextOutIdx++
			e.toolOutIdx[ev.ToolIndex] = outIdx
		}
		e.event("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "fc_" + callID,
			"output_index": outIdx, "delta": ev.ArgsDelta,
		})
	case StreamToolEnd:
		callID, ok := e.toolIdx[ev.ToolIndex]
		outIdx, outOK := e.toolOutIdx[ev.ToolIndex]
		if !ok {
			callID = fmt.Sprintf("call_%d", ev.ToolIndex)
		}
		if !outOK {
			outIdx = e.nextOutIdx
			e.nextOutIdx++
			e.toolOutIdx[ev.ToolIndex] = outIdx
		}
		e.event("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "fc_" + callID, "output_index": outIdx, "arguments": "",
		})
		e.event("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": outIdx,
			"item": map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "status": "completed"},
		})
	case StreamUsage:
		e.usage = ev.Usage
	case StreamEnd:
		e.stop = ev.StopReason
	case StreamError:
		e.event("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{
			"id": e.id, "status": "failed", "error": map[string]any{"code": ev.ErrorCode, "message": ev.ErrorMsg},
		}})
	}
	return e.writeErr
}

func (e *responsesEmitter) Finish() error {
	if e.finished || e.writeErr != nil {
		return e.writeErr
	}
	e.finished = true
	status := "completed"
	if e.stop == StopMaxTokens || e.stop == StopRefusal {
		status = "incomplete"
	}
	resp := map[string]any{
		"id": e.id, "object": "response", "status": status, "model": e.model,
		"output": []any{}, "usage": map[string]any{
			"input_tokens": 0, "output_tokens": 0,
		},
	}
	if e.usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens": e.usage.InputTokens, "output_tokens": e.usage.OutputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": e.usage.CacheReadTokens},
			"output_tokens_details": map[string]any{"reasoning_tokens": e.usage.ReasoningTokens},
		}
	}
	e.event("response.completed", map[string]any{"type": "response.completed", "response": resp})
	return e.writeErr
}

var _ = http.Flusher(nil)
