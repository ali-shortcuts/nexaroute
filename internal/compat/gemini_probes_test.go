package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type geminiRecordingTransport struct {
	paths    []string
	payloads [][]byte
	doCalls  int
}

func (t *geminiRecordingTransport) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	t.doCalls++
	return nil, fmt.Errorf("Gemini probe must not use protocol-generic Do")
}

func (t *geminiRecordingTransport) DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	t.paths = append(t.paths, path)
	t.payloads = append(t.payloads, append([]byte(nil), payload...))
	if method != http.MethodPost {
		return nil, fmt.Errorf("method=%s want POST", method)
	}
	if bytes.Contains(payload, []byte(`"messages"`)) {
		return nil, fmt.Errorf("Chat Completions messages leaked into Gemini payload")
	}
	if stream {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
		}, nil
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, err
	}
	if bytes.Contains(payload, []byte(`"functionResponse"`)) {
		return geminiTextResponse("It is 15C and sunny in Paris."), nil
	}

	declCount := 0
	if tools, ok := root["tools"].([]any); ok && len(tools) > 0 {
		if tm, ok := tools[0].(map[string]any); ok {
			if decls, ok := tm["functionDeclarations"].([]any); ok {
				declCount = len(decls)
			}
		}
	}
	if declCount > 0 {
		parts := make([]any, 0, declCount)
		names := []string{"get_weather", "get_time"}
		for i := 0; i < declCount; i++ {
			name := "tool"
			if i < len(names) {
				name = names[i]
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{"name": name, "args": map[string]any{"city": "Paris"}},
			})
		}
		body, _ := json.Marshal(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": parts},
				"finishReason": "STOP",
			}},
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	}

	if gc, ok := root["generationConfig"].(map[string]any); ok && gc["responseMimeType"] == "application/json" {
		return geminiTextResponse(`{"ok":true}`), nil
	}
	return geminiTextResponse("OK"), nil
}

func (t *geminiRecordingTransport) RedactBody(b []byte) []byte { return b }

func geminiTextResponse(text string) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"role":  "model",
				"parts": []any{map[string]any{"text": text}},
			},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 1, "candidatesTokenCount": 1},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func TestGeminiCapabilitySuiteUsesNativeGenerateContentWire(t *testing.T) {
	ft := &geminiRecordingTransport{}
	report := RunCapabilitySuiteGemini(context.Background(), ft, "dep", "models/gemini-test")
	if report.TransportFail != 0 {
		t.Fatalf("transport failures=%d report=%+v", report.TransportFail, report)
	}
	if report.Passed != 12 || report.Inconclusive != 1 {
		t.Fatalf("unexpected Gemini capability counts: passed=%d inconclusive=%d failed=%d", report.Passed, report.Inconclusive, report.Failed)
	}
	if ft.doCalls != 0 {
		t.Fatalf("protocol-generic Do called %d times", ft.doCalls)
	}
	if len(ft.paths) != 12 {
		t.Fatalf("path calls=%d want 12", len(ft.paths))
	}
	for _, path := range ft.paths {
		if !strings.HasPrefix(path, "/v1beta/models/gemini-test:") {
			t.Fatalf("non-Gemini probe path used: %q", path)
		}
		if strings.Contains(path, "chat/completions") {
			t.Fatalf("Chat Completions path leaked into Gemini probe: %q", path)
		}
	}
	foundReasoningUnknown := false
	for _, out := range report.Outcomes {
		if out.Capability == CapReasoningEffort && out.Verdict == UnknownSupport {
			foundReasoningUnknown = true
		}
	}
	if !foundReasoningUnknown {
		t.Fatal("Gemini reasoning must stay inconclusive until thinkingConfig is mapped")
	}
}

func TestGeminiAgentLoopUsesFunctionCallAndFunctionResponse(t *testing.T) {
	ft := &geminiRecordingTransport{}
	report := RunAgentLoopSimulationGemini(context.Background(), ft, "dep", "gemini-test")
	if !report.OK {
		t.Fatalf("Gemini agent loop failed: %+v", report)
	}
	if ft.doCalls != 0 || len(ft.paths) != 2 {
		t.Fatalf("unexpected dispatches: Do=%d paths=%v", ft.doCalls, ft.paths)
	}
	if !bytes.Contains(ft.payloads[0], []byte(`"functionDeclarations"`)) {
		t.Fatalf("first turn missing Gemini function declarations: %s", ft.payloads[0])
	}
	if !bytes.Contains(ft.payloads[1], []byte(`"functionResponse"`)) {
		t.Fatalf("second turn missing Gemini functionResponse: %s", ft.payloads[1])
	}
}
