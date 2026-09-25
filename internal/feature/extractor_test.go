package feature

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractor_BasicOpenAI(t *testing.T) {
	raw := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello world"}]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		Protocol:      ProtocolOpenAI,
		Model:         "gpt-4",
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning_effort", "reasoning"},
		ContentFields: []string{"messages"},
	})
	if feat.HasVision {
		t.Fatalf("unexpected vision")
	}
	if feat.HasReasoning {
		t.Fatalf("unexpected reasoning")
	}
	if feat.MessageCount != 1 {
		t.Fatalf("expected 1 message, got %d", feat.MessageCount)
	}
	if feat.EstimatedPromptTokens == 0 {
		t.Fatalf("expected token estimate")
	}
	if feat.BodySessionKey != "" {
		t.Fatalf("unexpected session key")
	}
	if feat.TooComplex {
		t.Fatalf("unexpected too complex")
	}
}

func TestExtractor_VisionDetection(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		visionType string
		expect     bool
		count      int
	}{
		{
			name:       "openai image_url",
			raw:        `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data"}}]}]}`,
			visionType: "image_url",
			expect:     true,
			count:      1,
		},
		{
			name:       "anthropic image",
			raw:        `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64"}}]}]}`,
			visionType: "image",
			expect:     true,
			count:      1,
		},
		{
			name:       "responses input_image",
			raw:        `{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data"}]}]}`,
			visionType: "input_image",
			expect:     true,
			count:      1,
		},
		{
			name:       "no vision",
			raw:        `{"messages":[{"role":"user","content":"hello"}]}`,
			visionType: "image_url",
			expect:     false,
			count:      0,
		},
	}
	ext := NewExtractor()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feat := ext.Extract([]byte(tc.raw), ExtractOptions{
				VisionType:    tc.visionType,
				ContentFields: []string{"messages", "input"},
			})
			if feat.HasVision != tc.expect {
				t.Fatalf("expected vision=%v got %v", tc.expect, feat.HasVision)
			}
			if feat.VisionImageCount != tc.count {
				t.Fatalf("expected count %d got %d", tc.count, feat.VisionImageCount)
			}
		})
	}
}

func TestExtractor_ReasoningDetection_TopLevelOnly(t *testing.T) {
	// Reasoning key in tool schema should NOT trigger
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo","description":"does reasoning"}}]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning", "reasoning_effort", "thinking"},
		ContentFields: []string{"messages"},
	})
	if feat.HasReasoning {
		t.Fatalf("reasoning should not be detected from tool schema")
	}
	// Top-level reasoning_effort should trigger
	raw2 := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	feat2 := ext.Extract(raw2, ExtractOptions{
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning_effort", "reasoning"},
		ContentFields: []string{"messages"},
	})
	if !feat2.HasReasoning {
		t.Fatalf("expected reasoning from top-level key")
	}
}

func TestExtractor_SessionKey(t *testing.T) {
	raw := []byte(`{"model":"test","session_id":"sess-123","messages":[]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if feat.BodySessionKey != "sess-123" {
		t.Fatalf("expected session key sess-123 got %q", feat.BodySessionKey)
	}
	if !feat.SessionKeyPresent {
		t.Fatalf("expected session present")
	}
	// metadata.session_id
	raw2 := []byte(`{"model":"test","metadata":{"session_id":"meta-456"},"messages":[]}`)
	feat2 := ext.Extract(raw2, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if feat2.BodySessionKey != "meta-456" {
		t.Fatalf("expected meta-456 got %q", feat2.BodySessionKey)
	}
}

func TestExtractor_TooComplex(t *testing.T) {
	// Build large nested structure exceeding node budget
	// Use 100001 nested arrays via repeated wrapping
	// Instead create a map with many keys
	m := make(map[string]any)
	// Create a slice with 100001 elements
	slice := make([]any, 100001)
	for i := range slice {
		slice[i] = "x"
	}
	m["messages"] = slice
	raw, _ := json.Marshal(m)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if !feat.TooComplex {
		t.Fatalf("expected TooComplex")
	}
}

func TestExtractor_LexicalSignals(t *testing.T) {
	// Use \x60 for backtick to avoid raw string delimiter conflict
	content := "Here is code:\n\x60\x60\x60go\nfunc foo() {}\n\x60\x60\x60\nAnd a stack trace:\nTraceback (most recent call last):\n File \"test.py\"\nAnd a diff:\ndiff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\nAnd file path src/internal/foo.go\nAnd keywords: refactor this code, debug the error, architecture design doc, agent tool use, extract summarize"
	m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if !feat.HasCodeBlock {
		t.Fatalf("expected code block")
	}
	if !feat.HasStackTrace {
		t.Fatalf("expected stack trace")
	}
	if !feat.HasDiff {
		t.Fatalf("expected diff")
	}
	if !feat.HasFilePath {
		t.Fatalf("expected file path")
	}
	if !feat.HasEditKeywords {
		t.Fatalf("expected edit keywords")
	}
	if !feat.HasDebugKeywords {
		t.Fatalf("expected debug keywords")
	}
	if !feat.HasArchKeywords {
		t.Fatalf("expected arch keywords")
	}
	if !feat.HasAgentKeywords {
		t.Fatalf("expected agent keywords")
	}
	if !feat.HasExtractionKeywords {
		t.Fatalf("expected extraction keywords")
	}
	if !feat.HasCodeIdentifiers {
		t.Fatalf("expected code identifiers")
	}
	if feat.RelevantTextLength == 0 {
		t.Fatalf("expected relevant text length")
	}
}

func TestExtractor_Truncation(t *testing.T) {
	// Build a message with >64KiB text
	large := strings.Repeat("a", 70*1024)
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":` + jsonMarshalString(large) + `}]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if !feat.RelevantTruncated {
		t.Fatalf("expected truncated")
	}
	if feat.RelevantTextLength != maxRelevantTextBytes {
		t.Fatalf("expected length %d got %d", maxRelevantTextBytes, feat.RelevantTextLength)
	}
}

func jsonMarshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestExtractor_Privacy_NoRawContent(t *testing.T) {
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":"my secret password is hunter2"}]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	// Ensure features don't contain raw secret
	// We check JSON marshaling of features doesn't contain secret
	b, _ := json.Marshal(feat)
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("privacy violation: raw content leaked into features")
	}
	if feat.BodySessionKey != "" {
		// BodySessionKey is allowed bounded, but not secret
	}
}

func TestExtractor_ToolCount(t *testing.T) {
	raw := []byte(`{"model":"test","messages":[],"tools":[{"type":"function"},{"type":"function"}]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if feat.ToolCount != 2 {
		t.Fatalf("expected 2 tools got %d", feat.ToolCount)
	}
	if !feat.HasTools {
		t.Fatalf("expected has tools")
	}
}

func TestExtractor_StructuredOutput(t *testing.T) {
	raw := []byte(`{"model":"test","messages":[],"response_format":{"type":"json_object"}}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if !feat.StructuredOutput {
		t.Fatalf("expected structured output")
	}
}

func TestExtractor_EstimatedTokens(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	// chars=11, messageCount=1, 11/4=2 +8+16=26
	if feat.EstimatedPromptTokens < 20 {
		t.Fatalf("unexpected token estimate %d", feat.EstimatedPromptTokens)
	}
}
