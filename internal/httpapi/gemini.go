package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/ali-shortcuts/nexaroute/internal/compat/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/compat/stream"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// Gemini upstream serving (Phase 11). Requests reach Gemini through the
// Canonical IR (client -> canonical -> GenerateContent) and responses come
// back through canonical decoders, so no N x N translators are needed.
// Streaming consumes the compat/stream GeminiDecoder; translators below only
// render canonical events into client SSE.

// geminiSendFor builds the per-attempt send closure for a Gemini deployment.
// Gemini URLs are model-scoped, so the plain adapter Do() (fixed default
// path) cannot serve them; hedging, repair and probing all share this path.
func geminiSendFor(a providers.Adapter, model string, stream bool, forward http.Header) func(context.Context, []byte) (*http.Response, error) {
	return func(ctx context.Context, payload []byte) (*http.Response, error) {
		return a.DoPath(ctx, http.MethodPost, providers.GeminiRequestPath(model, stream), payload, stream, forward)
	}
}

// validateGeminiResponseJSON rejects malformed envelopes and embedded error
// objects before a 2xx Gemini body is decoded. A prompt-level safety block
// (promptFeedback without candidates) fails here so it surfaces as an
// upstream error instead of an empty success.
func validateGeminiResponseJSON(b []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("invalid Gemini response JSON: %w", err)
	}
	if raw, ok := root["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		return fmt.Errorf("Gemini response contains an error envelope")
	}
	raw := root["candidates"]
	if len(raw) == 0 {
		return fmt.Errorf("invalid Gemini response: candidates missing")
	}
	var candidates []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &candidates); err != nil {
		return fmt.Errorf("invalid Gemini response candidates: %w", err)
	}
	if len(candidates) == 0 || len(candidates[0]["content"]) == 0 {
		return fmt.Errorf("invalid Gemini response: content missing")
	}
	return nil
}

// geminiResponseForClient decodes a non-streaming 2xx Gemini body into the
// client-facing response value (client is "openai" or "anthropic") plus
// token usage. The response body is always closed.
func geminiResponseForClient(resp *http.Response, client, model string) (any, int, int, error) {
	defer resp.Body.Close()
	var g canonical.GeminiResponse
	if err := decodeValidatedJSONLimited(resp.Body, &g, validateGeminiResponseJSON); err != nil {
		return nil, 0, 0, err
	}
	canonResp, err := canonical.FromGeminiResponse(g)
	if err != nil {
		return nil, 0, 0, err
	}
	if client == "anthropic" {
		return canonResp.ToAnthropicResponse(model), canonResp.InputTokens, canonResp.OutputTokens, nil
	}
	return canonResp.ToOpenAIResponse(model), canonResp.InputTokens, canonResp.OutputTokens, nil
}

// driveGeminiStream pumps an upstream Gemini SSE body through the canonical
// decoder. Content events go to visit (which aborts the pump by returning an
// error, e.g. on client write failure); usage and terminal state are folded
// into the return values. A nil error means a terminal finishReason was
// observed.
func driveGeminiStream(resp *http.Response, visit func(stream.Event) error) (finish string, in, out int, err error) {
	// The caller owns resp.Body (existing translator convention); the pump
	// only reads.
	dec := &stream.GeminiDecoder{}
	finish = "stop"
	pump := func(events []stream.Event) error {
		for _, ev := range events {
			switch ev.Kind {
			case stream.KindUsage:
				in, out = ev.InputTokens, ev.OutputTokens
			case stream.KindEnd:
				if ev.FinishReason != "" {
					finish = ev.FinishReason
				}
				if ev.InputTokens > 0 || ev.OutputTokens > 0 {
					in, out = ev.InputTokens, ev.OutputTokens
				}
			case stream.KindError:
				msg := ev.Text
				if msg == "" {
					msg = "upstream stream error"
				}
				return fmt.Errorf("%s", msg)
			default:
				if verr := visit(ev); verr != nil {
					return verr
				}
			}
		}
		return nil
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			events, ferr := dec.Feed(buf[:n])
			if perr := pump(events); perr != nil {
				return finish, in, out, perr
			}
			if ferr != nil {
				return finish, in, out, ferr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return finish, in, out, rerr
		}
	}
	events, ferr := dec.Finish()
	if perr := pump(events); perr != nil {
		return finish, in, out, perr
	}
	if ferr != nil {
		return finish, in, out, ferr
	}
	return finish, in, out, nil
}

// streamGeminiToOpenAI translates a Gemini GenerateContent SSE stream into
// OpenAI Chat Completions chunks. The role chunk is deferred until the first
// valid upstream event (pre-commit failover), empty tool argument streams
// are padded with "{}", and real usage goes out in a final usage-only chunk
// before [DONE].
func streamGeminiToOpenAI(w http.ResponseWriter, resp *http.Response, model string, usageSink func(prompt, completion int), requestID ...string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	var writeErr error
	emit := func(v any) {
		if writeErr != nil {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			writeErr = err
			return
		}
		_, writeErr = fmt.Fprintf(w, "data: %s\n\n", b)
		if writeErr == nil && fl != nil {
			fl.Flush()
		}
	}
	completionID := uniqueStreamID("chatcmpl", requestID...)
	chunk := func(delta map[string]any, finish *string) map[string]any {
		return map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	roleEmitted := false
	emitRoleOnce := func() {
		if roleEmitted {
			return
		}
		roleEmitted = true
		emit(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	}
	streamErrorChunk := func(message string) {
		if roleEmitted && writeErr == nil {
			emit(map[string]any{"error": map[string]any{"message": message, "type": "gateway_stream_error", "code": "provider_stream_error"}})
		}
	}
	toolIndex := map[int]int{}
	toolArgsSeen := map[int]bool{}
	nextTool := 0
	visit := func(ev stream.Event) error {
		emitRoleOnce()
		if writeErr != nil {
			return writeErr
		}
		switch ev.Kind {
		case stream.KindTextDelta:
			if ev.Text != "" {
				emit(chunk(map[string]any{"content": ev.Text}, nil))
			}
		case stream.KindToolCallStart:
			oi := nextTool
			nextTool++
			toolIndex[ev.ToolIndex] = oi
			emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "id": ev.ToolID, "type": "function", "function": map[string]any{"name": ev.ToolName, "arguments": ""}}}}, "finish_reason": nil}}})
		case stream.KindToolCallDelta:
			if oi, ok := toolIndex[ev.ToolIndex]; ok && ev.ToolArgs != "" {
				emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "function": map[string]any{"arguments": ev.ToolArgs}}}}, "finish_reason": nil}}})
				toolArgsSeen[oi] = true
			}
		}
		return writeErr
	}
	finish, inTokens, outTokens, derr := driveGeminiStream(resp, visit)
	if derr != nil {
		streamErrorChunk("upstream stream terminated before completion: " + derr.Error())
		return derr
	}
	emitRoleOnce()
	if writeErr != nil {
		return writeErr
	}
	for oi := 0; oi < nextTool; oi++ {
		if !toolArgsSeen[oi] {
			emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "function": map[string]any{"arguments": "{}"}}}}, "finish_reason": nil}}})
		}
	}
	if nextTool > 0 && finish == "stop" {
		// Gemini reports STOP even when the turn produced function calls;
		// OpenAI clients expect tool_calls.
		finish = "tool_calls"
	}
	emit(chunk(map[string]any{}, &finish))
	if writeErr != nil {
		return writeErr
	}
	usageSeen := inTokens > 0 || outTokens > 0
	if usageSeen {
		emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{}, "usage": map[string]any{"prompt_tokens": inTokens, "completion_tokens": outTokens, "total_tokens": inTokens + outTokens}})
		if writeErr != nil {
			return writeErr
		}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if usageSink != nil && usageSeen {
		usageSink(inTokens, outTokens)
	}
	return nil
}

type geminiToolStreamState struct {
	anthIndex int
	id, name  string
	started   bool
	pending   strings.Builder
}

// streamGeminiToAnthropic translates a Gemini GenerateContent SSE stream
// into Anthropic Messages SSE events. message_start is deferred until the
// first valid upstream event so a dead stream can still fail over
// pre-commit; after commit, truncation surfaces an error frame.
func streamGeminiToAnthropic(w http.ResponseWriter, resp *http.Response, model string, usageSink func(prompt, completion int), requestID ...string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	var writeErr error
	emit := func(name string, v any) {
		if writeErr != nil {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			writeErr = err
			return
		}
		_, writeErr = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if writeErr == nil && fl != nil {
			fl.Flush()
		}
	}
	messageID := uniqueStreamID("msg", requestID...)
	startEmitted := false
	ensureStart := func() {
		if startEmitted {
			return
		}
		startEmitted = true
		emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}})
		emit("ping", map[string]any{"type": "ping"})
	}
	emitErrorFrame := func(message string) {
		if startEmitted && writeErr == nil {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}})
		}
	}
	nextIndex := 0
	textIndex := -1
	textStarted := false
	tools := map[int]*geminiToolStreamState{}
	startText := func() {
		if textStarted {
			return
		}
		textIndex = nextIndex
		nextIndex++
		textStarted = true
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": textIndex, "content_block": map[string]any{"type": "text", "text": ""}})
	}
	startTool := func(st *geminiToolStreamState) {
		if st.started || st.name == "" {
			return
		}
		st.anthIndex = nextIndex
		nextIndex++
		st.started = true
		if st.id == "" {
			st.id = fmt.Sprintf("toolu_gemini_%d", st.anthIndex)
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": st.anthIndex, "content_block": map[string]any{"type": "tool_use", "id": st.id, "name": st.name, "input": map[string]any{}}})
		if st.pending.Len() > 0 {
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.anthIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": st.pending.String()}})
			st.pending.Reset()
		}
	}
	visit := func(ev stream.Event) error {
		ensureStart()
		if writeErr != nil {
			return writeErr
		}
		switch ev.Kind {
		case stream.KindTextDelta:
			if ev.Text != "" {
				startText()
				emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": textIndex, "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
			}
		case stream.KindToolCallStart:
			st := tools[ev.ToolIndex]
			if st == nil {
				st = &geminiToolStreamState{anthIndex: -1}
				tools[ev.ToolIndex] = st
			}
			if ev.ToolID != "" {
				st.id = ev.ToolID
			}
			if ev.ToolName != "" {
				st.name = ev.ToolName
			}
			startTool(st)
		case stream.KindToolCallDelta:
			st := tools[ev.ToolIndex]
			if st == nil {
				st = &geminiToolStreamState{anthIndex: -1}
				tools[ev.ToolIndex] = st
			}
			if ev.ToolName != "" && st.name == "" {
				st.name = ev.ToolName
			}
			if ev.ToolArgs != "" {
				if st.started {
					emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.anthIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolArgs}})
				} else {
					st.pending.WriteString(ev.ToolArgs)
				}
			}
			startTool(st)
		}
		return writeErr
	}
	finish, inTokens, outTokens, derr := driveGeminiStream(resp, visit)
	if derr != nil {
		emitErrorFrame("upstream stream terminated before completion: " + derr.Error())
		return derr
	}
	ensureStart()
	if writeErr != nil {
		return writeErr
	}
	if textStarted {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIndex})
	}
	orderedTools := make([]*geminiToolStreamState, 0, len(tools))
	for _, st := range tools {
		if !st.started {
			if st.name == "" {
				st.name = "tool"
			}
			startTool(st)
		}
		if st.started {
			orderedTools = append(orderedTools, st)
		}
	}
	sort.Slice(orderedTools, func(i, j int) bool { return orderedTools[i].anthIndex < orderedTools[j].anthIndex })
	for _, st := range orderedTools {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.anthIndex})
	}
	stop := "end_turn"
	switch finish {
	case "tool_calls":
		stop = "tool_use"
	case "length":
		stop = "max_tokens"
	case "content_filter":
		stop = "refusal"
	}
	if len(orderedTools) > 0 && stop == "end_turn" {
		stop = "tool_use"
	}
	usageSeen := inTokens > 0 || outTokens > 0
	usage := map[string]int{"output_tokens": outTokens}
	if usageSeen {
		usage["input_tokens"] = inTokens
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": usage})
	emit("message_stop", map[string]any{"type": "message_stop"})
	if writeErr != nil {
		return writeErr
	}
	if usageSink != nil && usageSeen {
		usageSink(inTokens, outTokens)
	}
	return nil
}
