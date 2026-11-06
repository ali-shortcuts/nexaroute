package translate

import (
	"math"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// Agent E coverage: OpenAIToAnthropic surfaces a JSON encoding failure
// instead of emitting a corrupt Anthropic request
// (openai_to_anthropic.go:187-188). A tool-result part may carry an
// unvalidated extra key; a non-finite float there is invisible to the
// content validator (which only checks type/text) but makes
// json.Marshal of the assembled blocks fail.
func TestAgentEOpenAIToAnthropicMarshalError(t *testing.T) {
	in := core.OpenAIRequest{Model: "m", MaxTokens: 10, Messages: []core.OpenAIMessage{
		{Role: "assistant", Content: "looking up", ToolCalls: []core.OpenAIToolCall{
			{ID: "call_1", Type: "function", Function: core.OpenAIFunctionCall{Name: "lookup", Arguments: `{"q":"canary"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: []any{
			map[string]any{"type": "text", "text": "ok", "junk": math.NaN()},
		}},
	}}
	_, _, err := OpenAIToAnthropic(in, "backend")
	if err == nil {
		t.Fatal("expected marshal error for non-finite tool-result content, got nil")
	}
	if !strings.Contains(err.Error(), "translate OpenAI message content") {
		t.Fatalf("unexpected error: %v", err)
	}
}
