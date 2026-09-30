package feature

import (
	"testing"
)

// Finding F7: collectRelevantAnthropic and collectRelevantResponses had 0%
// coverage. These tests drive them through the public Extract entry point and
// assert observable lexical/session/tool-choice behavior.
func TestExtractAnthropicRelevantText(t *testing.T) {
	raw := []byte("{\"model\": \"claude-x\"," +
		"\"system\": [{\"type\": \"text\", \"text\": \"You are a coding assistant. See src/main.go.\"}]," +
		"\"messages\": [" +
		"{\"role\": \"user\", \"content\": [{\"type\": \"text\", \"text\": \"Fix this bug ```go\\nfunc x() {}\\n``` traceback panic: goroutine\"}]}," +
		"{\"role\": \"assistant\", \"content\": [{\"type\": \"tool_use\", \"name\": \"bash\", \"input\": {\"cmd\": \"ls\"}}]}," +
		"{\"role\": \"user\", \"content\": \"plain follow-up\"}]}")
	feat := NewExtractor().Extract(raw, ExtractOptions{
		Protocol:      ProtocolAnthropic,
		Model:         "claude-x",
		ContentFields: []string{"messages"},
	})
	if !feat.HasCodeBlock {
		t.Fatal("expected code block signal from Anthropic text block")
	}
	if !feat.HasFilePath {
		t.Fatal("expected file path signal from system block")
	}
	if !feat.HasStackTrace {
		t.Fatal("expected stack trace signal")
	}
	if feat.RelevantTextLength == 0 {
		t.Fatal("expected relevant text to be collected")
	}
}

func TestExtractAnthropicStringSystemAndContent(t *testing.T) {
	raw := []byte(`{
		"system": "Be concise. Visit https://example.com/docs for details.",
		"messages": [{"role": "user", "content": "summarize this table as json output"}]
	}`)
	feat := NewExtractor().Extract(raw, ExtractOptions{Protocol: ProtocolAnthropic})
	if !feat.HasURL {
		t.Fatal("expected URL signal from string system prompt")
	}
	if !feat.HasExtractionKeywords {
		t.Fatal("expected extraction keywords signal")
	}
}

func TestExtractResponsesRelevantText(t *testing.T) {
	raw := []byte(`{
		"instructions": "Refactor the component. See internal/server.go diff --git a/x b/y.",
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "update the handler func process()"}]},
			{"role": "user", "content": "direct string content"},
			{"type": "input_text", "text": "bare block without content wrapper"}
		]
	}`)
	feat := NewExtractor().Extract(raw, ExtractOptions{Protocol: ProtocolResponses})
	if !feat.HasDiff {
		t.Fatal("expected diff signal from instructions")
	}
	if !feat.HasFilePath {
		t.Fatal("expected file path signal")
	}
	if !feat.HasEditKeywords {
		t.Fatal("expected edit keywords signal")
	}
	if !feat.HasCodeIdentifiers {
		t.Fatal("expected code identifier signal (func)")
	}
	if feat.RelevantTextLength == 0 {
		t.Fatal("expected relevant text from Responses input")
	}
}

func TestExtractSessionKeyVariants(t *testing.T) {
	ext := NewExtractor()
	if f := ext.Extract([]byte(`{"session_id":"abc123","messages":[]}`), ExtractOptions{}); !f.SessionKeyPresent || f.BodySessionKey != "abc123" {
		t.Fatalf("top-level session_id not detected: %+v", f)
	}
	if f := ext.Extract([]byte(`{"metadata":{"session_id":"meta-1"},"messages":[]}`), ExtractOptions{}); !f.SessionKeyPresent || f.BodySessionKey != "meta-1" {
		t.Fatalf("metadata session_id not detected: %+v", f)
	}
	if f := ext.Extract([]byte(`{"metadata":{"user_id":{"session_id":"nested-1"}},"messages":[]}`), ExtractOptions{}); !f.SessionKeyPresent || f.BodySessionKey != "nested-1" {
		t.Fatalf("nested user session_id not detected: %+v", f)
	}
	if f := ext.Extract([]byte(`{"messages":[]}`), ExtractOptions{}); f.SessionKeyPresent {
		t.Fatal("unexpected session key")
	}
}

func TestExtractToolChoiceAndStructuredOutput(t *testing.T) {
	ext := NewExtractor()
	required := ext.Extract([]byte(`{"messages":[],"tool_choice":"required"}`), ExtractOptions{})
	if !required.ToolChoicePresent || !required.ToolChoiceRequired {
		t.Fatalf("required tool_choice not detected: %+v", required)
	}
	mapped := ext.Extract([]byte(`{"messages":[],"tool_choice":{"type":"function","name":"get_weather"}}`), ExtractOptions{})
	if !mapped.ToolChoicePresent || !mapped.ToolChoiceRequired {
		t.Fatalf("function tool_choice not detected: %+v", mapped)
	}
	auto := ext.Extract([]byte(`{"messages":[],"tool_choice":"auto"}`), ExtractOptions{})
	if !auto.ToolChoicePresent || auto.ToolChoiceRequired {
		t.Fatalf("auto tool_choice misclassified: %+v", auto)
	}
	structured := ext.Extract([]byte(`{"messages":[],"response_format":{"type":"json_object"}}`), ExtractOptions{})
	if !structured.StructuredOutput {
		t.Fatal("response_format should mark structured output")
	}
	schema := ext.Extract([]byte(`{"messages":[],"json_schema":{"name":"s"}}`), ExtractOptions{})
	if !schema.StructuredOutput {
		t.Fatal("json_schema should mark structured output")
	}
	// ToolCountHint is clamped to the bounded maximum rather than stored raw.
	huge := ext.Extract([]byte(`{"messages":[]}`), ExtractOptions{ToolCountHint: 100000})
	if huge.ToolCount != 128 || !huge.HasTools {
		t.Fatalf("tool count hint not clamped: %+v", huge)
	}
}
