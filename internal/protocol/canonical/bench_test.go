package canonical

import (
	"encoding/json"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// M6-4: the canonical codec runs on every translated request.
// The bench covers the full wire->IR->wire round trip the
// handlers pay (JSON parse, decode, encode, JSON marshal).

var anthropicWire = []byte(`{"model":"m","max_tokens":1024,"system":[{"type":"text","text":"You are helpful."}],"messages":[{"role":"user","content":"Explain routing"},{"role":"assistant","content":[{"type":"text","text":"Routing is..."},{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"result body"}]}],"tools":[{"name":"lookup","description":"looks things up","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)

var openAIWire = []byte(`{"model":"m","max_tokens":1024,"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Explain routing"},{"role":"assistant","content":"Routing is...","tool_calls":[{"id":"t1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},{"role":"tool","tool_call_id":"t1","content":"result body"}],"tools":[{"type":"function","function":{"name":"lookup","description":"looks things up","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)

func BenchmarkAnthropicRoundTrip(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var in core.AnthropicRequest
		if err := json.Unmarshal(anthropicWire, &in); err != nil {
			b.Fatal(err)
		}
		ir, err := DecodeAnthropicRequest(in, "m")
		if err != nil {
			b.Fatal(err)
		}
		out, err := EncodeAnthropicRequest(ir, "m")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenAIChatRoundTrip(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var in core.OpenAIRequest
		if err := json.Unmarshal(openAIWire, &in); err != nil {
			b.Fatal(err)
		}
		ir, err := DecodeOpenAIChatRequest(in, "m")
		if err != nil {
			b.Fatal(err)
		}
		out, err := EncodeOpenAIChatRequest(ir, "m", true)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(out); err != nil {
			b.Fatal(err)
		}
	}
}
