package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestRoutePreviewOrdersExplainsAndMarksPin(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.FallbackOnUnknownModel = false // keep the unknown-model case unambiguous
	cfg.Providers = []config.ProviderConfig{
		{
			ID: "alpha", Name: "Alpha", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1/v1",
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "writer", Model: "writer-x", Enabled: true, Priority: 5, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
		},
		{
			ID: "beta", Name: "Beta", Type: "openai_compatible", BaseURL: "http://127.0.0.1:2/v1",
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "writer", Model: "writer-x", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
		},
	}
	s := testGateway(t, cfg)
	s.hm.RecordSuccess("beta/writer", time.Millisecond)
	s.hm.RecordSuccess("alpha/writer", time.Millisecond)

	preview := func(body string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/route-preview", strings.NewReader(body))
		req.Header.Set("x-admin-key", "k")
		req.Header.Set("content-type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("preview status %d body=%s", rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := preview(`{"model":"writer-x"}`)
	if out["candidate_count"].(float64) != 2 {
		t.Fatalf("expected 2 candidates: %v", out)
	}
	cands := out["candidates"].([]any)
	first := cands[0].(map[string]any)
	if first["deployment_id"] != "beta/writer" {
		t.Fatalf("priority tier 1 must outrank tier 5: %v", first["deployment_id"])
	}
	reasons, _ := first["reasons"].([]any)
	if len(reasons) < 3 {
		t.Fatalf("top candidate must carry an explanation: %v", reasons)
	}
	if !strings.Contains(reasons[0].(string), "verified healthy") {
		t.Fatalf("explanation should mention verified health: %v", reasons)
	}

	// Alias targeting works and requires tools capability when requested.
	out = preview(`{"model":"coding","tools":true}`)
	if out["candidate_count"].(float64) < 1 {
		t.Fatalf("alias routing failed: %v", out)
	}

	// Session pin is surfaced.
	out = preview(`{"model":"writer-x","session_key":"sess-42"}`)
	cands = out["candidates"].([]any)
	// Observe the session so the pin exists, then preview again.
	req := routerReqFor(t, "writer-x", "sess-42")
	s.rt.ObserveSession(req, "beta/writer")
	out = preview(`{"model":"writer-x","session_key":"sess-42"}`)
	if out["session_pinned"] != "beta/writer" {
		t.Fatalf("session pin not surfaced: %v", out["session_pinned"])
	}
	pin0 := out["candidates"].([]any)[0].(map[string]any)
	if pin0["session_pinned"] != true || pin0["deployment_id"] != "beta/writer" {
		t.Fatalf("pinned candidate not flagged: %v", pin0)
	}

	// Unknown model with no candidates explains itself.
	out = preview(`{"model":"no-such-model"}`)
	if out["candidate_count"].(float64) != 0 || len(out["notes"].([]any)) == 0 {
		t.Fatalf("empty preview must explain: %v", out)
	}

	// Method guard.
	req2 := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/route-preview", nil)
	req2.Header.Set("x-admin-key", "k")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req2)
	if rr.Code != 405 {
		t.Fatalf("GET preview must 405, got %d", rr.Code)
	}
}

func TestRoutePreviewRequiresModel(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/route-preview", strings.NewReader(`{"tools":true}`))
	req.Header.Set("x-admin-key", "k")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("empty model must 400, got %d", rr.Code)
	}
}

func routerReqFor(t *testing.T, model, session string) router.Requirement {
	t.Helper()
	return router.Requirement{Model: model, SessionKey: session}
}
