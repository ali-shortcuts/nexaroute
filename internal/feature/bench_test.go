package feature

import (
	"strings"
	"testing"
)

func BenchmarkExtractor_TinyChat(b *testing.B) {
	raw := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	ext := NewExtractor()
	opts := ExtractOptions{
		Protocol:      ProtocolOpenAI,
		Model:         "gpt-4",
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning_effort"},
		ContentFields: []string{"messages"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ext.Extract(raw, opts)
	}
}

func BenchmarkExtractor_Coding(b *testing.B) {
	content := "fix this bug in src/main.go\n```go\nfunc foo() { fmt.Println(\"hi\") }\n```\nStack trace: panic: nil pointer"
	raw := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":` + jsonMarshalString(content) + `}]}`)
	ext := NewExtractor()
	opts := ExtractOptions{
		Protocol:      ProtocolOpenAI,
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning_effort"},
		ContentFields: []string{"messages"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ext.Extract(raw, opts)
	}
}

func BenchmarkExtractor_ToolHeavy(b *testing.B) {
	tools := `[{"type":"function","function":{"name":"tool1"}},{"type":"function","function":{"name":"tool2"}},{"type":"function","function":{"name":"tool3"}},{"type":"function","function":{"name":"tool4"}},{"type":"function","function":{"name":"tool5"}}]`
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":"use tools"}],"tools":` + tools + `}`)
	ext := NewExtractor()
	opts := ExtractOptions{
		ContentFields: []string{"messages"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ext.Extract(raw, opts)
	}
}

func BenchmarkExtractor_ScanLimit(b *testing.B) {
	large := strings.Repeat("a", 70*1024)
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":` + jsonMarshalString(large) + `}]}`)
	ext := NewExtractor()
	opts := ExtractOptions{
		ContentFields: []string{"messages"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ext.Extract(raw, opts)
	}
}
