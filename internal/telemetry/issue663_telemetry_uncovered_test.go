package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIssue663CoreEndpointRedaction(t *testing.T) {
	// A long invalid endpoint must be truncated in the error: the tail
	// (which could carry a query token) must never appear.
	long := "not-a-url-" + strings.Repeat("x", 200)
	_, err := New(long, "svc")
	if err == nil || !strings.Contains(err.Error(), "...") || strings.Contains(err.Error(), strings.Repeat("x", 100)) {
		t.Fatalf("long invalid endpoint must be redacted in the error, got %v", err)
	}
	// A short invalid endpoint is reported in full.
	if _, err := New("ftp://collector:4318", "svc"); err == nil || !strings.Contains(err.Error(), "ftp://collector:4318") {
		t.Fatalf("short invalid endpoint must appear in the error, got %v", err)
	}
	if _, err := New("http://", "svc"); err == nil {
		t.Error("endpoint without a host must be rejected")
	}
	if _, err := New("  ", "svc"); err == nil {
		t.Error("blank endpoint must be rejected")
	}
	// Empty service name defaults to nexaroute, observable on the wire.
	collector := &fakeCollector{status: http.StatusOK}
	ts := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer ts.Close()
	e, err := New(ts.URL+"/v1/traces", "")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Export(testSpan("chat", 200)) {
		t.Fatal("export must be accepted")
	}
	e.Close()
	collector.mu.Lock()
	name := ""
	if len(collector.payloads) > 0 && len(collector.payloads[0].ResourceSpans) > 0 {
		for _, kv := range collector.payloads[0].ResourceSpans[0].Resource.Attributes {
			if kv.Key == "service.name" && kv.Value.StringValue != nil {
				name = *kv.Value.StringValue
			}
		}
	}
	collector.mu.Unlock()
	if name != "nexaroute" {
		t.Fatalf("default service name = %q, want nexaroute", name)
	}
}

func TestIssue663CoreExporterBatchAndClose(t *testing.T) {
	collector := &fakeCollector{status: http.StatusOK}
	ts := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer ts.Close()
	e, err := New(ts.URL+"/v1/traces", "svc")
	if err != nil {
		t.Fatal(err)
	}
	// Exporting a full batch (64) flushes synchronously on the 64th
	// export, without waiting for the 2s ticker.
	for i := 0; i < 64; i++ {
		if !e.Export(testSpan("chat", 200)) {
			t.Fatalf("export %d refused", i)
		}
	}
	e.Close()
	if got := len(collector.spans()); got != 64 {
		t.Fatalf("collector received %d spans, want 64", got)
	}
	if e.Sent() != 64 || e.Dropped() != 0 || e.Failed() != 0 {
		t.Fatalf("counters: sent=%d dropped=%d failed=%d", e.Sent(), e.Dropped(), e.Failed())
	}
	// Close is idempotent, and exports after Close are dropped and
	// counted rather than panicking or blocking.
	e.Close()
	if e.Export(testSpan("chat", 200)) {
		t.Fatal("export after Close must be refused")
	}
	if e.Dropped() != 1 {
		t.Fatalf("post-close drops = %d, want 1", e.Dropped())
	}
}

func TestIssue663CoreExporterSendFailures(t *testing.T) {
	// Collector down: the client's request fails and the batch counts
	// as failed, not sent.
	ts := httptest.NewServer(http.HandlerFunc((&fakeCollector{status: http.StatusOK}).handler))
	url := ts.URL + "/v1/traces"
	ts.Close()
	e, err := New(url, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Export(testSpan("chat", 200)) {
		t.Fatal("export must be accepted while the exporter is open")
	}
	e.Close()
	if e.Failed() != 1 || e.Sent() != 0 {
		t.Fatalf("collector-down counters: sent=%d failed=%d", e.Sent(), e.Failed())
	}

	// Error status from the collector also counts as failed.
	bad := &fakeCollector{status: http.StatusBadGateway}
	ts2 := httptest.NewServer(http.HandlerFunc(bad.handler))
	defer ts2.Close()
	e2, err := New(ts2.URL+"/v1/traces", "svc")
	if err != nil {
		t.Fatal(err)
	}
	if !e2.Export(testSpan("chat", 500)) {
		t.Fatal("export must be accepted")
	}
	e2.Close()
	if e2.Failed() != 1 || e2.Sent() != 0 {
		t.Fatalf("error-status counters: sent=%d failed=%d", e2.Sent(), e2.Failed())
	}
	// The error span itself is encoded with OTLP status Error (2).
	spans := bad.spans()
	if len(spans) != 1 || spans[0].Status.Code != 2 {
		t.Fatalf("error span encoding wrong: %+v", spans)
	}
}

func TestIssue663CoreEncodeAttributesTypes(t *testing.T) {
	collector := &fakeCollector{status: http.StatusOK}
	ts := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer ts.Close()
	e, err := New(ts.URL+"/v1/traces", "svc")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	span := Span{
		TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef",
		Name: "typed", Start: now, End: now.Add(time.Millisecond), StatusCode: 200,
		Attributes: map[string]any{
			"s":   "text",
			"i":   42,
			"i64": int64(-7),
			"f":   1.5,
			"b":   true,
			"odd": []string{"a", "b"},
		},
	}
	if !e.Export(span) {
		t.Fatal("export must be accepted")
	}
	e.Close()
	spans := collector.spans()
	if len(spans) != 1 {
		t.Fatalf("collector received %d spans, want 1", len(spans))
	}
	attrs := map[string]anyValue{}
	for _, kv := range spans[0].Attributes {
		attrs[kv.Key] = kv.Value
	}
	if v := attrs["s"].StringValue; v == nil || *v != "text" {
		t.Errorf("string attribute = %v", attrs["s"])
	}
	if v := attrs["i"].IntValue; v == nil || *v != "42" {
		t.Errorf("int attribute = %v", attrs["i"])
	}
	if v := attrs["i64"].IntValue; v == nil || *v != "-7" {
		t.Errorf("int64 attribute = %v", attrs["i64"])
	}
	if v := attrs["f"].DoubleValue; v == nil || *v != 1.5 {
		t.Errorf("float attribute = %v", attrs["f"])
	}
	if v := attrs["b"].BoolValue; v == nil || *v != true {
		t.Errorf("bool attribute = %v", attrs["b"])
	}
	if v := attrs["odd"].StringValue; v == nil || *v != "[a b]" {
		t.Errorf("unsupported-type attribute must fall back to %%v formatting, got %v", attrs["odd"])
	}
	// Empty attributes encode to no attribute list at all.
	e2, err := New(ts.URL+"/v1/traces", "svc")
	if err != nil {
		t.Fatal(err)
	}
	bare := testSpan("bare", 200)
	bare.Attributes = nil
	if !e2.Export(bare) {
		t.Fatal("export must be accepted")
	}
	e2.Close()
	spans = collector.spans()
	last := spans[len(spans)-1]
	if len(last.Attributes) != 0 {
		t.Fatalf("empty attributes must encode to none, got %d", len(last.Attributes))
	}
}
