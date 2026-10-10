package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

func coverageOpenAIGateway(t *testing.T, protocol string, upstream http.HandlerFunc) *Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Providers = []config.ProviderConfig{{
		ID: "coverage", Name: "Coverage", Type: protocol, BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "coverage-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true},
		}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func coverageOpenAIRequest(s *Server, method, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestCoverageOpenAIMethodAndRequiredSchema(t *testing.T) {
	s := coverageOpenAIGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		if got["tool_choice"] == nil {
			t.Errorf("tool_choice was not forwarded: %#v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})

	if rr := coverageOpenAIRequest(s, http.MethodGet, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d body=%s; want 405", rr.Code, rr.Body.String())
	}
	if rr := coverageOpenAIRequest(s, http.MethodPost, `{"model":"  ","messages":[{"role":"user","content":"hi"}]}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("blank model status=%d body=%s; want 400", rr.Code, rr.Body.String())
	}
	for _, choice := range []string{`"required"`, `{"type":"function","function":{"name":"lookup"}}`} {
		body := `{"model":"m","messages":[{"role":"user","content":"use the tool"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":` + choice + `}`
		rr := coverageOpenAIRequest(s, http.MethodPost, body)
		if rr.Code != http.StatusOK {
			t.Fatalf("tool_choice=%s status=%d body=%s", choice, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `"finish_reason":"tool_calls"`) {
			t.Fatalf("tool completion semantics lost for choice %s: %s", choice, rr.Body.String())
		}
	}
}

func TestCoverageOpenAICanonicalGeminiJSONAndMalformedOutput(t *testing.T) {
	valid := coverageOpenAIGateway(t, "gemini", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"canonical hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`)
	})
	rr := coverageOpenAIRequest(valid, http.MethodPost, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"content":"canonical hello"`) || !strings.Contains(rr.Body.String(), `"prompt_tokens":5`) {
		t.Fatalf("canonical Gemini response status=%d body=%s", rr.Code, rr.Body.String())
	}

	malformed := coverageOpenAIGateway(t, "gemini", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":7}]}}]}`)
	})
	bad := coverageOpenAIRequest(malformed, http.MethodPost, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if bad.Code != http.StatusBadGateway || !strings.Contains(bad.Body.String(), "invalid response") {
		t.Fatalf("malformed canonical response status=%d body=%s", bad.Code, bad.Body.String())
	}
}

func TestCoverageOpenAICanonicalStreamProtocolFailureStaysPrecommit(t *testing.T) {
	s := coverageOpenAIGateway(t, "gemini", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {not-json}\n\n")
	})
	rr := coverageOpenAIRequest(s, http.MethodPost, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "invalid response") {
		t.Fatalf("canonical stream failure status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "data:") {
		t.Fatalf("pre-commit stream failure leaked a client event: %s", rr.Body.String())
	}
}

func TestCoverageOpenAIFailureClassificationAndRetiredModel(t *testing.T) {
	capability := coverageOpenAIGateway(t, "openai_compatible", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"unsupported parameter: mystery is not supported","param":"mystery"}}`)
	})
	capabilityResp := coverageOpenAIRequest(capability, http.MethodPost, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if capabilityResp.Code != http.StatusBadRequest || !strings.Contains(capabilityResp.Body.String(), "mystery") {
		t.Fatalf("capability rejection status=%d body=%s", capabilityResp.Code, capabilityResp.Body.String())
	}

	retired := coverageOpenAIGateway(t, "openai_compatible", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"error":{"code":"model_retired","message":"model retired"}}`)
	})
	retiredResp := coverageOpenAIRequest(retired, http.MethodPost, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if retiredResp.Code != http.StatusServiceUnavailable || !strings.Contains(retiredResp.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("retired model status=%d body=%s", retiredResp.Code, retiredResp.Body.String())
	}
	foundRetirement := false
	for _, ev := range retired.bus.Snapshot() {
		if ev.Kind == "model_retired" {
			foundRetirement = true
		}
	}
	if !foundRetirement {
		t.Fatal("retired upstream model did not emit a model_retired event")
	}
}

func TestCoverageOpenAIAnthropicStreamToolUsageAndErrorSemantics(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":4}}}`,
		``,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"lookup","input":{"city":"Paris"}}}`,
		``,
		`data: {"type":"content_block_stop","index":2}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":6,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	var usageCalls, input, output int
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}
	err := streamAnthropicToOpenAIWithUsage(rr, resp, "client-model", translate.NewAnthropicNameMap([]string{"lookup"}), func(i, o int) {
		usageCalls++
		input, output = i, o
	}, "coverage-stream")
	if err != nil {
		t.Fatalf("tool/usage stream: %v", err)
	}
	out := rr.Body.String()
	for _, want := range []string{`"name":"lookup"`, `"arguments":"{\"city\":\"Paris\"}"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":6`, `"completion_tokens":3`, `"cached_tokens":2`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q: %s", want, out)
		}
	}
	if !strings.Contains(out, `"id":"call_`) {
		t.Errorf("missing synthesized tool-call id: %s", out)
	}
	if usageCalls != 1 || input != 6 || output != 3 {
		t.Fatalf("usage hook calls=%d input=%d output=%d, want 1,6,3", usageCalls, input, output)
	}

	errorBody := "data: {\"type\":\"error\",\"error\":{\"message\":\"upstream broke\"}}\n\n"
	errRecorder := httptest.NewRecorder()
	errResp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(errorBody))}
	if err := streamAnthropicToOpenAIWithUsage(errRecorder, errResp, "m", nil, nil); err == nil || !strings.Contains(err.Error(), "anthropic stream error") {
		t.Fatalf("upstream error event returned %v", err)
	}
	if !strings.Contains(errRecorder.Body.String(), `"error"`) || strings.Contains(errRecorder.Body.String(), "[DONE]") {
		t.Fatalf("error event must be forwarded without a success tail: %s", errRecorder.Body.String())
	}
}

func TestCoverageOpenAINumberIntJSONNumber(t *testing.T) {
	if got, ok := numberInt(json.Number("42")); !ok || got != 42 {
		t.Fatalf("numberInt(json.Number)=(%d,%t), want (42,true)", got, ok)
	}
	if _, ok := numberInt(json.Number("not-an-int")); ok {
		t.Fatal("invalid json.Number unexpectedly parsed")
	}
}
