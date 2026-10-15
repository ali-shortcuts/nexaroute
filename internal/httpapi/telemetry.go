package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/telemetry"
)

// Span export wiring (M4-6 part B). The middleware creates a spanRecord
// for the four data-plane LLM endpoints; the existing observation
// choke points (recordUsage for deployment + tokens, observeFirstByte
// callbacks for TTFT) note into it; the middleware exports the finished
// span after the handler returns. No prompts, completions, keys or
// client identities are ever noted.

type spanRecord struct {
	mu           sync.Mutex
	deployment   string
	inputTokens  int64
	outputTokens int64
	hasUsage     bool
	ttft         time.Duration
	hasTTFT      bool
}

type spanRecordKey struct{}

func withSpanRecord(ctx context.Context, rec *spanRecord) context.Context {
	return context.WithValue(ctx, spanRecordKey{}, rec)
}

func spanRecordFromContext(ctx context.Context) *spanRecord {
	rec, _ := ctx.Value(spanRecordKey{}).(*spanRecord)
	return rec
}

// noteSpanUsage records the serving deployment and token usage for the
// request's span, if one is being built.
func noteSpanUsage(ctx context.Context, deployment string, input, output int64) {
	rec := spanRecordFromContext(ctx)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.deployment = deployment
	rec.inputTokens = input
	rec.outputTokens = output
	rec.hasUsage = true
}

// noteSpanTTFT records the streaming time-to-first-byte for the
// request's span, if one is being built.
func noteSpanTTFT(ctx context.Context, d time.Duration) {
	rec := spanRecordFromContext(ctx)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.hasTTFT {
		rec.ttft = d
		rec.hasTTFT = true
	}
}

// dataPlaneOperation maps the data-plane LLM paths to their span /
// gen_ai.operation.name values. Everything else returns "" (no span).
func dataPlaneOperation(path string) string {
	switch path {
	case "/v1/messages", "/v1/chat/completions":
		return "chat"
	case "/v1/responses":
		return "responses"
	case "/v1/messages/count_tokens":
		return "count_tokens"
	}
	return ""
}

// SetTelemetryExporter attaches the OTLP span exporter. Nil (the
// default) disables span export entirely.
func (s *Server) SetTelemetryExporter(e *telemetry.Exporter) {
	s.telemetryExporter = e
}

// exportSpan builds and queues the finished span for one request. It
// never blocks meaningfully: Export drops on a full queue.
func (s *Server) exportSpan(r *http.Request, trace traceContext, start time.Time, duration time.Duration, status int) {
	exporter := s.telemetryExporter
	if exporter == nil {
		return
	}
	operation := dataPlaneOperation(r.URL.Path)
	if operation == "" {
		return
	}
	rec := spanRecordFromContext(r.Context())
	attrs := map[string]any{
		"gen_ai.operation.name": operation,
		"http.request.method":   r.Method,
		"http.route":            r.URL.Path,
		"http.status_code":      status,
	}
	if rec != nil {
		rec.mu.Lock()
		if rec.deployment != "" {
			attrs["nexaroute.deployment"] = rec.deployment
			if provider, model, found := strings.Cut(rec.deployment, "/"); found {
				attrs["gen_ai.system"] = provider
				attrs["gen_ai.response.model"] = model
			}
		}
		if rec.hasUsage {
			attrs["gen_ai.usage.input_tokens"] = rec.inputTokens
			attrs["gen_ai.usage.output_tokens"] = rec.outputTokens
		}
		if rec.hasTTFT {
			attrs["nexaroute.ttft_ms"] = float64(rec.ttft.Milliseconds())
		}
		rec.mu.Unlock()
	}
	exporter.Export(telemetry.Span{
		TraceID:      trace.TraceID,
		SpanID:       trace.SpanID,
		ParentSpanID: trace.ParentSpanID,
		Name:         operation,
		Start:        start,
		End:          start.Add(duration),
		StatusCode:   status,
		Attributes:   attrs,
	})
}
