package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Reconciliation tests for PRs #694/#695 (see
// docs/OPENCODE_RECONCILIATION.md). R3 proves the committed-bytes
// invariant at the gateway level; R7 proves the masked-email
// round-trip fix. All canaries are test-only strings.

// R3 — CRITICAL INVARIANT: no failover after response bytes have been
// committed to the client. The first candidate streams one valid chunk
// and then dies abruptly; the second candidate must never be hit, the
// client keeps the first chunk, and never sees the second answer.
func TestReconciledNoFailoverAfterStreamCommit(t *testing.T) {
	var mu sync.Mutex
	var goHits, genHits int
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		goHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"first-chunk\"},\"finish_reason\":null}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Abrupt death mid-stream: hijack and close, no [DONE].
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
	}))
	defer goSrv.Close()
	genSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		genHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"second-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer genSrv.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 0
	mk := func(id, typ, base string, priority int) config.ProviderConfig {
		p := config.ProviderConfig{ID: id, Name: id, Type: typ, BaseURL: base, APIKey: "test-key-1", AuthMode: "bearer", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Priority: priority, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}
		p.ApplyDefaults()
		return p
	}
	cfg.Providers = []config.ProviderConfig{mk("go", "opencode-go", goSrv.URL, 0), mk("gen", "openai_compatible", genSrv.URL, 1)}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"go/m", "gen/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve", PublicModel: "nexa-opencode", RouteProfile: "profile", Enabled: &trueVal, Protocols: []string{"openai"}}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := conformancePost(t, s, "/v1/chat/completions", `{"model":"nexa-opencode","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := rr.Body.String()
	mu.Lock()
	defer mu.Unlock()
	if goHits != 1 {
		t.Fatalf("first candidate hits = %d, want 1", goHits)
	}
	if genHits != 0 {
		t.Fatalf("FAILOVER AFTER COMMIT: second candidate hit %d times after the client received stream bytes", genHits)
	}
	if !strings.Contains(body, "first-chunk") {
		t.Fatalf("client lost the committed first chunk: status=%d body=%s", rr.Code, body)
	}
	if strings.Contains(body, "second-answer") {
		t.Fatalf("client received the second candidate's answer after commit: body=%s", body)
	}
}

// R7: a masked pool email round-tripped through the UI pool editor
// must not be persisted as the stored email; a genuine new email
// still updates.
func TestReconciledPoolEmailMaskRoundTrip(t *testing.T) {
	const poolEmail = "operator-pool@example.com"
	newServer := func() *Server {
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Providers = []config.ProviderConfig{{
			ID: "pooltest", Name: "Pool Test", Type: "openai_compatible",
			BaseURL: "http://127.0.0.1:9/v1", APIKey: "POOLTEST_PRIMARY_111",
			APIKeyEnv: "POOLTEST_UNSET_ENV_XYZ", AuthMode: "bearer", Enabled: false,
			Models: []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1}},
			Credentials: []config.CredentialConfig{
				{Name: "literal-pool", APIKey: "POOLTEST_POOL_LITERAL_333", Email: poolEmail, Enabled: true},
			},
		}}
		return testGateway(t, cfg)
	}
	serve := func(s *Server, method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, "http://localhost"+path, nil)
		} else {
			r = httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.RemoteAddr = "127.0.0.1:12345"
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		return rr
	}
	masked := func(s *Server) string {
		rr := serve(s, http.MethodGet, "/admin/api/providers/pooltest", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
		}
		var got struct {
			CredentialsMeta []struct {
				EmailMasked string `json:"email_masked"`
			} `json:"credentials_meta"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET decode: %v", err)
		}
		if len(got.CredentialsMeta) != 1 {
			t.Fatalf("credentials_meta = %+v, want 1 entry", got.CredentialsMeta)
		}
		return got.CredentialsMeta[0].EmailMasked
	}
	poolPut := func(email string) string {
		return `{"provider":{"id":"pooltest","name":"Pool Test","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":false,"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}]},"preserve_secret":false,"replace_key":false,"credential_pool":{"replace":true,"entries":[{"index":0,"email":"` + email + `"}]}}`
	}

	t.Run("masked value round-trip leaves stored email unchanged", func(t *testing.T) {
		s := newServer()
		m := masked(s)
		if m == poolEmail || !strings.Contains(m, "•") || !strings.Contains(m, "example.com") {
			t.Fatalf("GET must mask the email, got %q", m)
		}
		rr := serve(s, http.MethodPut, "/admin/api/providers/pooltest", poolPut(m))
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT status=%d body=%s", rr.Code, rr.Body.String())
		}
		live := s.currentConfig().Providers[0]
		if len(live.Credentials) != 1 || live.Credentials[0].Email != poolEmail {
			t.Fatalf("masked round-trip corrupted stored email to %q, want %q", live.Credentials[0].Email, poolEmail)
		}
		if strings.Contains(rr.Body.String(), poolEmail) {
			t.Fatalf("PUT response leaked the full email")
		}
	})
	t.Run("genuine new email still updates", func(t *testing.T) {
		s := newServer()
		rr := serve(s, http.MethodPut, "/admin/api/providers/pooltest", poolPut("new-operator@example.org"))
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT status=%d body=%s", rr.Code, rr.Body.String())
		}
		live := s.currentConfig().Providers[0]
		if len(live.Credentials) != 1 || live.Credentials[0].Email != "new-operator@example.org" {
			t.Fatalf("genuine email update failed, stored %q", live.Credentials[0].Email)
		}
	})
}
