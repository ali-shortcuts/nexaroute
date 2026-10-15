package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeCollector records decoded OTLP payloads.
type fakeCollector struct {
	mu       sync.Mutex
	payloads []exportRequest
	status   int
}

func (f *fakeCollector) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var doc exportRequest
	if err := json.Unmarshal(body, &doc); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.payloads = append(f.payloads, doc)
	f.mu.Unlock()
	w.WriteHeader(f.status)
}

func (f *fakeCollector) spans() []otlpSpan {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []otlpSpan
	for _, p := range f.payloads {
		for _, rs := range p.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}

func testSpan(name string, status int) Span {
	now := time.Now()
	return Span{
		TraceID:      "0123456789abcdef0123456789abcdef",
		SpanID:       "0123456789abcdef",
		ParentSpanID: "fedcba9876543210",
		Name:         name,
		Start:        now,
		End:          now.Add(250 * time.Millisecond),
		StatusCode:   status,
		Attributes: map[string]any{
			"gen_ai.system":              "prov1",
			"gen_ai.operation.name":      "chat",
			"gen_ai.response.model":      "model-a",
			"gen_ai.usage.input_tokens":  int64(120),
			"gen_ai.usage.output_tokens": int64(45),
			"http.status_code":           status,
		},
	}
}

func TestExporterSendsOTLPJSONWithGenAIAttributes(t *testing.T) {
	collector := &fakeCollector{status: http.StatusOK}
	ts := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer ts.Close()

	e, err := New(ts.URL+"/v1/traces", "nexaroute-test")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Export(testSpan("chat", 200)) {
		t.Fatal("export refused a span on an open exporter")
	}
	e.Close() // final flush is synchronous with Close

	spans := collector.spans()
	if len(spans) != 1 {
		t.Fatalf("collector received %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.TraceID != "0123456789abcdef0123456789abcdef" || s.ParentSpanID != "fedcba9876543210" || s.Kind != 2 {
		t.Errorf("span ids/kind wrong: %+v", s)
	}
	if s.Status.Code != 1 {
		t.Errorf("status code = %d, want 1 (Ok)", s.Status.Code)
	}
	if s.StartTimeUnixNano == "" || s.StartTimeUnixNano >= s.EndTimeUnixNano {
		t.Errorf("bad timestamps: %s..%s", s.StartTimeUnixNano, s.EndTimeUnixNano)
	}
	attrs := map[string]anyValue{}
	for _, kv := range s.Attributes {
		attrs[kv.Key] = kv.Value
	}
	if got := attrs["gen_ai.system"].StringValue; got == nil || *got != "prov1" {
		t.Errorf("gen_ai.system = %v", got)
	}
	if got := attrs["gen_ai.usage.input_tokens"].IntValue; got == nil || *got != "120" {
		t.Errorf("input_tokens = %v, want \"120\" (int64 as string in OTLP JSON)", got)
	}
	if e.Sent() != 1 || e.Dropped() != 0 || e.Failed() != 0 {
		t.Errorf("counters: sent=%d dropped=%d failed=%d", e.Sent(), e.Dropped(), e.Failed())
	}
	// Resource carries the configured service name.
	collector.mu.Lock()
	resAttrs := collector.payloads[0].ResourceSpans[0].Resource.Attributes
	collector.mu.Unlock()
	if len(resAttrs) != 1 || resAttrs[0].Key != "service.name" || *resAttrs[0].Value.StringValue != "nexaroute-test" {
		t.Errorf("resource attributes wrong: %+v", resAttrs)
	}
}

func TestExporterErrorStatusAndCollectorFailure(t *testing.T) {
	collector := &fakeCollector{status: http.StatusInternalServerError}
	ts := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer ts.Close()

	e, err := New(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	e.Export(testSpan("chat", 503))
	e.Close()
	if e.Failed() != 1 || e.Sent() != 0 {
		t.Errorf("failed=%d sent=%d, want 1/0", e.Failed(), e.Sent())
	}
	spans := collector.spans()
	if len(spans) != 1 || spans[0].Status.Code != 2 {
		t.Errorf("error span status = %+v, want code 2", spans)
	}
}

func TestExporterDropsWhenClosedAndValidatesEndpoint(t *testing.T) {
	if _, err := New("not-a-url", ""); err == nil {
		t.Error("expected an error for a scheme-less endpoint")
	}
	if _, err := New("ftp://collector:4318", ""); err == nil {
		t.Error("expected an error for a non-http(s) endpoint")
	}
	e, err := New("http://127.0.0.1:1/v1/traces", "")
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	if e.Export(testSpan("chat", 200)) {
		t.Error("export after Close must be refused")
	}
	if e.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", e.Dropped())
	}
	// The unreachable endpoint's queued span failed during Close's flush
	// rather than blocking shutdown.
}
