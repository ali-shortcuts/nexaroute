package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func responsesStreamEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("invalid stream event %q: %v", payload, err)
		}
		out = append(out, obj)
	}
	return out
}

func responsesEventTypes(evs []map[string]any) []string {
	types := make([]string, 0, len(evs))
	for _, ev := range evs {
		s, _ := ev["type"].(string)
		types = append(types, s)
	}
	return types
}

func responsesTestConfig(upstreamURL, providerType string) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: providerType, BaseURL: upstreamURL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	return cfg
}

func postResponses(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://gateway/v1/responses", strings.NewReader(body))
	req.Header.Set("x-request-id", "test-stream-1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestResponsesStreamingFromOpenAIUpstream(t *testing.T) {
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	defer up.Close()
	s := testGateway(t, responsesTestConfig(up.URL, "openai_compatible"))
	rr := postResponses(t, s.Handler(), `{"model":"m","input":"Say hello","stream":true}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if dep := rr.Header().Get("X-Gateway-Deployment"); dep != "p/m" {
		t.Fatalf("deployment = %q", dep)
	}
	if gotBody["stream"] != true {
		t.Fatalf("upstream stream flag = %v", gotBody["stream"])
	}
	opts, _ := gotBody["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Fatalf("upstream stream_options = %v", gotBody["stream_options"])
	}
	evs := responsesStreamEvents(t, rr.Body.String())
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	if got := responsesEventTypes(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v", got)
	}
	if evs[4]["delta"] != "Hel" || evs[5]["delta"] != "lo" {
		t.Fatalf("deltas = %v %v", evs[4]["delta"], evs[5]["delta"])
	}
	done, _ := evs[6]["text"].(string)
	if done != "Hello" {
		t.Fatalf("done text = %q", done)
	}
	completed, _ := evs[9]["response"].(map[string]any)
	if completed["status"] != "completed" || completed["object"] != "response" {
		t.Fatalf("completed = %v", completed)
	}
	usage, _ := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(2) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", usage)
	}
	output, _ := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", completed["output"])
	}
}

func TestResponsesStreamingFromAnthropicUpstream(t *testing.T) {
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","usage":{"input_tokens":2,"output_tokens":0}}}`+"\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: message_delta\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`+"\n\n")
		_, _ = io.WriteString(w, "event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer up.Close()
	s := testGateway(t, responsesTestConfig(up.URL, "anthropic_compatible"))
	rr := postResponses(t, s.Handler(), `{"model":"m","input":"Say hello","instructions":"Be brief","stream":true}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotBody["model"] != "m" || gotBody["max_tokens"] != float64(4096) || gotBody["stream"] != true {
		t.Fatalf("upstream envelope = %v", gotBody)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("upstream messages missing: %v", gotBody)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "user" {
		t.Fatalf("first message = %v", m0)
	}
	if gotBody["system"] != "Be brief" {
		t.Fatalf("system = %v", gotBody["system"])
	}
	evs := responsesStreamEvents(t, rr.Body.String())
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	if got := responsesEventTypes(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v", got)
	}
	completed, _ := evs[len(evs)-1]["response"].(map[string]any)
	usage, _ := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(2) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestResponsesStreamingToolCalls(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	defer up.Close()
	s := testGateway(t, responsesTestConfig(up.URL, "openai_compatible"))
	rr := postResponses(t, s.Handler(), `{"model":"m","input":"weather?","stream":true,"tools":[{"type":"function","name":"get_weather"}]}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	evs := responsesStreamEvents(t, rr.Body.String())
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.output_item.done",
		"response.completed",
	}
	if got := responsesEventTypes(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v\nbody=%s", got, rr.Body.String())
	}
	added, _ := evs[2]["item"].(map[string]any)
	if added["type"] != "function_call" || added["name"] != "get_weather" || added["call_id"] != "call_1" {
		t.Fatalf("added item = %v", added)
	}
	doneItem, _ := evs[5]["item"].(map[string]any)
	if doneItem["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("done arguments = %v", doneItem["arguments"])
	}
	completed, _ := evs[6]["response"].(map[string]any)
	output, _ := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", completed["output"])
	}
	fn, _ := output[0].(map[string]any)
	if fn["type"] != "function_call" || fn["name"] != "get_weather" {
		t.Fatalf("function_call = %v", fn)
	}
}

func TestResponsesStreamUpstreamDiesPreCommit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 200 with an immediately closed body: no valid upstream event.
	}))
	defer up.Close()
	s := testGateway(t, responsesTestConfig(up.URL, "openai_compatible"))
	rr := postResponses(t, s.Handler(), `{"model":"m","input":"hi","stream":true}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "response.created") {
		t.Fatalf("must not commit before the first upstream event: %s", rr.Body.String())
	}
}

func TestResponsesStreamMidStreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: {oops-invalid\n\n")
	}))
	defer up.Close()
	s := testGateway(t, responsesTestConfig(up.URL, "openai_compatible"))
	rr := postResponses(t, s.Handler(), `{"model":"m","input":"hi","stream":true}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "response.created") || !strings.Contains(body, "response.failed") {
		t.Fatalf("expected created + failed events: %s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("must not complete after a mid-stream error: %s", body)
	}
}

func TestResponsesNonStreamingAnthropicUpstream(t *testing.T) {
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":5}}`)
	}))
	defer up.Close()
	s := testGateway(t, responsesTestConfig(up.URL, "anthropic_compatible"))
	rr := postResponses(t, s.Handler(), `{"model":"m","input":"Say hello","instructions":"Be brief"}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotBody["model"] != "m" || gotBody["max_tokens"] != float64(4096) {
		t.Fatalf("upstream envelope = %v", gotBody)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "response" {
		t.Fatalf("object = %v", out["object"])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(5) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestResponsesStreamOptionsRetry(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		if n == 1 {
			if _, ok := obj["stream_options"]; !ok {
				t.Errorf("first attempt should carry stream_options")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"Unrecognized request argument supplied: stream_options"}}`)
			return
		}
		if _, ok := obj["stream_options"]; ok {
			t.Errorf("retry should strip stream_options")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	s := testGateway(t, responsesTestConfig(up.URL, "openai_compatible"))
	rr := postResponses(t, s.Handler(), `{"model":"m","input":"hi","stream":true}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if !strings.Contains(rr.Body.String(), "response.completed") {
		t.Fatalf("body = %s", rr.Body.String())
	}
}
