package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestResponsesIngressDispatchesToCanonicalProviderPath(t *testing.T) {
	cases := []struct {
		name, kind, path, reply string
	}{
		{
			"Gemini", "gemini", "/v1beta/models/up-model:generateContent",
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}]}`,
		},
		{
			"Responses native", "openai_responses", "/custom/responses",
			`{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != tc.path {
					t.Errorf("upstream received %q, want %q", r.URL.Path, tc.path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("invalid upstream payload: %v", err)
				}
				if tc.kind == "gemini" && payload["contents"] == nil || tc.kind == "openai_responses" && payload["input"] == nil {
					t.Errorf("wrong upstream protocol: %v", payload)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer up.Close()
			cfg := config.Default()
			cfg.Probe.Enabled = false
			cfg.Providers = []config.ProviderConfig{{
				ID: "p", Type: tc.kind, BaseURL: up.URL, AuthMode: "none", Enabled: true,
				ChatPath: "/wrong/chat", ResponsesPath: "/custom/responses",
				Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1}},
			}}
			s := testGateway(t, cfg)
			s.SyncCapabilityContracts()
			req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
				strings.NewReader(`{"model":"m","input":"hi","max_output_tokens":16}`))
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "hello") || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", rr.Code, calls.Load(), rr.Body.String())
			}
		})
	}
}

func TestCapabilityContractsMatchDeploymentIDAndRefreshOnEdit(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "one", Model: "same-upstream-model", Enabled: true, Weight: 1, ContextWindow: 1024},
			{ID: "two", Model: "same-upstream-model", Enabled: true, Weight: 1, ContextWindow: 2048,
				Capabilities: config.Capabilities{Tools: true, Streaming: true}},
		},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	first := s.capStore.Get("p/one")
	second := s.capStore.Get("p/two")
	if first.Capabilities.Tools != compat.Unsupported || second.Capabilities.Tools != compat.Supported || second.Capabilities.ContextWindow != 2048 {
		t.Fatalf("contracts picked config by upstream model name: first=%+v second=%+v", first.Capabilities, second.Capabilities)
	}
	s.capStore.LearnUnsupported("p/one", compat.CapTemperature, compat.SourceRuntime, "learned", first.InvalidationKey)
	next := s.currentConfig()
	next.Providers[0].Models[1].Capabilities.Tools = false
	next.Providers[0].Models[1].ContextWindow = 0
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	second = s.capStore.Get("p/two")
	if second.Capabilities.Tools != compat.Unsupported || second.Capabilities.ContextWindow != 0 {
		t.Fatalf("model edit kept stale compatibility facts: %+v", second.Capabilities)
	}
	if got := s.capStore.Get("p/one"); got.Capabilities.Temperature != compat.Unsupported {
		t.Fatalf("unaffected deployment lost learned evidence: %+v", got.Capabilities)
	}
}

func TestResponsesPathEditInvalidatesReadinessAndAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Type: "openai_responses", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
		ResponsesPath: "/old", Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	old, _ := s.reg.Get("p")
	if s.hm.Get("p/m").Status != health.Healthy {
		t.Fatal("test deployment should start ready")
	}
	next := s.currentConfig()
	next.Providers[0].ResponsesPath = "/new"
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	newAdapter, _ := s.reg.Get("p")
	if old == newAdapter || s.hm.Get("p/m").Status != health.Unknown {
		t.Fatalf("Responses endpoint edit reused adapter or retained stale readiness: old=%p new=%p state=%+v", old, newAdapter, s.hm.Get("p/m"))
	}
}

func TestCacheSeparatesClientKeyForwardedHeadersAndSession(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"%s-%d"}}]}`, r.Header.Get("x-tenant"), n)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Cache.Enabled = true
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"client-key-alpha", "client-key-bravo"}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		ForwardHeaders: []string{"x-tenant"},
		Models:         []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	request := func(client, tenant, session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("x-api-key", client)
		req.Header.Set("x-tenant", tenant)
		if session != "" {
			req.Header.Set("x-session-id", session)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("client=%q tenant=%q status=%d body=%s", client, tenant, rr.Code, rr.Body.String())
		}
		return rr
	}
	first := request("client-key-alpha", "team-a", "s1")
	if hit := first.Header().Get("X-NexaRoute-Cache"); hit != "" {
		t.Fatalf("first request unexpectedly hit cache: %q", hit)
	}
	second := request("client-key-alpha", "team-a", "s1")
	if second.Header().Get("X-NexaRoute-Cache") != "HIT" || second.Body.String() != first.Body.String() {
		t.Fatalf("identical client request did not hit cache: %s", second.Body.String())
	}
	for _, input := range []struct{ client, tenant, session string }{
		{"client-key-bravo", "team-a", "s1"},
		{"client-key-alpha", "team-b", "s1"},
		{"client-key-alpha", "team-a", "s2"},
	} {
		rr := request(input.client, input.tenant, input.session)
		if rr.Header().Get("X-NexaRoute-Cache") == "HIT" || rr.Body.String() == first.Body.String() {
			t.Fatalf("cache leaked across client/forwarded-header/session scope: %s", rr.Body.String())
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("upstream calls=%d want 4 (one cache hit)", calls.Load())
	}
}

func TestHotReloadRejectsInFlightOldResponseFromCacheAndHealth(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Model == "old-model" {
			close(started)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"%s"}}]}`, payload.Model)
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		up.Close()
	}()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Cache.Enabled = true
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "old-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	request := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	var first *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() { first = request(); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first upstream request never started")
	}
	next := s.currentConfig()
	next.Providers[0].Models[0].Model = "new-model"
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old request never finished")
	}
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "old-model") {
		t.Fatalf("old response status=%d body=%s", first.Code, first.Body.String())
	}
	if st := s.hm.Get("p/m"); st.Status != health.Unknown {
		t.Fatalf("in-flight old-model success re-admitted unprobed new model: %+v", st)
	}
	s.hm.RecordSuccess("p/m", time.Millisecond)
	fresh := request()
	if fresh.Code != http.StatusOK || fresh.Header().Get("X-NexaRoute-Cache") == "HIT" || !strings.Contains(fresh.Body.String(), "new-model") {
		t.Fatalf("new request hit stale old-model cache: status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	if calls.Load() != 2 || request().Header().Get("X-NexaRoute-Cache") != "HIT" {
		t.Fatalf("cache did not store new-model response, upstream calls=%d", calls.Load())
	}
}

func TestHotReloadRejectsInFlightOldProbeEvidence(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Model == "old-model" {
			close(started)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		up.Close()
	}()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "old-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	finished := make(chan struct{})
	go func() { s.probe.RunOnce(context.Background()); close(finished) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old availability probe never started")
	}
	next := s.currentConfig()
	next.Providers[0].Models[0].Model = "new-model"
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("old availability probe never completed")
	}
	if st := s.hm.Get("p/m"); st.Status != health.Unknown {
		t.Fatalf("old-model probe marked new-model ready: %+v", st)
	}
}

func TestResponsesRepairKeepsNativeProviderPath(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/responses" {
			t.Errorf("repair attempt used wrong endpoint: %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("invalid payload: %v", err)
		}
		if calls.Add(1) == 1 {
			if payload["temperature"] == nil {
				t.Error("first attempt omitted requested temperature")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error","param":"temperature"}}`)
			return
		}
		if payload["temperature"] != nil {
			t.Errorf("repaired request retained unsupported temperature: %v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"fixed"}]}]}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_responses", BaseURL: up.URL,
		ChatPath: "/wrong/chat", ResponsesPath: "/custom/responses", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
		strings.NewReader(`{"model":"m","input":"hello","temperature":0.5}`)))
	if rr.Code != http.StatusOK || calls.Load() != 2 || !strings.Contains(rr.Body.String(), "fixed") {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, calls.Load(), rr.Body.String())
	}
}

func TestHotReloadRejectsInFlightOldProviderFailure(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Model == "old-model" {
			close(started)
			<-release
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"old endpoint failed"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"new"}}]}`)
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		up.Close()
	}()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "old-model", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	finished := make(chan struct{})
	go func() {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old request never started")
	}
	next := s.currentConfig()
	next.Providers[0].Models[0].Model = "new-model"
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("old request never completed")
	}
	if st := s.hm.Get("p/m"); st.Status != health.Unknown {
		t.Fatalf("old-model failure quarantined new-model: %+v", st)
	}
	if states := s.hm.ProviderSnapshot(); len(states) != 0 {
		t.Fatalf("old-model failure contaminated provider health: %+v", states)
	}
}

func TestHotReloadRejectsOldRepairEvidence(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/old/responses" {
			t.Errorf("in-flight request used new path: %q", r.URL.Path)
		}
		if calls.Add(1) == 1 {
			close(started)
			<-release
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error","param":"temperature"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		up.Close()
	}()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_responses", BaseURL: up.URL,
		ResponsesPath: "/old/responses", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "model", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	finished := make(chan struct{})
	go func() {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
			strings.NewReader(`{"model":"m","input":"hi","temperature":0.5}`)))
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old request never started")
	}
	next := s.currentConfig()
	next.Providers[0].ResponsesPath = "/new/responses"
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("old repair never completed")
	}
	if calls.Load() != 2 {
		t.Fatalf("repair attempts=%d want two attempts on original adapter", calls.Load())
	}
	if contract := s.capStore.Get("p/m"); contract.Capabilities.Temperature == compat.Unsupported || contract.LastRepair != "" {
		t.Fatalf("old endpoint repair poisoned replacement contract: %+v", contract)
	}
	if st := s.hm.Get("p/m"); st.Status != health.Unknown {
		t.Fatalf("old endpoint response re-admitted replacement: %+v", st)
	}
}

func TestResponsesIngressRejectsMalformedNativeSuccess(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"embedded error", `{"object":"response","status":"completed","output":[],"error":{"message":"bad key"}}`},
		{"unfinished response", `{"object":"response","status":"in_progress","output":[]}`},
		{"missing output", `{"object":"response","status":"completed","output":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer up.Close()
			cfg := config.Default()
			cfg.Probe.Enabled = false
			cfg.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_responses", BaseURL: up.URL,
				AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
			s := testGateway(t, cfg)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
				strings.NewReader(`{"model":"m","input":"hi"}`)))
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("malformed HTTP 200 was treated as successful: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if st := s.hm.Get("p/m"); st.Status != health.Degraded {
				t.Fatalf("invalid response left deployment ready: %+v", st)
			}
		})
	}
}
