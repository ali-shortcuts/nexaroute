package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Codex CLI conformance (Phase 5). Codex always streams the OpenAI
// Responses API and reconstructs ResponseItems from
// response.output_item.added / response.output_item.done only
// (openai/codex codex-rs/codex-api/src/sse/responses.rs). These tests
// drive a Codex-shaped /v1/responses request through a non-Responses
// upstream and assert the Codex-observable wire contract.

type codexUpstream struct {
	kind string

	mu   sync.Mutex
	body []byte
}

func (u *codexUpstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return string(u.body)
}

func (u *codexUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.body = body
	u.mu.Unlock()
	switch u.kind {
	case "openai_chat":
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n")
		write("data: [DONE]\n\n")
	case "anthropic":
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

const codexToolsJSON = `[{"type":"function","name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]`

func codexResponsesRequest() string {
	return `{"model":"m","stream":true,"store":false,"instructions":"You are Codex.","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"run lookup"}]}],"tools":` + codexToolsJSON + `}`
}

func codexGateway(t *testing.T, upstream *codexUpstream, providerType string) *Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "cx", Name: "Codex upstream", Type: providerType, BaseURL: up.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func codexSSEFrames(t *testing.T, body string) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	var current string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			current = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var frame map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				t.Fatalf("Codex client received a non-JSON Responses frame: %v (%s)", err, line)
			}
			out[current] = append(out[current], frame)
		}
	}
	return out
}

func TestCodexResponsesStreamToolLoopPreservesToolCallOverChatUpstream(t *testing.T) {
	upstream := &codexUpstream{kind: "openai_chat"}
	s := codexGateway(t, upstream, "openai_compatible")

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(codexResponsesRequest()))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("Responses stream must not use the Chat [DONE] sentinel: %s", out)
	}

	// The upstream must have received a Chat-shaped request with the
	// Codex instructions as a system message and reasoning effort
	// mapped, and must not leak the Responses-only store flag.
	seen := upstream.lastBody()
	for _, want := range []string{`"role":"system"`, "You are Codex.", `"reasoning_effort":"high"`, `"lookup"`} {
		if !strings.Contains(seen, want) {
			t.Fatalf("Chat upstream request missing %s: %s", want, seen)
		}
	}
	if strings.Contains(seen, `"store"`) {
		t.Fatalf("Responses-only store flag leaked to a Chat upstream: %s", seen)
	}

	frames := codexSSEFrames(t, out)

	// Codex reconstructs the tool call from response.output_item.done.
	var callItem map[string]any
	for _, f := range frames["response.output_item.done"] {
		if it, _ := f["item"].(map[string]any); it != nil && it["type"] == "function_call" {
			callItem = it
		}
	}
	if callItem == nil {
		t.Fatalf("no function_call item for Codex to parse: %s", out)
	}
	if callItem["name"] != "lookup" {
		t.Fatalf("function_call item name=%v want lookup", callItem["name"])
	}
	if callItem["arguments"] != `{"q":"x"}` {
		t.Fatalf("function_call item arguments=%q want {\"q\":\"x\"} (Codex drops empty args)", callItem["arguments"])
	}
	if cid, _ := callItem["call_id"].(string); cid == "" {
		t.Fatalf("function_call item lost call_id: %v", callItem)
	}

	// Codex requires a parseable response.completed with total_tokens.
	completed := frames["response.completed"]
	if len(completed) != 1 {
		t.Fatalf("want one response.completed, got %d: %s", len(completed), out)
	}
	resp, _ := completed[0]["response"].(map[string]any)
	if resp == nil || resp["id"] == nil {
		t.Fatalf("response.completed missing aggregate response id: %s", out)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("response.completed lost usage: %s", out)
	}
	if tt, ok := usage["total_tokens"].(float64); !ok || int(tt) != 12 {
		t.Fatalf("response.completed usage total_tokens=%v want 12", usage["total_tokens"])
	}
}

func TestCodexResponsesStreamTextEmitsMessageItemOverAnthropicUpstream(t *testing.T) {
	upstream := &codexUpstream{kind: "anthropic"}
	s := codexGateway(t, upstream, "anthropic_compatible")

	body := `{"model":"m","stream":true,"instructions":"You are Codex.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"say hi"}]}],"tools":` + codexToolsJSON + `}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	frames := codexSSEFrames(t, out)

	// Anthropic upstream must have received the Codex instructions as a
	// system prompt and the tool definition.
	seen := upstream.lastBody()
	if !strings.Contains(seen, "You are Codex.") || !strings.Contains(seen, "lookup") {
		t.Fatalf("anthropic upstream request lost instructions/tools: %s", seen)
	}

	foundAddedMessage := false
	for _, f := range frames["response.output_item.added"] {
		if it, _ := f["item"].(map[string]any); it != nil && it["type"] == "message" {
			foundAddedMessage = true
		}
	}
	if !foundAddedMessage {
		t.Fatalf("no assistant message item announced for Codex: %s", out)
	}
	var doneItem map[string]any
	for _, f := range frames["response.output_item.done"] {
		if it, _ := f["item"].(map[string]any); it != nil && it["type"] == "message" {
			doneItem = it
		}
	}
	if doneItem == nil {
		t.Fatalf("no completed assistant message item for Codex history: %s", out)
	}
	content, _ := doneItem["content"].([]any)
	var text string
	for _, c := range content {
		if m, _ := c.(map[string]any); m != nil {
			if s, _ := m["text"].(string); s != "" {
				text += s
			}
		}
	}
	if text != "Hi" {
		t.Fatalf("assistant message content=%q want Hi", text)
	}
	completed := frames["response.completed"]
	if len(completed) != 1 {
		t.Fatalf("want one response.completed, got %d: %s", len(completed), out)
	}
	usage, _ := completed[0]["response"].(map[string]any)["usage"].(map[string]any)
	if tt, ok := usage["total_tokens"].(float64); !ok || int(tt) != 11 {
		t.Fatalf("response.completed usage total_tokens=%v want 11", usage["total_tokens"])
	}
}

// TestResponsesUpstreamArgumentsOnlyInDoneSurviveToAnthropicClient is
// the end-to-end regression for a Responses-dialect upstream (for
// example gpt-5.3-codex-spark) that never emits
// response.function_call_arguments.delta and puts the full JSON only in
// response.function_call_arguments.done / the completed item.
func TestResponsesUpstreamArgumentsOnlyInDoneSurviveToAnthropicClient(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\n")
		write("event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_call_1\",\"output_index\":0,\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}\n\n")
		write("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n")
		write("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}],\"usage\":{\"input_tokens\":8,\"output_tokens\":4,\"total_tokens\":12}}}\n\n")
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "cx", Name: "Responses upstream", Type: "openai_responses", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	body := `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"run lookup"}],"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, "tool_use") {
		t.Fatalf("Anthropic client never saw a tool_use block: %s", out)
	}
	var args strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil && ev.Type == "content_block_delta" && ev.Delta.Type == "input_json_delta" {
			args.WriteString(ev.Delta.PartialJSON)
		}
	}
	if args.String() != `{"q":"x"}` {
		t.Fatalf("tool arguments delivered only in .done were lost/doubled: %q\n%s", args.String(), out)
	}
}
