package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

func geminiStub(t *testing.T, handler http.HandlerFunc) (*httptest.Server, providers.Adapter) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	a, err := providers.NewAdapter(config.ProviderConfig{
		ID:      "gemini-test",
		Type:    "gemini",
		BaseURL: up.URL,
		APIKey:  "secret",
	}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return up, a
}

func TestGeminiSendForPaths(t *testing.T) {
	var gotPath, gotQuery, gotKey string
	_, a := geminiStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()

	resp, err := geminiSendFor(a, "gemini-2.0-flash", false, nil)(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotPath != "/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("wrong non-stream path: %s", gotPath)
	}
	if gotKey != "secret" {
		t.Fatalf("missing x-goog-api-key auth, got %q", gotKey)
	}

	resp, err = geminiSendFor(a, "gemini-2.0-flash", true, nil)(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotPath != "/v1beta/models/gemini-2.0-flash:streamGenerateContent" || gotQuery != "alt=sse" {
		t.Fatalf("wrong stream path: %s?%s", gotPath, gotQuery)
	}
}

const geminiSampleResponse = `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2}}`

func TestGeminiResponseForClient(t *testing.T) {
	newResp := func() *http.Response {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(geminiSampleResponse))}
	}

	v, in, out, err := geminiResponseForClient(newResp(), "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	if in != 7 || out != 2 {
		t.Fatalf("wrong usage: %d/%d", in, out)
	}
	o, ok := v.(core.OpenAIResponse)
	if !ok || len(o.Choices) != 1 || fmt.Sprintf("%v", o.Choices[0].Message.Content) != "hi" {
		t.Fatalf("wrong OpenAI translation: %#v", v)
	}

	v, in, out, err = geminiResponseForClient(newResp(), "anthropic", "m")
	if err != nil {
		t.Fatal(err)
	}
	if in != 7 || out != 2 {
		t.Fatalf("wrong usage: %d/%d", in, out)
	}
	an, ok := v.(core.AnthResponse)
	if !ok || an.Model != "m" || an.Usage.InputTokens != 7 || len(an.Content) != 1 {
		t.Fatalf("wrong Anthropic translation: %#v", v)
	}
	raw, _ := json.Marshal(an.Content[0])
	if !strings.Contains(string(raw), "hi") {
		t.Fatalf("wrong Anthropic content: %s", raw)
	}
}

func TestGeminiResponseForClientInvalid(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"bogus":true}`))}
	if _, _, _, err := geminiResponseForClient(resp, "openai", "m"); err == nil {
		t.Fatalf("expected validation error for content-less Gemini body")
	}
}
