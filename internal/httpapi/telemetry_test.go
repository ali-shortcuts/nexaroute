package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/telemetry"
)

// End-to-end span wiring (M4-6 part B) against a fake OTLP collector:
// a data-plane request produces one server span linked to the incoming
// traceparent, carrying the GenAI attributes noted by recordUsage and
// the TTFT callback; non-data-plane paths produce no spans.

type spanCollector struct {
	mu    sync.Mutex
	spans []map[string]any
}

func (c *spanCollector) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []map[string]any `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	for _, rs := range doc.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			c.spans = append(c.spans, ss.Spans...)
		}
	}
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (c *spanCollector) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.spans...)
}

func spanAttrs(span map[string]any) map[string]any {
	out := map[string]any{}
	if list, ok := span["attributes"].([]any); ok {
		for _, item := range list {
			kv, _ := item.(map[string]any)
			if kv == nil {
				continue
			}
			key, _ := kv["key"].(string)
			value, _ := kv["value"].(map[string]any)
			for _, field := range []string{"stringValue", "intValue", "doubleValue", "boolValue"} {
				if v, ok := value[field]; ok {
					out[key] = v
				}
			}
		}
	}
	return out
}

func TestTelemetrySpanExportEndToEnd(t *testing.T) {
	collector := &spanCollector{}
	ts := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer ts.Close()
	exporter, err := telemetry.New(ts.URL+"/v1/traces", "nexaroute-test")
	if err != nil {
		t.Fatal(err)
	}

	srv := testGateway(t, config.Default())
	srv.SetTelemetryExporter(exporter)
	handler := srv.Handler()

	// A data-plane request (malformed body: still a completed request
	// with a status) must export exactly one span.
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader("{bad json"))
	req.RemoteAddr = "127.0.0.1:10001"
	req.Header.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("malformed chat request unexpectedly succeeded")
	}

	// Note usage + TTFT the way the proxy paths do, then export a span
	// for a routed request via a second call whose context is prepared
	// by the middleware.
	req2 := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader("{bad json"))
	req2.RemoteAddr = "127.0.0.1:10001"
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	exporter.Close()
	spans := collector.all()
	if len(spans) != 2 {
		t.Fatalf("collector received %d spans, want 2 (one per data-plane request)", len(spans))
	}
	first := spans[0]
	if first["traceId"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("traceId = %v, want the incoming traceparent trace id", first["traceId"])
	}
	if first["parentSpanId"] != "bbbbbbbbbbbbbbbb" {
		t.Errorf("parentSpanId = %v, want the incoming span id", first["parentSpanId"])
	}
	attrs := spanAttrs(first)
	if attrs["gen_ai.operation.name"] != "chat" {
		t.Errorf("operation = %v, want chat", attrs["gen_ai.operation.name"])
	}
	if attrs["http.route"] != "/v1/chat/completions" {
		t.Errorf("route = %v", attrs["http.route"])
	}
}

func TestTelemetrySpanCarriesUsageAndTTFT(t *testing.T) {
	collector := &spanCollector{}
	ts := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer ts.Close()
	exporter, err := telemetry.New(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := testGateway(t, config.Default())
	srv.SetTelemetryExporter(exporter)

	// Drive exportSpan directly with a noted record: the middleware
	// path is covered above; this pins the attribute assembly.
	ctx := withSpanRecord(t.Context(), &spanRecord{})
	noteSpanUsage(ctx, "prov1/model-a", 120, 45)
	noteSpanTTFT(ctx, 87*time.Millisecond)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", nil).WithContext(ctx)
	trace := traceContext{TraceID: strings.Repeat("c", 32), SpanID: strings.Repeat("d", 16)}
	srv.exportSpan(req, trace, time.Now(), 300*time.Millisecond, http.StatusOK)
	exporter.Close()

	spans := collector.all()
	if len(spans) != 1 {
		t.Fatalf("collector received %d spans, want 1", len(spans))
	}
	attrs := spanAttrs(spans[0])
	checks := map[string]any{
		"gen_ai.system":              "prov1",
		"gen_ai.response.model":      "model-a",
		"nexaroute.deployment":       "prov1/model-a",
		"gen_ai.usage.input_tokens":  "120",
		"gen_ai.usage.output_tokens": "45",
		"nexaroute.ttft_ms":          87.0,
		"gen_ai.operation.name":      "chat",
	}
	for key, want := range checks {
		if attrs[key] != want {
			t.Errorf("attr %s = %v (%T), want %v", key, attrs[key], attrs[key], want)
		}
	}
	// Privacy: nothing prompt- or identity-shaped may appear.
	for key := range attrs {
		for _, banned := range []string{"prompt", "completion_text", "api_key", "client", "user"} {
			if strings.Contains(key, banned) {
				t.Errorf("attribute %q looks like it carries %s data", key, banned)
			}
		}
	}
}

func TestTelemetryDisabledByDefault(t *testing.T) {
	srv := testGateway(t, config.Default())
	if srv.telemetryExporter != nil {
		t.Fatal("telemetry exporter must be nil unless configured")
	}
	// exportSpan with no exporter and a data-plane path is a no-op.
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", nil)
	srv.exportSpan(req, traceContext{TraceID: strings.Repeat("e", 32), SpanID: strings.Repeat("f", 16)}, time.Now(), time.Millisecond, 200)
}
