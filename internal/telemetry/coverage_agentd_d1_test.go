package telemetry

import (
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Agent-D1 coverage: batch-full flush, ticker flush, queue-full drops,
// NewRequest failure in send, and non-finite float encoding failure.

func TestAgentD1ExporterFlushesFullBatch(t *testing.T) {
	collector := &fakeCollector{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer srv.Close()

	exp, err := New(srv.URL, "svc")
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close()
	for i := 0; i < 64; i++ {
		if !exp.Export(testSpan("batch-span", 200)) {
			t.Fatalf("export %d should be accepted", i)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := exp.Sent(); got == 64 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("Sent = %d, want 64 (batch-full flush); collector saw %d spans", exp.Sent(), len(collector.spans()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(collector.spans()); got != 64 {
		t.Fatalf("collector got %d spans, want 64", got)
	}
	if got := exp.Failed(); got != 0 {
		t.Fatalf("Failed = %d, want 0", got)
	}
}

func TestAgentD1ExporterTickerFlushesPartialBatch(t *testing.T) {
	collector := &fakeCollector{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer srv.Close()

	exp, err := New(srv.URL, "svc")
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close()
	if !exp.Export(testSpan("ticker-span", 200)) {
		t.Fatal("export should be accepted")
	}
	// The 2s flush ticker must deliver the partial batch even though
	// it never reaches the 64-span batch size.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := exp.Sent(); got == 1 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("Sent = %d, want 1 (ticker flush); collector saw %d spans", exp.Sent(), len(collector.spans()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(collector.spans()); got != 1 {
		t.Fatalf("collector got %d spans, want 1", got)
	}
}

func TestAgentD1ExporterDropsWhenQueueFull(t *testing.T) {
	entered := make(chan struct{})
	var enterOnce sync.Once
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp, err := New(srv.URL, "svc")
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close()
	// Fill the first batch so the flusher blocks inside send.
	for i := 0; i < 64; i++ {
		if !exp.Export(testSpan("blocker", 200)) {
			t.Fatalf("export %d should be accepted", i)
		}
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("collector never received the first batch")
	}
	// While the flusher is blocked, flood far past the 1024-span
	// queue: the excess must be dropped, never block the caller.
	const flood = 1500
	dropped := 0
	for i := 0; i < flood; i++ {
		if !exp.Export(testSpan("flood", 200)) {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("expected queue-full drops while the flusher is blocked, got none")
	}
	if got := exp.Dropped(); got != uint64(dropped) {
		t.Fatalf("Dropped = %d, want %d (one per rejected export)", got, dropped)
	}
	close(release)
	exp.Close()
	if got := exp.Sent() + exp.Dropped(); got != 64+uint64(flood) {
		t.Fatalf("Sent+Dropped = %d, want %d (every span accounted for)", got, 64+flood)
	}
	if got := exp.Sent(); got == 0 {
		t.Fatal("expected some spans to be sent after the collector unblocked")
	}
}

func TestAgentD1ExporterSendFailsOnUnbuildableRequest(t *testing.T) {
	// NewRequestWithContext rejects a URL that url.Parse in New would
	// have rejected too, so build the exporter directly with an
	// endpoint that cannot become an HTTP request.
	exp := &Exporter{
		endpoint: "://not a url",
		client:   &http.Client{},
		queue:    make(chan Span, 1),
		done:     make(chan struct{}),
	}
	exp.send([]Span{testSpan("bad-url", 200), testSpan("bad-url-2", 200)})
	if got := exp.Failed(); got != 2 {
		t.Fatalf("Failed = %d, want 2 (whole batch failed on request build)", got)
	}
	if got := exp.Sent(); got != 0 {
		t.Fatalf("Sent = %d, want 0", got)
	}
}

func TestAgentD1EncodeNonFiniteFloatReturnsNil(t *testing.T) {
	exp := &Exporter{serviceName: "svc"}
	for _, v := range []float64{math.Inf(1), math.NaN()} {
		out := exp.encode([]Span{{
			TraceID: "0123456789abcdef0123456789abcdef",
			SpanID:  "0123456789abcdef",
			Name:    "non-finite",
			Attributes: map[string]any{
				"ratio": v,
			},
		}})
		if out != nil {
			t.Fatalf("encode with attribute %v should fail (JSON cannot encode it), got %d bytes", v, len(out))
		}
	}
	// A finite float still encodes.
	out := exp.encode([]Span{{
		TraceID: "0123456789abcdef0123456789abcdef",
		SpanID:  "0123456789abcdef",
		Name:    "finite",
		Attributes: map[string]any{
			"ratio": 1.5,
		},
	}})
	if len(out) == 0 {
		t.Fatal("encode with a finite float attribute should succeed")
	}
}
