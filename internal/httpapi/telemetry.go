package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// reqTelemetry accumulates one data-plane request's resolved route and usage
// while it is being served. The middleware creates it, the handlers fill it
// (deployment, model, tokens, cost), and the middleware finalizes it into the
// bounded recent-requests ring when the response completes.
type reqTelemetry struct {
	mu           sync.Mutex
	path         string
	keyName      string
	requestID    string
	model        string
	deployment   string
	providerName string
	stream       bool
	inputTokens  int64
	outputTokens int64
	estCostUSD   float64
	recorded     bool
}

type telemetryKey struct{}

func requestTelemetry(r *http.Request) *reqTelemetry {
	if v, ok := r.Context().Value(telemetryKey{}).(*reqTelemetry); ok {
		return v
	}
	return nil
}

// markRequestTelemetry attaches a fresh telemetry record to the request.
func markRequestTelemetry(r *http.Request, requestID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), telemetryKey{}, &reqTelemetry{path: r.URL.Path, requestID: requestID, keyName: "local"}))
}

// finalizeTelemetry snapshots the telemetry into the recent-requests log.
func (s *Server) finalizeTelemetry(tm *reqTelemetry, status int, latency time.Duration, errMsg string) {
	if tm == nil {
		return
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.recorded {
		return
	}
	tm.recorded = true
	s.usage.Recent(usage.RequestRecord{
		Time: time.Now(), RequestID: tm.requestID, Path: tm.path, Model: tm.model,
		Deployment: tm.deployment, ProviderName: tm.providerName, KeyName: tm.keyName,
		Status: status, LatencyMS: latency.Milliseconds(), Stream: tm.stream,
		InputTokens: tm.inputTokens, OutputTokens: tm.outputTokens, EstCostUSD: tm.estCostUSD,
		Error: errMsg,
	})
}
