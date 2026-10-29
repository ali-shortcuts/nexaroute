package canonical

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---------- Gemini client stream emitter ----------
//
// streamGenerateContent with alt=sse delivers one GenerateContentResponse
// JSON object per SSE data frame. The terminal frame carries finishReason
// (and usually usageMetadata); there is no [DONE] sentinel and no
// dedicated error frame. A StreamError after bytes are committed therefore
// ends the stream without a finishReason — the client sees a truncated
// stream — never a silent restart or a fabricated completion.

// geminiEmitter encodes canonical stream events into Gemini SSE frames.
type geminiEmitter struct {
	w        http.ResponseWriter
	fl       http.Flusher
	model    string
	writeErr error
	finished bool

	stopReason string
	stopSeen   bool
	usage      Usage
	usageSeen  bool

	toolArgs  map[int]*strings.Builder
	toolNames map[int]string
}

// NewGeminiEmitter streams canonical events as Gemini GenerateContent SSE.
func NewGeminiEmitter(w http.ResponseWriter, model string) *geminiEmitter {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	return &geminiEmitter{
		w: w, fl: fl, model: model,
		toolArgs:  map[int]*strings.Builder{},
		toolNames: map[int]string{},
	}
}

func (e *geminiEmitter) chunk(v any) {
	if e.writeErr != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		e.writeErr = err
		return
	}
	_, e.writeErr = fmt.Fprintf(e.w, "data: %s\n\n", b)
	if e.writeErr == nil && e.fl != nil {
		e.fl.Flush()
	}
}

func (e *geminiEmitter) candidateChunk(parts []GeminiPart) {
	e.chunk(GeminiResponse_{Candidates: []GeminiCandidate{{
		Content: &GeminiContent{Role: "model", Parts: parts},
	}}})
}

// Emit implements StreamEmitter for the Gemini client dialect.
func (e *geminiEmitter) Emit(ev StreamEvent) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	switch ev.Type {
	case StreamStart:
		if ev.Usage != nil {
			e.usage = *ev.Usage
			e.usageSeen = true
		}
	case StreamText:
		if ev.Text != "" {
			e.candidateChunk([]GeminiPart{{Text: ev.Text}})
		}
	case StreamThinking:
		if ev.Text != "" {
			thought := true
			e.candidateChunk([]GeminiPart{{Text: ev.Text, Thought: &thought}})
		}
	case StreamToolStart:
		if ev.ToolIndex < 0 || ev.ToolIndex > maxStreamToolIndex || len(ev.ToolName) > maxStreamToolName {
			return fmt.Errorf("stream tool metadata exceeds safe limit")
		}
		if _, exists := e.toolArgs[ev.ToolIndex]; !exists && len(e.toolArgs) >= maxStreamTools {
			return fmt.Errorf("stream exceeds safe tool count %d", maxStreamTools)
		}
		if _, ok := e.toolArgs[ev.ToolIndex]; !ok {
			e.toolArgs[ev.ToolIndex] = &strings.Builder{}
		}
		if ev.ToolName != "" {
			e.toolNames[ev.ToolIndex] = ev.ToolName
		}
	case StreamToolDelta:
		b, ok := e.toolArgs[ev.ToolIndex]
		if !ok {
			return fmt.Errorf("stream tool delta arrived before tool start")
		}
		if ev.ArgsFinal && b.Len() > 0 {
			break
		}
		_, _ = b.WriteString(ev.ArgsDelta)
	case StreamToolEnd:
		b, ok := e.toolArgs[ev.ToolIndex]
		if !ok {
			return fmt.Errorf("stream tool end arrived before tool start")
		}
		name := e.toolNames[ev.ToolIndex]
		args := strings.TrimSpace(b.String())
		if args == "" {
			args = "{}"
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(args), &obj); err != nil {
			return fmt.Errorf("stream tool arguments must be a JSON object")
		}
		e.candidateChunk([]GeminiPart{{FunctionCall: &GeminiCall{Name: name, Args: json.RawMessage(args)}}})
		delete(e.toolArgs, ev.ToolIndex)
		delete(e.toolNames, ev.ToolIndex)
	case StreamUsage:
		if ev.Usage != nil {
			e.usage = *ev.Usage
			e.usageSeen = true
		}
	case StreamEnd:
		e.stopReason = ev.StopReason
		e.stopSeen = true
	case StreamError:
		// Gemini SSE has no error frame: ending the stream here lets
		// the client observe a truncation instead of a fabricated
		// completion. The pump returns the error and the handler
		// closes the connection.
	default:
		// Unknown event kinds are dropped, never invented as content.
	}
	return e.writeErr
}

// Finish emits the terminal frame: finishReason plus usageMetadata when
// usage was observed. Idempotent.
func (e *geminiEmitter) Finish() error {
	if e.finished {
		return e.writeErr
	}
	e.finished = true
	reason := e.stopReason
	if !e.stopSeen || reason == "" {
		reason = StopEndTurn
	}
	cand := GeminiCandidate{FinishReason: stopReasonToGeminiFinish(reason)}
	// A terminal frame without content is the standard Gemini trailing
	// chunk; content chunks were already emitted as deltas.
	out := GeminiResponse_{Candidates: []GeminiCandidate{cand}}
	if e.usageSeen && (e.usage.InputTokens > 0 || e.usage.OutputTokens > 0) {
		out.UsageMetadata = &struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
			ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
		}{
			PromptTokenCount:        e.usage.InputTokens,
			CandidatesTokenCount:    e.usage.OutputTokens,
			CachedContentTokenCount: e.usage.CacheReadTokens,
			ThoughtsTokenCount:      e.usage.ReasoningTokens,
		}
	}
	e.chunk(out)
	return e.writeErr
}
