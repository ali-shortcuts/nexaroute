package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

func activityRequest(t *testing.T, s *Server, key, target string, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = remote
	if key != "" {
		req.Header.Set("x-admin-key", key)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAdminActivityRequiresAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	s := testGateway(t, cfg)

	unauthorized := activityRequest(t, s, "", "http://gateway/admin/api/activity/requests", "127.0.0.2:1234")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	authorized := activityRequest(t, s, cfg.Admin.APIKey, "http://gateway/admin/api/activity/requests", "127.0.0.1:1234")
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", authorized.Code, authorized.Body.String())
	}
}

func TestAdminActivityIsReadOnlyMethodGuarded(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/activity/requests", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminActivityRejectsInvalidParams(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	s := testGateway(t, cfg)
	for _, target := range []string{
		"http://gateway/admin/api/activity/requests?limit=0",
		"http://gateway/admin/api/activity/requests?limit=9999",
		"http://gateway/admin/api/activity/requests?window=0",
		"http://gateway/admin/api/activity/requests?window=99999",
	} {
		rr := activityRequest(t, s, cfg.Admin.APIKey, target, "127.0.0.1:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", target, rr.Code, rr.Body.String())
		}
	}
}

func TestAdminActivityAggregatesRealRequest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	cfg.Providers = []config.ProviderConfig{{ID: "prov-a", Name: "A", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Aliases: []string{"client-model"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	s := testGateway(t, cfg)

	body := `{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-request-id", "req-activity-e2e")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("data-plane status=%d body=%s", rr.Code, rr.Body.String())
	}

	resp := activityRequest(t, s, cfg.Admin.APIKey, "http://gateway/admin/api/activity/requests?limit=50", "127.0.0.1:1234")
	if resp.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", resp.Code, resp.Body.String())
	}
	var parsed activityResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode activity response: %v", err)
	}
	var found *events.RequestLifecycle
	for i := range parsed.Requests {
		if parsed.Requests[i].RequestID == "req-activity-e2e" {
			found = &parsed.Requests[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("request not present in activity feed: %+v", parsed.Requests)
	}
	if found.RequestedModel != "client-model" {
		t.Errorf("requested model=%q want client-model", found.RequestedModel)
	}
	if found.State != events.TerminalSucceeded {
		t.Errorf("state=%q want succeeded", found.State)
	}
	if found.AttemptCount < 1 || found.Attempts[0].Deployment == "" {
		t.Errorf("expected a routed attempt, got %+v", found.Attempts)
	}
	if found.ResolvedModel != "upstream-model" {
		t.Errorf("resolved model=%q want upstream-model", found.ResolvedModel)
	}
	if found.InputTokens != 3 || found.OutputTokens != 2 || found.CostBasis != "actual" {
		t.Errorf("usage/cost basis not captured: in=%d out=%d basis=%q", found.InputTokens, found.OutputTokens, found.CostBasis)
	}
	if parsed.Counts.Total != 1 {
		t.Errorf("counts total=%d want 1", parsed.Counts.Total)
	}
}

func TestAdminActivityDetectsFailoverOrder(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	s := testGateway(t, cfg)
	rid := "req-failover-view"
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "request_accepted", Phase: events.PhaseAccepted, RequestedModel: "alias"})
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "route_attempt", Phase: events.PhaseAttempt, Attempt: 1, Deployment: "dep-a", Provider: "prov-a"})
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "route_fail", Deployment: "dep-a", ErrorType: "provider_timeout", StatusCode: 429})
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "failover", Deployment: "dep-a"})
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "route_attempt", Phase: events.PhaseAttempt, Attempt: 2, Deployment: "dep-b", Provider: "prov-b"})
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "route_ok", Deployment: "dep-b"})
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "request_finished", Phase: events.PhaseFinished, Terminal: events.TerminalSucceeded})

	resp := activityRequest(t, s, cfg.Admin.APIKey, "http://gateway/admin/api/activity/requests", "127.0.0.1:1234")
	var parsed activityResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Requests) != 1 {
		t.Fatalf("requests=%+v", parsed.Requests)
	}
	lc := parsed.Requests[0]
	if len(lc.Attempts) != 2 {
		t.Fatalf("attempts=%+v", lc.Attempts)
	}
	if lc.Attempts[0].Deployment != "dep-a" || lc.Attempts[0].Outcome != events.TerminalFailed {
		t.Fatalf("failed attempt not first: %+v", lc.Attempts[0])
	}
	if lc.Attempts[1].Deployment != "dep-b" || lc.Attempts[1].Outcome != events.TerminalSucceeded {
		t.Fatalf("backup attempt not second: %+v", lc.Attempts[1])
	}
	if lc.FailoverCount != 1 {
		t.Fatalf("failover count=%d", lc.FailoverCount)
	}
}

func TestAdminActivityRedactsEventMessage(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	s := testGateway(t, cfg)
	rid := "req-redact"
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "request_accepted", Phase: events.PhaseAccepted, Message: "Authorization: Bearer sk-live-secret-value", RequestedModel: "m"})
	s.bus.Add(events.Event{RequestID: rid, CorrelationID: rid, Kind: "request_finished", Phase: events.PhaseFinished, Terminal: events.TerminalSucceeded, Message: "prompt=hello-world"})

	resp := activityRequest(t, s, cfg.Admin.APIKey, "http://gateway/admin/api/activity/requests", "127.0.0.1:1234")
	out := resp.Body.String()
	for _, secret := range []string{"sk-live-secret-value", "Authorization", "hello-world"} {
		if strings.Contains(out, secret) {
			t.Fatalf("activity feed leaked %q: %s", secret, out)
		}
	}
}

func TestAdminActivityMarksStaleRequests(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	s := testGateway(t, cfg)
	old := time.Now().Add(-30 * time.Minute)
	s.bus.Add(events.Event{Time: old, RequestID: "req-old", CorrelationID: "req-old", Kind: "request_accepted", Phase: events.PhaseAccepted, RequestedModel: "m"})
	resp := activityRequest(t, s, cfg.Admin.APIKey, "http://gateway/admin/api/activity/requests", "127.0.0.1:1234")
	var parsed activityResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Requests) != 1 || parsed.Requests[0].State != events.StateStale {
		t.Fatalf("expected stale request, got %+v", parsed.Requests)
	}
	if parsed.Counts.Stale != 1 || parsed.Counts.Active != 0 {
		t.Fatalf("stale never rendered live: counts=%+v", parsed.Counts)
	}
}

func TestMetricsActivityLabelsAreBounded(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "activity-secret"
	s := testGateway(t, cfg)
	distinctive := "req-high-cardinality-9f3c2a"
	s.bus.Add(events.Event{RequestID: distinctive, CorrelationID: distinctive, Kind: "request_accepted", Phase: events.PhaseAccepted, RequestedModel: "some-model-name"})
	s.bus.Add(events.Event{RequestID: distinctive, CorrelationID: distinctive, Kind: "request_finished", Phase: events.PhaseFinished, Terminal: events.TerminalFailed, StatusCode: 500})

	req := httptest.NewRequest(http.MethodGet, "http://gateway/metrics", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status=%d", rr.Code)
	}
	out := rr.Body.String()
	for _, forbidden := range []string{distinctive, "some-model-name"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("metrics leaked a high-cardinality value %q", forbidden)
		}
	}
	for _, want := range []string{
		`nexaroute_activity_requests{state="active"}`,
		`nexaroute_activity_requests{state="failed"}`,
		`nexaroute_activity_attempts{outcome=`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing bounded activity series %q", want)
		}
	}
}
