package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type wave4SSEWriter struct {
	header  http.Header
	body    strings.Builder
	status  int
	writes  int
	failOn  int // 1-based write index that fails; 0 means never
	failErr error
	flushed int
}

func wave4NewSSEWriter(failOn int) *wave4SSEWriter {
	return &wave4SSEWriter{header: http.Header{}, failOn: failOn, failErr: errors.New("wave4 injected write failure")}
}

func (w *wave4SSEWriter) Header() http.Header { return w.header }
func (w *wave4SSEWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *wave4SSEWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.failOn > 0 && w.writes == w.failOn {
		return 0, w.failErr
	}
	return w.body.Write(b)
}
func (w *wave4SSEWriter) Flush() { w.flushed++ }

// wave4SSEFrames splits the emitted event stream into parsed data payloads.
func wave4SSEFrames(t *testing.T, w *wave4SSEWriter) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range strings.Split(w.body.String(), "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		lines := strings.Split(frame, "\n")
		var data string
		for _, ln := range lines {
			if strings.HasPrefix(ln, "data: ") {
				data = strings.TrimPrefix(ln, "data: ")
			}
		}
		if data == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatalf("unparsable emitted frame %q: %v", data, err)
		}
		out = append(out, v)
	}
	return out
}

func wave4SSEEventTypes(frames []map[string]any) []string {
	var types []string
	for _, f := range frames {
		if s, ok := f["type"].(string); ok {
			types = append(types, s)
		}
	}
	return types
}

func wave4TextDeltas(frames []map[string]any) []string {
	var texts []string
	for _, f := range frames {
		if f["type"] != "content_block_delta" {
			continue
		}
		delta, _ := f["delta"].(map[string]any)
		if delta["type"] == "text_delta" {
			if txt, ok := delta["text"].(string); ok {
				texts = append(texts, txt)
			}
		}
	}
	return texts
}

func wave4RunAnthropicStream(t *testing.T, sse string, failOn int) (*wave4SSEWriter, error, int, int) {
	t.Helper()
	w := wave4NewSSEWriter(failOn)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	var gotPrompt, gotCompletion int
	err := streamOpenAIToAnthropicWithUsage(w, resp, "model-x", nil, func(prompt, completion int) {
		gotPrompt, gotCompletion = prompt, completion
	})
	return w, err, gotPrompt, gotCompletion
}

func wave4SSEData(jsonBody string) string { return "data: " + jsonBody + "\n\n" }

func wave4TextChunk(text string) string {
	return wave4SSEData(`{"choices":[{"delta":{"content":` + strconvQuoteWave4(text) + `}}]}`)
}

func strconvQuoteWave4(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestWave4AnthropicStreamEmptyDataSkipped(t *testing.T) {
	sse := wave4TextChunk("hi") + "data:\n\n" + wave4TextChunk("!") + "data: [DONE]\n\n"
	w, err, _, _ := wave4RunAnthropicStream(t, sse, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	texts := wave4TextDeltas(wave4SSEFrames(t, w))
	if len(texts) != 2 || texts[0] != "hi" || texts[1] != "!" {
		t.Fatalf("text deltas = %v, want [hi !]", texts)
	}
}

func TestWave4AnthropicStreamUpstreamError(t *testing.T) {
	sse := wave4SSEData(`{"error":{"type":"api_error","message":"boom"}}`)
	w, err, _, _ := wave4RunAnthropicStream(t, sse, 0)
	if err == nil || err.Error() != "openai stream error" {
		t.Fatalf("err = %v, want openai stream error", err)
	}
	frames := wave4SSEFrames(t, w)
	last := frames[len(frames)-1]
	if last["type"] != "error" {
		t.Fatalf("last frame = %v, want an error frame", last)
	}
	inner, _ := last["error"].(map[string]any)
	if inner["message"] != "boom" {
		t.Fatalf("error frame = %v, want message boom", last)
	}
}

func TestWave4AnthropicStreamInvalidChunkShape(t *testing.T) {
	sse := wave4SSEData(`{"choices":"not-an-array"}`)
	w, err, _, _ := wave4RunAnthropicStream(t, sse, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid OpenAI SSE chunk") {
		t.Fatalf("err = %v, want invalid OpenAI SSE chunk", err)
	}
	frames := wave4SSEFrames(t, w)
	last := frames[len(frames)-1]
	if last["type"] != "error" {
		t.Fatalf("last frame = %v, want an error frame", last)
	}
}

func TestWave4AnthropicStreamNonMapPartSkipped(t *testing.T) {
	sse := wave4SSEData(`{"choices":[{"delta":{"content":["plain-string",{"type":"text","text":"real"}]}}]}`) + "data: [DONE]\n\n"
	w, err, _, _ := wave4RunAnthropicStream(t, sse, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	texts := wave4TextDeltas(wave4SSEFrames(t, w))
	if len(texts) != 1 || texts[0] != "real" {
		t.Fatalf("text deltas = %v, want [real]", texts)
	}
}

func TestWave4AnthropicStreamToolArgsTooLong(t *testing.T) {
	args := strings.Repeat("x", (1<<20)+1)
	chunk := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":` + strconvQuoteWave4(args) + `}}]}}]}`
	w, err, _, _ := wave4RunAnthropicStream(t, wave4SSEData(chunk), 0)
	if err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("err = %v, want the args limit error", err)
	}
	frames := wave4SSEFrames(t, w)
	last := frames[len(frames)-1]
	if last["type"] != "error" {
		t.Fatalf("last frame = %v, want an error frame", last)
	}
}

// A tool call that never names its tool is started at stream close
// with the fallback name "tool" instead of being dropped silently.
func TestWave4AnthropicStreamNamelessToolDefaults(t *testing.T) {
	sse := wave4SSEData(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{}}]}}]}`) + "data: [DONE]\n\n"
	w, err, _, _ := wave4RunAnthropicStream(t, sse, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	found := false
	for _, f := range wave4SSEFrames(t, w) {
		if f["type"] != "content_block_start" {
			continue
		}
		block, _ := f["content_block"].(map[string]any)
		if block["type"] == "tool_use" && block["name"] == "tool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("frames = %v, want a tool_use block named tool", w.body.String())
	}
}

func TestWave4AnthropicStreamFinishReasons(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"length", "max_tokens"},
		{"content_filter", "refusal"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			sse := wave4SSEData(`{"choices":[{"finish_reason":"`+tc.reason+`"}]}`) + "data: [DONE]\n\n"
			w, err, _, _ := wave4RunAnthropicStream(t, sse, 0)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			var stop string
			for _, f := range wave4SSEFrames(t, w) {
				if f["type"] == "message_delta" {
					delta, _ := f["delta"].(map[string]any)
					stop, _ = delta["stop_reason"].(string)
				}
			}
			if stop != tc.want {
				t.Fatalf("stop_reason = %q, want %q", stop, tc.want)
			}
		})
	}
}

func TestWave4AnthropicStreamUsageAndCacheRead(t *testing.T) {
	sse := wave4TextChunk("hi") +
		wave4SSEData(`{"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}`) +
		"data: [DONE]\n\n"
	w, err, gotPrompt, gotCompletion := wave4RunAnthropicStream(t, sse, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if gotPrompt != 10 || gotCompletion != 3 {
		t.Fatalf("usage sink = (%d, %d), want (10, 3)", gotPrompt, gotCompletion)
	}
	var usage map[string]any
	for _, f := range wave4SSEFrames(t, w) {
		if f["type"] == "message_delta" {
			usage, _ = f["usage"].(map[string]any)
		}
	}
	if usage["input_tokens"] != float64(10) || usage["cache_read_input_tokens"] != float64(4) || usage["output_tokens"] != float64(3) {
		t.Fatalf("final usage = %v, want input 10 / cache_read 4 / output 3", usage)
	}
}

func TestWave4AnthropicStreamWriterFailures(t *testing.T) {
	// Failing the ping write aborts before any chunk is processed.
	{
		w, err, _, _ := wave4RunAnthropicStream(t, wave4TextChunk("hi")+"data: [DONE]\n\n", 2)
		if err == nil {
			t.Fatal("ping write failure must return an error")
		}
		types := wave4SSEEventTypes(wave4SSEFrames(t, w))
		for _, typ := range types {
			if typ == "ping" {
				t.Fatalf("types = %v, ping must not be emitted after its write failed", types)
			}
		}
	}
	// Failing the text block start leaves the following delta emit a
	// no-op (writeErr already set) and the function returns that error
	// at the end without a message_stop.
	{
		w, err, _, _ := wave4RunAnthropicStream(t, wave4TextChunk("hi")+"data: [DONE]\n\n", 3)
		if err == nil {
			t.Fatal("block-start write failure must return an error")
		}
		types := wave4SSEEventTypes(wave4SSEFrames(t, w))
		for _, typ := range types {
			if typ == "content_block_delta" || typ == "message_stop" {
				t.Fatalf("types = %v, no delta or stop may follow a failed block start", types)
			}
		}
	}
	// Failing the upstream-error frame write returns the write error
	// itself, not the translated stream error.
	{
		sse := wave4SSEData(`{"error":{"type":"api_error","message":"boom"}}`)
		w, err, _, _ := wave4RunAnthropicStream(t, sse, 3)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
	}
	// Failing the tool block start is caught by the post-tool
	// writeErr check, before any finish handling.
	{
		chunk := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f"}}]}}]}`
		w, err, _, _ := wave4RunAnthropicStream(t, wave4SSEData(chunk)+"data: [DONE]\n\n", 3)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
	}
	// Failing the final message_delta write aborts before message_stop
	// and returns the write error.
	{
		sse := wave4TextChunk("hi") + "data: [DONE]\n\n"
		w, err, _, _ := wave4RunAnthropicStream(t, sse, 6)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
		types := wave4SSEEventTypes(wave4SSEFrames(t, w))
		for _, typ := range types {
			if typ == "message_stop" {
				t.Fatalf("types = %v, message_stop must not follow a failed delta", types)
			}
		}
	}
}
