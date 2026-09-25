package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

const (
	promptCanary = "SECRET_EXTERNAL_PROMPT_CANARY_94af"
	keyCanary    = "SECRET_JEV_KEY_CANARY_21df"
	remoteCanary = "SECRET_REMOTE_ERROR_CANARY_64ac"
)

// eventsDump renders every bus event for canary assertions.
func eventsDump(s *Server) string {
	var b strings.Builder
	for _, e := range s.bus.Snapshot() {
		b.WriteString(e.Kind)
		b.WriteString("\x00")
		b.WriteString(e.Deployment)
		b.WriteString("\x00")
		b.WriteString(e.Message)
		b.WriteString("\x00")
		b.WriteString(e.ErrorType)
		b.WriteString("\n")
	}
	return b.String()
}

func metricsDump(t *testing.T, s *Server) string {
	t.Helper()
	req := httptest.NewRequest("GET", "http://gateway/metrics", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("metrics status=%d", rr.Code)
	}
	return rr.Body.String()
}

func adminDump(t *testing.T, s *Server) string {
	t.Helper()
	req := httptest.NewRequest("GET", "http://gateway/admin/api/snapshot", nil)
	req.Header.Set("x-admin-key", "adm-decision-test")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("admin status=%d", rr.Code)
	}
	return rr.Body.String()
}

func TestPromptCanaryNeverLeavesGateway(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.8))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	body := `{"model":"client-model","messages":[{"role":"user","content":"debug this: ` + promptCanary + `"}],"max_tokens":8}`
	rr, req := chatRequest(body)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("jev hits=%d", cap.hits.Load())
	}
	// The mock Jev server captured the full HTTP request: no canary.
	if strings.Contains(cap.lastBody(t), promptCanary) {
		t.Fatal("raw prompt canary reached the external provider")
	}
	// No leak into observability surfaces either.
	if strings.Contains(eventsDump(s), promptCanary) {
		t.Fatal("prompt canary leaked into events")
	}
	if strings.Contains(metricsDump(t, s), promptCanary) {
		t.Fatal("prompt canary leaked into metrics")
	}
	if strings.Contains(adminDump(t, s), promptCanary) {
		t.Fatal("prompt canary leaked into admin snapshot")
	}
}

func TestAPIKeyCanaryContained(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c0", 0.5))
	cfg := twoDeploymentConfig(up.URL)
	cfg.DecisionProviders[0].APIKey = keyCanary
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// The key MUST arrive in Authorization (proves correct auth)...
	cap.mu.Lock()
	auths := append([]string(nil), cap.auths...)
	cap.mu.Unlock()
	if len(auths) != 1 || auths[0] != "Bearer "+keyCanary {
		t.Fatalf("auths=%v", auths)
	}
	// ...and MUST appear nowhere else.
	if strings.Contains(cap.lastBody(t), keyCanary) {
		t.Fatal("key canary in Jev request body (header-only required)")
	}
	if strings.Contains(eventsDump(s), keyCanary) {
		t.Fatal("key canary leaked into events")
	}
	if strings.Contains(metricsDump(t, s), keyCanary) {
		t.Fatal("key canary leaked into metrics")
	}
	if strings.Contains(adminDump(t, s), keyCanary) {
		t.Fatal("key canary leaked into admin snapshot")
	}
	if strings.Contains(rr.Body.String(), keyCanary) {
		t.Fatal("key canary leaked into client response")
	}
}

func TestRemoteErrorBodyCanaryContained(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, _ := startMockJev(t, 500, `{"code":1,"message":"`+remoteCanary+`"}`)
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("must fail open, got status=%d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), remoteCanary) {
		t.Fatal("remote error body leaked into client response")
	}
	if strings.Contains(eventsDump(s), remoteCanary) {
		t.Fatal("remote error body leaked into events")
	}
	if strings.Contains(metricsDump(t, s), remoteCanary) {
		t.Fatal("remote error body leaked into metrics")
	}
	if strings.Contains(adminDump(t, s), remoteCanary) {
		t.Fatal("remote error body leaked into admin snapshot")
	}
	// Only the bounded error class is recorded.
	if !strings.Contains(eventsDump(s), "EXTERNAL_HTTP_ERROR") {
		t.Fatal("expected bounded EXTERNAL_HTTP_ERROR class in events")
	}
}

func TestOpaqueIDsHidePhysicalDeployments(t *testing.T) {
	const physicalCanary = "CANARY_PHYSICAL_DEPLOYMENT_31cc"
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c0", 0.5))
	cfg := twoDeploymentConfig(up.URL)
	cfg.Providers[0].Models[0].ID = physicalCanary
	cfg.Providers[0].Models[0].Model = "up-" + physicalCanary
	cfg.Providers[0].ID = "canary-provider"
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	sent := cap.lastBody(t)
	if strings.Contains(sent, physicalCanary) || strings.Contains(sent, "canary-provider") {
		t.Fatalf("physical identity leaked to Jev: %s", sent)
	}
	if !strings.Contains(sent, `"c0"`) || !strings.Contains(sent, `"c1"`) {
		t.Fatalf("opaque IDs missing: %s", sent)
	}
}

func TestCredentialIndependence(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.7))
	cfg := twoDeploymentConfig(up.URL)
	// Upstream credentials with canary names and values.
	cfg.Providers[0].APIKey = "UPSTREAM_KEY_CANARY_A1"
	cfg.Providers[0].Credentials = []config.CredentialConfig{
		{Name: "cred-alpha-canary", APIKey: "UPSTREAM_KEY_CANARY_B2", Enabled: true},
	}
	cfg.Providers[1].APIKeyEnv = "NEXAROUTE_TEST_UPSTREAM_ENV"
	t.Setenv("NEXAROUTE_TEST_UPSTREAM_ENV", "UPSTREAM_KEY_CANARY_C3")
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	sent := cap.lastBody(t)
	for _, bad := range []string{"UPSTREAM_KEY_CANARY_A1", "UPSTREAM_KEY_CANARY_B2", "UPSTREAM_KEY_CANARY_C3",
		"cred-alpha-canary", "NEXAROUTE_TEST_UPSTREAM_ENV", "api_key", "credential"} {
		if strings.Contains(sent, bad) {
			t.Fatalf("credential material %q reached Jev: %s", bad, sent)
		}
	}
}
