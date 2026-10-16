package httpapi

// Wave C-2 (#403) supervisor-takeover fuzz target for the canonical stream
// pump (canonicalStreamPump): the end-to-end client-visible invariant.
//
// Under arbitrary upstream SSE bytes, for every upstream dialect:
//   - the pump must never panic;
//   - after the first terminal client frame (a finish_reason chunk or
//     [DONE]), no later client frame may carry tool-call chunks
//     ("tool_calls":[). Tool events decoded after the terminal point are
//     dropped by the pump: a hostile or buggy upstream must not inject
//     tool calls after the stream ended.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

var fuzzPumpKinds = []string{"openai_chat", "openai_responses", "anthropic", "gemini", "bedrock"}

// clientDataFrames returns the data: payloads of a client SSE body.
func clientDataFrames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return out
}

// upstreamToolNames replays the pump's terminal latch over the raw upstream
// frames and reports tool names whose StreamToolStart was decoded strictly
// after the terminal point (post) versus at/before it (pre). It mirrors the
// latch in canonicalStreamPump: chunkTerminal booleans OR-ed per frame,
// plus StreamEnd events latching terminal for every dialect.
func upstreamToolNames(kind string, raw []byte) (post, pre map[string]bool) {
	post, pre = map[string]bool{}, map[string]bool{}
	sr := canonical.NewSSEReader(bytes.NewReader(raw))
	terminal := false
frames:
	for {
		name, data, done, err := sr.Next()
		if err != nil || done {
			break
		}
		frameTerminal := terminal
		var evs []canonical.StreamEvent
		var chunkTerminal bool
		var derr error
		switch kind {
		case "anthropic":
			evs, derr = canonical.DecodeAnthropicStreamEvent(name, data)
		case "gemini":
			evs, _, derr = canonical.DecodeGeminiStreamChunk(data)
		case "bedrock":
			evs, chunkTerminal, derr = canonical.DecodeBedrockStreamChunk(data)
		case "openai_responses":
			evs, chunkTerminal, derr = canonical.DecodeResponsesStreamEvent(name, data)
		default:
			evs, chunkTerminal, derr = canonical.DecodeOpenAIStreamChunk(data)
		}
		if derr != nil {
			break // the pump breaks the frame loop on decode error
		}
		terminal = terminal || chunkTerminal
		for _, ev := range evs {
			if ev.Type == canonical.StreamError {
				break frames // the pump breaks the frame loop on stream error
			}
			if ev.Type == canonical.StreamEnd {
				terminal = true
			}
			if ev.Type == canonical.StreamToolStart {
				if frameTerminal {
					post[ev.ToolName] = true
				} else {
					pre[ev.ToolName] = true
				}
			}
		}
	}
	return post, pre
}

func FuzzStreamPumpClient(f *testing.F) {
	f.Add(byte(0), []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n"))
	// Adversarial: terminal frame, then tool frames, then another end.
	// The tool chunks must never reach the client after the terminal chunk.
	f.Add(byte(0), []byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t-evil\",\"type\":\"function\",\"function\":{\"name\":\"evil\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]},\"index\":0}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\n"))
	f.Add(byte(2), []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	// Bedrock sends usage metadata AFTER messageStop; that must keep working.
	f.Add(byte(4), []byte("data: {\"eventType\":\"contentBlockDelta\",\"payload\":{\"contentBlockIndex\":0,\"delta\":{\"text\":\"hi\"}}}\n\ndata: {\"eventType\":\"messageStop\",\"payload\":{\"stopReason\":\"end_turn\"}}\n\ndata: {\"eventType\":\"metadata\",\"payload\":{\"usage\":{\"inputTokens\":5,\"outputTokens\":9}}}\n\n"))
	f.Add(byte(1), []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"))
	f.Fuzz(func(t *testing.T, kindByte byte, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip("input too large for a bounded fuzz iteration")
		}
		kind := fuzzPumpKinds[int(kindByte)%len(fuzzPumpKinds)]
		rr := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
		}
		// Must never panic, whatever the upstream bytes.
		_ = (&Server{}).canonicalStreamPump(rr, resp, kind, "openai_chat", "m", "req-fuzz", nil, nil)
		body := rr.Body.String()
		// The client stream must stay well-formed SSE: every data payload
		// is [DONE] or valid JSON.
		for _, p := range clientDataFrames(body) {
			if p == "[DONE]" {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(p), &v); err != nil {
				t.Fatalf("kind %s: malformed client SSE payload: %q", kind, p)
			}
		}
		// No tool events after End: a tool whose start was decoded after
		// the terminal point must never appear in the client stream
		// (unless the same name also started pre-terminal).
		post, pre := upstreamToolNames(kind, raw)
		for name := range post {
			if pre[name] {
				continue
			}
			if strings.Contains(body, `"name":"`+name+`"`) {
				t.Fatalf("kind %s: post-terminal tool %q reached the client stream:\n%s",
					kind, name, body)
			}
		}
	})
}

// TestStreamPumpDropsPostTerminalTools is the minimized regression for the
// defect found by FuzzStreamPumpClient (#403): tool events decoded after
// the terminal frame were emitted to the client after the terminal chunk.
func TestStreamPumpDropsPostTerminalTools(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t-evil\",\"type\":\"function\",\"function\":{\"name\":\"evil\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\n"
	rr := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(raw))),
	}
	_ = (&Server{}).canonicalStreamPump(rr, resp, "openai_chat", "openai_chat", "m", "req-red", nil, nil)
	body := rr.Body.String()
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("expected a terminal finish chunk:\n%s", body)
	}
	if strings.Contains(body, `"name":"evil"`) {
		t.Fatalf("RED: post-terminal tool call reached the client stream:\n%s", body)
	}
}
