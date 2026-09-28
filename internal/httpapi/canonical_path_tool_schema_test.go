package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

type fragmentedSSEBody struct {
	chunks []string
}

func newFragmentedSSEBody(stream string) *fragmentedSSEBody {
	const chunkSize = 7
	chunks := make([]string, 0, (len(stream)+chunkSize-1)/chunkSize)
	for len(stream) > 0 {
		n := chunkSize
		if n > len(stream) {
			n = len(stream)
		}
		chunks = append(chunks, stream[:n])
		stream = stream[n:]
	}
	return &fragmentedSSEBody{chunks: chunks}
}

func (b *fragmentedSSEBody) Read(p []byte) (int, error) {
	for len(b.chunks) > 0 && len(b.chunks[0]) == 0 {
		b.chunks = b.chunks[1:]
	}
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	b.chunks[0] = b.chunks[0][n:]
	return n, nil
}

func (b *fragmentedSSEBody) Close() error { return nil }

func sseFrame(eventName, data string) string {
	if eventName == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + eventName + "\ndata: " + data + "\n\n"
}

func quoteSSEString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func splitSSEArguments(args string) (string, string) {
	cut := len(args) / 2
	if cut == 0 {
		cut = len(args)
	}
	return args[:cut], args[cut:]
}

func malformedToolSSE(kind, toolName, args string) string {
	first, second := splitSSEArguments(args)
	switch kind {
	case "anthropic":
		return strings.Join([]string{
			sseFrame("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":%s,"input":{}}}`, quoteSSEString(toolName))),
			sseFrame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, quoteSSEString(first))),
			sseFrame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, quoteSSEString(second))),
			sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			sseFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`),
		}, "")
	case "openai_responses":
		return strings.Join([]string{
			sseFrame("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":%s}}`, quoteSSEString(toolName))),
			sseFrame("response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%s}`, quoteSSEString(first))),
			sseFrame("response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%s}`, quoteSSEString(second))),
			sseFrame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call"}}`),
			sseFrame("response.completed", `{"type":"response.completed","response":{"status":"completed","output":[]}}`),
		}, "")
	default:
		return strings.Join([]string{
			sseFrame("", fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`, quoteSSEString(toolName), quoteSSEString(first))),
			sseFrame("", fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}]}`, quoteSSEString(second))),
			sseFrame("", `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		}, "")
	}
}

func toolSchemaDefs() []canonical.ToolDef {
	return []canonical.ToolDef{
		{Name: "Bash", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
		{Name: "Read", Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)},
		{Name: "WriteFile", Parameters: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}`)},
	}
}

func TestCanonicalStreamPumpRejectsMalformedToolArgumentsAcrossMappings(t *testing.T) {
	mappings := []struct {
		name           string
		kind           string
		clientProtocol string
		failureMarker  string
		successMarker  string
		toolMarker     string
	}{
		{name: "openai-compatible", kind: "openai_chat", clientProtocol: "openai_chat", failureMarker: `"invalid_request_error"`, successMarker: "data: [DONE]", toolMarker: `"tool_calls"`},
		{name: "anthropic-compatible", kind: "anthropic", clientProtocol: "anthropic", failureMarker: "event: error", successMarker: "event: message_stop", toolMarker: "event: content_block_start"},
		{name: "responses", kind: "openai_responses", clientProtocol: "openai_responses", failureMarker: "event: response.failed", successMarker: "event: response.completed", toolMarker: "event: response.output_item.added"},
	}
	malformed := []struct {
		name   string
		tool   string
		args   string
		field  string
		actual string
	}{
		{name: "command object", tool: "Bash", args: `{"command":{}}`, field: "command", actual: "object"},
		{name: "file path array", tool: "Read", args: `{"file_path":[]}`, field: "file_path", actual: "array"},
		{name: "command number", tool: "Bash", args: `{"command":7}`, field: "command", actual: "integer"},
		{name: "command null", tool: "Bash", args: `{"command":null}`, field: "command", actual: "null"},
		{name: "custom schema object", tool: "WriteFile", args: `{"content":{}}`, field: "content", actual: "object"},
	}

	for _, mapping := range mappings {
		mapping := mapping
		t.Run(mapping.name, func(t *testing.T) {
			for _, tc := range malformed {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					body := newFragmentedSSEBody(malformedToolSSE(mapping.kind, tc.tool, tc.args))
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       body,
					}
					rr := httptest.NewRecorder()
					err := (&Server{}).canonicalStreamPump(rr, resp, mapping.kind, mapping.clientProtocol, "client-model", "req-schema", toolSchemaDefs(), nil)
					if err == nil {
						t.Fatal("expected streamed tool validation failure")
					}
					var validationErr *canonical.ToolCallValidationError
					if !errors.As(err, &validationErr) {
						t.Fatalf("expected ToolCallValidationError, got %T: %v", err, err)
					}
					if validationErr.Tool != tc.tool || validationErr.Field != tc.field || validationErr.ExpectedType != "string" || validationErr.ActualType != tc.actual {
						t.Fatalf("diagnostic=%+v, want tool=%s field=%s expected=string actual=%s", validationErr, tc.tool, tc.field, tc.actual)
					}
					if !validationErr.Streaming {
						t.Fatalf("expected streaming diagnostic, got %+v", validationErr)
					}
					out := rr.Body.String()
					if !strings.Contains(out, mapping.failureMarker) {
						t.Fatalf("missing structured stream error marker %q in %s", mapping.failureMarker, out)
					}
					if strings.Contains(out, mapping.successMarker) {
						t.Fatalf("stream emitted success terminal %q after validation failure: %s", mapping.successMarker, out)
					}
					if strings.Contains(out, mapping.toolMarker) {
						t.Fatalf("stream exposed invalid tool invocation before validation: %s", out)
					}
				})
			}
		})
	}
}

func TestCanonicalIngressesPassClientSchemasToResponseValidation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		if !strings.Contains(string(body), `"WriteFile"`) || !strings.Contains(string(body), `"content"`) {
			t.Errorf("canonical attempt lost client tool schema: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"upstream","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"WriteFile","arguments":"{\"content\":{}}"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	newGateway := func(t *testing.T) *Server {
		t.Helper()
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Routing.MaxAttempts = 1
		cfg.Providers = []config.ProviderConfig{{
			ID: "upstream", Name: "Canonical upstream", Type: "openai_responses", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{
				ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
				Capabilities: config.Capabilities{Tools: true, Streaming: true},
			}},
		}}
		s := testGateway(t, cfg)
		s.SyncCapabilityContracts()
		return s
	}

	writeFileSchema := `{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}`
	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "OpenAI Chat", path: "/v1/chat/completions",
			body: fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"write"}],"tools":[{"type":"function","function":{"name":"WriteFile","parameters":%s}}]}`, writeFileSchema),
		},
		{
			name: "Anthropic Messages", path: "/v1/messages",
			body: fmt.Sprintf(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"write"}],"tools":[{"name":"WriteFile","input_schema":%s}]}`, writeFileSchema),
		},
		{
			name: "OpenAI Responses", path: "/v1/responses",
			body: fmt.Sprintf(`{"model":"m","input":"write","tools":[{"type":"function","name":"WriteFile","parameters":%s}]}`, writeFileSchema),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := newGateway(t)
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+tc.path, strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("status=%d want=%d body=%s", rr.Code, http.StatusBadGateway, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), `"function_call"`) || strings.Contains(rr.Body.String(), `"content":{}`) {
				t.Fatalf("client received malformed successful tool call: %s", rr.Body.String())
			}
		})
	}
}

func TestCanonicalRepairKeepsClientToolSchemas(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			if !strings.Contains(string(body), `"temperature"`) {
				t.Errorf("initial canonical request omitted temperature: %s", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`))
			return
		}
		if strings.Contains(string(body), `"temperature"`) {
			t.Errorf("repair retry retained unsupported temperature: %s", body)
		}
		if !strings.Contains(string(body), `"WriteFile"`) {
			t.Errorf("repair retry lost client tool definition: %s", body)
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"upstream","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"WriteFile","arguments":"{\"content\":{}}"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "upstream", Name: "Canonical upstream", Type: "openai_responses", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Tools: true, Streaming: true},
		}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	request := `{"model":"m","input":"write","temperature":0.4,"tools":[{"type":"function","name":"WriteFile","parameters":{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}}]}`
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(request)))
	if calls != 2 {
		t.Fatalf("upstream calls=%d want repair retry count 2", calls)
	}
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, http.StatusBadGateway, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"function_call"`) {
		t.Fatalf("client received malformed successful tool call after repair: %s", rr.Body.String())
	}
}

func TestCanonicalPoisonedStreamFailsClosedAfterCommit(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":"partial"}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"WriteFile"}}`,
			``,
			`event: response.function_call_arguments.delta`,
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"content\":{}}"}`,
			``,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call"}}`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"status":"completed","output":[]}}`,
			``,
		}, "\n"))
	}))
	defer failing.Close()

	fallbackCalls := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fallback","model":"fallback","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"fallback output"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", Name: "Poisoned", Type: "openai_responses", BaseURL: failing.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-a", Enabled: true, Weight: 1, Priority: 0, Capabilities: config.Capabilities{Tools: true, Streaming: true}}}},
		{ID: "b", Name: "Fallback", Type: "openai_responses", BaseURL: fallback.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-b", Enabled: true, Weight: 1, Priority: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}}},
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"write"}],"tools":[{"type":"function","function":{"name":"WriteFile","parameters":{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}}}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Request-ID", "poisoned-stream")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, "partial") || !strings.Contains(out, `"invalid_request_error"`) {
		t.Fatalf("committed stream did not expose text then structured terminal error: %s", out)
	}
	if strings.Contains(out, "fallback output") || strings.Contains(out, "data: [DONE]") {
		t.Fatalf("mid-stream failure blended or completed a response: %s", out)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback upstream was called after client-visible bytes: %d", fallbackCalls)
	}
	for _, ev := range s.bus.SnapshotLimit(64) {
		if ev.RequestID == "poisoned-stream" && ev.Kind == "route_ok" {
			t.Fatalf("poisoned stream recorded route_ok: %+v", ev)
		}
	}
}
