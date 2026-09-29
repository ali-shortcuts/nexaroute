package core

import (
	"encoding/json"
	"testing"
)

// Audit item 7: core was at 30.4% — ParseSystem had zero coverage.
func TestParseSystemBranches(t *testing.T) {
	if got := ParseSystem(nil); got != "" {
		t.Fatalf("nil = %q, want empty", got)
	}
	if got := ParseSystem(json.RawMessage(`"hello"`)); got != "hello" {
		t.Fatalf("string = %q, want hello", got)
	}
	if got := ParseSystem(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)); got != "a\nb" {
		t.Fatalf("blocks = %q, want a\\nb", got)
	}
	if got := ParseSystem(json.RawMessage(`[{"type":"image","text":"x"}]`)); got != "" {
		t.Fatalf("non-text blocks = %q, want empty", got)
	}
	if got := ParseSystem(json.RawMessage(`{invalid`)); got != "" {
		t.Fatalf("invalid = %q, want empty", got)
	}
}

func TestParseAnthContentBranches(t *testing.T) {
	blocks, err := ParseAnthContent(nil)
	if err != nil || blocks != nil {
		t.Fatalf("nil = %v, %v; want nil, nil", blocks, err)
	}
	blocks, err = ParseAnthContent(json.RawMessage(`"hi"`))
	if err != nil || len(blocks) != 1 || blocks[0].Text != "hi" || blocks[0].Type != "text" {
		t.Fatalf("string = %+v, %v", blocks, err)
	}
	blocks, err = ParseAnthContent(json.RawMessage(`[{"type":"text","text":"x"}]`))
	if err != nil || len(blocks) != 1 || blocks[0].Text != "x" {
		t.Fatalf("blocks = %+v, %v", blocks, err)
	}
	if _, err = ParseAnthContent(json.RawMessage(`{invalid`)); err == nil {
		t.Fatal("invalid JSON should error")
	}
}

func TestWireTypesRoundTrip(t *testing.T) {
	// Wire types carry no statements but must stay JSON-stable.
	req := OpenAIRequest{Model: "m", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}}}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var back OpenAIRequest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Model != "m" || len(back.Messages) != 1 {
		t.Fatalf("round trip = %+v", back)
	}
	aresp := AnthResponse{ID: "r1", Type: "message", Role: "assistant", Model: "m",
		Content: []AnthContentBlock{{Type: "text", Text: "ok"}}, Usage: AnthUsage{InputTokens: 3, OutputTokens: 5}}
	if b, err = json.Marshal(aresp); err != nil {
		t.Fatal(err)
	}
	var aback AnthResponse
	if err := json.Unmarshal(b, &aback); err != nil {
		t.Fatal(err)
	}
	if aback.Usage.OutputTokens != 5 || aback.Content[0].Text != "ok" {
		t.Fatalf("round trip = %+v", aback)
	}
}
