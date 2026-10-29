package httpapi

// Priority-strategy counterparts to the Wave A Gemini tests (issue #663).
// The config default strategy ("ready_mesh") IS a ready strategy, so the
// unmarked Wave A tests all exercised the ready branches; the priority
// branches (RecordFailure / HardCooldown / cooldown-event paths) need
// these explicit Strategy="priority" tests.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave2GeminiPriorityServer(t *testing.T, providers []config.ProviderConfig) *Server {
	t.Helper()
	return wave2GeminiServer(t, providers, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
}

func TestWave2GeminiPriorityTransportTerminal502(t *testing.T) {
	deadCfg, _ := wave2GeminiDeadProvider(t, "a")
	s := wave2GeminiPriorityServer(t, []config.ProviderConfig{deadCfg})
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "all candidate deployments failed") {
		t.Fatalf("status=%d want 502 all-failed body=%s", rr.Code, rr.Body.String())
	}
	// Transport errors emit no bus route_fail event (that event belongs
	// to the non-2xx path); the priority branch is exercised by the
	// RecordFailure call on this path, and a single candidate cannot
	// fail over.
	if wave2GeminiBusHas(t, s, "failover", "") {
		t.Fatal("single-candidate terminal failure must not emit a failover event")
	}
}

func TestWave2GeminiPriorityUpstream429(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	}))
	defer up.Close()
	s := wave2GeminiPriorityServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)})
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", rr.Code, rr.Body.String())
	}
	if got := wave2GeminiError(t, rr)["status"]; got != "RESOURCE_EXHAUSTED" {
		t.Fatalf("status name=%v want RESOURCE_EXHAUSTED", got)
	}
	if !wave2GeminiBusHas(t, s, "route_fail", "g/m") {
		t.Fatal("route_fail event missing for g/m")
	}
}

func TestWave2GeminiPriorityUpstream500(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()
	s := wave2GeminiPriorityServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)})
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 body=%s", rr.Code, rr.Body.String())
	}
	if !wave2GeminiBusHas(t, s, "route_fail", "g/m") {
		t.Fatal("route_fail event missing for g/m")
	}
}

func TestWave2GeminiPriorityMalformedResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `this is not a chat completion`)
	}))
	defer up.Close()
	s := wave2GeminiPriorityServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)})
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "upstream returned an invalid response") {
		t.Fatalf("status=%d want 502 invalid-response body=%s", rr.Code, rr.Body.String())
	}
}
