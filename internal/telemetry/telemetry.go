// Package telemetry exports request spans to an OpenTelemetry
// collector over OTLP/HTTP (JSON), without third-party dependencies.
//
// It is deliberately small: a bounded queue, a batching flusher
// goroutine, and a hand-encoded OTLP payload. Export is opt-in and
// non-blocking — a full queue or a failing collector drops spans and
// counts the drops; telemetry must never slow down or fail a request.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Span is one completed data-plane request.
type Span struct {
	TraceID      string // 32 lowercase hex chars (W3C)
	SpanID       string // 16 lowercase hex chars
	ParentSpanID string // may be empty for a root span
	Name         string
	Start        time.Time
	End          time.Time
	StatusCode   int // HTTP status of the response
	Attributes   map[string]any
}

// anyValue is the OTLP AnyValue union (one field set).
type anyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 is a string in OTLP JSON
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"` // 2 = server
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []keyValue `json:"attributes,omitempty"`
	Status            otlpStatus `json:"status"`
}

type otlpStatus struct {
	Code int `json:"code"` // 1 = Ok, 2 = Error
}

type exportRequest struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource  resource    `json:"resource"`
	ScopeSpans []scopeSpan `json:"scopeSpans"`
}

type resource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeSpan struct {
	Scope scope      `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type scope struct {
	Name string `json:"name"`
}

const (
	defaultQueueSize   = 1024
	defaultBatchSize   = 64
	defaultFlushPeriod = 2 * time.Second
	defaultTimeout     = 5 * time.Second
)

// Exporter batches spans to one OTLP/HTTP traces endpoint.
type Exporter struct {
	endpoint    string
	serviceName string
	client      *http.Client

	queue   chan Span
	done    chan struct{}
	flushWg sync.WaitGroup
	closed  atomic.Bool
	dropped atomic.Uint64
	failed  atomic.Uint64
	sent    atomic.Uint64
}

// New validates the endpoint and starts the flusher. The endpoint must
// be the full OTLP/HTTP traces URL (e.g. http://collector:4318/v1/traces).
func New(endpoint, serviceName string) (*Exporter, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("telemetry: invalid OTLP endpoint %q: must be an http(s) URL", endpointRedacted(endpoint))
	}
	if serviceName == "" {
		serviceName = "nexaroute"
	}
	e := &Exporter{
		endpoint:    endpoint,
		serviceName: serviceName,
		client:      &http.Client{Timeout: defaultTimeout},
		queue:       make(chan Span, defaultQueueSize),
		done:        make(chan struct{}),
	}
	e.flushWg.Add(1)
	go e.run()
	return e, nil
}

func endpointRedacted(endpoint string) string {
	if len(endpoint) > 64 {
		return endpoint[:64] + "..."
	}
	return endpoint
}

// Export queues a span without blocking. It returns false when the
// span was dropped (queue full or exporter closed).
func (e *Exporter) Export(span Span) bool {
	if e.closed.Load() {
		e.dropped.Add(1)
		return false
	}
	select {
	case e.queue <- span:
		return true
	default:
		e.dropped.Add(1)
		return false
	}
}

// Dropped reports spans lost to a full queue or post-close exports.
// Failed reports spans in batches the collector rejected or that
// timed out. Sent counts successfully exported spans.
func (e *Exporter) Dropped() uint64 { return e.dropped.Load() }
func (e *Exporter) Failed() uint64  { return e.failed.Load() }
func (e *Exporter) Sent() uint64    { return e.sent.Load() }

func (e *Exporter) run() {
	defer e.flushWg.Done()
	ticker := time.NewTicker(defaultFlushPeriod)
	defer ticker.Stop()
	batch := make([]Span, 0, defaultBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		e.send(batch)
		batch = batch[:0]
	}
	for {
		select {
		case span := <-e.queue:
			batch = append(batch, span)
			if len(batch) >= defaultBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-e.done:
			// Drain whatever is left, then do a final flush.
			for {
				select {
				case span := <-e.queue:
					batch = append(batch, span)
					if len(batch) >= defaultBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (e *Exporter) send(batch []Span) {
	payload := e.encode(batch)
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		e.failed.Add(uint64(len(batch)))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		e.failed.Add(uint64(len(batch)))
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e.failed.Add(uint64(len(batch)))
		return
	}
	e.sent.Add(uint64(len(batch)))
}

func (e *Exporter) encode(batch []Span) []byte {
	spans := make([]otlpSpan, 0, len(batch))
	for _, s := range batch {
		code := 1
		if s.StatusCode >= 400 {
			code = 2
		}
		spans = append(spans, otlpSpan{
			TraceID:           s.TraceID,
			SpanID:            s.SpanID,
			ParentSpanID:      s.ParentSpanID,
			Name:              s.Name,
			Kind:              2,
			StartTimeUnixNano: fmt.Sprintf("%d", s.Start.UnixNano()),
			EndTimeUnixNano:   fmt.Sprintf("%d", s.End.UnixNano()),
			Attributes:        encodeAttributes(s.Attributes),
			Status:            otlpStatus{Code: code},
		})
	}
	doc := exportRequest{ResourceSpans: []resourceSpans{{
		Resource: resource{Attributes: []keyValue{{
			Key:   "service.name",
			Value: anyValue{StringValue: strPtr(e.serviceName)},
		}}},
		ScopeSpans: []scopeSpan{{Scope: scope{Name: "nexaroute"}, Spans: spans}},
	}}}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	return out
}

func encodeAttributes(attrs map[string]any) []keyValue {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]keyValue, 0, len(keys))
	for _, k := range keys {
		var v anyValue
		switch value := attrs[k].(type) {
		case string:
			v.StringValue = strPtr(value)
		case int:
			v.IntValue = strPtr(fmt.Sprintf("%d", value))
		case int64:
			v.IntValue = strPtr(fmt.Sprintf("%d", value))
		case float64:
			v.DoubleValue = &value
		case bool:
			v.BoolValue = &value
		default:
			v.StringValue = strPtr(fmt.Sprintf("%v", value))
		}
		out = append(out, keyValue{Key: k, Value: v})
	}
	return out
}

func strPtr(s string) *string { return &s }

// Close stops the exporter and flushes the remaining queue. It is
// safe to call once; exports after Close are dropped.
func (e *Exporter) Close() {
	if e.closed.Swap(true) {
		return
	}
	close(e.done)
	e.flushWg.Wait()
}
