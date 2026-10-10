package compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type coverageCompatProbeHTTPTransport struct {
	url    string
	client *http.Client
}

func coverageCompatProbeNewHTTPTransport(t *testing.T, handler http.HandlerFunc) *coverageCompatProbeHTTPTransport {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &coverageCompatProbeHTTPTransport{url: server.URL, client: server.Client()}
}

func (p *coverageCompatProbeHTTPTransport) Do(ctx context.Context, payload []byte, stream bool, _ http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("X-Probe-Stream", "true")
	}
	return p.client.Do(req)
}

func (p *coverageCompatProbeHTTPTransport) RedactBody(body []byte) []byte {
	return []byte(strings.ReplaceAll(string(body), "coverage-secret", "[redacted]"))
}

type coverageCompatProbeFailSecondTransport struct {
	base  ProbeTransport
	calls int
}

func (p *coverageCompatProbeFailSecondTransport) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	p.calls++
	if p.calls == 2 {
		return nil, context.DeadlineExceeded
	}
	return p.base.Do(ctx, payload, stream, forward)
}

func (p *coverageCompatProbeFailSecondTransport) RedactBody(body []byte) []byte {
	return p.base.RedactBody(body)
}

func coverageCompatProbeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestCoverageCompatProbeResponsesCapabilityMatrix(t *testing.T) {
	var calls atomic.Int32
	transport := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		index := int(calls.Add(1)) - 1
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("invalid request JSON: %v", err)
			return
		}
		if _, hasMessages := req["messages"]; hasMessages {
			t.Errorf("request %d used Chat Completions messages instead of Responses input", index)
		}
		if req["input"] == nil {
			t.Errorf("request %d had no Responses input", index)
		}
		switch index {
		case 2:
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"delta\":\"ok\"}\n\n")
		case 3:
			coverageCompatProbeJSON(w, http.StatusOK, `{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"get_time","arguments":"{}"}]}`)
		case 4:
			coverageCompatProbeJSON(w, http.StatusBadRequest, `{"error":{"message":"maximum context length exceeded"}}`)
		case 5:
			coverageCompatProbeJSON(w, http.StatusOK, `{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"}]}`)
		case 7:
			coverageCompatProbeJSON(w, http.StatusInternalServerError, `{"error":{"message":"temporary upstream failure"}}`)
		case 8:
			coverageCompatProbeJSON(w, http.StatusBadRequest, `{"error":{"message":"maximum context length exceeded"}}`)
		case 10:
			coverageCompatProbeJSON(w, http.StatusOK, `{malformed response`)
		case 11:
			coverageCompatProbeJSON(w, http.StatusOK, `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":" {\"ok\":true} "}]}],"usage":{"input_tokens":3,"output_tokens":2}}`)
		default:
			coverageCompatProbeJSON(w, http.StatusOK, `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`)
		}
	})

	report := RunCapabilitySuiteResponses(context.Background(), transport, "dep-responses", "model-r")
	if calls.Load() != 13 || len(report.Outcomes) != 13 {
		t.Fatalf("requests=%d outcomes=%d, want 13 each", calls.Load(), len(report.Outcomes))
	}
	want := []Support{Supported, Supported, Supported, Supported, UnknownSupport, UnknownSupport, Supported, UnknownSupport, UnknownSupport, Supported, UnknownSupport, Supported, Supported}
	for i, outcome := range report.Outcomes {
		if outcome.Verdict != want[i] {
			t.Errorf("outcome[%d] %s verdict=%v want %v (detail %q)", i, outcome.Capability, outcome.Verdict, want[i], outcome.Detail)
		}
	}
	if report.Dialect != "openai_responses" || report.Level != "capability" || report.Passed != 8 || report.Failed != 0 || report.Inconclusive != 5 || report.TransportFail != 2 || report.OK {
		t.Fatalf("unexpected Responses report summary: %+v", report)
	}
	if !strings.Contains(report.Outcomes[10].Detail, "malformed response") {
		t.Errorf("malformed Responses payload detail=%q", report.Outcomes[10].Detail)
	}
}

func TestCoverageCompatProbeAnthropicCapabilityMatrix(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var captured [][]byte
	transport := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		mu.Lock()
		captured = append(captured, append([]byte(nil), body...))
		mu.Unlock()
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("invalid Anthropic request: %v", err)
			return
		}
		if req["max_tokens"] == nil || req["model"] != "claude-test" {
			t.Errorf("missing Anthropic request fields: %s", body)
		}
		if _, hasTools := req["tools"]; hasTools {
			tools, _ := req["tools"].([]any)
			if len(tools) != 1 || tools[0].(map[string]any)["input_schema"] == nil {
				t.Errorf("Anthropic tool schema not encoded: %s", body)
			}
		}
		index := int(calls.Add(1)) - 1
		if r.Header.Get("X-Probe-Stream") == "true" {
			w.Header().Set("Content-Type", "TEXT/EVENT-STREAM; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"delta\":{\"text\":\"ok\"}}\n\n")
			return
		}
		if index == 3 || index == 4 {
			coverageCompatProbeJSON(w, http.StatusOK, `{"content":[{"type":"tool_use","id":"call-1","name":"get_weather","input":{"city":"Paris"}}]}`)
			return
		}
		coverageCompatProbeJSON(w, http.StatusOK, `{"content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	})

	report := RunCapabilitySuiteAnthropic(context.Background(), transport, "dep-anthropic", "claude-test")
	if calls.Load() != 9 || len(report.Outcomes) != 9 {
		t.Fatalf("requests=%d outcomes=%d, want 9 each", calls.Load(), len(report.Outcomes))
	}
	if !report.OK || report.Passed != 7 || report.Failed != 0 || report.Inconclusive != 2 || report.TransportFail != 0 {
		t.Fatalf("Anthropic suite did not verify all capabilities: %+v", report)
	}
	for i, outcome := range report.Outcomes {
		want := Supported
		if i == 3 || i == 4 {
			// The Anthropic verdict adapter currently verifies text only; tool
			// blocks in the successful response remain inconclusive.
			want = UnknownSupport
		}
		if outcome.Verdict != want {
			t.Errorf("Anthropic outcome[%d] %s=%v detail=%q", i, outcome.Capability, outcome.Verdict, outcome.Detail)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 9 {
		t.Fatalf("captured %d requests", len(captured))
	}
	var thinking, vision, toolChoice bool
	for _, raw := range captured {
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		thinking = thinking || req["thinking"] != nil
		vision = vision || strings.Contains(string(raw), `"type":"image"`)
		toolChoice = toolChoice || req["tool_choice"] != nil
	}
	if !thinking || !vision || !toolChoice {
		t.Errorf("Anthropic-specific requests absent: thinking=%v vision=%v tool_choice=%v", thinking, vision, toolChoice)
	}
}

func TestCoverageCompatProbeAnthropicFailuresAndVerdicts(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		contentType string
		want        Support
		wantErr     bool
	}{
		{name: "capability rejection", status: 400, body: `{"error":{"message":"temperature is not supported"}}`, want: Unsupported},
		{name: "context limit inconclusive", status: 400, body: `{"error":{"message":"maximum context length exceeded"}}`, want: UnknownSupport},
		{name: "caller error is surfaced", status: 401, body: `{"error":{"message":"invalid API key"}}`, want: UnknownSupport, wantErr: true},
		{name: "provider failure", status: 503, body: `{"error":{"message":"temporary unavailable"}}`, want: UnknownSupport, wantErr: true},
		{name: "stream without SSE", status: 200, body: `{"content":[]}`, contentType: "application/json", want: UnknownSupport},
		{name: "stream with SSE", status: 200, body: "event: ping\ndata: {}\n\n", contentType: "text/event-stream", want: Supported},
		{name: "unparseable Anthropic response", status: 200, body: `{broken`, contentType: "application/json", want: UnknownSupport},
		{name: "no text content", status: 200, body: `{"content":[{"type":"tool_use"}]}`, contentType: "application/json", want: UnknownSupport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			verdict := func(body []byte, _ ToolExpectation) (Support, string) {
				var parsed struct {
					Content []map[string]any `json:"content"`
				}
				if err := json.Unmarshal(body, &parsed); err != nil {
					return UnknownSupport, "unparseable anthropic body"
				}
				for _, part := range parsed.Content {
					if part["type"] == "text" && part["text"] != "" {
						return Supported, "text found"
					}
				}
				return UnknownSupport, "no text"
			}
			out, err := runProbeAnthropic(context.Background(), transport, []byte(`{"model":"m"}`), tc.name == "stream without SSE" || tc.name == "stream with SSE", CapTemperature, verdict)
			if (err != nil) != tc.wantErr || out.Verdict != tc.want {
				t.Fatalf("outcome=%+v err=%v, want verdict=%v err=%v", out, err, tc.want, tc.wantErr)
			}
		})
	}

	transport := coverageCompatProbeNewHTTPTransport(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(250 * time.Millisecond):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	out, err := runProbeAnthropic(ctx, transport, []byte(`{"model":"m"}`), false, CapText, func([]byte, ToolExpectation) (Support, string) {
		return Supported, "unexpected response"
	})
	if err == nil || out.Verdict != UnknownSupport || !strings.Contains(out.Detail, "transport:") {
		t.Fatalf("deadline should be inconclusive transport error: outcome=%+v err=%v", out, err)
	}
}

func TestCoverageCompatProbeRunProbeBranchesAndRedaction(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		contentType string
		stream      bool
		expect      ToolExpectation
		want        Support
		wantErr     bool
	}{
		{name: "matching unsupported parameter", status: 400, body: `{"error":{"message":"temperature is not supported"}}`, want: Unsupported},
		{name: "context overflow", status: 400, body: `{"error":{"message":"maximum context length exceeded"}}`, want: UnknownSupport},
		{name: "caller rejection is surfaced", status: 401, body: `{"error":{"message":"invalid API key"}}`, want: UnknownSupport, wantErr: true},
		{name: "server failure", status: 500, body: `{"error":{"message":"secret coverage-secret"}}`, want: UnknownSupport, wantErr: true},
		{name: "stream wrong content type", status: 200, body: `{}`, contentType: "application/json", stream: true, want: UnknownSupport},
		{name: "stream headers verified", status: 200, body: "event: ping\ndata: {}\n\n", contentType: "text/event-stream", stream: true, want: Supported},
		{name: "malformed completion", status: 200, body: `{broken`, contentType: "application/json", want: UnknownSupport, wantErr: true},
		{name: "plain generation", status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`, contentType: "application/json", expect: ExpectText, want: Supported},
		{name: "tool absent", status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`, contentType: "application/json", expect: ExpectToolCall, want: UnknownSupport},
		{name: "one parallel tool", status: 200, body: `{"choices":[{"message":{"tool_calls":[{}]}}]}`, contentType: "application/json", expect: ExpectParallelToolCalls, want: UnknownSupport},
		{name: "no parallel tools", status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`, contentType: "application/json", expect: ExpectParallelToolCalls, want: UnknownSupport},
		{name: "valid JSON object", status: 200, body: `{"choices":[{"message":{"content":" [1,2] "}}]}`, contentType: "application/json", expect: ExpectJSON, want: Supported},
		{name: "non JSON text", status: 200, body: `{"choices":[{"message":{"content":"plain"}}]}`, contentType: "application/json", expect: ExpectJSON, want: UnknownSupport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			out, err := runProbe(context.Background(), transport, []byte(`{"model":"m"}`), tc.stream, CapTemperature, tc.expect)
			if (err != nil) != tc.wantErr || out.Verdict != tc.want {
				t.Fatalf("outcome=%+v err=%v, want verdict=%v err=%v", out, err, tc.want, tc.wantErr)
			}
		})
	}

	transport := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		coverageCompatProbeJSON(w, http.StatusOK, `{"id":"r","status":"completed","output":[]}`)
	})
	got := string((responsesProbeTransport{base: transport}).RedactBody([]byte("echo coverage-secret remains")))
	if got != "echo [redacted] remains" || strings.Contains(got, "coverage-secret") {
		t.Fatalf("Responses adapter failed to delegate redaction: %q", got)
	}
}

func TestCoverageCompatProbeAgentLoopsOverHTTP(t *testing.T) {
	for _, responses := range []bool{false, true} {
		name := "openai-chat"
		if responses {
			name = "openai-responses"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var requests [][]byte
			transport := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
					return
				}
				mu.Lock()
				requests = append(requests, append([]byte(nil), body...))
				mu.Unlock()
				index := calls.Add(1)
				if responses {
					var req map[string]any
					if err := json.Unmarshal(body, &req); err != nil || req["input"] == nil || req["messages"] != nil {
						t.Errorf("not a Responses request: %s (err=%v)", body, err)
					}
					if index == 1 {
						coverageCompatProbeJSON(w, http.StatusOK, `{"id":"r1","status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}]}`)
					} else {
						coverageCompatProbeJSON(w, http.StatusOK, `{"id":"r2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Sunny in Paris"}]}],"usage":{"input_tokens":4,"output_tokens":2}}`)
					}
					return
				}
				if index == 1 {
					coverageCompatProbeJSON(w, http.StatusOK, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}}]}`)
				} else {
					coverageCompatProbeJSON(w, http.StatusOK, `{"choices":[{"message":{"content":"Sunny in Paris"}}]}`)
				}
			})
			var report ProbeReport
			if responses {
				report = RunAgentLoopSimulationResponses(context.Background(), transport, "dep", "model-r")
			} else {
				report = RunAgentLoopSimulation(context.Background(), transport, "dep", "model-chat")
			}
			if !report.OK || len(report.AgentSteps) != 2 || !report.AgentSteps[0].Passed || !report.AgentSteps[1].Passed || calls.Load() != 2 {
				t.Fatalf("agent loop failed: report=%+v requests=%d", report, calls.Load())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 2 {
				t.Fatalf("captured %d agent requests", len(requests))
			}
			if responses {
				var req map[string]any
				if err := json.Unmarshal(requests[1], &req); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, raw := range req["input"].([]any) {
					item, _ := raw.(map[string]any)
					if item["type"] == "function_call_output" && item["call_id"] == "call-1" {
						found = true
					}
				}
				if !found {
					t.Fatalf("Responses continuation omitted function result: %s", requests[1])
				}
			} else if !strings.Contains(string(requests[1]), "tool_call_id") {
				t.Fatalf("Chat continuation omitted tool result: %s", requests[1])
			}
		})
	}
}

func TestCoverageCompatProbeAgentLoopFailureStages(t *testing.T) {
	cases := []struct {
		name      string
		responses []struct {
			status int
			body   string
		}
		wantSteps int
		wantOK    bool
	}{
		{name: "first HTTP error", responses: []struct {
			status int
			body   string
		}{{400, `{"error":{"message":"bad request"}}`}}, wantSteps: 1},
		{name: "no tool call", responses: []struct {
			status int
			body   string
		}{{200, `{"choices":[{"message":{"content":"plain answer"}}]}`}}, wantSteps: 1},
		{name: "bad tool-call JSON", responses: []struct {
			status int
			body   string
		}{{200, `{`}}, wantSteps: 1},
		{name: "continuation HTTP error", responses: []struct {
			status int
			body   string
		}{{200, `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]}}]}`}, {500, `{"error":{"message":"failed"}}`}}, wantSteps: 2},
		{name: "bad continuation JSON", responses: []struct {
			status int
			body   string
		}{{200, `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]}}]}`}, {200, `{`}}, wantSteps: 2},
		{name: "empty final answer", responses: []struct {
			status int
			body   string
		}{{200, `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]}}]}`}, {200, `{"choices":[{"message":{"content":"  "}}]}`}}, wantSteps: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			transport := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(tc.responses) {
					t.Errorf("unexpected request %d", index)
					coverageCompatProbeJSON(w, http.StatusInternalServerError, `{"error":{"message":"unexpected"}}`)
					return
				}
				response := tc.responses[index]
				coverageCompatProbeJSON(w, response.status, response.body)
			})
			report := RunAgentLoopSimulation(context.Background(), transport, "dep", "model")
			if report.OK != tc.wantOK || len(report.AgentSteps) != tc.wantSteps {
				t.Fatalf("unexpected failed-loop report: %+v", report)
			}
			if tc.wantSteps == 2 && report.AgentSteps[1].Passed {
				t.Fatalf("continuation should fail for %q: %+v", tc.name, report)
			}
		})
	}

	base := coverageCompatProbeNewHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		coverageCompatProbeJSON(w, http.StatusOK, `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]}}]}`)
	})
	transport := &coverageCompatProbeFailSecondTransport{base: base}
	report := RunAgentLoopSimulation(context.Background(), transport, "dep", "model")
	if report.OK || len(report.AgentSteps) != 2 || !strings.Contains(report.AgentSteps[1].Detail, context.DeadlineExceeded.Error()) {
		t.Fatalf("second-step transport failure not reported: %+v", report)
	}
}
