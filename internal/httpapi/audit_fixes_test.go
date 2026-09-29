package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Audit item 3: a handler panic must be contained as HTTP 500 plus an
// internal_panic bus event (fail-closed), never propagated to the caller.
func TestMiddlewarePanicIsFailClosed(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	panicHandler := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("audit canary panic")
	}))
	req := httptest.NewRequest(http.MethodGet, "http://gateway/healthz", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	// Must not propagate the panic.
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("panic escaped middleware: %v", recovered)
			}
		}()
		panicHandler.ServeHTTP(rr, req)
	}()
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	found := false
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "internal_panic" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no internal_panic event recorded")
	}
}

// Audit item 6: every routeContext caller must defer cancel immediately so
// the zero-timeout WithCancel branch cannot leak.
func TestRouteContextCancelOwnership(t *testing.T) {
	// Streaming and zero-timeout branches return a cancel func that must be
	// callable and idempotent; bounded branch must carry a deadline.
	for _, tc := range []struct {
		name      string
		streaming bool
		timeout   time.Duration
		bounded   bool
	}{
		{"streaming", true, 0, false},
		{"zero-timeout", false, 0, false},
		{"bounded", false, time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := routeContext(t.Context(), tc.streaming, tc.timeout)
			defer cancel()
			cancel()
			if _, ok := ctx.Deadline(); ok != tc.bounded {
				t.Fatalf("bounded=%v, want %v", ok, tc.bounded)
			}
		})
	}
}

func TestRouteContextDocumentsCallers(t *testing.T) {
	// Static guard: all callers must pair routeContext with defer routeCancel().
	for _, file := range []string{"anthropic.go", "openai.go", "canonical_path.go"} {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		call := strings.Count(string(body), "routeContext(")
		defers := strings.Count(string(body), "defer routeCancel()")
		if call == 0 || defers < call {
			t.Fatalf("%s: %d routeContext calls but %d defer routeCancel()", file, call, defers)
		}
	}
}
