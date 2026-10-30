package canonical

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// issue663FailWriter is an http.ResponseWriter whose writes always fail.
type issue663FailWriter struct {
	header http.Header
}

func (w *issue663FailWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *issue663FailWriter) Write([]byte) (int, error) {
	return 0, errors.New("issue663 fake write failure")
}

func (w *issue663FailWriter) WriteHeader(int) {}

// issue663CountWriter succeeds for the first n writes, then fails.
type issue663CountWriter struct {
	header http.Header
	body   strings.Builder
	left   int
}

func (w *issue663CountWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *issue663CountWriter) Write(p []byte) (int, error) {
	if w.left <= 0 {
		return 0, errors.New("issue663 fake write failure")
	}
	w.left--
	return w.body.Write(p)
}

func (w *issue663CountWriter) WriteHeader(int) {}

func issue663GeminiBase() GeminiRequest {
	return GeminiRequest{Contents: []GeminiContent{{
		Role:  "user",
		Parts: []GeminiPart{{Text: "hi"}},
	}}}
}

func TestIssue663ProvidersGeminiIngressRejects(t *testing.T) {
	if _, err := DecodeGeminiIngressRequest(issue663GeminiBase(), "  "); err == nil {
		t.Fatal("empty model was accepted")
	}
	req := issue663GeminiBase()
	req.SafetySettings = []map[string]any{{"category": "HARM"}}
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil {
		t.Fatal("safetySettings were accepted")
	}
	req = issue663GeminiBase()
	req.Contents = append(req.Contents, GeminiContent{Parts: []GeminiPart{{Text: "later"}}})
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "role is required") {
		t.Fatalf("later role-less content = %v, want a role-required error", err)
	}
	req = issue663GeminiBase()
	req.Contents[0].Role = "system"
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "unsupported role") {
		t.Fatalf("unsupported role = %v, want an unsupported-role error", err)
	}
	req = issue663GeminiBase()
	req.Contents[0].Parts = nil
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "at least one part") {
		t.Fatalf("empty parts = %v, want an at-least-one-part error", err)
	}
	req = issue663GeminiBase()
	req.Tools = []GeminiTool{{FunctionDeclarations: []map[string]any{{"description": "no name"}}}}
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil {
		t.Fatal("unnamed function declaration was accepted")
	}
	req = issue663GeminiBase()
	req.Tools = []GeminiTool{{FunctionDeclarations: []map[string]any{{"name": "f", "parameters": "not-an-object"}}}}
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("non-object parameters = %v, want a JSON-object error", err)
	}
	// A system instruction part without text fails closed.
	req = issue663GeminiBase()
	req.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{Text: ""}}}
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "unsupported systemInstruction part") {
		t.Fatalf("empty system part = %v, want an unsupported-part error", err)
	}
	// Parameters that cannot be marshaled fail at the marshal step.
	req = issue663GeminiBase()
	req.Tools = []GeminiTool{{FunctionDeclarations: []map[string]any{{"name": "f", "parameters": func() {}}}}}
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "not serializable") {
		t.Fatalf("func parameters = %v, want a not-serializable error", err)
	}
}

func TestIssue663ProvidersGeminiToolConfigModes(t *testing.T) {
	mode := func(m string, names []string) *ToolChoice {
		req := issue663GeminiBase()
		req.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: &struct {
			Mode                 string   `json:"mode,omitempty"`
			AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
		}{Mode: m, AllowedFunctionNames: names}}
		out, err := DecodeGeminiIngressRequest(req, "m")
		if err != nil {
			t.Fatal(err)
		}
		return out.ToolChoice
	}
	if got := mode("MODE_AUTO", nil); got == nil || got.Mode != "auto" {
		t.Fatalf("MODE_AUTO = %+v, want auto", got)
	}
	if got := mode("", nil); got == nil || got.Mode != "auto" {
		t.Fatalf("empty mode = %+v, want auto", got)
	}
	if got := mode("NONE", nil); got == nil || got.Mode != "none" {
		t.Fatalf("NONE = %+v, want none", got)
	}
	if got := mode("ANY", []string{"a", "b"}); got == nil || got.Mode != "required" {
		t.Fatalf("ANY with two names = %+v, want required", got)
	}
}

func TestIssue663ProvidersGeminiResponseMime(t *testing.T) {
	req := issue663GeminiBase()
	req.GenerationConfig = &GeminiGenerationConfig{ResponseMimeType: "text/plain", ResponseSchema: json.RawMessage(`{"type":"object"}`)}
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil {
		t.Fatal("text/plain with a schema was accepted")
	}
	req.GenerationConfig = &GeminiGenerationConfig{ResponseMimeType: "text/plain"}
	out, err := DecodeGeminiIngressRequest(req, "m")
	if err != nil || out.ResponseFormat != nil {
		t.Fatalf("text/plain without schema = %+v %v, want no response format", out.ResponseFormat, err)
	}
	req.GenerationConfig = &GeminiGenerationConfig{ResponseMimeType: "application/json", ResponseSchema: json.RawMessage(`{"type":"object"}`)}
	out, err = DecodeGeminiIngressRequest(req, "m")
	if err != nil || out.ResponseFormat == nil || out.ResponseFormat.Kind != FormatJSONSchema {
		t.Fatalf("json with schema = %+v %v, want json_schema", out.ResponseFormat, err)
	}
	req.GenerationConfig = &GeminiGenerationConfig{ResponseMimeType: "text/html"}
	if _, err := DecodeGeminiIngressRequest(req, "m"); err == nil {
		t.Fatal("unknown responseMimeType was accepted")
	}
}

func TestIssue663ProvidersGeminiIngressParts(t *testing.T) {
	if _, err := geminiIngressPart(GeminiPart{FunctionCall: &GeminiCall{Name: " "}}); err == nil {
		t.Fatal("unnamed functionCall was accepted")
	}
	p, err := geminiIngressPart(GeminiPart{FunctionCall: &GeminiCall{Name: "f"}})
	if err != nil || p.Type != PartToolCall || p.ToolCall.Arguments != "{}" {
		t.Fatalf("empty-args call = %+v %v, want tool call with {}", p, err)
	}
	if _, err := geminiIngressPart(GeminiPart{FunctionCall: &GeminiCall{Name: "f", Args: json.RawMessage(`not-json`)}}); err == nil {
		t.Fatal("non-object functionCall args were accepted")
	}
	p, err = geminiIngressPart(GeminiPart{FunctionCallV1: &GeminiCall{Name: "g", Args: json.RawMessage(`{"x":1}`)}})
	if err != nil || p.Type != PartToolCall || p.ToolCall.Name != "g" {
		t.Fatalf("V1 call = %+v %v, want tool call g", p, err)
	}
	if _, err := geminiIngressPart(GeminiPart{FunctionResponse: &GeminiResponse{Name: " "}}); err == nil {
		t.Fatal("unnamed functionResponse was accepted")
	}
	p, err = geminiIngressPart(GeminiPart{FunctionResponse: &GeminiResponse{Name: "f", Response: json.RawMessage(`"ok"`)}})
	if err != nil || p.Type != PartToolResult || p.ToolResult.ToolUseID != "f" || p.ToolResult.Content != `"ok"` {
		t.Fatalf("functionResponse = %+v %v, want tool result f/ok", p, err)
	}
	p, err = geminiIngressPart(GeminiPart{FunctionResponseV1: &GeminiResponse{Name: "h"}})
	if err != nil || p.Type != PartToolResult || p.ToolResult.ToolUseID != "h" || p.ToolResult.Content != "" {
		t.Fatalf("V1 response = %+v %v, want tool result h/empty", p, err)
	}
	if _, err := geminiIngressPart(GeminiPart{InlineData: &GeminiBlob{}}); err == nil {
		t.Fatal("inlineData without data was accepted")
	}
	p, err = geminiIngressPart(GeminiPart{InlineData: &GeminiBlob{Data: "aGVsbG8="}})
	if err != nil || p.Type != PartImage || p.Image.MediaType != "image/png" {
		t.Fatalf("mime-less inlineData = %+v %v, want image/png", p, err)
	}
	p, err = geminiIngressPart(GeminiPart{InlineData: &GeminiBlob{MimeType: "image/jpeg", Data: "x"}})
	if err != nil || p.Image.MediaType != "image/jpeg" {
		t.Fatalf("jpeg inlineData = %+v %v, want image/jpeg", p, err)
	}
	p, err = geminiIngressPart(GeminiPart{InlineDataV1: &GeminiBlob{MimeV1: "image/gif", Data: "x"}})
	if err != nil || p.Image.MediaType != "image/gif" {
		t.Fatalf("V1 inlineData = %+v %v, want image/gif", p, err)
	}
	p, err = geminiIngressPart(GeminiPart{Text: "t", Thought: boolPtr(true)})
	if err != nil || p.Type != PartThinking || p.Thinking.Text != "t" {
		t.Fatalf("thought part = %+v %v, want thinking", p, err)
	}
	p, err = geminiIngressPart(GeminiPart{Text: "hi"})
	if err != nil || p.Type != PartText || p.Text != "hi" {
		t.Fatalf("text part = %+v %v, want text hi", p, err)
	}
	if _, err := geminiIngressPart(GeminiPart{}); err == nil {
		t.Fatal("empty part was accepted")
	}
}

func TestIssue663ProvidersEncodeGeminiResponse(t *testing.T) {
	out, err := EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartText, Text: ""}, {Type: PartThinking}}})
	if err != nil || len(out.Candidates) != 1 || len(out.Candidates[0].Content.Parts) != 1 || out.Candidates[0].Content.Parts[0].Text != "" {
		t.Fatalf("empty blocks = %+v %v, want a single empty text part", out, err)
	}
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartThinking, Thinking: &Thinking{Text: "th"}}}})
	if err != nil || out.Candidates[0].Content.Parts[0].Thought == nil || !*out.Candidates[0].Content.Parts[0].Thought {
		t.Fatalf("thinking block = %+v %v, want a thought part", out, err)
	}
	if _, err := EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: " "}}}}); err == nil {
		t.Fatal("unnamed tool call was encoded")
	}
	if _, err := EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartToolCall}}}); err == nil {
		t.Fatal("nil tool call was encoded")
	}
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "f"}}}})
	if err != nil || string(out.Candidates[0].Content.Parts[0].FunctionCall.Args) != "{}" {
		t.Fatalf("empty-args tool call = %+v %v, want {}", out, err)
	}
	out, err = EncodeGeminiResponse(Response{
		StopReason: StopMaxTokens,
		Usage:      Usage{InputTokens: 3, OutputTokens: 4, CacheReadTokens: 1, ReasoningTokens: 2},
	})
	if err != nil || out.Candidates[0].FinishReason != "MAX_TOKENS" || out.UsageMetadata == nil || out.UsageMetadata.PromptTokenCount != 3 || out.UsageMetadata.ThoughtsTokenCount != 2 {
		t.Fatalf("max-tokens response = %+v %v, want MAX_TOKENS with usage", out, err)
	}
	out, err = EncodeGeminiResponse(Response{StopReason: StopRefusal})
	if err != nil || out.Candidates[0].FinishReason != "SAFETY" || out.PromptFeedback == nil || out.PromptFeedback.BlockReason != "SAFETY" {
		t.Fatalf("refusal response = %+v %v, want SAFETY with prompt feedback", out, err)
	}
	out, err = EncodeGeminiResponse(Response{StopReason: StopEndTurn})
	if err != nil || out.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("end-turn response = %+v %v, want STOP", out, err)
	}
}

func TestIssue663ProvidersGeminiEmitter(t *testing.T) {
	// NewStreamEmitter routes gemini to the Gemini emitter.
	rec := httptest.NewRecorder()
	em, ok := NewStreamEmitter("gemini", rec, "m", "r").(*geminiEmitter)
	if !ok {
		t.Fatal("gemini routing did not yield a geminiEmitter")
	}
	if err := em.Emit(StreamEvent{Type: StreamText, Text: "hi"}); err != nil || !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("gemini text emit = %v %q, want a hi frame", err, rec.Body.String())
	}
	// Start with usage, then Finish carries usageMetadata.
	rec = httptest.NewRecorder()
	em = NewGeminiEmitter(rec, "m")
	_ = em.Emit(StreamEvent{Type: StreamStart, Usage: &Usage{InputTokens: 3, OutputTokens: 4}})
	_ = em.Emit(StreamEvent{Type: StreamEnd, StopReason: StopEndTurn})
	if err := em.Finish(); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"promptTokenCount":3`) || !strings.Contains(body, `"candidatesTokenCount":4`) {
		t.Fatalf("gemini usage finish = %s, want token counts", body)
	}
	// Thinking chunk carries the thought flag.
	rec = httptest.NewRecorder()
	em = NewGeminiEmitter(rec, "m")
	_ = em.Emit(StreamEvent{Type: StreamThinking, Text: "th"})
	if !strings.Contains(rec.Body.String(), `"thought":true`) {
		t.Fatalf("gemini thinking = %s, want thought:true", rec.Body.String())
	}
	// Tool metadata limits.
	em = NewGeminiEmitter(httptest.NewRecorder(), "m")
	for _, ev := range []StreamEvent{
		{Type: StreamToolStart, ToolIndex: -1, ToolName: "f"},
		{Type: StreamToolStart, ToolIndex: 4097, ToolName: "f"},
		{Type: StreamToolStart, ToolIndex: 0, ToolName: strings.Repeat("x", 1025)},
	} {
		if err := em.Emit(ev); err == nil {
			t.Fatalf("tool start %+v was accepted", ev)
		}
	}
	// Tool count limit: 128 starts succeed, the 129th fails.
	em = NewGeminiEmitter(httptest.NewRecorder(), "m")
	for i := 0; i < 128; i++ {
		if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: i, ToolName: "f"}); err != nil {
			t.Fatalf("tool start %d: %v", i, err)
		}
	}
	if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 128, ToolName: "f"}); err == nil {
		t.Fatal("129th tool start was accepted")
	}
	// Delta / end before start.
	em = NewGeminiEmitter(httptest.NewRecorder(), "m")
	if err := em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 3, ArgsDelta: "{}"}); err == nil {
		t.Fatal("tool delta before start was accepted")
	}
	if err := em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 9}); err == nil {
		t.Fatal("tool end before start was accepted")
	}
	// ArgsFinal with existing args is ignored, not appended.
	rec = httptest.NewRecorder()
	em = NewGeminiEmitter(rec, "m")
	_ = em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: "f"})
	_ = em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `{"a":1}`})
	_ = em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `{"b":2}`, ArgsFinal: true})
	_ = em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 0})
	body = rec.Body.String()
	if !strings.Contains(body, `"a":1`) || strings.Contains(body, `"b":2`) {
		t.Fatalf("args-final body = %s, want only a:1", body)
	}
	// Empty tool args default to {}.
	rec = httptest.NewRecorder()
	em = NewGeminiEmitter(rec, "m")
	_ = em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: "f"})
	_ = em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 0})
	if !strings.Contains(rec.Body.String(), `"args":{}`) {
		t.Fatalf("empty tool args = %s, want args {}", rec.Body.String())
	}
	// Invalid tool args fail at end.
	em = NewGeminiEmitter(httptest.NewRecorder(), "m")
	_ = em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: "f"})
	_ = em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `not-json`})
	if err := em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 0}); err == nil {
		t.Fatal("invalid tool args were accepted")
	}
	// Chunk marshal failure is sticky.
	em = NewGeminiEmitter(httptest.NewRecorder(), "m")
	em.candidateChunk([]GeminiPart{{FunctionCall: &GeminiCall{Name: "f", Args: json.RawMessage("{bad")}}})
	if em.writeErr == nil {
		t.Fatal("chunk marshal failure did not stick")
	}
	// Write failure: Emit reports it, a second Emit reports the same
	// error, and Finish ends through the chunk early return.
	em = NewGeminiEmitter(&issue663FailWriter{}, "m")
	if err := em.Emit(StreamEvent{Type: StreamText, Text: "x"}); err == nil {
		t.Fatal("failing write was accepted")
	}
	if err := em.Emit(StreamEvent{Type: StreamText, Text: "y"}); err == nil {
		t.Fatal("second emit after write failure was accepted")
	}
	if err := em.Finish(); err == nil {
		t.Fatal("Finish after write failure was accepted")
	}
	// Finish is idempotent and defaults an unseen stop reason to STOP.
	rec = httptest.NewRecorder()
	em = NewGeminiEmitter(rec, "m")
	if err := em.Finish(); err != nil {
		t.Fatal(err)
	}
	body = rec.Body.String()
	if !strings.Contains(body, `"finishReason":"STOP"`) {
		t.Fatalf("default finish = %s, want STOP", body)
	}
	if err := em.Finish(); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != body {
		t.Fatal("second Finish wrote again")
	}
	// Error and unknown events are dropped without a frame.
	rec = httptest.NewRecorder()
	em = NewGeminiEmitter(rec, "m")
	if err := em.Emit(StreamEvent{Type: StreamError, ErrorMsg: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: "bogus"}); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "" {
		t.Fatalf("dropped events wrote %q", rec.Body.String())
	}
}

func TestIssue663ProvidersAnthropicEmitterBranches(t *testing.T) {
	// frame early return on a set write error writes nothing.
	rec := httptest.NewRecorder()
	ae := &anthropicEmitter{w: rec, writeErr: errors.New("prior")}
	ae.frame("x", map[string]any{"type": "x"})
	if rec.Body.String() != "" {
		t.Fatalf("guarded frame wrote %q", rec.Body.String())
	}
	// frame marshal failure sticks.
	ae = &anthropicEmitter{w: rec}
	ae.frame("x", map[string]any{"f": func() {}})
	if ae.writeErr == nil {
		t.Fatal("frame marshal failure did not stick")
	}
	// Nameless frame is a bare data frame.
	rec = httptest.NewRecorder()
	ae = &anthropicEmitter{w: rec}
	ae.frame("", map[string]any{"type": "t"})
	if body := rec.Body.String(); !strings.HasPrefix(body, "data:") || strings.Contains(body, "event:") {
		t.Fatalf("nameless frame = %q, want a bare data frame", body)
	}
	// Emit guard: the constructor frame fails on a failing writer, so
	// the next Emit reports the stored error.
	ae2 := NewAnthropicEmitter(&issue663FailWriter{}, "m", "r")
	if err := ae2.Emit(StreamEvent{Type: StreamText, Text: "x"}); err == nil {
		t.Fatal("emit after constructor write failure was accepted")
	}
	// Tool delta without start: the constructor frames message_start
	// and ping (two writes), so the recursive start's frame is the
	// third write; failing it makes the outer Emit report that failure.
	cw := &issue663CountWriter{left: 2}
	ae3 := NewAnthropicEmitter(cw, "m", "r")
	err := ae3.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 7, ToolName: "f", ArgsDelta: `{"a":1}`})
	if err == nil || !strings.Contains(err.Error(), "fake write failure") {
		t.Fatalf("tool delta with a failing recursive start = %v, want the write failure", err)
	}
	// Finish closes an open thinking block.
	rec = httptest.NewRecorder()
	ae4 := NewAnthropicEmitter(rec, "m", "r")
	_ = ae4.Emit(StreamEvent{Type: StreamThinking, Text: "th"})
	if err := ae4.Finish(); err != nil {
		t.Fatal(err)
	}
	if body := rec.Body.String(); !strings.Contains(body, "thinking_delta") || !strings.Contains(body, "content_block_stop") {
		t.Fatalf("thinking finish = %s, want a closed thinking block", body)
	}
}

func TestIssue663ProvidersOpenAIAndResponsesEmitters(t *testing.T) {
	// OpenAI chunk guard and marshal failure (direct, white-box).
	rec := httptest.NewRecorder()
	oe := &openAIEmitter{w: rec, writeErr: errors.New("prior"), id: "x", model: "m", created: 1}
	oe.chunk(map[string]any{"a": 1})
	if rec.Body.String() != "" {
		t.Fatalf("guarded openai chunk wrote %q", rec.Body.String())
	}
	oe = &openAIEmitter{w: rec, id: "x", model: "m", created: 1}
	oe.chunk(map[string]any{"f": func() {}})
	if oe.writeErr == nil {
		t.Fatal("openai chunk marshal failure did not stick")
	}
	// OpenAI Emit guard after a write failure.
	oe2 := NewOpenAIEmitter(&issue663FailWriter{}, "m")
	if err := oe2.Emit(StreamEvent{Type: StreamText, Text: "x"}); err == nil {
		t.Fatal("openai emit after write failure was accepted")
	}
	if err := oe2.Emit(StreamEvent{Type: StreamText, Text: "y"}); err == nil {
		t.Fatal("second openai emit after write failure was accepted")
	}
	// Responses event guard and marshal failure (direct, white-box).
	rec = httptest.NewRecorder()
	re_ := &responsesEmitter{w: rec, writeErr: errors.New("prior")}
	re_.event("x", map[string]any{"type": "x"})
	if rec.Body.String() != "" {
		t.Fatalf("guarded responses event wrote %q", rec.Body.String())
	}
	re_ = &responsesEmitter{w: rec}
	re_.event("x", map[string]any{"f": func() {}})
	if re_.writeErr == nil {
		t.Fatal("responses event marshal failure did not stick")
	}
	// Responses Emit guard after a constructor write failure.
	re2 := NewResponsesEmitter(&issue663FailWriter{}, "m")
	if err := re2.Emit(StreamEvent{Type: StreamText, Text: "x"}); err == nil {
		t.Fatal("responses emit after write failure was accepted")
	}
	// Responses tool delta limit.
	re3 := NewResponsesEmitter(httptest.NewRecorder(), "m")
	if err := re3.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: -1, ArgsDelta: "{}"}); err == nil {
		t.Fatal("responses tool delta with a bad index was accepted")
	}
	// Responses Start with nil usage is a no-op; with usage it feeds
	// Finish, including reasoning tokens.
	rec = httptest.NewRecorder()
	re4 := NewResponsesEmitter(rec, "m")
	if err := re4.Emit(StreamEvent{Type: StreamStart}); err != nil {
		t.Fatal(err)
	}
	_ = re4.Emit(StreamEvent{Type: StreamStart, Usage: &Usage{InputTokens: 3, OutputTokens: 2, CacheReadTokens: 1, ReasoningTokens: 5}})
	if err := re4.Finish(); err != nil {
		t.Fatal(err)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"reasoning_tokens":5`) || !strings.Contains(body, `"cached_tokens":1`) {
		t.Fatalf("responses usage finish = %s, want reasoning/cached tokens", body)
	}
	// Responses Finish guard after a constructor write failure.
	re5 := NewResponsesEmitter(&issue663FailWriter{}, "m")
	if err := re5.Finish(); err == nil {
		t.Fatal("responses Finish after write failure was accepted")
	}
}

func TestIssue663ProvidersStragglers(t *testing.T) {
	if got := marshalSchema(map[string]any{"f": func() {}}); got != nil {
		t.Fatalf("marshalSchema of a func-valued map = %s, want nil", got)
	}
	if got := anthToolInputArgs(map[string]any{"f": func() {}}); got != "{}" {
		t.Fatalf("anthToolInputArgs of a func-valued map = %s, want {}", got)
	}
	if _, _, err := EncodeBedrockRequest(Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}},
		Tools:    []ToolDef{{Name: "f", Parameters: json.RawMessage(`{bad`)}},
	}, "m"); err == nil {
		t.Fatal("bedrock encode with invalid tool schema was accepted")
	}
	resp, err := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"function_call":{"name":"f"}}]}}]}`))
	if err != nil || len(resp.Blocks) != 1 || resp.Blocks[0].ToolCall.Arguments != "{}" {
		t.Fatalf("V1 empty-args response = %+v %v, want {} args", resp, err)
	}
	// A number where the schema wants a string fails at the
	// type-mismatch branch (the redundant string re-check below it
	// is unreachable: typeMatches("string") already proves a string).
	meta := ValidationMeta{Stage: "test", Protocol: "anthropic", Provider: "p"}
	if err := ValidateToolCallArguments(
		json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		`{"name":42}`, meta); err == nil || !strings.Contains(err.Error(), "expected string got integer") {
		t.Fatalf("string-type mismatch = %v, want expected-string-got-integer", err)
	}
	// Penalties with no prior metadata initialize the metadata map.
	req, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
		FrequencyPenalty: f64p(0.5),
		Messages:         []core.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "m")
	if err != nil || req.Metadata["frequency_penalty"] != 0.5 {
		t.Fatalf("penalty-only decode = %+v %v, want frequency_penalty 0.5", req.Metadata, err)
	}
	// An assistant message with an unsupported part fails closed at
	// the assistant branch (the user branch is already covered).
	if _, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
		Messages: []core.OpenAIMessage{{Role: "assistant", Content: []any{map[string]any{"type": "audio"}}}},
	}, "m"); err == nil || !strings.Contains(err.Error(), "unsupported OpenAI content part") {
		t.Fatalf("assistant audio part = %v, want an unsupported-part error", err)
	}
	// messageClock's monotonic guard: a stored future value forces the
	// n <= last branch, returning last+1 deterministically.
	prev := messageClockNanos.Load()
	defer messageClockNanos.Store(prev)
	future := prev + 1000000000
	messageClockNanos.Store(future)
	if got := messageClock(); got != future+1 {
		t.Fatalf("messageClock with a future stored value = %d, want %d", got, future+1)
	}
	if got := openAIContentToText(func() {}); got != "" {
		t.Fatalf("openAIContentToText of a func = %q, want empty", got)
	}
	if parts, err := openAIContentToParts(func() {}, false); err != nil || parts != nil {
		t.Fatalf("openAIContentToParts of a func = %v %v, want nil, nil", parts, err)
	}
	out, err := EncodeOpenAIChatRequest(Request{Messages: []Message{{Role: RoleAssistant,
		Parts: []Part{{Type: PartText, Text: "a"}, {Type: PartText, Text: "b"}}}}}, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.Messages[0].Content.([]map[string]any); !ok {
		t.Fatalf("two-text assistant message content = %#v, want a parts array", out.Messages[0].Content)
	}
	if got := partsToText([]Part{{Type: PartText, Text: "a"}, {Type: PartText, Text: "b"}}); got != "a\nb" {
		t.Fatalf("partsToText = %q, want a\\nb", got)
	}
	if v, ok := floatFromMetadata(map[string]any{"k": float64(2.5)}, "k"); !ok || v != 2.5 {
		t.Fatalf("floatFromMetadata = %v %v, want 2.5 true", v, ok)
	}
	chatResp, err := DecodeOpenAIChatResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":["oops",{"type":"text","text":"hi"}]},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range chatResp.Blocks {
		if b.Text == "hi" {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-map content element: blocks = %+v, want a hi block", chatResp.Blocks)
	}
	if parts, err := responsesContentToParts(json.RawMessage(`""`)); err != nil || parts != nil {
		t.Fatalf("empty-string responses content = %v %v, want nil, nil", parts, err)
	}
	// A bare "{bad" fails at the shared envelope parse, not in the
	// case bodies; a type-mismatched response field reaches the
	// per-case unmarshal errors.
	if _, _, err := DecodeResponsesStreamEvent("response.incomplete", `{"type":"response.incomplete","response":{"usage":"nope"}}`); err == nil || !strings.Contains(err.Error(), "invalid Responses incomplete event") {
		t.Fatalf("mismatched incomplete event = %v, want an incomplete-event error", err)
	}
	if _, _, err := DecodeResponsesStreamEvent("response.completed", `{"type":"response.completed","response":{"usage":"nope"}}`); err == nil || !strings.Contains(err.Error(), "invalid Responses completed event") {
		t.Fatalf("mismatched completed event = %v, want a completed-event error", err)
	}
	evs, _, err := DecodeResponsesStreamEvent("response.completed",
		`{"response":{"usage":{"input_tokens":5,"output_tokens":7,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":3}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var usage *Usage
	for _, ev := range evs {
		if ev.Type == StreamUsage {
			usage = ev.Usage
		}
	}
	if usage == nil || usage.InputTokens != 5 || usage.OutputTokens != 7 || usage.CacheReadTokens != 2 || usage.ReasoningTokens != 3 {
		t.Fatalf("completed usage = %+v, want 5/7/2/3", usage)
	}
}
