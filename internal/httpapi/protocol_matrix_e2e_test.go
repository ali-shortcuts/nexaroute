package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// These tests are intentionally handler-to-upstream E2E tests rather than
// isolated translator tests. Every advertised Messages/Chat mapping traverses
// routing, payload construction, the real HTTP adapter, response validation,
// translation (where applicable), and the public response encoder.
type protocolMatrixPath struct {
	name     string
	ingress  string
	upstream string
}

var protocolMatrixPaths = []protocolMatrixPath{
	{name: "anthropic_to_anthropic", ingress: "anthropic", upstream: "anthropic_compatible"},
	{name: "anthropic_to_openai", ingress: "anthropic", upstream: "openai_compatible"},
	{name: "openai_to_openai", ingress: "openai", upstream: "openai_compatible"},
	{name: "openai_to_anthropic", ingress: "openai", upstream: "anthropic_compatible"},
}

func protocolMatrixConfig(upstreamURL, providerType string) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{{
		ID: "matrix", Name: "Matrix", Type: providerType, BaseURL: upstreamURL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "physical", Model: "upstream-model", Aliases: []string{"client-model"},
			Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true},
		}},
	}}
	return cfg
}

func protocolMatrixRequest(ingress string, stream bool) (string, string) {
	if ingress == "anthropic" {
		return "/v1/messages", fmt.Sprintf(`{
			"model":"client-model","max_tokens":321,"temperature":0.25,"top_p":0.75,"stream":%t,
			"system":"matrix system",
			"tools":[
				{"name":"read_file","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}},
				{"name":"search","description":"search","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}
			],
			"tool_choice":{"type":"tool","name":"read_file"},
			"messages":[
				{"role":"user","content":"first turn"},
				{"role":"assistant","content":[
					{"type":"text","text":"calling two tools"},
					{"type":"tool_use","id":"call-a","name":"read_file","input":{"path":"a.txt"}},
					{"type":"tool_use","id":"call-b","name":"search","input":{"query":"needle"}}
				]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"call-a","content":"alpha"},
					{"type":"tool_result","tool_use_id":"call-b","content":"beta"},
					{"type":"text","text":"continue"}
				]}
			]
		}`, stream)
	}
	return "/v1/chat/completions", fmt.Sprintf(`{
		"model":"client-model","max_tokens":321,"temperature":0.25,"top_p":0.75,"stream":%t,
		"messages":[
			{"role":"system","content":"matrix system"},
			{"role":"user","content":"first turn"},
			{"role":"assistant","content":"calling two tools","tool_calls":[
				{"id":"call-a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}},
				{"id":"call-b","type":"function","function":{"name":"search","arguments":"{\"query\":\"needle\"}"}}
			]},
			{"role":"tool","tool_call_id":"call-a","content":"alpha"},
			{"role":"tool","tool_call_id":"call-b","content":"beta"},
			{"role":"user","content":"continue"}
		],
		"tools":[
			{"type":"function","function":{"name":"read_file","description":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},
			{"type":"function","function":{"name":"search","description":"search","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}
		],
		"tool_choice":{"type":"function","function":{"name":"read_file"}}
	}`, stream)
}

func matrixMap(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", label, value)
	}
	return out
}

func matrixSlice(t *testing.T, value any, label string) []any {
	t.Helper()
	out, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, want array", label, value)
	}
	return out
}

func assertProtocolMatrixUpstreamRequest(t *testing.T, providerType string, got map[string]any) {
	t.Helper()
	if got["model"] != "upstream-model" {
		t.Fatalf("physical model not selected: %#v", got["model"])
	}
	if got["max_tokens"] != float64(321) || got["temperature"] != 0.25 || got["top_p"] != 0.75 {
		t.Fatalf("generation controls lost: max=%v temperature=%v top_p=%v", got["max_tokens"], got["temperature"], got["top_p"])
	}
	tools := matrixSlice(t, got["tools"], "tools")
	if len(tools) != 2 {
		t.Fatalf("tools=%d want 2", len(tools))
	}
	choice := matrixMap(t, got["tool_choice"], "tool_choice")
	messages := matrixSlice(t, got["messages"], "messages")
	if len(messages) < 3 {
		t.Fatalf("multi-turn history was lost: %#v", messages)
	}

	if providerType == "openai_compatible" {
		if matrixMap(t, choice["function"], "tool_choice.function")["name"] != "read_file" {
			t.Fatalf("named OpenAI tool_choice lost: %#v", choice)
		}
		var system, toolResults int
		var parallelCalls bool
		for _, raw := range messages {
			msg := matrixMap(t, raw, "message")
			switch msg["role"] {
			case "system":
				if strings.Contains(fmt.Sprint(msg["content"]), "matrix system") {
					system++
				}
			case "tool":
				toolResults++
			case "assistant":
				if calls, ok := msg["tool_calls"].([]any); ok && len(calls) == 2 {
					parallelCalls = true
				}
			}
		}
		if system != 1 || toolResults != 2 || !parallelCalls {
			t.Fatalf("OpenAI history mismatch: system=%d tool_results=%d parallel=%v body=%#v", system, toolResults, parallelCalls, got)
		}
		return
	}

	if choice["type"] != "tool" || choice["name"] != "read_file" {
		t.Fatalf("named Anthropic tool_choice lost: %#v", choice)
	}
	if !strings.Contains(fmt.Sprint(got["system"]), "matrix system") {
		t.Fatalf("Anthropic system prompt lost: %#v", got["system"])
	}
	var toolUses, toolResults int
	for _, raw := range messages {
		msg := matrixMap(t, raw, "message")
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue // native Anthropic permits a plain string content value
		}
		for _, rawBlock := range blocks {
			block := matrixMap(t, rawBlock, "content block")
			switch block["type"] {
			case "tool_use":
				toolUses++
			case "tool_result":
				toolResults++
			}
		}
	}
	if toolUses != 2 || toolResults != 2 {
		t.Fatalf("Anthropic tool history mismatch: uses=%d results=%d body=%#v", toolUses, toolResults, got)
	}
}

func writeProtocolMatrixResponse(w http.ResponseWriter, providerType string) {
	w.Header().Set("Content-Type", "application/json")
	if providerType == "anthropic_compatible" {
		_, _ = io.WriteString(w, `{"id":"msg_matrix","type":"message","role":"assistant","content":[{"type":"text","text":"matrix answer"},{"type":"tool_use","id":"out-a","name":"read_file","input":{"path":"x"}},{"type":"tool_use","id":"out-b","name":"search","input":{"query":"y"}}],"model":"upstream-model","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":7}}`)
		return
	}
	_, _ = io.WriteString(w, `{"id":"chat_matrix","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"matrix answer","tool_calls":[{"id":"out-a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x\"}"}},{"id":"out-b","type":"function","function":{"name":"search","arguments":"{\"query\":\"y\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
}

func assertProtocolMatrixClientResponse(t *testing.T, ingress string, body []byte) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("client response is not JSON: %v body=%s", err, body)
	}
	if ingress == "anthropic" {
		if got["type"] != "message" || got["model"] != "client-model" || got["stop_reason"] != "tool_use" {
			t.Fatalf("Anthropic response metadata mismatch: %#v", got)
		}
		content := matrixSlice(t, got["content"], "content")
		var text, calls int
		for _, raw := range content {
			block := matrixMap(t, raw, "content block")
			if block["type"] == "text" && block["text"] == "matrix answer" {
				text++
			}
			if block["type"] == "tool_use" {
				calls++
			}
		}
		usage := matrixMap(t, got["usage"], "usage")
		if text != 1 || calls != 2 || usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) {
			t.Fatalf("Anthropic response content/usage mismatch: %#v", got)
		}
		return
	}

	choices := matrixSlice(t, got["choices"], "choices")
	choice := matrixMap(t, choices[0], "choice")
	message := matrixMap(t, choice["message"], "message")
	calls := matrixSlice(t, message["tool_calls"], "tool_calls")
	usage := matrixMap(t, got["usage"], "usage")
	if message["content"] != "matrix answer" || len(calls) != 2 || choice["finish_reason"] != "tool_calls" ||
		usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(7) {
		t.Fatalf("OpenAI response content/finish/usage mismatch: %#v", got)
	}
}

func TestProtocolMatrixE2ENonStreamingSemantics(t *testing.T) {
	for _, tc := range protocolMatrixPaths {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			type capturedRequest struct {
				path string
				body map[string]any
				err  error
			}
			captured := make(chan capturedRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var got map[string]any
				err := json.NewDecoder(r.Body).Decode(&got)
				captured <- capturedRequest{path: r.URL.Path, body: got, err: err}
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				writeProtocolMatrixResponse(w, tc.upstream)
			}))
			defer upstream.Close()

			gateway := testGateway(t, protocolMatrixConfig(upstream.URL, tc.upstream))
			path, body := protocolMatrixRequest(tc.ingress, false)
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls=%d want 1", calls.Load())
			}
			got := <-captured
			if got.err != nil {
				t.Fatalf("decode upstream request: %v", got.err)
			}
			if tc.upstream == "openai_compatible" && got.path != "/v1/chat/completions" {
				t.Fatalf("OpenAI upstream path=%q", got.path)
			}
			if tc.upstream == "anthropic_compatible" && got.path != "/v1/messages" {
				t.Fatalf("Anthropic upstream path=%q", got.path)
			}
			assertProtocolMatrixUpstreamRequest(t, tc.upstream, got.body)
			assertProtocolMatrixClientResponse(t, tc.ingress, rr.Body.Bytes())
		})
	}
}

func writeProtocolMatrixStream(w http.ResponseWriter, providerType string) {
	w.Header().Set("Content-Type", "text/event-stream")
	if providerType == "anthropic_compatible" {
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"upstream-model\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return
	}
	_, _ = io.WriteString(w, "data: {\"id\":\"chat_s\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
	_, _ = io.WriteString(w, "data: {\"id\":\"chat_s\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	_, _ = io.WriteString(w, "data: {\"id\":\"chat_s\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func TestProtocolMatrixE2EStreamingStopAndUsage(t *testing.T) {
	for _, tc := range protocolMatrixPaths {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode upstream stream request: %v", err)
				}
				if got["stream"] != true {
					t.Errorf("stream flag lost: %#v", got["stream"])
				}
				writeProtocolMatrixStream(w, tc.upstream)
			}))
			defer upstream.Close()

			gateway := testGateway(t, protocolMatrixConfig(upstream.URL, tc.upstream))
			path, body := protocolMatrixRequest(tc.ingress, true)
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
			rr := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			out := rr.Body.String()
			if tc.ingress == "anthropic" {
				for _, want := range []string{`"text":"hello"`, `"stop_reason":"end_turn"`, `"input_tokens":11`, `"output_tokens":7`, `"type":"message_stop"`} {
					if !strings.Contains(out, want) {
						t.Fatalf("Anthropic stream missing %s: %s", want, out)
					}
				}
			} else {
				for _, want := range []string{`"content":"hello"`, `"finish_reason":"stop"`, `"prompt_tokens":11`, `"completion_tokens":7`, "data: [DONE]"} {
					if !strings.Contains(out, want) {
						t.Fatalf("OpenAI stream missing %s: %s", want, out)
					}
				}
			}
		})
	}
}

func protocolMatrixSimpleRequest(ingress string) (string, string) {
	if ingress == "anthropic" {
		return "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	}
	return "/v1/chat/completions", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
}

func protocolErrorShapeMatchesIngress(t *testing.T, ingress string, body []byte) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("error is not JSON: %v body=%s", err, body)
	}
	if ingress == "anthropic" {
		if got["type"] != "error" {
			t.Fatalf("not an Anthropic error envelope: %#v", got)
		}
	}
	if _, ok := got["error"].(map[string]any); !ok {
		t.Fatalf("missing bounded error object: %#v", got)
	}
}

func TestProtocolMatrixE2EFailureSemantics(t *testing.T) {
	failures := []struct {
		name       string
		status     int
		malformed  bool
		retryAfter string
	}{
		{name: "auth_failure", status: http.StatusUnauthorized},
		{name: "quota_429_retry_after", status: http.StatusTooManyRequests, retryAfter: "2"},
		{name: "model_not_found", status: http.StatusNotFound},
		{name: "upstream_5xx", status: http.StatusServiceUnavailable},
		{name: "malformed_success", status: http.StatusOK, malformed: true},
	}
	for _, pathCase := range protocolMatrixPaths {
		for _, failure := range failures {
			t.Run(pathCase.name+"/"+failure.name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if failure.retryAfter != "" {
						w.Header().Set("Retry-After", failure.retryAfter)
					}
					w.WriteHeader(failure.status)
					if failure.malformed {
						_, _ = io.WriteString(w, `{}`)
					} else if pathCase.upstream == "anthropic_compatible" {
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"matrix failure"}}`)
					} else {
						_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"matrix failure"}}`)
					}
				}))
				defer upstream.Close()

				gateway := testGateway(t, protocolMatrixConfig(upstream.URL, pathCase.upstream))
				path, body := protocolMatrixSimpleRequest(pathCase.ingress)
				req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
				rr := httptest.NewRecorder()
				gateway.Handler().ServeHTTP(rr, req)
				wantStatus := failure.status
				if failure.malformed {
					wantStatus = http.StatusBadGateway
				}
				if rr.Code != wantStatus {
					t.Fatalf("status=%d want=%d body=%s", rr.Code, wantStatus, rr.Body.String())
				}
				if rr.Body.Len() > 4096 {
					t.Fatalf("error response is unbounded: %d bytes", rr.Body.Len())
				}
				protocolErrorShapeMatchesIngress(t, pathCase.ingress, rr.Body.Bytes())
				if failure.retryAfter != "" && rr.Header().Get("Retry-After") != failure.retryAfter {
					t.Fatalf("Retry-After=%q want %q", rr.Header().Get("Retry-After"), failure.retryAfter)
				}
			})
		}
	}
}

func TestProtocolMatrixE2EClientCancellation(t *testing.T) {
	for _, tc := range protocolMatrixPaths {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(entered)
				<-r.Context().Done()
				close(cancelled)
			}))
			defer upstream.Close()

			gateway := testGateway(t, protocolMatrixConfig(upstream.URL, tc.upstream))
			path, body := protocolMatrixRequest(tc.ingress, true)
			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body)).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				gateway.Handler().ServeHTTP(httptest.NewRecorder(), req)
				close(done)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("upstream was not entered")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("gateway did not return after client cancellation")
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("upstream streaming request was not cancelled")
			}
			if got := gateway.bus.Counts()["client_disconnect"]; got != 1 {
				t.Fatalf("client_disconnect events=%d want 1", got)
			}
		})
	}
}

func TestProtocolMatrixE2ERequestDeadline(t *testing.T) {
	for _, tc := range protocolMatrixPaths {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				<-release
			}))
			defer upstream.Close()
			defer close(release)
			cfg := protocolMatrixConfig(upstream.URL, tc.upstream)
			cfg.Routing.RequestTimeoutMS = 30
			gateway := testGateway(t, cfg)
			path, body := protocolMatrixSimpleRequest(tc.ingress)
			rr := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body)))
			if rr.Code != http.StatusGatewayTimeout {
				t.Fatalf("status=%d want 504 body=%s", rr.Code, rr.Body.String())
			}
			protocolErrorShapeMatchesIngress(t, tc.ingress, rr.Body.Bytes())
		})
	}
}

func TestProtocolMatrixRejectsUnsupportedCrossProtocolContent(t *testing.T) {
	cases := []struct {
		name, ingress, upstream, path, body string
	}{
		{
			name: "anthropic_unknown_block_to_openai", ingress: "anthropic", upstream: "openai_compatible", path: "/v1/messages",
			body: `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":[{"type":"server_tool_use","id":"x","name":"web_search","input":{}}]}]}`,
		},
		{
			name: "openai_audio_part_to_anthropic", ingress: "openai", upstream: "anthropic_compatible", path: "/v1/chat/completions",
			body: `{"model":"client-model","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer upstream.Close()
			gateway := testGateway(t, protocolMatrixConfig(upstream.URL, tc.upstream))
			rr := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway"+tc.path, strings.NewReader(tc.body)))
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("status=%d want 502 body=%s", rr.Code, rr.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatalf("lossy request reached upstream %d times", calls.Load())
			}
			if rr.Body.Len() > 1024 || !strings.Contains(rr.Body.String(), "unsupported protocol mapping") {
				t.Fatalf("mapping error is not explicit and bounded: len=%d body=%s", rr.Body.Len(), rr.Body.String())
			}
			protocolErrorShapeMatchesIngress(t, tc.ingress, rr.Body.Bytes())
		})
	}
}

func TestProtocolIngressClientAuthErrorsAreProtocolShaped(t *testing.T) {
	for _, ingress := range []string{"anthropic", "openai"} {
		t.Run(ingress, func(t *testing.T) {
			cfg := config.Default()
			cfg.Probe.Enabled = false
			cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"correct-key"}}
			gateway := testGateway(t, cfg)
			path, body := protocolMatrixSimpleRequest(ingress)
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer wrong-key")
			rr := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			protocolErrorShapeMatchesIngress(t, ingress, rr.Body.Bytes())
		})
	}
}
