package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestTracePropagationAndResponseHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	for _, incoming := range []string{"", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", "malformed"} {
		r := httptest.NewRequest(http.MethodGet, "http://gateway/healthz", nil)
		if incoming != "" {
			r.Header.Set("traceparent", incoming)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d", rr.Code)
		}
		traceID := rr.Header().Get("x-trace-id")
		parent := rr.Header().Get("traceparent")
		if len(traceID) != 32 || len(parent) != 55 || !strings.HasPrefix(parent, "00-"+traceID+"-") {
			t.Fatalf("bad trace headers trace_id=%q parent=%q", traceID, parent)
		}
	}
}
