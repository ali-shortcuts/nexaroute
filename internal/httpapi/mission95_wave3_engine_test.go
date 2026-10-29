package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// Wave-3 coverage tests: canonical stream pump, stream translators, repair
// engine, decision events, task-pool switching, evaluation persistence,
// config application, and admin provider/evaluation endpoints.

type wave3ErrorReader struct{ err error }

func (r wave3ErrorReader) Read([]byte) (int, error) { return 0, r.err }

func wave3PumpResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func wave3Float(v float64) *float64 { return &v }

func TestWave3CanonicalPumpDoubleStop(t *testing.T) {
	s := wave3Gateway(t, nil, nil)
	oneStop := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	threeStops := oneStop +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	run := func(body string) (string, canonical.Usage, error) {
		rr := httptest.NewRecorder()
		var got canonical.Usage
		err := s.canonicalStreamPump(rr, wave3PumpResp(body), "anthropic", "anthropic", "m", "wave3-pump", nil, func(u canonical.Usage) { got = u })
		return rr.Body.String(), got, err
	}
	out1, usage1, err1 := run(oneStop)
	if err1 != nil {
		t.Fatalf("one-stop pump err=%v", err1)
	}
	out3, usage3, err3 := run(threeStops)
	if err3 != nil {
		t.Fatalf("three-stop pump err=%v", err3)
	}
	// The second and third terminal frames must be dropped: the event
	// sequences are identical, and the usage hook still reports the
	// delta usage. The pump rewrites message_start with a fresh
	// generated id per run, so ids are normalized before comparing.
	wave3MsgID := regexp.MustCompile(`"id":"msg_[^"]*"`)
	out1, out3 = wave3MsgID.ReplaceAllString(out1, `"id":"msg_X"`), wave3MsgID.ReplaceAllString(out3, `"id":"msg_X"`)
	if out1 != out3 {
		t.Fatalf("double-stop outputs differ:\none=%q\nthree=%q", out1, out3)
	}
	if usage1.InputTokens != 3 || usage1.OutputTokens != 5 {
		t.Fatalf("one-stop usage=%+v want 3/5", usage1)
	}
	if usage3 != usage1 {
		t.Fatalf("three-stop usage=%+v want %+v", usage3, usage1)
	}
	if !strings.Contains(out1, "message_stop") {
		t.Fatalf("pump output missing terminal event: %q", out1)
	}
}

func TestWave3CanonicalPumpToolAfterTerminalDropped(t *testing.T) {
	s := wave3Gateway(t, nil, nil)
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"evil_tool\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	rr := httptest.NewRecorder()
	if err := s.canonicalStreamPump(rr, wave3PumpResp(body), "anthropic", "anthropic", "m", "wave3-pump-tool", nil, nil); err != nil {
		t.Fatalf("tool-after-terminal pump err=%v", err)
	}
	if strings.Contains(rr.Body.String(), "evil_tool") {
		t.Fatalf("tool injected after terminal leaked: %q", rr.Body.String())
	}
}

func TestWave3StreamOpenAIToAnthropic(t *testing.T) {
	nm := translate.NewAnthropicNameMap(nil)
	// Normal translation with usage.
	{
		rr := httptest.NewRecorder()
		var gotP, gotC int
		err := streamOpenAIToAnthropicWithUsage(rr, wave3PumpResp(wave3ChatSSE), "m", nm, func(p, c int) { gotP, gotC = p, c }, "wave3-tr")
		if err != nil {
			t.Fatalf("normal translation err=%v", err)
		}
		if gotP != 3 || gotC != 5 {
			t.Fatalf("usage sink=%d/%d want 3/5", gotP, gotC)
		}
		if !strings.Contains(rr.Body.String(), "ok") || !strings.Contains(rr.Body.String(), "message_stop") {
			t.Fatalf("translated body missing text/stop: %q", rr.Body.String())
		}
	}
	// Tool-count cap: 129 distinct tool calls exceed maxTranslatedStreamTools.
	{
		var b strings.Builder
		for i := 0; i < 129; i++ {
			fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"function\":{\"name\":\"t%d\",\"arguments\":\"\"}}]}}]}\n\n", i, i)
		}
		rr := httptest.NewRecorder()
		if err := streamOpenAIToAnthropicWithUsage(rr, wave3PumpResp(b.String()), "m", nm, func(int, int) {}, "wave3-tr"); err == nil {
			t.Fatalf("129 tools: expected error, got nil")
		}
	}
	// Tool-index cap: index 4097 exceeds maxTranslatedToolIndex.
	{
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":4097,\"function\":{\"name\":\"t\",\"arguments\":\"\"}}]}}]}\n\n"
		rr := httptest.NewRecorder()
		if err := streamOpenAIToAnthropicWithUsage(rr, wave3PumpResp(body), "m", nm, func(int, int) {}, "wave3-tr"); err == nil {
			t.Fatalf("tool index 4097: expected error, got nil")
		}
	}
	// Tool-name byte cap: name longer than 1024 bytes.
	{
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"" + strings.Repeat("n", 1025) + "\",\"arguments\":\"\"}}]}}]}\n\n"
		rr := httptest.NewRecorder()
		if err := streamOpenAIToAnthropicWithUsage(rr, wave3PumpResp(body), "m", nm, func(int, int) {}, "wave3-tr"); err == nil {
			t.Fatalf("oversize tool name: expected error, got nil")
		}
	}
	// Args-delta byte cap: cumulative arguments exceed 1 MiB.
	{
		var b strings.Builder
		b.WriteString("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"t\",\"arguments\":\"\"}}]}}]}\n\n")
		chunk := strings.Repeat("a", 1<<16)
		for i := 0; i < 65; i++ {
			fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%q}}]}}]}\n\n", chunk)
		}
		rr := httptest.NewRecorder()
		if err := streamOpenAIToAnthropicWithUsage(rr, wave3PumpResp(b.String()), "m", nm, func(int, int) {}, "wave3-tr"); err == nil {
			t.Fatalf("oversize tool args: expected error, got nil")
		}
	}
	// Malformed chunk JSON.
	{
		rr := httptest.NewRecorder()
		if err := streamOpenAIToAnthropicWithUsage(rr, wave3PumpResp("data: {broken\n\n"), "m", nm, func(int, int) {}, "wave3-tr"); err == nil {
			t.Fatalf("malformed chunk: expected error, got nil")
		}
	}
	// Failing writer: the first emit fails.
	{
		w := newWave3FailWriter(0)
		if err := streamOpenAIToAnthropicWithUsage(w, wave3PumpResp(wave3ChatSSE), "m", nm, func(int, int) {}, "wave3-tr"); err == nil {
			t.Fatalf("failing writer: expected error, got nil")
		}
	}
	// Reader error.
	{
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(wave3ErrorReader{err: errors.New("wave3 read boom")})}
		rr := httptest.NewRecorder()
		if err := streamOpenAIToAnthropicWithUsage(rr, resp, "m", nm, func(int, int) {}, "wave3-tr"); err == nil {
			t.Fatalf("reader error: expected error, got nil")
		}
	}
}

func TestWave3StreamAnthropicToOpenAI(t *testing.T) {
	nm := translate.NewOpenAINameMap(nil)
	// Normal translation with usage.
	{
		rr := httptest.NewRecorder()
		var gotP, gotC int
		err := streamAnthropicToOpenAIWithUsage(rr, wave3PumpResp(wave3AnthropicSSE), "m", nm, func(u usage.Counts) { gotP, gotC = int(u.Input), int(u.Output) }, "wave3-tr2")
		if err != nil {
			t.Fatalf("normal translation err=%v", err)
		}
		if gotP != 3 || gotC != 5 {
			t.Fatalf("usage sink=%d/%d want 3/5", gotP, gotC)
		}
		if !strings.Contains(rr.Body.String(), "ok") {
			t.Fatalf("translated body missing text: %q", rr.Body.String())
		}
	}
	// Tool use translation.
	{
		body := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Oslo\\\"}\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		rr := httptest.NewRecorder()
		if err := streamAnthropicToOpenAIWithUsage(rr, wave3PumpResp(body), "m", nm, func(usage.Counts) {}, "wave3-tr2"); err != nil {
			t.Fatalf("tool translation err=%v", err)
		}
		if !strings.Contains(rr.Body.String(), "tool_calls") || !strings.Contains(rr.Body.String(), "get_weather") {
			t.Fatalf("tool translation body missing tool_calls: %q", rr.Body.String())
		}
	}
	// Malformed event data.
	{
		rr := httptest.NewRecorder()
		if err := streamAnthropicToOpenAIWithUsage(rr, wave3PumpResp("event: message_start\ndata: {broken\n\n"), "m", nm, func(usage.Counts) {}, "wave3-tr2"); err == nil {
			t.Fatalf("malformed event: expected error, got nil")
		}
	}
	// Failing writer.
	{
		w := newWave3FailWriter(0)
		if err := streamAnthropicToOpenAIWithUsage(w, wave3PumpResp(wave3AnthropicSSE), "m", nm, func(usage.Counts) {}, "wave3-tr2"); err == nil {
			t.Fatalf("failing writer: expected error, got nil")
		}
	}
	// Reader error.
	{
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(wave3ErrorReader{err: errors.New("wave3 read boom")})}
		rr := httptest.NewRecorder()
		if err := streamAnthropicToOpenAIWithUsage(rr, resp, "m", nm, func(usage.Counts) {}, "wave3-tr2"); err == nil {
			t.Fatalf("reader error: expected error, got nil")
		}
	}
}

func wave3RepairAdapter(t *testing.T, s *Server) providers.Adapter {
	t.Helper()
	a, ok := s.reg.Get("a")
	if !ok {
		t.Fatal("adapter for provider a missing")
	}
	return a
}

func TestWave3DoUpstreamWithRepair(t *testing.T) {
	ctx := context.Background()
	// Repair success: temperature dropped, canonical request mirrored.
	{
		var bodies [][]byte
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, b)
			w.Header().Set("Content-Type", "application/json")
			if len(bodies) == 1 {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
				return
			}
			_, _ = io.WriteString(w, wave3ChatOK)
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		a := wave3RepairAdapter(t, s)
		dep := router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}
		canReq := &canonical.Request{Temperature: wave3Float(0.7)}
		resp, payload, out, err := s.doUpstreamWithRepair(ctx, "wave3-repair", a, dep, []byte(`{"model":"upstream-model","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`), "", false, nil, s.dialectFor("a", "openai_compatible", ""), compat.RequirementProfile{}, 2, canReq)
		if err != nil {
			t.Fatalf("repair success err=%v", err)
		}
		if resp == nil || resp.StatusCode != 200 {
			t.Fatalf("repair success status=%v want 200", resp)
		}
		if !out.Repaired {
			t.Fatalf("repair success out.Repaired=false")
		}
		if strings.Contains(string(payload), "temperature") {
			t.Fatalf("repaired payload still has temperature: %s", payload)
		}
		if canReq.Temperature != nil {
			t.Fatalf("canonical temperature not mirrored to nil")
		}
		if len(bodies) != 2 {
			t.Fatalf("repair success calls=%d want 2", len(bodies))
		}
		_ = resp.Body.Close()
	}
	// maxRepairs clamp: -1 behaves as 0 (no repair attempt).
	{
		var calls int
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		a := wave3RepairAdapter(t, s)
		dep := router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}
		resp, _, out, err := s.doUpstreamWithRepair(ctx, "wave3-repair", a, dep, []byte(`{"model":"upstream-model","temperature":0.7}`), "", false, nil, s.dialectFor("a", "openai_compatible", ""), compat.RequirementProfile{}, -1, nil)
		if err != nil || resp == nil || resp.StatusCode != 400 || out.Repaired || calls != 1 {
			t.Fatalf("clamp -1: err=%v status=%v repaired=%v calls=%d want 400/false/1", err, resp, out.Repaired, calls)
		}
		_ = resp.Body.Close()
	}
	// maxRepairs clamp: 99 behaves as 2 (two repairs, then terminal 400).
	{
		var calls int
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			switch calls {
			case 1:
				_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
			case 2:
				_, _ = io.WriteString(w, `{"error":{"message":"top_p is not supported by this model","type":"invalid_request_error"}}`)
			default:
				_, _ = io.WriteString(w, `{"error":{"message":"stop is not supported by this model","type":"invalid_request_error"}}`)
			}
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		a := wave3RepairAdapter(t, s)
		dep := router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}
		resp, _, out, err := s.doUpstreamWithRepair(ctx, "wave3-repair", a, dep, []byte(`{"model":"upstream-model","temperature":0.7,"top_p":0.9,"stop":["x"]}`), "", false, nil, s.dialectFor("a", "openai_compatible", ""), compat.RequirementProfile{}, 99, nil)
		if err != nil || resp == nil || resp.StatusCode != 400 || !out.Repaired || calls != 3 {
			t.Fatalf("clamp 99: err=%v status=%v repaired=%v calls=%d want 400/true/3", err, resp, out.Repaired, calls)
		}
		_ = resp.Body.Close()
	}
	// Non-capability 400 passes through untouched.
	{
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"bad request, nope","type":"invalid_request_error"}}`)
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		a := wave3RepairAdapter(t, s)
		dep := router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}
		resp, _, out, err := s.doUpstreamWithRepair(ctx, "wave3-repair", a, dep, []byte(`{"model":"upstream-model"}`), "", false, nil, s.dialectFor("a", "openai_compatible", ""), compat.RequirementProfile{}, 2, nil)
		if err != nil || resp == nil || resp.StatusCode != 400 || out.Repaired {
			t.Fatalf("non-capability: err=%v status=%v repaired=%v want 400/false", err, resp, out.Repaired)
		}
		_ = resp.Body.Close()
	}
	// Transport error on the first dispatch returns the error.
	{
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadURL := dead.URL
		dead.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		a := wave3RepairAdapter(t, s)
		dep := router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}
		resp, _, _, err := s.doUpstreamWithRepair(ctx, "wave3-repair", a, dep, []byte(`{"model":"upstream-model"}`), "", false, nil, s.dialectFor("a", "openai_compatible", ""), compat.RequirementProfile{}, 2, nil)
		if err == nil || resp != nil {
			t.Fatalf("transport error: err=%v resp=%v want err non-nil resp nil", err, resp)
		}
	}
	// Read error on the error body returns the response with restored body.
	{
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"partial`)
			// Flush so the partial bytes actually reach the client
			// before the connection is cut; without it the server's
			// buffer discards them on hijack and the read yields "".
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		a := wave3RepairAdapter(t, s)
		dep := router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}
		resp, _, out, err := s.doUpstreamWithRepair(ctx, "wave3-repair", a, dep, []byte(`{"model":"upstream-model"}`), "", false, nil, s.dialectFor("a", "openai_compatible", ""), compat.RequirementProfile{}, 2, nil)
		if err != nil || resp == nil || resp.StatusCode != 400 || out.Repaired {
			t.Fatalf("read error: err=%v status=%v repaired=%v want 400/false", err, resp, out.Repaired)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(b), "partial") {
			t.Fatalf("read error restored body=%q want partial prefix", b)
		}
	}
	// Proactive sanitizer: a known-unsupported field is stripped pre-dispatch.
	{
		var bodies [][]byte
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, wave3ChatOK)
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) {
			cfg.Routing.Strategy = "priority"
			cfg.Routing.SanitizeEnabled = true
		})
		s.capStore.LearnUnsupported("a/m", compat.CapTemperature, compat.SourceRuntime, "wave3 seed", "wave3-key")
		a := wave3RepairAdapter(t, s)
		dep := router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}
		resp, payload, _, err := s.doUpstreamWithRepair(ctx, "wave3-repair", a, dep, []byte(`{"model":"upstream-model","temperature":0.7}`), "", false, nil, s.dialectFor("a", "openai_compatible", ""), compat.RequirementProfile{}, 2, nil)
		if err != nil || resp == nil || resp.StatusCode != 200 {
			t.Fatalf("sanitize: err=%v status=%v want 200", err, resp)
		}
		_ = resp.Body.Close()
		if len(bodies) != 1 || strings.Contains(string(bodies[0]), "temperature") {
			t.Fatalf("sanitize bodies=%v want single call without temperature", bodies)
		}
		if strings.Contains(string(payload), "temperature") {
			t.Fatalf("sanitize returned payload still has temperature: %s", payload)
		}
		if !wave3BusHasKind(s, "compat_sanitize") {
			t.Fatalf("compat_sanitize event missing")
		}
	}
}

func TestWave3MaybeRepairUpstream(t *testing.T) {
	ctx := context.Background()
	mkBundle := func(s *Server) hedgeAttemptBundle {
		a := wave3RepairAdapter(t, s)
		return hedgeAttemptBundle{
			c: router.Scored{Deployment: router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "upstream-model"}},
			a: a,
		}
	}
	mkResp := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
	}
	// Nil response passes through.
	{
		// The adapter is never dispatched on this path, but the
		// bundle still needs a registered provider a to build.
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", "http://127.0.0.1:1", "m", "upstream-model", nil),
		}, nil)
		resp, _, out := s.maybeRepairUpstream(ctx, "wave3-maybe", mkBundle(s), []byte(`{}`), nil, false, nil, 2, compat.RequirementProfile{})
		if resp != nil || out.Repaired {
			t.Fatalf("nil resp: resp=%v repaired=%v want nil/false", resp, out.Repaired)
		}
	}
	// 200 passes through untouched.
	{
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", "http://127.0.0.1:1", "m", "upstream-model", nil),
		}, nil)
		resp, payload, out := s.maybeRepairUpstream(ctx, "wave3-maybe", mkBundle(s), []byte(`{"a":1}`), mkResp(200, wave3ChatOK), false, nil, 2, compat.RequirementProfile{})
		if resp == nil || resp.StatusCode != 200 || out.Repaired || string(payload) != `{"a":1}` {
			t.Fatalf("200 passthrough: status=%v repaired=%v payload=%s", resp, out.Repaired, payload)
		}
		_ = resp.Body.Close()
	}
	// Repair success on the legacy path.
	{
		var calls int
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, wave3ChatOK)
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		resp, payload, out := s.maybeRepairUpstream(ctx, "wave3-maybe", mkBundle(s), []byte(`{"model":"upstream-model","temperature":0.7}`), mkResp(400, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`), false, nil, 2, compat.RequirementProfile{})
		if resp == nil || resp.StatusCode != 200 || !out.Repaired || calls != 1 {
			t.Fatalf("legacy repair: status=%v repaired=%v calls=%d want 200/true/1", resp, out.Repaired, calls)
		}
		if strings.Contains(string(payload), "temperature") {
			t.Fatalf("legacy repaired payload still has temperature: %s", payload)
		}
		_ = resp.Body.Close()
	}
	// Repair-retry transport failure returns nil response with Repaired set.
	{
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		resp, _, out := s.maybeRepairUpstream(ctx, "wave3-maybe", mkBundle(s), []byte(`{"model":"upstream-model","temperature":0.7}`), mkResp(400, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`), false, nil, 2, compat.RequirementProfile{})
		if resp != nil || !out.Repaired {
			t.Fatalf("repair transport failure: resp=%v repaired=%v want nil/true", resp, out.Repaired)
		}
	}
	// Unrepairable capability failure learns the fact and returns the error.
	{
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, wave3ChatOK)
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		// No temperature in the payload, so there is nothing to drop: the
		// classifier still fires on the message but Repair finds no rule.
		resp, _, out := s.maybeRepairUpstream(ctx, "wave3-maybe", mkBundle(s), []byte(`{"model":"upstream-model"}`), mkResp(400, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`), false, nil, 2, compat.RequirementProfile{})
		if resp == nil || resp.StatusCode != 400 || out.Repaired {
			t.Fatalf("unrepairable: status=%v repaired=%v want 400/false", resp, out.Repaired)
		}
		contract := s.capStore.Get("a/m")
		if contract.Capabilities.Temperature != compat.Unsupported {
			t.Fatalf("unrepairable contract temperature=%v want unsupported", contract.Capabilities.Temperature)
		}
		_ = resp.Body.Close()
	}
	// 5xx passes through (only 4xx is repair-eligible).
	{
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", "http://127.0.0.1:1", "m", "upstream-model", nil),
		}, nil)
		resp, _, out := s.maybeRepairUpstream(ctx, "wave3-maybe", mkBundle(s), []byte(`{}`), mkResp(500, `{"error":"boom"}`), false, nil, 2, compat.RequirementProfile{})
		if resp == nil || resp.StatusCode != 500 || out.Repaired {
			t.Fatalf("5xx passthrough: status=%v repaired=%v want 500/false", resp, out.Repaired)
		}
		_ = resp.Body.Close()
	}
}

func TestWave3EmitDecisionEvent(t *testing.T) {
	last := func(s *Server) (kind, breakdown string, evIdx int) {
		snap := s.bus.Snapshot()
		if len(snap) == 0 {
			t.Fatal("no bus events")
		}
		ev := snap[len(snap)-1]
		return ev.Kind, ev.DecisionBreakdown, len(snap) - 1
	}
	// Result-policy breakdown wins over trace-policy breakdown.
	{
		s := wave3Gateway(t, nil, nil)
		s.emitDecisionEvent("wave3-dec", decision.DecisionResult{
			Action: decision.ActionSelect, SelectedID: "a/m", ProviderID: "a", Confidence: 0.9,
			PolicyTrace: &decision.PolicyTrace{PolicyID: "pol-result", PolicyVersion: "v1", SelectedBreakdown: map[string]float64{"latency": 0.5}},
		}, decision.DecisionTrace{PolicyTrace: &decision.PolicyTrace{PolicyID: "pol-trace"}}, nil, 2, false)
		kind, breakdown, _ := last(s)
		if kind != "decision_ok" {
			t.Fatalf("result-policy kind=%q want decision_ok", kind)
		}
		if !strings.Contains(breakdown, "pol-result") || strings.Contains(breakdown, "pol-trace") {
			t.Fatalf("result-policy breakdown=%q want pol-result only", breakdown)
		}
		ev := s.bus.Snapshot()[len(s.bus.Snapshot())-1]
		if ev.DecisionPolicyID != "pol-result" || ev.DecisionPolicyVersion != "v1" {
			t.Fatalf("result-policy fields=%q/%q want pol-result/v1", ev.DecisionPolicyID, ev.DecisionPolicyVersion)
		}
	}
	// Trace-policy breakdown used when the result has none.
	{
		s := wave3Gateway(t, nil, nil)
		s.emitDecisionEvent("wave3-dec", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "a/m", ProviderID: "a"},
			decision.DecisionTrace{PolicyTrace: &decision.PolicyTrace{PolicyID: "pol-trace", TaskType: "coding", SelectedScore: 0.8}}, nil, 1, false)
		kind, breakdown, _ := last(s)
		if kind != "decision_ok" || !strings.Contains(breakdown, "pol-trace") {
			t.Fatalf("trace-policy kind=%q breakdown=%q want decision_ok with pol-trace", kind, breakdown)
		}
		ev := s.bus.Snapshot()[len(s.bus.Snapshot())-1]
		if ev.DecisionTaskType != "coding" || ev.DecisionSelectedScore != 0.8 {
			t.Fatalf("trace-policy fields task=%q score=%v want coding/0.8", ev.DecisionTaskType, ev.DecisionSelectedScore)
		}
	}
	// Oversize breakdown maps fail closed to an empty breakdown string.
	{
		s := wave3Gateway(t, nil, nil)
		big := map[string]float64{}
		for i := 0; i < 200; i++ {
			big[strings.Repeat("k", 100)+fmt.Sprint(i)] = 0.5
		}
		s.emitDecisionEvent("wave3-dec", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "a/m",
			PolicyTrace: &decision.PolicyTrace{PolicyID: "pol-big", SelectedBreakdown: big}}, decision.DecisionTrace{}, nil, 1, false)
		_, breakdown, _ := last(s)
		// jsonMarshalBounded renders an oversize breakdown as an
		// empty object, not an empty string.
		if breakdown != "{}" {
			t.Fatalf("oversize breakdown=%q want {}", breakdown)
		}
	}
	// Kind table: abstain, rejected, fail, timeout.
	{
		cases := []struct {
			name   string
			result decision.DecisionResult
			want   string
		}{
			{"abstain", decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true, ReasonCodes: []decision.ReasonCode{decision.ReasonAbstained}}, "decision_abstain"},
			{"abstain-offmode", decision.DecisionResult{Action: decision.ActionAbstain, ReasonCodes: []decision.ReasonCode{decision.ReasonOffMode}}, "decision_abstain"},
			{"rejected-invalid", decision.DecisionResult{Action: decision.ActionSelect, ReasonCodes: []decision.ReasonCode{decision.ReasonInvalidResult}}, "decision_rejected"},
			{"rejected-validation", decision.DecisionResult{Action: decision.ActionSelect, ReasonCodes: []decision.ReasonCode{decision.ReasonValidationFailed}}, "decision_rejected"},
			{"fail-provider", decision.DecisionResult{Action: decision.ActionSelect, ReasonCodes: []decision.ReasonCode{decision.ReasonProviderError}}, "decision_fail"},
			{"fail-budget", decision.DecisionResult{Action: decision.ActionSelect, ReasonCodes: []decision.ReasonCode{decision.ReasonBudgetExceeded}}, "decision_fail"},
			{"timeout", decision.DecisionResult{Action: decision.ActionSelect, ReasonCodes: []decision.ReasonCode{decision.ReasonTimeout}}, "decision_timeout"},
		}
		for _, tc := range cases {
			s := wave3Gateway(t, nil, nil)
			s.emitDecisionEvent("wave3-dec", tc.result, decision.DecisionTrace{}, nil, 1, false)
			kind, _, _ := last(s)
			if kind != tc.want {
				t.Fatalf("%s kind=%q want %q", tc.name, kind, tc.want)
			}
		}
	}
	// Chain outcome overrides.
	{
		cases := []struct {
			name    string
			outcome string
			action  decision.Action
			want    string
		}{
			{"chain-budget", decision.ChainOutcomeBudgetExhausted, decision.ActionSelect, "decision_fail"},
			{"chain-deadline", decision.ChainOutcomeDeadlineExhausted, decision.ActionSelect, "decision_timeout"},
			{"chain-affinity-select", decision.ChainOutcomeAffinityPreserved, decision.ActionSelect, "decision_ok"},
			{"chain-affinity-abstain", decision.ChainOutcomeAffinityPreserved, decision.ActionAbstain, "decision_abstain"},
			{"chain-exhausted", decision.ChainOutcomeExhausted, decision.ActionSelect, "decision_abstain"},
			{"chain-selected", decision.ChainOutcomeSelected, decision.ActionSelect, "decision_ok"},
		}
		for _, tc := range cases {
			s := wave3Gateway(t, nil, nil)
			s.emitDecisionEvent("wave3-dec", decision.DecisionResult{Action: tc.action, SelectedID: "a/m"},
				decision.DecisionTrace{ChainTrace: &decision.ChainTrace{ChainID: "chain-1", Outcome: tc.outcome, CallsUsed: 2, SelectedProviderID: "a"}}, nil, 1, false)
			kind, _, _ := last(s)
			if kind != tc.want {
				t.Fatalf("%s kind=%q want %q", tc.name, kind, tc.want)
			}
		}
	}
	// Shadow mode maps ok/abstain to decision_shadow but keeps failures.
	{
		s := wave3Gateway(t, nil, nil)
		s.emitDecisionEvent("wave3-dec", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "a/m"}, decision.DecisionTrace{}, nil, 1, true)
		kind, _, _ := last(s)
		if kind != "decision_shadow" {
			t.Fatalf("shadow ok kind=%q want decision_shadow", kind)
		}
		s.emitDecisionEvent("wave3-dec", decision.DecisionResult{Action: decision.ActionSelect, ReasonCodes: []decision.ReasonCode{decision.ReasonProviderError}}, decision.DecisionTrace{}, nil, 1, true)
		kind, _, _ = last(s)
		if kind != "decision_fail" {
			t.Fatalf("shadow fail kind=%q want decision_fail", kind)
		}
	}
	// Chain provider fallback + resolved route fields.
	{
		s := wave3Gateway(t, nil, nil)
		s.emitDecisionEvent("wave3-dec", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "a/m", ProviderID: "prov-a"},
			decision.DecisionTrace{ChainTrace: &decision.ChainTrace{ChainID: "chain-9", Outcome: decision.ChainOutcomeSelected, SelectedProviderID: ""}},
			&route.ResolvedRoute{VirtualEndpointID: "ve-1", PublicModel: "pub", RouteProfileID: "rp", PrimaryPoolID: "pool"}, 3, false)
		ev := s.bus.Snapshot()[len(s.bus.Snapshot())-1]
		if ev.DecisionSelectedProvider != "prov-a" {
			t.Fatalf("chain provider fallback=%q want prov-a", ev.DecisionSelectedProvider)
		}
		if ev.VirtualEndpoint != "ve-1" || ev.PublicModel != "pub" || ev.RouteProfile != "rp" || ev.Pool != "pool" {
			t.Fatalf("route fields ve=%q pub=%q rp=%q pool=%q", ev.VirtualEndpoint, ev.PublicModel, ev.RouteProfile, ev.Pool)
		}
		if ev.DecisionChainID != "chain-9" || ev.DecisionCandidateCount != 3 {
			t.Fatalf("chain fields id=%q count=%d want chain-9/3", ev.DecisionChainID, ev.DecisionCandidateCount)
		}
	}
}

func TestWave3SwitchTaskPool(t *testing.T) {
	mkGateway := func() *Server {
		return wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", "http://127.0.0.1:1", "m", "upstream-model", []string{"cm"}),
			wave3Provider("b", "openai_compatible", "http://127.0.0.1:2", "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) {
			cfg.Routing.Strategy = "priority"
			cfg.CandidatePools = []config.CandidatePoolConfig{
				{ID: "pool-a", Mode: "explicit", Deployments: []string{"a/m"}},
				{ID: "pool-b", Mode: "explicit", Deployments: []string{"b/m"}},
			}
			cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp", CandidatePool: "pool-a"}}
		})
	}
	base := &route.ResolvedRoute{
		Pools: map[string]config.CandidatePoolConfig{
			"pool-a": {ID: "pool-a", Name: "A", Mode: "explicit"},
			"pool-b": {ID: "pool-b", Name: "B", Mode: "explicit"},
		},
		OrderedPoolIDs: []string{"pool-a"},
		PrimaryPoolID:  "pool-a",
	}
	// Nil resolver.
	{
		s := mkGateway()
		s.routeResolver = nil
		if _, _, ok := s.switchTaskPool(router.Requirement{}, s.currentConfig(), base, "pool-b"); ok {
			t.Fatalf("nil resolver switch unexpectedly succeeded")
		}
	}
	// Target pool absent from the resolved route.
	{
		s := mkGateway()
		if _, _, ok := s.switchTaskPool(router.Requirement{}, s.currentConfig(), base, "pool-missing"); ok {
			t.Fatalf("missing pool switch unexpectedly succeeded")
		}
	}
	// Target pool present in the route but not expanded by the resolver.
	{
		s := mkGateway()
		resolved := *base
		resolved.Pools = map[string]config.CandidatePoolConfig{
			"pool-a": {ID: "pool-a", Mode: "explicit"},
			"pool-x": {ID: "pool-x", Mode: "explicit"},
		}
		if _, _, ok := s.switchTaskPool(router.Requirement{}, s.currentConfig(), &resolved, "pool-x"); ok {
			t.Fatalf("unexpanded pool switch unexpectedly succeeded")
		}
	}
	// Success: target first, original fallbacks deduped, candidates re-filtered.
	{
		s := mkGateway()
		switched, scored, ok := s.switchTaskPool(router.Requirement{}, s.currentConfig(), base, "pool-b")
		if !ok {
			t.Fatalf("pool switch failed")
		}
		if switched.PrimaryPoolID != "pool-b" || switched.PrimaryPoolName != "B" || switched.PrimaryMode != "explicit" {
			t.Fatalf("switched route=%+v want pool-b/B/explicit", switched)
		}
		if len(switched.OrderedPoolIDs) == 0 || switched.OrderedPoolIDs[0] != "pool-b" {
			t.Fatalf("switched order=%v want pool-b first", switched.OrderedPoolIDs)
		}
		found := false
		for _, c := range scored {
			if c.Deployment.ID == "b/m" {
				found = true
			}
		}
		if !found {
			t.Fatalf("switched candidates=%v want b/m present", scored)
		}
	}
}

func TestWave3EvaluationPersist(t *testing.T) {
	newPlane := func(statePath string) *evaluationPlane {
		return newEvaluationPlane(config.EvaluationConfig{Enabled: true, StatePath: statePath}, nil)
	}
	mkRun := func(id string) eval.Result {
		return eval.Result{
			RunID: id, SuiteID: "coding", DeploymentID: "a/m",
			Score: 1.0, Samples: 1,
			Counts: map[eval.Verdict]int{eval.VerdictPass: 1},
			Cases:  []eval.CaseResult{{CaseID: "c1", Verdict: eval.VerdictPass, Weight: 1}},
		}
	}
	// Empty path is a no-op.
	{
		p := newPlane("")
		if err := p.persist(); err != nil {
			t.Fatalf("empty path persist err=%v want nil", err)
		}
	}
	// Success round-trip with mode 0600 and version 1.
	{
		dir := t.TempDir()
		path := dir + "/state.json"
		p := newPlane(path)
		if err := p.Store().Save(mkRun("run-1")); err != nil {
			t.Fatalf("store save err=%v", err)
		}
		if err := p.persist(); err != nil {
			t.Fatalf("persist err=%v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("state file missing: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("state file mode=%o want 0600", fi.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("state read err=%v", err)
		}
		var doc struct {
			Version int           `json:"version"`
			Runs    []eval.Result `json:"runs"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("state JSON err=%v", err)
		}
		if doc.Version != 1 || len(doc.Runs) != 1 || doc.Runs[0].RunID != "run-1" {
			t.Fatalf("state doc version=%d runs=%v want 1/[run-1]", doc.Version, doc.Runs)
		}
	}
	// Trim: many fat runs collapse to a fitting subset, oldest dropped.
	{
		dir := t.TempDir()
		path := dir + "/state.json"
		p := newPlane(path)
		fat := func(id string) eval.Result {
			r := eval.Result{RunID: id, SuiteID: "coding", DeploymentID: "a/m", Score: 1.0}
			for i := 0; i < 512; i++ {
				verdicts := make([]eval.VerdictResult, 0, 8)
				for j := 0; j < 8; j++ {
					verdicts = append(verdicts, eval.VerdictResult{EvaluatorID: strings.Repeat("e", 100), Reason: strings.Repeat("r", 256), Verdict: eval.VerdictPass, Kind: eval.KindDeterministic})
				}
				r.Cases = append(r.Cases, eval.CaseResult{CaseID: strings.Repeat("c", 100), Verdict: eval.VerdictPass, Weight: 1, ErrorType: strings.Repeat("x", 256), Verdicts: verdicts})
			}
			r.Counts = map[eval.Verdict]int{eval.VerdictPass: len(r.Cases)}
			r.Samples = len(r.Cases)
			return r
		}
		for i := 0; i < 6; i++ {
			if err := p.Store().Save(fat(fmt.Sprintf("run-%d", i))); err != nil {
				t.Fatalf("fat save %d err=%v", i, err)
			}
		}
		if err := p.persist(); err != nil {
			t.Fatalf("trim persist err=%v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("trim state read err=%v", err)
		}
		var doc struct {
			Runs []eval.Result `json:"runs"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("trim state JSON err=%v", err)
		}
		if len(doc.Runs) >= 6 || len(doc.Runs) < 1 {
			t.Fatalf("trim runs=%d want 1..5", len(doc.Runs))
		}
		if doc.Runs[0].RunID == "run-0" {
			t.Fatalf("trim kept oldest run-0")
		}
		if doc.Runs[len(doc.Runs)-1].RunID != "run-5" {
			t.Fatalf("trim last run=%q want run-5", doc.Runs[len(doc.Runs)-1].RunID)
		}
	}
	// MkdirAll failure: state path under a regular file.
	{
		dir := t.TempDir()
		blocker := dir + "/blocker"
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatalf("blocker write err=%v", err)
		}
		p := newPlane(blocker + "/state.json")
		if err := p.Store().Save(mkRun("run-1")); err != nil {
			t.Fatalf("store save err=%v", err)
		}
		if err := p.persist(); err == nil {
			t.Fatalf("mkdir-failure persist unexpectedly succeeded")
		}
		// persistBestEffort records the failure for observability.
		p.persistBestEffort()
		if p.stateWritesFailed.Load() != 1 {
			t.Fatalf("stateWritesFailed=%d want 1", p.stateWritesFailed.Load())
		}
	}
}

func TestWave3ApplyConfig(t *testing.T) {
	// First-Validate failure: duplicate provider id; old config retained.
	{
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", "http://127.0.0.1:1", "m", "upstream-model", []string{"cm"}),
		}, nil)
		bad := s.currentConfig()
		bad.Providers = append(bad.Providers, wave3Provider("a", "openai_compatible", "http://127.0.0.1:2", "m", "upstream-model", nil))
		if err := s.applyConfig(bad); err == nil {
			t.Fatalf("duplicate provider apply unexpectedly succeeded")
		}
		if len(s.currentConfig().Providers) != 1 {
			t.Fatalf("duplicate provider apply mutated config: %d providers", len(s.currentConfig().Providers))
		}
	}
	// Env-override failure: invalid boolean for the local-only flag.
	// Subtest scope: t.Setenv restores only at (sub)test end, so a
	// plain block would leak the bad value into the later cases.
	t.Run("bad-env-bool", func(t *testing.T) {
		s := wave3Gateway(t, nil, nil)
		t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "not-a-bool")
		if err := s.applyConfig(s.currentConfig()); err == nil {
			t.Fatalf("bad env boolean apply unexpectedly succeeded")
		}
	})
	// Post-env Validate failure: malformed client base URL from the env.
	t.Run("bad-env-base-url", func(t *testing.T) {
		s := wave3Gateway(t, nil, nil)
		t.Setenv("NEXAROUTE_CLIENT_BASE_URL", "://bad")
		if err := s.applyConfig(s.currentConfig()); err == nil {
			t.Fatalf("bad client base URL apply unexpectedly succeeded")
		}
	})
	// SaveAtomic failure: config path points at a directory.
	{
		s := wave3Gateway(t, nil, nil)
		dir := t.TempDir()
		s.configPath = dir
		if err := s.applyConfig(s.currentConfig()); err == nil {
			t.Fatalf("directory config path apply unexpectedly succeeded")
		}
	}
	// Success: a benign routing change lands.
	{
		s := wave3Gateway(t, nil, nil)
		next := s.currentConfig()
		next.Routing.Strategy = "priority"
		if err := s.applyConfig(next); err != nil {
			t.Fatalf("apply success err=%v", err)
		}
		if s.currentConfig().Routing.Strategy != "priority" {
			t.Fatalf("apply success strategy=%q want priority", s.currentConfig().Routing.Strategy)
		}
	}
}

// ---- admin: provider-test ----

func wave3ProviderFormJSON(baseURL, mode string) string {
	m := ""
	if mode != "" {
		m = `,"mode":"` + mode + `"`
	}
	return `{"provider":{"id":"p","name":"p","type":"openai_compatible","base_url":"` + baseURL + `","auth_mode":"none","enabled":true,"models":[{"model":"upstream-model","enabled":true}]}` + m + `}`
}

func TestWave3AdminProviderTestProbe(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3ChatOK, nil)
	defer up.Close()
	s := wave3Gateway(t, nil, nil)
	rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", wave3ProviderFormJSON(up.URL, ""))
	if rr.Code != 200 {
		t.Fatalf("provider probe status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	out := wave3JSON(t, rr)
	if out["ok"] != true || out["passed"] != float64(1) || out["total"] != float64(1) {
		t.Fatalf("provider probe body=%s want ok=true passed=1 total=1", rr.Body.String())
	}
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("provider probe results=%v want 1", results)
	}
	res, _ := results[0].(map[string]any)
	if res["ok"] != true || res["model"] != "upstream-model" {
		t.Fatalf("provider probe result=%v want ok=true model=upstream-model", res)
	}
}

func TestWave3AdminProviderTestGuards(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3ChatOK, nil)
	defer up.Close()
	s := wave3Gateway(t, nil, nil)
	// GET on an admin write path is denied 403 by the admin-authz
	// layer before the handler's own method check (same as the
	// existing coverage_admin_routes_test.go GET expectation).
	if rr := wave3AdminRequest(s, "GET", "/admin/api/provider-test", ""); rr.Code != 403 {
		t.Fatalf("GET provider-test status=%d want 403", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", "{bad"); rr.Code != 400 {
		t.Fatalf("invalid JSON provider-test status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", `{"provider":{"type":"openai_compatible"}}`); rr.Code != 400 {
		t.Fatalf("missing id/base_url provider-test status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", `{"provider":{"id":"p","base_url":"http://127.0.0.1:1","type":"nope"}}`); rr.Code != 400 {
		t.Fatalf("unsupported type provider-test status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", `{"provider":{"id":"p","base_url":"http://127.0.0.1:1","type":"openai_compatible","models":[]}}`); rr.Code != 400 {
		t.Fatalf("no models provider-test status=%d want 400", rr.Code)
	}
	big := strings.Repeat("m", 1025)
	if rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", `{"provider":{"id":"p","base_url":"http://127.0.0.1:1","type":"openai_compatible"},"test_models":["`+big+`"]}`); rr.Code != 400 {
		t.Fatalf("oversize model provider-test status=%d want 400", rr.Code)
	}
	// Unreachable provider: probe fails, response still 200 with ok=false.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", wave3ProviderFormJSON(deadURL, ""))
	if rr.Code != 200 {
		t.Fatalf("unreachable provider-test status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	out := wave3JSON(t, rr)
	if out["ok"] != false || out["passed"] != float64(0) {
		t.Fatalf("unreachable provider-test body=%s want ok=false passed=0", rr.Body.String())
	}
	// Provider-discover method guard.
	// Same authz-first rule as provider-test above: GET on this admin
	// path is denied 403 before any handler method check.
	if rr := wave3AdminRequest(s, "GET", "/admin/api/provider-discover", ""); rr.Code != 403 {
		t.Fatalf("GET provider-discover status=%d want 403", rr.Code)
	}
}

func TestWave3AdminProviderTestFull(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", "application/json")
		if m["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, wave3ChatSSE)
			return
		}
		if _, hasTools := m["tools"]; hasTools {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"model":"upstream-model","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
			return
		}
		if _, hasFormat := m["response_format"]; hasFormat {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],"model":"upstream-model","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
			return
		}
		_, _ = io.WriteString(w, wave3ChatOK)
	}))
	defer up.Close()
	s := wave3Gateway(t, nil, nil)
	rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", wave3ProviderFormJSON(up.URL, "full"))
	if rr.Code != 200 {
		t.Fatalf("full provider-test status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	out := wave3JSON(t, rr)
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("full provider-test results=%v want 1", results)
	}
	res, _ := results[0].(map[string]any)
	if _, ok := res["capability_report"]; !ok {
		t.Fatalf("full provider-test result missing capability_report: %v", res)
	}
	if out["ok"] != true {
		t.Fatalf("full provider-test body=%s want ok=true", rr.Body.String())
	}
}

func TestWave3AdminProviderTestClaudeCode(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3ChatOK, nil)
	defer up.Close()
	s := wave3Gateway(t, nil, nil)
	rr := wave3AdminRequest(s, "POST", "/admin/api/provider-test", wave3ProviderFormJSON(up.URL, "claude_code"))
	if rr.Code != 200 {
		t.Fatalf("claude_code provider-test status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	out := wave3JSON(t, rr)
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("claude_code provider-test results=%v want 1", results)
	}
	res, _ := results[0].(map[string]any)
	if _, ok := res["agent_report"]; !ok {
		t.Fatalf("claude_code provider-test result missing agent_report: %v", res)
	}
}

func TestWave3AdminProviderTestParentCtxDone(t *testing.T) {
	entered := make(chan struct{}, 32)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	s := wave3Gateway(t, nil, nil)
	models := make([]string, 0, 17)
	for i := 0; i < 17; i++ {
		models = append(models, fmt.Sprintf("m%d", i))
	}
	var b strings.Builder
	b.WriteString(`{"provider":{"id":"p","name":"p","type":"openai_compatible","base_url":"` + up.URL + `","auth_mode":"none","enabled":true},"test_models":[`)
	for i, m := range models {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q", m)
	}
	b.WriteString(`]}`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequestWithContext(ctx, "POST", "http://127.0.0.1/admin/api/provider-test", strings.NewReader(b.String()))
		req.Host = "127.0.0.1"
		req.RemoteAddr = "127.0.0.1:29371"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		done <- rr
	}()
	// 16 probes fill the default semaphore; the 17th blocks on it. Only
	// then is cancellation deterministic for the queued worker.
	for i := 0; i < 16; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d probes entered, want 16", i)
		}
	}
	cancel()
	select {
	case rr := <-done:
		if rr.Body.Len() != 0 {
			t.Fatalf("cancelled provider-test body=%q want empty (bare return)", rr.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled provider-test handler did not return")
	}
	// Sandbox observation (see TestWave3AnthropicHedgeSecondaryWins):
	// upstream handlers blocked before writing any bytes do not
	// observe cancellation promptly, so release them after the
	// empty-body assertion instead of hanging in up.Close().
	close(release)
}

// ---- admin: evaluation run guards ----

func wave3EvalGateway(t *testing.T, live bool) *Server {
	t.Helper()
	up := wave3JSONServer(t, 200, wave3ChatOK, nil)
	t.Cleanup(up.Close)
	return wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Evaluation.Enabled = true
		cfg.Evaluation.LiveEnabled = live
	})
}

func wave3EvalRunBody(mode, suite, dep string, extra string) string {
	return `{"mode":"` + mode + `","suite_id":"` + suite + `","deployment_id":"` + dep + `"` + extra + `}`
}

func TestWave3AdminEvaluationRunGuards(t *testing.T) {
	// Plane disabled: 409.
	{
		s := wave3Gateway(t, nil, nil)
		rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", `{"mode":"live","suite_id":"coding","deployment_id":"a/m"}`)
		if rr.Code != 409 {
			t.Fatalf("disabled plane status=%d body=%s want 409", rr.Code, rr.Body.String())
		}
	}
	// Live disabled: 409.
	{
		s := wave3EvalGateway(t, false)
		rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("live", "coding", "a/m", `,"prompts":[{"case_id":"coding-bugfix","prompt":"hi"}]`))
		if rr.Code != 409 || !strings.Contains(rr.Body.String(), "live evaluation is disabled") {
			t.Fatalf("live disabled status=%d body=%s want 409 live disabled", rr.Code, rr.Body.String())
		}
	}
	s := wave3EvalGateway(t, true)
	live := func(extra string) *httptest.ResponseRecorder {
		return wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("live", "coding", "a/m", extra))
	}
	// Artifacts rejected for live runs.
	if rr := live(`,"artifacts":[{}],"prompts":[{"case_id":"coding-bugfix","prompt":"hi"}]`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "not artifacts") {
		t.Fatalf("live artifacts status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
	// No prompts.
	if rr := live(``); rr.Code != 400 || !strings.Contains(rr.Body.String(), "prompts are required") {
		t.Fatalf("live no-prompts status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
	// Too many prompts (>512).
	{
		var b strings.Builder
		b.WriteString(`,"prompts":[`)
		for i := 0; i < 513; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"case_id":"coding-bugfix","prompt":"hi"}`)
		}
		b.WriteString(`]`)
		if rr := live(b.String()); rr.Code != 400 || !strings.Contains(rr.Body.String(), "too many prompts") {
			t.Fatalf("live too-many-prompts status=%d body=%s want 400", rr.Code, rr.Body.String())
		}
	}
	// Unknown case_id.
	if rr := live(`,"prompts":[{"case_id":"nope","prompt":"hi"}]`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "unknown case_id") {
		t.Fatalf("live unknown case status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
	// Unknown fields rejected.
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("live", "coding", "a/m", `,"prompts":[{"case_id":"coding-bugfix","prompt":"hi"}],"bogus":1`)); rr.Code != 400 {
		t.Fatalf("live unknown field status=%d want 400", rr.Code)
	}
	// Invalid mode.
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("bogus", "coding", "a/m", `,"prompts":[{"case_id":"coding-bugfix","prompt":"hi"}]`)); rr.Code != 400 {
		t.Fatalf("live invalid mode status=%d want 400", rr.Code)
	}
	// Missing suite / deployment, mismatched provider / model, unknown suite / deployment.
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", `{"mode":"live","deployment_id":"a/m"}`); rr.Code != 400 {
		t.Fatalf("missing suite status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", `{"mode":"live","suite_id":"coding"}`); rr.Code != 400 {
		t.Fatalf("missing deployment status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("live", "coding", "a/m", `,"provider_id":"other"`)); rr.Code != 400 {
		t.Fatalf("provider mismatch status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("live", "coding", "a/m", `,"model":"other-model"`)); rr.Code != 400 {
		t.Fatalf("model mismatch status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("live", "nope", "a/m", ``)); rr.Code != 400 {
		t.Fatalf("unknown suite status=%d want 400", rr.Code)
	}
	if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", wave3EvalRunBody("live", "coding", "ghost/m", ``)); rr.Code != 404 {
		t.Fatalf("unknown deployment status=%d want 404", rr.Code)
	}
	// Oversize body (>4 MiB).
	{
		big := strings.Repeat("x", (4<<20)+1)
		body := `{"mode":"live","suite_id":"coding","deployment_id":"a/m","prompts":[{"case_id":"coding-bugfix","prompt":"` + big + `"}]}`
		if rr := wave3AdminRequest(s, "POST", "/admin/api/evaluation/run", body); rr.Code != 413 {
			t.Fatalf("oversize eval body status=%d want 413", rr.Code)
		}
	}
	// Method guard: GET on an admin write path is denied 403 by the
	// admin-authz layer before the handler's own method check.
	if rr := wave3AdminRequest(s, "GET", "/admin/api/evaluation/run", ""); rr.Code != 403 {
		t.Fatalf("GET evaluation run status=%d want 403", rr.Code)
	}
}

func TestWave3AdminEvaluationRunLiveAdapterMissing(t *testing.T) {
	s := wave3EvalGateway(t, true)
	plane := s.evaluationSnapshot()
	if plane == nil {
		t.Fatal("evaluation plane missing")
	}
	suite, ok := plane.LookupSuite("coding")
	if !ok {
		t.Fatal("coding suite missing")
	}
	cfg := plane.config()
	req := httptest.NewRequest("POST", "http://127.0.0.1/admin/api/evaluation/run", nil)
	rr := httptest.NewRecorder()
	s.adminEvaluationRunLive(rr, req, plane, cfg, evaluationRunRequest{
		Mode: "live", SuiteID: "coding", DeploymentID: "ghost/m",
		Prompts: []livePrompt{{CaseID: "coding-bugfix", Prompt: "hi"}},
	}, deploymentView{ID: "ghost/m", ProviderID: "ghost", Model: "m", ProviderType: "openai_compatible"}, suite)
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "no provider adapter") {
		t.Fatalf("live adapter-missing status=%d body=%s want 404 no provider adapter", rr.Code, rr.Body.String())
	}
}
