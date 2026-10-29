package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func wave4RunOpenAIStream(t *testing.T, sse string, failOn int) (*wave4SSEWriter, error, int, int) {
	t.Helper()
	w := wave4NewSSEWriter(failOn)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	var gotPrompt, gotCompletion int
	err := streamAnthropicToOpenAIWithUsage(w, resp, "model-x", nil, func(prompt, completion int) {
		gotPrompt, gotCompletion = prompt, completion
	})
	return w, err, gotPrompt, gotCompletion
}

func wave4OpenAIChunks(t *testing.T, w *wave4SSEWriter) []map[string]any {
	t.Helper()
	var out []map[string]any
	body := w.body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]: %q", body)
	}
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" || frame == "data: [DONE]" {
			continue
		}
		data := strings.TrimPrefix(frame, "data: ")
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatalf("unparsable chunk %q: %v", data, err)
		}
		out = append(out, v)
	}
	return out
}

func wave4OpenAIContents(chunks []map[string]any) []string {
	var texts []string
	for _, c := range chunks {
		choices, _ := c["choices"].([]any)
		for _, ch := range choices {
			delta, _ := ch.(map[string]any)["delta"].(map[string]any)
			if txt, ok := delta["content"].(string); ok && txt != "" {
				texts = append(texts, txt)
			}
		}
	}
	return texts
}

func TestWave4OpenAIStreamEmptyDataSkipped(t *testing.T) {
	sse := wave4SSEData(`{"type":"message_start","message":{"usage":{"input_tokens":4}}}`) +
		"data:\n\n" +
		wave4SSEData(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`) +
		wave4SSEData(`{"type":"message_stop"}`)
	w, err, gotPrompt, _ := wave4RunOpenAIStream(t, sse, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	texts := wave4OpenAIContents(wave4OpenAIChunks(t, w))
	if len(texts) != 1 || texts[0] != "hi" {
		t.Fatalf("contents = %v, want [hi]", texts)
	}
	if gotPrompt != 4 {
		t.Fatalf("usage sink prompt = %d, want 4", gotPrompt)
	}
}

// The message_start cache_creation figure arms the final usage
// details gate even when nothing was read from cache.
func TestWave4OpenAIStreamCacheCreationArmsUsageDetails(t *testing.T) {
	sse := wave4SSEData(`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":5}}}`) +
		wave4SSEData(`{"type":"message_stop"}`)
	w, err, _, _ := wave4RunOpenAIStream(t, sse, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var usage map[string]any
	for _, c := range wave4OpenAIChunks(t, w) {
		if u, ok := c["usage"].(map[string]any); ok {
			usage = u
		}
	}
	if usage == nil {
		t.Fatal("no final usage chunk")
	}
	details, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %v, want prompt_tokens_details present via cache_creation", usage)
	}
	if details["cached_tokens"] != float64(0) {
		t.Fatalf("details = %v, want cached_tokens 0 (creation is not read)", details)
	}
}

func TestWave4OpenAIStreamWriterFailures(t *testing.T) {
	// A tool_use block start with a non-empty input emits twice; when
	// the first emit fails, the second is a no-op and the loop-end
	// check returns the write error.
	{
		sse := wave4SSEData(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"f","input":{"a":1}}}`)
		w, err, _, _ := wave4RunOpenAIStream(t, sse, 2)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
		if strings.Contains(w.body.String(), `"arguments"`) {
			t.Fatalf("body = %s, tool args chunk must not follow a failed tool chunk", w.body.String())
		}
	}
	// A failing text-delta emit is caught by the same loop-end check.
	{
		sse := wave4SSEData(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)
		w, err, _, _ := wave4RunOpenAIStream(t, sse, 2)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
	}
	// Failing the error-event emit returns the write error, not the
	// translated stream error.
	{
		sse := wave4SSEData(`{"type":"error","error":{"type":"api_error","message":"boom"}}`)
		w, err, _, _ := wave4RunOpenAIStream(t, sse, 2)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
	}
	// Failing the terminal finish chunk emit returns the write error.
	{
		sse := wave4SSEData(`{"type":"message_stop"}`)
		w, err, _, _ := wave4RunOpenAIStream(t, sse, 2)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
	}
	// Failing the final usage chunk emit returns the write error.
	{
		sse := wave4SSEData(`{"type":"message_start","message":{"usage":{"input_tokens":4}}}`) +
			wave4SSEData(`{"type":"message_stop"}`)
		w, err, _, _ := wave4RunOpenAIStream(t, sse, 3)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
	}
	// Failing the closing [DONE] write returns that write error.
	{
		sse := wave4SSEData(`{"type":"message_stop"}`)
		w, err, _, _ := wave4RunOpenAIStream(t, sse, 3)
		if !errors.Is(err, w.failErr) {
			t.Fatalf("err = %v, want the injected write error", err)
		}
	}
}
