package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestGatewayEndpointAuthRoutingAndRotation(t *testing.T) {
	var failed, passed atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { failed.Add(1); w.WriteHeader(503) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passed.Add(1)
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("gateway key leaked or upstream key lost")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "actual-good-model" {
			t.Errorf("wrong upstream model: %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer good.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.FallbackOnUnknownModel = false
	cfg.Routing.HedgingEnabled = false
	for i, u := range []string{bad.URL, good.URL} {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: fmt.Sprint(i), Name: "test", Type: "openai_compatible", BaseURL: u, APIKey: "upstream-secret", AuthMode: "bearer", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: []string{"actual-bad-model", "actual-good-model"}[i], Enabled: true, Priority: i, Weight: 1}}})
	}
	s := testGateway(t, cfg)
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	rr := call("POST", "/admin/api/endpoint", `{"model":"my-coding"}`, "")
	if rr.Code != 200 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	var ep struct {
		Key string `json:"api_key"`
	}
	json.Unmarshal(rr.Body.Bytes(), &ep)
	if !strings.HasPrefix(ep.Key, "nx_") || len(ep.Key) != 67 {
		t.Fatal("missing generated key")
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("secret response cacheable")
	}
	if got := call("GET", "/v1/models", "", ""); got.Code != 401 {
		t.Fatalf("missing key accepted: %d", got.Code)
	}
	if got := call("GET", "/v1/models", "", ep.Key); got.Code != 200 || !strings.Contains(got.Body.String(), "my-coding") {
		t.Fatalf("model discovery: %s", got.Body)
	}
	if len(s.rt.Candidates(router.Requirement{Model: "my-coding"})) != 2 {
		t.Fatal("virtual model does not include whole pool")
	}
	if len(s.rt.Candidates(router.Requirement{Model: "unknown"})) != 0 {
		t.Fatal("unknown model unexpectedly routed")
	}
	rr = call("POST", "/v1/messages", `{"model":"my-coding","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`, ep.Key)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "hello") || failed.Load() != 1 || passed.Load() != 1 {
		t.Fatalf("failover: %d %s bad=%d good=%d", rr.Code, rr.Body, failed.Load(), passed.Load())
	}
	if got := call("GET", "/admin/api/snapshot", "", ""); strings.Contains(got.Body.String(), ep.Key) {
		t.Fatal("snapshot leaked key")
	}
	saved, err := config.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Routing.PublicModel != "my-coding" || saved.ClientAuth.Keys[0] != ep.Key {
		t.Fatal("endpoint not persisted")
	}
	// Rejected rotation must leave the old key intact in memory and on disk.
	rr = call("POST", "/admin/api/endpoint", `{"model":"bad model","rotate_key":true}`, "")
	if rr.Code != 400 || s.currentConfig().ClientAuth.Keys[0] != ep.Key {
		t.Fatal("invalid mutation changed active key")
	}
	// Future provider models join without alias edits.
	_, err = s.mutateConfig(func(c *config.Config) error {
		c.Providers[1].Models = append(c.Providers[1].Models, config.ModelConfig{ID: "new", Model: "future-model", Enabled: true, Weight: 1})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range s.rt.Candidates(router.Requirement{Model: "my-coding"}) {
		if d.Deployment.Model == "future-model" {
			found = true
		}
	}
	if !found {
		t.Fatal("future model did not join public pool")
	}
	old := ep.Key
	rr = call("POST", "/admin/api/endpoint", `{"model":"my-coding","rotate_key":true}`, "")
	json.Unmarshal(rr.Body.Bytes(), &ep)
	if ep.Key == old {
		t.Fatal("key did not rotate")
	}
	if got := call("GET", "/v1/models", "", old); got.Code != 401 {
		t.Fatal("old key remains accepted")
	}
	if got := call("GET", "/v1/models", "", ep.Key); got.Code != 200 {
		t.Fatal("new key rejected")
	}
}

func TestEndpointRejectsRemoteAndInvalidMutation(t *testing.T) {
	s := testGateway(t, config.Default())
	req := httptest.NewRequest("GET", "http://localhost/admin/api/endpoint", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("remote key access: %d", rr.Code)
	}
}
