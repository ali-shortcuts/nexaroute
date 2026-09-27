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
	raw2 := []byte(`{"model":"test","metadata":{"session_id":"meta-456"},"messages":[]}`)
	feat2 := ext.Extract(raw2, ExtractOptions{
		ContentFields: []string{"messages"},
	})
	if feat2.BodySessionKey != "meta-456" {
		t.Fatalf("expected meta-456 got %q", feat2.BodySessionKey)
	}
}

func TestExtractor_TooComplex(t *testing.T) {
	m := make(map[string]any)
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
	content := "Here is code:\n\x60\x60\x60go\nfunc foo() {}\n\x60\x60\x60\nAnd a stack trace:\nTraceback (most recent call last):\n File \"test.py\"\nAnd a diff:\ndiff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\nAnd file path src/internal/foo.go\nAnd keywords: refactor this code, debug the error, architecture design doc, agent tool use, extract summarize"
	m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		Protocol:      ProtocolOpenAI,
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
	large := strings.Repeat("a", 70*1024)
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":` + jsonMarshalString(large) + `}]}`)
	ext := NewExtractor()
	feat := ext.Extract(raw, ExtractOptions{
		Protocol:      ProtocolOpenAI,
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
		Protocol:      ProtocolOpenAI,
		ContentFields: []string{"messages"},
	})
	b, _ := json.Marshal(feat)
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("privacy violation: raw content leaked into features")
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

func TestExtractor_ToolChoiceSemantics(t *testing.T) {
	tests := []struct {
		name           string
		raw            string
		expectPresent  bool
		expectRequired bool
	}{
		{
			name:           "no tool_choice",
			raw:            `{"model":"test","messages":[]}`,
			expectPresent:  false,
			expectRequired: false,
		},
		{
			name:           "tool_choice auto",
			raw:            `{"model":"test","messages":[],"tool_choice":"auto"}`,
			expectPresent:  true,
			expectRequired: false,
		},
		{
			name:           "tool_choice required",
			raw:            `{"model":"test","messages":[],"tool_choice":"required"}`,
			expectPresent:  true,
			expectRequired: true,
		},
		{
			name:           "tool_choice any anthropic",
			raw:            `{"model":"test","messages":[],"tool_choice":{"type":"any"}}`,
			expectPresent:  true,
			expectRequired: true,
		},
		{
			name:           "tool_choice tool forced",
			raw:            `{"model":"test","messages":[],"tool_choice":{"type":"tool","name":"my_tool"}}`,
			expectPresent:  true,
			expectRequired: true,
		},
		{
			name:           "tool_choice function forced",
			raw:            `{"model":"test","messages":[],"tool_choice":{"type":"function","function":{"name":"foo"}}}`,
			expectPresent:  true,
			expectRequired: true,
		},
	}
	ext := NewExtractor()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feat := ext.Extract([]byte(tc.raw), ExtractOptions{
				ContentFields: []string{"messages"},
			})
			if feat.ToolChoicePresent != tc.expectPresent {
				t.Fatalf("expected present %v got %v", tc.expectPresent, feat.ToolChoicePresent)
			}
			if feat.ToolChoiceRequired != tc.expectRequired {
				t.Fatalf("expected required %v got %v", tc.expectRequired, feat.ToolChoiceRequired)
			}
		})
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
		Protocol:      ProtocolOpenAI,
		ContentFields: []string{"messages"},
	})
	if feat.EstimatedPromptTokens < 20 {
		t.Fatalf("unexpected token estimate %d", feat.EstimatedPromptTokens)
	}
}

func TestExtractor_RelevantTextOnly(t *testing.T) {
	ext := NewExtractor()
	// Keywords inside tool schema should NOT trigger lexical signals
	raw := []byte(`{
		"model":"test",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"edit_code","description":"refactor this code, debug the error, architecture design doc, extract summarize, agent tool use, src/main.go"}}]
	}`)
	feat := ext.Extract(raw, ExtractOptions{
		Protocol:      ProtocolOpenAI,
		ContentFields: []string{"messages"},
	})
	if feat.HasCodeBlock || feat.HasEditKeywords || feat.HasDebugKeywords || feat.HasArchKeywords || feat.HasExtractionKeywords || feat.HasAgentKeywords || feat.HasFilePath {
		t.Fatalf("false positive: keywords inside tool schema triggered lexical signals: %+v", feat)
	}

	// Keywords inside tool result should NOT trigger
	raw2 := []byte(`{
		"model":"test",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"tool","tool_call_id":"1","content":"refactor this code with bug in src/main.go\n\x60\x60\x60go\nfunc foo(){}\n\x60\x60\x60\nTraceback..."}
		]
	}`)
	feat2 := ext.Extract(raw2, ExtractOptions{
		Protocol:      ProtocolOpenAI,
		ContentFields: []string{"messages"},
	})
	if feat2.HasCodeBlock || feat2.HasEditKeywords || feat2.HasFilePath {
		t.Fatalf("false positive: keywords inside tool result triggered: %+v", feat2)
	}

	// Keywords inside image_url should NOT trigger
	raw3 := []byte(`{
		"model":"test",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"https://example.com/refactor src/main.go debug"}}]}]
	}`)
	feat3 := ext.Extract(raw3, ExtractOptions{
		Protocol:      ProtocolOpenAI,
		VisionType:    "image_url",
		ContentFields: []string{"messages"},
	})
	if feat3.HasEditKeywords || feat3.HasFilePath || feat3.HasDebugKeywords {
		t.Fatalf("false positive: keywords inside image_url triggered: %+v", feat3)
	}

	// Anthropic: tool_result should not trigger
	raw4 := []byte(`{
		"model":"test",
		"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"refactor src/main.go \n\x60\x60\x60go\nfunc foo(){}\n\x60\x60\x60"}]}]
	}`)
	feat4 := ext.Extract(raw4, ExtractOptions{
		Protocol:      ProtocolAnthropic,
		ContentFields: []string{"messages"},
	})
	if feat4.HasCodeBlock || feat4.HasFilePath {
		t.Fatalf("false positive: anthropic tool_result triggered: %+v", feat4)
	}
}

func TestExtractor_FalsePositives_Spec(t *testing.T) {
	ext := NewExtractor()
	tests := []struct {
		name    string
		content string
		check   func(RequestFeatures) bool // true if should NOT be classified as specific type
	}{
		{
			name:    "edit sentence not code_edit",
			content: "Can you edit this sentence?",
			check: func(f RequestFeatures) bool {
				// Should not have code context
				return !f.HasCodeBlock && !f.HasFilePath && !f.HasDiff
			},
		},
		{
			name:    "error statistics not debugging",
			content: "Tell me what an error means in statistics.",
			check: func(f RequestFeatures) bool {
				return !f.HasStackTrace
			},
		},
		{
			name:    "birthday card not architecture",
			content: "Design a birthday card.",
			check: func(f RequestFeatures) bool {
				return !f.HasArchKeywords
			},
		},
		{
			name:    "JSON data format not structured_output",
			content: "JSON is a data format.",
			check: func(f RequestFeatures) bool {
				// Structured output flag comes from response_format, not lexical
				return !f.StructuredOutput
			},
		},
		{
			name:    "saw image not vision",
			content: "I saw an image yesterday.",
			check: func(f RequestFeatures) bool {
				return !f.HasVision
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": tc.content}}}
			raw, _ := json.Marshal(m)
			feat := ext.Extract(raw, ExtractOptions{
				Protocol:      ProtocolOpenAI,
				ContentFields: []string{"messages"},
			})
			if !tc.check(feat) {
				t.Fatalf("false positive for %q: %+v", tc.name, feat)
			}
		})
	}
}
