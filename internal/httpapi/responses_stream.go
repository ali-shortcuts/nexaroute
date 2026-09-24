package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/compat/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/compat/stream"
)

// Responses SSE streaming (Phase 10). Upstream SSE bytes are normalized
// through a compat/stream decoder, and the canonical events are rendered as
// OpenAI Responses stream events. The translator never parses provider SSE
// itself.
//
// Wire contract (subset of the Responses streaming API):
//
//	response.created -> response.in_progress
//	[per text output] response.output_item.added (message)
//	  response.content_part.added -> response.output_text.delta*
//	  response.output_text.done -> response.content_part.done
//	  -> response.output_item.done
//	[per tool call] response.output_item.added (function_call)
//	  -> response.function_call_arguments.delta* -> response.output_item.done
//	response.completed (full response object + usage)
//
// Commit rule (same as the chat translators): nothing is written until the
// first valid upstream event proves the stream is alive, so a 200 response
// that dies before streaming can still fail over pre-commit. After commit,
// a truncation surfaces response.failed instead of a silent cut.
func streamUpstreamToResponses(w http.ResponseWriter, resp *http.Response, dec stream.Decoder, model, responseID string, usageSink func(prompt, completion int), requestID ...string) error {
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
	createdEmitted := false
	emitCreated := func() {
		if createdEmitted {
			return
		}
		createdEmitted = true
		base := map[string]any{"id": responseID, "object": "response", "model": model, "status": "in_progress", "output": []any{}}
		emit(map[string]any{"type": "response.created", "response": base})
		emit(map[string]any{"type": "response.in_progress", "response": base})
	}
	emitFailed := func(message string) {
		if createdEmitted && writeErr == nil {
			emit(map[string]any{"type": "response.failed", "response": map[string]any{
				"id": responseID, "object": "response", "model": model,
				"status": "failed", "output": []any{},
				"error": map[string]any{"message": message, "type": "gateway_stream_error", "code": "provider_stream_error"},
			}})
		}
	}

	type toolState struct {
		outputIndex int
		itemID      string
		callID      string
		name        string
		args        strings.Builder
		argsSeen    bool
		closed      bool
	}
	nextOutputIndex := 0
	textOpen := false
	textOutputIndex := 0
	textItemID := ""
	var fullText strings.Builder
	tools := map[int]*toolState{}
	outputOrder := []int{} // tool indices in emission order (for final output)
	inputTokens, outputTokens := 0, 0
	finish := "stop"

	openText := func() {
		if textOpen {
			return
		}
		textOpen = true
		textOutputIndex = nextOutputIndex
		nextOutputIndex++
		textItemID = uniqueStreamID("msg", requestID...)
		emit(map[string]any{"type": "response.output_item.added", "output_index": textOutputIndex,
			"item": map[string]any{"id": textItemID, "type": "message", "role": "assistant", "content": []any{}}})
		emit(map[string]any{"type": "response.content_part.added", "output_index": textOutputIndex,
			"item_id": textItemID, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": ""}})
	}
	openTool := func(ev stream.Event) *toolState {
		if st, ok := tools[ev.ToolIndex]; ok {
			return st
		}
		st := &toolState{outputIndex: nextOutputIndex, itemID: uniqueStreamID("fc", requestID...)}
		nextOutputIndex++
		st.callID = ev.ToolID
		if st.callID == "" {
			st.callID = fmt.Sprintf("call_%s_%d", responseID, ev.ToolIndex)
		}
		st.name = ev.ToolName
		tools[ev.ToolIndex] = st
		outputOrder = append(outputOrder, ev.ToolIndex)
		emit(map[string]any{"type": "response.output_item.added", "output_index": st.outputIndex,
			"item": map[string]any{"id": st.itemID, "type": "function_call",
				"call_id": st.callID, "name": st.name, "arguments": ""}})
		return st
	}
	closeTool := func(st *toolState) {
		if st.closed {
			return
		}
		st.closed = true
		args := st.args.String()
		if args == "" {
			// Same padding rule as the chat translators: a tool call that
			// never streamed arguments still needs a valid JSON object.
			args = "{}"
		}
		emit(map[string]any{"type": "response.output_item.done", "output_index": st.outputIndex,
			"item": map[string]any{"id": st.itemID, "type": "function_call",
				"call_id": st.callID, "name": st.name, "arguments": args}})
	}

	feedEvents := func(events []stream.Event) {
		for _, ev := range events {
			if writeErr != nil {
				return
			}
			emitCreated()
			switch ev.Kind {
			case stream.KindTextDelta:
				if ev.Text == "" {
					continue
				}
				openText()
				fullText.WriteString(ev.Text)
				emit(map[string]any{"type": "response.output_text.delta",
					"output_index": textOutputIndex, "content_index": 0,
					"item_id": textItemID, "delta": ev.Text})
			case stream.KindReasoningDelta:
				// Dropped, consistent with the non-streaming Responses
				// object (canonicalToResponsesObject carries text + tool
				// calls only). Reasoning-aware Responses items are future work.
			case stream.KindToolCallStart:
				openTool(ev)
			case stream.KindToolCallDelta:
				st := openTool(ev)
				if ev.ToolArgs != "" {
					st.args.WriteString(ev.ToolArgs)
					st.argsSeen = true
					emit(map[string]any{"type": "response.function_call_arguments.delta",
						"output_index": st.outputIndex, "item_id": st.itemID, "delta": ev.ToolArgs})
				}
				if st.name == "" && ev.ToolName != "" {
					st.name = ev.ToolName
				}
				if ev.ToolID != "" && strings.HasPrefix(st.callID, "call_"+responseID) {
					st.callID = ev.ToolID
				}
			case stream.KindToolCallEnd:
				if st, ok := tools[ev.ToolIndex]; ok {
					closeTool(st)
				}
			case stream.KindUsage:
				inputTokens, outputTokens = ev.InputTokens, ev.OutputTokens
			case stream.KindStart:
				// No Responses equivalent; creation events already emitted.
			case stream.KindEnd:
				sawEnd = true
				if ev.FinishReason != "" {
					finish = ev.FinishReason
				}
				if ev.InputTokens > 0 || ev.OutputTokens > 0 {
					inputTokens, outputTokens = ev.InputTokens, ev.OutputTokens
				}
			case stream.KindError:
				// Handled by the caller via the decoder error return; a
				// bare error event without a Go error still fails loudly.
				if writeErr == nil {
					writeErr = fmt.Errorf("upstream stream error: %s", ev.Text)
				}
			}
		}
	}

	buf := make([]byte, 32<<10)
	terminal := false
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			events, ferr := dec.Feed(buf[:n])
			feedEvents(events)
			if writeErr != nil {
				emitFailed(writeErr.Error())
				return writeErr
			}
			if ferr != nil {
				emitFailed("upstream stream sent an invalid event")
				return ferr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			emitFailed("upstream stream terminated before completion: " + err.Error())
			return err
		}
	}
	events, ferr := dec.Finish()
	feedEvents(events)
	if writeErr != nil {
		emitFailed(writeErr.Error())
		return writeErr
	}
	if ferr != nil {
		emitFailed("upstream stream ended without a terminal event")
		return ferr
	}
	for _, ev := range events {
		if ev.Kind == stream.KindEnd {
			terminal = true
		}
	}
	if !terminal {
		// Decoders guarantee a terminal KindEnd on clean Finish; reaching
		// here means the upstream closed a well-formed but empty stream.
		// An empty completion is still a completion: commit and finish.
		emitCreated()
	}

	if textOpen {
		text := fullText.String()
		emit(map[string]any{"type": "response.output_text.done",
			"output_index": textOutputIndex, "content_index": 0,
			"item_id": textItemID, "text": text})
		emit(map[string]any{"type": "response.content_part.done",
			"output_index": textOutputIndex, "content_index": 0, "item_id": textItemID,
			"part": map[string]any{"type": "output_text", "text": text}})
		emit(map[string]any{"type": "response.output_item.done", "output_index": textOutputIndex,
			"item": map[string]any{"id": textItemID, "type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text}}}})
	}
	for _, idx := range outputOrder {
		closeTool(tools[idx])
	}
	if writeErr != nil {
		return writeErr
	}
	canonResp := canonical.Response{Text: fullText.String(), StopReason: finish, InputTokens: inputTokens, OutputTokens: outputTokens}
	for _, idx := range outputOrder {
		st := tools[idx]
		args := map[string]any{}
		raw := strings.TrimSpace(st.args.String())
		if raw == "" {
			raw = "{}"
		}
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			args = map[string]any{"_raw": raw}
		}
		canonResp.ToolCalls = append(canonResp.ToolCalls, canonical.ToolCall{ID: st.callID, Name: st.name, Arguments: args, RawArgs: raw})
	}
	completed := canonicalToResponsesObject(model, canonResp, responseID)
	switch finish {
	case "length":
		completed["status"] = "incomplete"
		completed["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	default:
		completed["status"] = "completed"
	}
	emit(map[string]any{"type": "response.completed", "response": completed})
	if writeErr != nil {
		return writeErr
	}
	if fl != nil {
		fl.Flush()
	}
	if usageSink != nil {
		usageSink(inputTokens, outputTokens)
	}
	return nil
}
