package canonical

import (
	"encoding/json"
	"errors"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Continue95GeminiIngress(body string) GeminiRequest {
	var in GeminiRequest
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		panic(err)
	}
	return in
}

func TestContinue95GeminiIngressRejectEdges(t *testing.T) {
	base := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	// Empty model.
	if _, err := DecodeGeminiIngressRequest(Continue95GeminiIngress(base), "  "); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("empty model err=%v", err)
	}
	// System instruction with a non-text part.
	in := Continue95GeminiIngress(base)
	in.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{InlineData: &GeminiBlob{MimeType: "image/png", Data: "AAAA"}}}}
	if _, err := DecodeGeminiIngressRequest(in, "m"); err == nil || !strings.Contains(err.Error(), "only text parts are supported") {
		t.Fatalf("system non-text err=%v", err)
	}
	// Content with no parts.
	if _, err := DecodeGeminiIngressRequest(Continue95GeminiIngress(`{"contents":[{"role":"user","parts":[]}]}`), "m"); err == nil || !strings.Contains(err.Error(), "at least one part is required") {
		t.Fatalf("empty parts err=%v", err)
	}
	// Function declaration without a name, and with a non-object parameters value.
	if _, err := DecodeGeminiIngressRequest(Continue95GeminiIngress(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"description":"d"}]}]}`), "m"); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("nameless decl err=%v", err)
	}
	if _, err := DecodeGeminiIngressRequest(Continue95GeminiIngress(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":"just-a-string"}]}]}`), "m"); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("non-object params err=%v", err)
	}
	// Parameters that cannot be marshaled (constructed directly, not via JSON).
	in = Continue95GeminiIngress(base)
	in.Tools = []GeminiTool{{FunctionDeclarations: []map[string]any{{"name": "f", "parameters": func() {}}}}}
	if _, err := DecodeGeminiIngressRequest(in, "m"); err == nil || !strings.Contains(err.Error(), "not serializable") {
		t.Fatalf("unserializable params err=%v", err)
	}
	// A declaration with parameters and description maps through.
	in = Continue95GeminiIngress(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","description":"does things","parameters":{"type":"object"}}]}]}`)
	req, err := DecodeGeminiIngressRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Description != "does things" || string(req.Tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("decl mapped=%+v", req.Tools)
	}
}

func TestContinue95GeminiIngressToolChoiceAndMime(t *testing.T) {
	base := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	modes := []struct {
		mode string
		want string
		name string
	}{
		{"", "auto", ""},
		{"AUTO", "auto", ""},
		{"MODE_AUTO", "auto", ""},
		{"MODE_NONE", "none", ""},
		{"NONE", "none", ""},
		{"ANY", "required", ""},
		{"ANY", "tool", "lookup"},
		{"MODE_ANY", "tool", "lookup"},
	}
	for _, tc := range modes {
		in := Continue95GeminiIngress(base)
		names := []string{}
		if tc.name != "" {
			names = []string{tc.name}
		} else if tc.mode == "ANY" || tc.mode == "MODE_ANY" {
			// Multi-name ANY (or zero names) falls back to required; the
			// single-name case above is covered by the name row.
			names = []string{"a", "b"}
		}
		in.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: &struct {
			Mode                 string   `json:"mode,omitempty"`
			AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
		}{Mode: tc.mode, AllowedFunctionNames: names}}
		req, err := DecodeGeminiIngressRequest(in, "m")
		if err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		if req.ToolChoice == nil || req.ToolChoice.Mode != tc.want || req.ToolChoice.Name != tc.name {
			t.Fatalf("mode %q: choice=%+v want %s/%s", tc.mode, req.ToolChoice, tc.want, tc.name)
		}
	}
	// text/plain with a schema is rejected; application/json with a schema
	// yields a JSON-schema format; an unsupported mime is rejected.
	in := Continue95GeminiIngress(base)
	in.GenerationConfig = &GeminiGenerationConfig{ResponseMimeType: "text/plain", ResponseSchema: json.RawMessage(`{"type":"object"}`)}
	if _, err := DecodeGeminiIngressRequest(in, "m"); err == nil || !strings.Contains(err.Error(), "responseSchema requires") {
		t.Fatalf("text/plain+schema err=%v", err)
	}
	in.GenerationConfig = &GeminiGenerationConfig{ResponseMimeType: "application/json", ResponseSchema: json.RawMessage(`{"type":"object"}`)}
	req, err := DecodeGeminiIngressRequest(in, "m")
	if err != nil || req.ResponseFormat == nil || req.ResponseFormat.Kind != FormatJSONSchema {
		t.Fatalf("json+schema req=%+v err=%v", req.ResponseFormat, err)
	}
	in.GenerationConfig = &GeminiGenerationConfig{ResponseMimeType: "application/xml"}
	if _, err := DecodeGeminiIngressRequest(in, "m"); err == nil || !strings.Contains(err.Error(), "unsupported responseMimeType") {
		t.Fatalf("xml mime err=%v", err)
	}
	// First content may omit its role (defaults to user); generation
	// config scalars map through.
	in = Continue95GeminiIngress(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.25,"topP":0.9,"topK":40,"maxOutputTokens":33,"stopSequences":["STOP"]}}`)
	req, err = DecodeGeminiIngressRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != RoleUser {
		t.Fatalf("roleless first messages=%+v", req.Messages)
	}
	if req.Temperature == nil || *req.Temperature != 0.25 || req.TopP == nil || *req.TopP != 0.9 || req.TopK == nil || *req.TopK != 40 || req.MaxOutputTokens != 33 || len(req.Stop) != 1 {
		t.Fatalf("generation scalars=%+v", req)
	}
}

func TestContinue95GeminiIngressParts(t *testing.T) {
	decodePart := func(body string) (Request, error) {
		return DecodeGeminiIngressRequest(Continue95GeminiIngress(body), "m")
	}
	// Function call without a name.
	if _, err := decodePart(`{"contents":[{"role":"model","parts":[{"functionCall":{"args":{}}}]}]}`); err == nil || !strings.Contains(err.Error(), "functionCall without a name") {
		t.Fatalf("nameless call err=%v", err)
	}
	// Function call with empty args defaults to {}.
	req, err := decodePart(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f"}}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Parts[0].ToolCall.Arguments != "{}" {
		t.Fatalf("empty args=%q", req.Messages[0].Parts[0].ToolCall.Arguments)
	}
	// V1 spellings of functionCall / functionResponse / inlineData.
	req, err = decodePart(`{"contents":[{"role":"model","parts":[{"function_call":{"name":"f","args":{"q":1}}}]}]}`)
	if err != nil || req.Messages[0].Parts[0].ToolCall.Name != "f" {
		t.Fatalf("v1 call req=%+v err=%v", req.Messages, err)
	}
	req, err = decodePart(`{"contents":[{"role":"function","parts":[{"function_response":{"name":"f"}}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Parts[0].ToolResult.ToolUseID != "f" || req.Messages[0].Parts[0].ToolResult.Content != "" {
		t.Fatalf("v1 response part=%+v", req.Messages[0].Parts[0])
	}
	// Function response without a name.
	if _, err := decodePart(`{"contents":[{"role":"function","parts":[{"functionResponse":{"response":{}}}]}]}`); err == nil || !strings.Contains(err.Error(), "functionResponse without a name") {
		t.Fatalf("nameless response err=%v", err)
	}
	// Inline data without data, with a V1 mime type, and with no mime at all.
	if _, err := decodePart(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png"}}]}]}`); err == nil || !strings.Contains(err.Error(), "inlineData without data") {
		t.Fatalf("empty blob err=%v", err)
	}
	req, err = decodePart(`{"contents":[{"role":"user","parts":[{"inline_data":{"mime_type":"image/jpeg","data":"AAAA"}}]}]}`)
	if err != nil || req.Messages[0].Parts[0].Image.MediaType != "image/jpeg" {
		t.Fatalf("v1 mime req=%+v err=%v", req.Messages, err)
	}
	req, err = decodePart(`{"contents":[{"role":"user","parts":[{"inlineData":{"data":"AAAA"}}]}]}`)
	if err != nil || req.Messages[0].Parts[0].Image.MediaType != "image/png" {
		t.Fatalf("default mime req=%+v err=%v", req.Messages, err)
	}
	// Thought part.
	thought := true
	in := Continue95GeminiIngress(`{"contents":[{"role":"user","parts":[{"text":"thinking aloud"}]}]}`)
	in.Contents[0].Parts[0].Thought = &thought
	req, err = DecodeGeminiIngressRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Parts[0].Type != PartThinking || req.Messages[0].Parts[0].Thinking.Text != "thinking aloud" {
		t.Fatalf("thought part=%+v", req.Messages[0].Parts[0])
	}
	// Thought=false stays text.
	thought = false
	in.Contents[0].Parts[0].Thought = &thought
	req, err = DecodeGeminiIngressRequest(in, "m")
	if err != nil || req.Messages[0].Parts[0].Type != PartText {
		t.Fatalf("thought=false req=%+v err=%v", req.Messages, err)
	}
}

func TestContinue95EncodeGeminiResponseEdges(t *testing.T) {
	// Finish mapping: max tokens and refusal.
	out, err := EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartText, Text: "x"}}, StopReason: StopMaxTokens})
	if err != nil || out.Candidates[0].FinishReason != "MAX_TOKENS" {
		t.Fatalf("max tokens out=%+v err=%v", out, err)
	}
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartText, Text: "x"}}, StopReason: StopRefusal, Usage: Usage{InputTokens: 3, OutputTokens: 1, CacheReadTokens: 2, ReasoningTokens: 4}})
	if err != nil || out.Candidates[0].FinishReason != "SAFETY" || out.PromptFeedback == nil || out.PromptFeedback.BlockReason != "SAFETY" {
		t.Fatalf("refusal out=%+v err=%v", out, err)
	}
	if out.UsageMetadata == nil || out.UsageMetadata.CachedContentTokenCount != 2 || out.UsageMetadata.ThoughtsTokenCount != 4 {
		t.Fatalf("refusal usage=%+v", out.UsageMetadata)
	}
	// Empty text and empty thinking blocks fall back to a single empty part.
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartText, Text: ""}, {Type: PartThinking, Thinking: &Thinking{Text: ""}}, {Type: PartThinking}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Candidates) != 1 || len(out.Candidates[0].Content.Parts) != 1 || out.Candidates[0].Content.Parts[0].Text != "" {
		t.Fatalf("fallback parts=%+v", out.Candidates[0].Content.Parts)
	}
	// A real thinking block carries thought=true; a nil tool call and a
	// nameless tool call fail closed; empty args default to {}.
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartThinking, Thinking: &Thinking{Text: "hmm"}}}})
	if err != nil || out.Candidates[0].Content.Parts[0].Thought == nil || !*out.Candidates[0].Content.Parts[0].Thought {
		t.Fatalf("thinking encode=%+v err=%v", out, err)
	}
	if _, err := EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartToolCall}}}); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("nil toolcall err=%v", err)
	}
	if _, err := EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "  "}}}}); err == nil {
		t.Fatalf("nameless toolcall should error")
	}
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "f", Arguments: "  "}}}})
	if err != nil || string(out.Candidates[0].Content.Parts[0].FunctionCall.Args) != "{}" {
		t.Fatalf("empty args encode=%+v err=%v", out, err)
	}
	// Unknown block types are dropped, not emitted.
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: "bogus-block", Text: "dropped"}, {Type: PartText, Text: "kept"}}})
	if err != nil || len(out.Candidates[0].Content.Parts) != 1 || out.Candidates[0].Content.Parts[0].Text != "kept" {
		t.Fatalf("unknown block drop=%+v err=%v", out, err)
	}
	// No usage, no usage metadata.
	out, err = EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartText, Text: "x"}}})
	if err != nil || out.UsageMetadata != nil {
		t.Fatalf("zero usage metadata=%+v err=%v", out.UsageMetadata, err)
	}
}

type Continue95FailWriter struct{ header http.Header }

func (w *Continue95FailWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *Continue95FailWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic writer failure")
}
func (w *Continue95FailWriter) WriteHeader(int) {}

func TestContinue95GeminiEmitterEdges(t *testing.T) {
	rr := httptest.NewRecorder()
	em := NewGeminiEmitter(rr, "m")
	// Start with usage, empty text (no chunk), thinking, unknown event.
	if err := em.Emit(StreamEvent{Type: StreamStart, Usage: &Usage{InputTokens: 4, OutputTokens: 0}}); err != nil {
		t.Fatal(err)
	}
	before := rr.Body.Len()
	if err := em.Emit(StreamEvent{Type: StreamText, Text: ""}); err != nil {
		t.Fatal(err)
	}
	if rr.Body.Len() != before {
		t.Fatal("empty text must not emit a chunk")
	}
	if err := em.Emit(StreamEvent{Type: StreamThinking, Text: "hmm"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: "bogus-event"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamError, ErrorMsg: "boom"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), `"thought":true`) {
		t.Fatalf("thinking chunk missing: %s", rr.Body.String())
	}
	// Tool limits.
	if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: -1, ToolName: "f"}); err == nil || !strings.Contains(err.Error(), "safe limit") {
		t.Fatalf("negative tool index err=%v", err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: strings.Repeat("n", maxStreamToolName+1)}); err == nil {
		t.Fatalf("oversize tool name should error")
	}
	// Delta/end before start.
	if err := em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 7, ArgsDelta: "{}"}); err == nil || !strings.Contains(err.Error(), "before tool start") {
		t.Fatalf("delta before start err=%v", err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 7}); err == nil || !strings.Contains(err.Error(), "before tool start") {
		t.Fatalf("end before start err=%v", err)
	}
	// ArgsFinal with existing deltas is ignored (no doubling).
	if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 3, ToolName: "lookup"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 3, ArgsDelta: `{"q":"x"}`}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 3, ArgsDelta: `{"q":"y"}`, ArgsFinal: true}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 3}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), `"q":"x"`) || strings.Contains(rr.Body.String(), `"q":"y"`) {
		t.Fatalf("ArgsFinal doubled or replaced deltas: %s", rr.Body.String())
	}
	// Tool end with no args defaults to {}; bad JSON fails.
	if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 4, ToolName: "f"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 4}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 5, ToolName: "f"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 5, ArgsDelta: `{bad`}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 5}); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("bad tool args err=%v", err)
	}
	// Too many distinct tools.
	em2 := NewGeminiEmitter(httptest.NewRecorder(), "m")
	for i := 0; i < maxStreamTools; i++ {
		if err := em2.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: i, ToolName: "f"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := em2.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: maxStreamTools, ToolName: "f"}); err == nil || !strings.Contains(err.Error(), "safe tool count") {
		t.Fatalf("tool count err=%v", err)
	}
	// Finish without a stop reason defaults to end_turn and is idempotent.
	em3 := NewGeminiEmitter(httptest.NewRecorder(), "m")
	if err := em3.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := em3.Finish(); err != nil {
		t.Fatal(err)
	}
	// Finish with zero-token usage omits usageMetadata.
	rr4 := httptest.NewRecorder()
	em4 := NewGeminiEmitter(rr4, "m")
	if err := em4.Emit(StreamEvent{Type: StreamUsage, Usage: &Usage{}}); err != nil {
		t.Fatal(err)
	}
	if err := em4.Finish(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rr4.Body.String(), "usageMetadata") {
		t.Fatalf("zero usage must not emit usageMetadata: %s", rr4.Body.String())
	}
}

func TestContinue95StreamEmitterWriteErrors(t *testing.T) {
	// Anthropic: failing writer, marshal failure, delta-before-start error,
	// thinking block closed on Finish.
	ae := NewAnthropicEmitter(&Continue95FailWriter{}, "m", "req-1").(*anthropicEmitter)
	if err := ae.Emit(StreamEvent{Type: StreamText, Text: "hi"}); err == nil {
		t.Fatal("anthropic failing writer should error")
	}
	if err := ae.Emit(StreamEvent{Type: StreamText, Text: "again"}); err == nil {
		t.Fatal("anthropic second emit after write error should return writeErr")
	}
	ae2 := NewAnthropicEmitter(httptest.NewRecorder(), "m", "req-1").(*anthropicEmitter)
	ae2.frame("x", map[string]any{"bad": func() {}})
	if ae2.writeErr == nil {
		t.Fatal("anthropic frame marshal failure should set writeErr")
	}
	// A tool delta without a start recurses into a start; when that start
	// itself fails (failing writer), the delta returns the start error.
	ae3 := NewAnthropicEmitter(&Continue95FailWriter{}, "m", "req-1")
	if err := ae3.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ToolName: "lookup", ArgsDelta: "{}"}); err == nil {
		t.Fatal("anthropic delta whose implicit start fails should error")
	}
	ae4 := NewAnthropicEmitter(httptest.NewRecorder(), "m", "req-1")
	if err := ae4.Emit(StreamEvent{Type: StreamThinking, Text: "thinking"}); err != nil {
		t.Fatal(err)
	}
	if err := ae4.Finish(); err != nil {
		t.Fatal(err)
	}
	// OpenAI: failing writer, chunk marshal failure.
	oe := NewOpenAIEmitter(&Continue95FailWriter{}, "m").(*openAIEmitter)
	if err := oe.Emit(StreamEvent{Type: StreamText, Text: "hi"}); err == nil {
		t.Fatal("openai failing writer should error")
	}
	if err := oe.Emit(StreamEvent{Type: StreamText, Text: "again"}); err == nil {
		t.Fatal("openai second emit should return writeErr")
	}
	oe2 := NewOpenAIEmitter(httptest.NewRecorder(), "m").(*openAIEmitter)
	oe2.chunk(map[string]any{"bad": func() {}})
	if oe2.writeErr == nil {
		t.Fatal("openai chunk marshal failure should set writeErr")
	}
	// Responses: failing writer (constructor events fail), marshal failure,
	// usage merge, delta limit, double Finish.
	re := NewResponsesEmitter(&Continue95FailWriter{}, "m").(*responsesEmitter)
	if err := re.Emit(StreamEvent{Type: StreamText, Text: "hi"}); err == nil {
		t.Fatal("responses emit after constructor write error should return writeErr")
	}
	if err := re.Finish(); err == nil {
		// Finish returns writeErr, which is non-nil here.
		t.Fatal("responses finish after write error should return writeErr")
	}
	re2 := NewResponsesEmitter(httptest.NewRecorder(), "m").(*responsesEmitter)
	re2.event("x", map[string]any{"bad": func() {}})
	if re2.writeErr == nil {
		t.Fatal("responses event marshal failure should set writeErr")
	}
	re3 := NewResponsesEmitter(httptest.NewRecorder(), "m")
	if err := re3.Emit(StreamEvent{Type: StreamUsage}); err != nil {
		t.Fatal(err)
	}
	if err := re3.Emit(StreamEvent{Type: StreamUsage, Usage: &Usage{ReasoningTokens: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := re3.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: -1, ArgsDelta: "{}"}); err == nil || !strings.Contains(err.Error(), "safe limit") {
		t.Fatalf("responses delta limit err=%v", err)
	}
	if err := re3.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := re3.Finish(); err != nil {
		t.Fatal(err)
	}
	// NewStreamEmitter dispatches gemini.
	if got := NewStreamEmitter("gemini", httptest.NewRecorder(), "m", "r"); got == nil {
		t.Fatal("gemini emitter nil")
	}
}

func TestContinue95SSEReaderAndGeminiChunk(t *testing.T) {
	// Trailing block without a final blank line is still delivered.
	sr := NewSSEReader(strings.NewReader("data: hello\n"))
	name, data, done, err := sr.Next()
	if err != nil || data != "hello" || done {
		t.Fatalf("trailing block name=%q data=%q done=%v err=%v", name, data, done, err)
	}
	if _, _, done, err := sr.Next(); err != nil || !done {
		t.Fatalf("second next done=%v err=%v", done, err)
	}
	// Invalid Gemini chunk fails closed.
	if _, _, err := DecodeGeminiStreamChunk("not json"); err == nil {
		t.Fatal("invalid gemini chunk should error")
	}
	// Empty and [DONE]-style inputs are benign for the OpenAI chunk decoder.
	if _, term, err := DecodeOpenAIStreamChunk(""); err != nil || term {
		t.Fatalf("empty chunk term=%v err=%v", term, err)
	}
}

func TestContinue95OpenAIChatCodecEdges(t *testing.T) {
	seed := int64(42)
	fp := 0.5
	in := core.OpenAIRequest{
		Model: "m", Seed: &seed, FrequencyPenalty: &fp,
		Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	req, err := DecodeOpenAIChatRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if req.Metadata["seed"] != int64(42) || req.Metadata["frequency_penalty"] != 0.5 {
		t.Fatalf("metadata=%v", req.Metadata)
	}
	// A bogus part in system content is dropped by design (system is
	// text-only); the same part in an assistant message fails closed.
	// (The system error return itself is unreachable: with system=true
	// the content translator never errors.)
	bad := core.OpenAIRequest{Model: "m", Messages: []core.OpenAIMessage{{Role: "system", Content: []any{map[string]any{"type": "bogus"}}}}}
	req, err = DecodeOpenAIChatRequest(bad, "m")
	if err != nil || len(req.System) != 0 {
		t.Fatalf("system bogus part dropped req=%+v err=%v", req.System, err)
	}
	bad.Messages[0].Role = "assistant"
	if _, err := DecodeOpenAIChatRequest(bad, "m"); err == nil || !strings.Contains(err.Error(), "unsupported OpenAI content part") {
		t.Fatalf("assistant bogus part err=%v", err)
	}
	// Content that cannot be marshaled yields no parts, not an error.
	unmarshalable := core.OpenAIRequest{Model: "m", Messages: []core.OpenAIMessage{{Role: "user", Content: func() {}}}}
	req, err = DecodeOpenAIChatRequest(unmarshalable, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 0 {
		t.Fatalf("unmarshalable content parts=%+v", req.Messages)
	}
	// Assistant with two text parts encodes to a content list; system
	// with two text parts joins with a newline.
	enc, err := EncodeOpenAIChatRequest(Request{Model: "m", Messages: []Message{{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "a"}, {Type: PartText, Text: "b"}}}}}, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc.Messages) != 1 {
		t.Fatalf("assistant messages=%+v", enc.Messages)
	}
	enc2, err := EncodeOpenAIChatRequest(Request{Model: "m", System: []Part{{Type: PartText, Text: "one"}, {Type: PartText, Text: "two"}}, Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}}}, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(enc2)
	if !strings.Contains(string(b), `one\ntwo`) {
		t.Fatalf("system join missing in %s", b)
	}
	// Metadata float from json.Number.
	enc3, err := EncodeOpenAIChatRequest(Request{Model: "m", Metadata: map[string]any{"frequency_penalty": json.Number("0.25")}, Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}}}, "m", false)
	if err != nil || enc3.FrequencyPenalty == nil || *enc3.FrequencyPenalty != 0.25 {
		t.Fatalf("json.Number penalty=%+v err=%v", enc3.FrequencyPenalty, err)
	}
	// Response decode skips non-map content elements.
	resp, err := DecodeOpenAIChatResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":["stray-string",{"type":"text","text":"kept"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "kept" {
		t.Fatalf("response text=%q", resp.Text())
	}
}

func TestContinue95ResponsesSSEAndHelpers(t *testing.T) {
	// Content that is neither a string nor an array yields no parts.
	parts, err := responsesContentToParts(json.RawMessage(`42`))
	if err != nil || len(parts) != 0 {
		t.Fatalf("numeric content parts=%v err=%v", parts, err)
	}
	// Incomplete/completed events with a non-object response fail closed.
	if _, _, err := DecodeResponsesStreamEvent("response.incomplete", `{"type":"response.incomplete","response":"nope"}`); err == nil || !strings.Contains(err.Error(), "incomplete event") {
		t.Fatalf("incomplete err=%v", err)
	}
	if _, _, err := DecodeResponsesStreamEvent("response.completed", `{"type":"response.completed","response":"nope"}`); err == nil || !strings.Contains(err.Error(), "completed event") {
		t.Fatalf("completed err=%v", err)
	}
	// Completed event maps cached + reasoning token details.
	events, term, err := DecodeResponsesStreamEvent("response.completed", `{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":7}}}}`)
	if err != nil || !term {
		t.Fatalf("completed events=%v term=%v err=%v", events, term, err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == StreamUsage && ev.Usage != nil && ev.Usage.CacheReadTokens == 2 && ev.Usage.ReasoningTokens == 7 {
			found = true
		}
	}
	if !found {
		t.Fatalf("completed usage details missing: %+v", events)
	}
	// Anthropic schema/args helpers fail closed on unmarshalable maps.
	if got := marshalSchema(map[string]any{"f": func() {}}); got != nil {
		t.Fatalf("marshalSchema unmarshalable=%q", got)
	}
	if got := anthToolInputArgs(map[string]any{"f": func() {}}); got != "{}" {
		t.Fatalf("anthToolInputArgs unmarshalable=%q", got)
	}
	// Bedrock tool-call arguments that are not JSON break final marshaling.
	if _, _, err := EncodeBedrockRequest(Request{Model: "m", Messages: []Message{{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{Name: "f", Arguments: "{bad"}}}}}}, "m"); err == nil {
		t.Fatalf("bedrock bad args should error")
	}
	// Gemini response with empty function-call args defaults to {}.
	resp, err := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].ToolCall.Arguments != "{}" {
		t.Fatalf("gemini empty args blocks=%+v", resp.Blocks)
	}
	// Tool-call validation: a wrong-typed argument is reported, not coerced.
	def := ToolDef{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}
	if err := ValidateToolCall(def, ToolCall{Name: "lookup", Arguments: `{"q":42}`}); err == nil || !strings.Contains(err.Error(), "expected string") {
		t.Fatalf("validation err=%v", err)
	}
	if err := ValidateToolCall(def, ToolCall{Name: "lookup", Arguments: `{"q":"x"}`}); err != nil {
		t.Fatalf("valid call rejected: %v", err)
	}
}

// Continue95CountingWriter fails after a fixed number of successful
// writes, so a constructor can succeed while a later frame fails.
type Continue95CountingWriter struct {
	header   http.Header
	allow    int
	writes   int
	recorder *httptest.ResponseRecorder
}

func (w *Continue95CountingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *Continue95CountingWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writes > w.allow {
		return 0, errors.New("synthetic writer failure after allowance")
	}
	if w.recorder != nil {
		return w.recorder.Write(b)
	}
	return len(b), nil
}
func (w *Continue95CountingWriter) WriteHeader(int) {}

func TestContinue95CanonicalRemainingEdges(t *testing.T) {
	// Gemini response V1 function-call with empty args defaults to {}.
	resp, err := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"parts":[{"function_call":{"name":"f"}}]},"finishReason":"STOP"}]}`))
	if err != nil || len(resp.Blocks) != 1 || resp.Blocks[0].ToolCall.Arguments != "{}" {
		t.Fatalf("gemini v1 empty args blocks=%+v err=%v", resp.Blocks, err)
	}
	// Gemini emitter: failing writer sets writeErr; a later Emit returns
	// it early and chunk short-circuits; a marshal failure also sticks.
	ge := NewGeminiEmitter(&Continue95FailWriter{}, "m")
	if err := ge.Emit(StreamEvent{Type: StreamText, Text: "hi"}); err == nil {
		t.Fatal("gemini failing writer should error")
	}
	if err := ge.Emit(StreamEvent{Type: StreamText, Text: "again"}); err == nil {
		t.Fatal("gemini second emit should return writeErr")
	}
	ge2 := NewGeminiEmitter(httptest.NewRecorder(), "m")
	ge2.chunk(map[string]any{"bad": func() {}})
	if ge2.writeErr == nil {
		t.Fatal("gemini chunk marshal failure should set writeErr")
	}
	ge2.chunk(map[string]any{"ok": true})
	// Anthropic frame with an empty event name emits a bare data frame.
	rr := httptest.NewRecorder()
	ae := NewAnthropicEmitter(rr, "m", "req-1").(*anthropicEmitter)
	ae.frame("", map[string]any{"type": "bare"})
	body := rr.Body.String()
	if !strings.HasSuffix(body, "data: {\"type\":\"bare\"}\n\n") {
		t.Fatalf("bare frame body=%q", body)
	}
	// Anthropic delta recursion: constructor writes succeed (allowance 2),
	// the implicit start's frame then fails and the delta returns it.
	cw := &Continue95CountingWriter{allow: 2, recorder: httptest.NewRecorder()}
	ae2 := NewAnthropicEmitter(cw, "m", "req-1")
	if err := ae2.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ToolName: "lookup", ArgsDelta: "{}"}); err == nil {
		t.Fatal("delta whose implicit start fails should error")
	}
	// OpenAI chunk short-circuits once writeErr is set.
	oe := NewOpenAIEmitter(httptest.NewRecorder(), "m").(*openAIEmitter)
	oe.chunk(map[string]any{"bad": func() {}})
	if oe.writeErr == nil {
		t.Fatal("openai chunk marshal failure should set writeErr")
	}
	oe.chunk(map[string]any{"choices": []any{}})
	// OpenAI metadata float64 (not json.Number) maps through.
	enc, err := EncodeOpenAIChatRequest(Request{Model: "m", Metadata: map[string]any{"frequency_penalty": 0.75}, Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}}}, "m", false)
	if err != nil || enc.FrequencyPenalty == nil || *enc.FrequencyPenalty != 0.75 {
		t.Fatalf("float64 penalty=%+v err=%v", enc.FrequencyPenalty, err)
	}
	// Metadata json.Number that does not parse is ignored, not fatal.
	enc, err = EncodeOpenAIChatRequest(Request{Model: "m", Metadata: map[string]any{"frequency_penalty": json.Number("abc")}, Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}}}, "m", false)
	if err != nil || enc.FrequencyPenalty != nil {
		t.Fatalf("bad json.Number penalty=%+v err=%v", enc.FrequencyPenalty, err)
	}
	// Responses content that is an empty string yields no parts.
	parts, err := responsesContentToParts(json.RawMessage(`""`))
	if err != nil || len(parts) != 0 {
		t.Fatalf("empty string content parts=%v err=%v", parts, err)
	}
	// Content that cannot be marshaled flattens to "".
	if got := openAIContentToText(func() {}); got != "" {
		t.Fatalf("unmarshalable content text=%q", got)
	}
	// OpenAI decode with only a presence penalty creates metadata lazily.
	pp := 0.25
	req, err := DecodeOpenAIChatRequest(core.OpenAIRequest{Model: "m", PresencePenalty: &pp, Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}}, "m")
	if err != nil || req.Metadata["presence_penalty"] != 0.25 {
		t.Fatalf("presence-only metadata=%v err=%v", req.Metadata, err)
	}
}
