package feature

import (
	"fmt"
	"strings"
	"testing"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

func agentFExtractor() *Extractor { return NewExtractor() }

// TestAgentFExtractTooManyNodes verifies the node bound: a request naming
// more than maxNodes content fields is flagged TooComplex instead of
// walking unbounded input.
func TestAgentFExtractTooManyNodes(t *testing.T) {
	const n = 100001
	var sb strings.Builder
	sb.WriteString("{")
	fields := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		key := fmt.Sprintf("k%d", i)
		fmt.Fprintf(&sb, "%q:1", key)
		fields = append(fields, key)
	}
	sb.WriteString("}")
	out := agentFExtractor().Extract([]byte(sb.String()), ExtractOptions{ContentFields: fields})
	if !out.TooComplex {
		t.Fatal("100001 content fields must flag TooComplex")
	}
}

// TestAgentFExtractCharsClamped verifies over-long text is clamped to the
// total-chars bound instead of inflating the token estimate unboundedly.
func TestAgentFExtractCharsClamped(t *testing.T) {
	body := `{"messages":[{"role":"user","content":` + fmt.Sprintf("%q", strings.Repeat("x", maxTotalChars+1)) + `}]}`
	out := agentFExtractor().Extract([]byte(body), ExtractOptions{})
	if out.TotalChars != maxTotalChars {
		t.Fatalf("TotalChars = %d, want clamped to %d", out.TotalChars, maxTotalChars)
	}
	if out.TooComplex {
		t.Fatal("a single over-long string must clamp, not flag TooComplex")
	}
}

// TestAgentFToolChoiceAuto verifies a tool_choice of type "auto" is present
// but not required.
func TestAgentFToolChoiceAuto(t *testing.T) {
	out := agentFExtractor().Extract([]byte(`{"tool_choice":{"type":"auto"}}`), ExtractOptions{})
	if !out.ToolChoicePresent {
		t.Fatal("tool_choice auto must be present")
	}
	if out.ToolChoiceRequired {
		t.Fatal("tool_choice auto must not be required")
	}
}

// TestAgentFToolChoiceNamedWithoutType verifies a tool_choice naming a
// function without a type field is treated as forced.
func TestAgentFToolChoiceNamedWithoutType(t *testing.T) {
	out := agentFExtractor().Extract([]byte(`{"tool_choice":{"name":"get_weather"}}`), ExtractOptions{})
	if !out.ToolChoicePresent {
		t.Fatal("named tool_choice must be present")
	}
	if !out.ToolChoiceRequired {
		t.Fatal("named tool_choice without a type must be required")
	}
}

// TestAgentFBoundIntClamps verifies the integer bound helper clamps both
// ends and passes through in-range values.
func TestAgentFBoundIntClamps(t *testing.T) {
	if got := boundInt(-5, 0, 10); got != 0 {
		t.Fatalf("boundInt(-5,0,10) = %d, want 0", got)
	}
	if got := boundInt(15, 0, 10); got != 10 {
		t.Fatalf("boundInt(15,0,10) = %d, want 10", got)
	}
	if got := boundInt(5, 0, 10); got != 5 {
		t.Fatalf("boundInt(5,0,10) = %d, want 5", got)
	}
}

// TestAgentFSessionKeyTruncated verifies an over-long session id is
// truncated to the bound instead of being stored in full.
func TestAgentFSessionKeyTruncated(t *testing.T) {
	long := strings.Repeat("s", maxSessionKeyLen+50)
	body := fmt.Sprintf(`{"session_id":%q}`, long)
	out := agentFExtractor().Extract([]byte(body), ExtractOptions{})
	if !out.SessionKeyPresent {
		t.Fatal("session key must be present")
	}
	if len(out.BodySessionKey) != maxSessionKeyLen {
		t.Fatalf("BodySessionKey len = %d, want %d", len(out.BodySessionKey), maxSessionKeyLen)
	}
	if out.BodySessionKey != long[:maxSessionKeyLen] {
		t.Fatal("BodySessionKey must be the truncated prefix")
	}
}

// TestAgentFBoundedBuilderDefaults verifies a builder with no limit falls
// back to the default relevant-text bound.
func TestAgentFBoundedBuilderDefaults(t *testing.T) {
	b := &boundedStringBuilder{}
	b.WriteString("hello")
	if b.String() != "hello" {
		t.Fatalf("builder = %q, want hello", b.String())
	}
	if b.truncated {
		t.Fatal("short write must not truncate")
	}
}

// TestAgentFBoundedBuilderExactLimit verifies a second write once the
// builder is exactly full marks it truncated and keeps the original text.
func TestAgentFBoundedBuilderExactLimit(t *testing.T) {
	b := &boundedStringBuilder{limit: 3}
	b.WriteString("abc")
	if b.truncated {
		t.Fatal("exact-limit write must not truncate yet")
	}
	b.WriteString("d")
	if !b.truncated {
		t.Fatal("write past an exactly-full builder must truncate")
	}
	if b.String() != "abc" {
		t.Fatalf("builder = %q, want abc", b.String())
	}
}

// TestAgentFAnthropicMessagesNotArray verifies non-array messages are
// ignored instead of crashing relevant-text collection.
func TestAgentFAnthropicMessagesNotArray(t *testing.T) {
	out := agentFExtractor().Extract([]byte(`{"messages":"hello"}`), ExtractOptions{Protocol: ProtocolAnthropic})
	if out.RelevantTextLength != 0 {
		t.Fatalf("RelevantTextLength = %d, want 0 for non-array messages", out.RelevantTextLength)
	}
}

// TestAgentFAnthropicNilContent verifies a message with null content is
// skipped in relevant-text collection.
func TestAgentFAnthropicNilContent(t *testing.T) {
	out := agentFExtractor().Extract([]byte(`{"messages":[{"role":"user","content":null}]}`), ExtractOptions{Protocol: ProtocolAnthropic})
	if out.RelevantTextLength != 0 {
		t.Fatalf("RelevantTextLength = %d, want 0 for null content", out.RelevantTextLength)
	}
}

// TestAgentFResponsesInputString verifies a Responses-API string input is
// collected as relevant text.
func TestAgentFResponsesInputString(t *testing.T) {
	out := agentFExtractor().Extract([]byte(`{"input":"refactor the handler"}`), ExtractOptions{Protocol: ProtocolResponses})
	if out.RelevantTextLength == 0 {
		t.Fatal("string input must be collected as relevant text")
	}
}

// TestAgentFResponsesInputArray verifies a Responses-API array input of
// strings is collected as relevant text.
func TestAgentFResponsesInputArray(t *testing.T) {
	out := agentFExtractor().Extract([]byte(`{"input":["hello","world"]}`), ExtractOptions{Protocol: ProtocolResponses})
	if out.RelevantTextLength == 0 {
		t.Fatal("array input must be collected as relevant text")
	}
}

// TestAgentFEditKeywordsBroaderWords verifies the broader edit-word check:
// with code context present, a plain "edit" counts as an edit signal even
// though it is not one of the specific edit phrases.
func TestAgentFEditKeywordsBroaderWords(t *testing.T) {
	content := "```go\nx := 1\n```\nplease edit the function"
	body := `{"messages":[{"role":"user","content":` + fmt.Sprintf("%q", content) + `}]}`
	out := agentFExtractor().Extract([]byte(body), ExtractOptions{Protocol: ProtocolAnthropic})
	if !out.HasCodeBlock {
		t.Fatal("code block must be detected")
	}
	if !out.HasEditKeywords {
		t.Fatal("plain 'edit' with code context must count as edit keywords")
	}
}

// TestAgentFClientClassValid verifies Valid rejects values outside the
// declared client-class vocabulary.
func TestAgentFClientClassValid(t *testing.T) {
	if got := ClientClass("bogus").Valid(); got {
		t.Fatal("undeclared client class must not be valid")
	}
	for _, c := range []ClientClass{ClientClassUnknown, ClientClassWeb, ClientClassCLI, ClientClassSDK, ClientClassAgent} {
		if !c.Valid() {
			t.Fatalf("declared client class %q must be valid", c)
		}
	}
}
