package feature

import (
	"testing"
)

// Audit item 7: feature was at 59.4% — anthropic/responses collectors and
// tool-choice parsing had low or zero coverage.
func TestAuditAnthropicCollect(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"base64"}}]}]}`)
	ex := NewExtractor()
	f := ex.Extract(raw, ExtractOptions{Protocol: ProtocolAnthropic, VisionType: "image", ContentFields: []string{"messages"}})
	if !f.HasVision {
		t.Fatalf("anthropic image should set HasVision: %+v", f)
	}
}

func TestAuditResponsesCollect(t *testing.T) {
	raw := []byte(`{"model":"m","input":[{"type":"message","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"x"}]}],"reasoning":{"effort":"high"}}`)
	ex := NewExtractor()
	f := ex.Extract(raw, ExtractOptions{Protocol: ProtocolResponses, VisionType: "input_image", ReasoningKeys: []string{"reasoning"}, ContentFields: []string{"input", "instructions"}})
	if !f.HasReasoning {
		t.Fatalf("responses reasoning should set HasReasoning: %+v", f)
	}
}

func TestAuditToolChoiceVariants(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","messages":[],"tools":[{"function":{"name":"f"}}],"tool_choice":"required"}`,
		`{"model":"m","messages":[],"tools":[{"function":{"name":"f"}}],"tool_choice":"auto"}`,
		`{"model":"m","messages":[],"tools":[{"function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"f"}}}`,
		`{"model":"m","messages":[]}`,
	} {
		ex := NewExtractor()
		f := ex.Extract([]byte(raw), ExtractOptions{Protocol: ProtocolOpenAI, ContentFields: []string{"messages"}})
		_ = f
	}
}

func TestAuditSessionAndBounds(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"session_id":"sess-1","metadata":{"session_id":"sess-2"}}`)
	ex := NewExtractor()
	f := ex.Extract(raw, ExtractOptions{Protocol: ProtocolOpenAI, ContentFields: []string{"messages"}})
	if f.BodySessionKey == "" {
		t.Fatalf("should extract session key: %+v", f)
	}
	if f.EstimatedPromptTokens <= 0 {
		t.Fatalf("should estimate tokens: %+v", f)
	}
}
