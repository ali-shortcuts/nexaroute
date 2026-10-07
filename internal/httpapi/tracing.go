package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

type traceContext struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
}
type traceContextKey struct{}

func newTraceContext(incoming string) traceContext {
	parts := strings.Split(strings.TrimSpace(incoming), "-")
	if len(parts) == 4 && len(parts[1]) == 32 && len(parts[2]) == 16 && parts[0] == "00" {
		if _, err1 := hex.DecodeString(parts[1]); err1 == nil {
			if _, err2 := hex.DecodeString(parts[2]); err2 == nil {
				return traceContext{TraceID: parts[1], SpanID: randomHex(8), ParentSpanID: parts[2]}
			}
		}
	}
	return traceContext{TraceID: randomHex(16), SpanID: randomHex(8)}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
func (t traceContext) Traceparent() string { return "00-" + t.TraceID + "-" + t.SpanID + "-01" }
func withTrace(ctx context.Context, t traceContext) context.Context {
	return context.WithValue(ctx, traceContextKey{}, t)
}
func traceFromContext(ctx context.Context) (traceContext, bool) {
	t, ok := ctx.Value(traceContextKey{}).(traceContext)
	return t, ok
}
