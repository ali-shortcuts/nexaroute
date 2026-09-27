package translate

import (
	"encoding/json"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// FuzzAnthropicToOpenAI ensures arbitrary client payload structure cannot panic
// protocol translation. Invalid input is expected to return an error.
func FuzzAnthropicToOpenAI(f *testing.F) {
	for _, seed := range []string{
		`{"model":"public","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"public","max_tokens":20,"system":[{"type":"text","text":"policy"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a.b","input":{"x":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}]}`,
		`null`, `[]`, `{"messages":[{"content":[{"type":"image","source":{"type":"base64"}}]}]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var in core.AnthropicRequest
		if err := json.Unmarshal(raw, &in); err != nil {
			return
		}
		_, _, _ = AnthropicToOpenAI(in, "physical-model")
	})
}

// FuzzOpenAIToAnthropic covers Chat Completions content/tool parsing and
// Anthropic alternation constraints with arbitrary JSON inputs.
func FuzzOpenAIToAnthropic(f *testing.F) {
	for _, seed := range []string{
		`{"model":"public","messages":[{"role":"user","content":"hello"}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"run","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"ok"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`,
		`null`, `{"messages":[{"role":"tool","content":"orphan"}]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var in core.OpenAIRequest
		if err := json.Unmarshal(raw, &in); err != nil {
			return
		}
		_, _, _ = OpenAIToAnthropic(in, "physical-model")
	})
}
