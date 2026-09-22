package translate

import (
	"encoding/json"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/core"
	"testing"
)

func TestAnthropicToOpenAITool(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{{"type": "text", "text": "hi"}})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 10, Messages: []core.AnthMessage{{Role: "user", Content: content}}, Tools: []core.AnthTool{{Name: "shell", InputSchema: map[string]any{"type": "object"}}}}
	o, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if o.Model != "backend" || len(o.Tools) != 1 {
		t.Fatalf("bad output %#v", o)
	}
}

func TestAnthropicImageToOpenAIDataURL(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}}})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 8, Messages: []core.AnthMessage{{Role: "user", Content: content}}}
	o, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := o.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("bad parts %#v", o.Messages[0].Content)
	}
	iu := parts[0]["image_url"].(map[string]any)["url"]
	if iu != "data:image/png;base64,AAAA" {
		t.Fatalf("bad data url %v", iu)
	}
}

func TestOpenAIImageURLToAnthropic(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{{Role: "user", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}}}}}}
	a, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(a.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	src := blocks[0]["source"].(map[string]any)
	if src["type"] != "url" {
		t.Fatalf("bad source %#v", src)
	}
}
