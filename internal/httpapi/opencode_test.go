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
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// OpenCode conformance cells. The shipped matrix in conformance_test.go
// is hardcoded to 4 upstream kinds, so opencode gets its own battery
// here: S1-S5 for every client ingress (anthropic, openai_chat,
// openai_responses) against every opencode upstream protocol, with the
// upstream path asserted per cell. All upstreams are httptest fakes;
// no live OpenCode call is made.

// opencodeConformanceFake delegates to the dialect-faithful
// conformanceFake after rewriting the opencode path to the dialect
// path the fake validates, while recording the original path and the
// session header the gateway sent.
type opencodeConformanceFake struct {
	inner *conformanceFake
	mu    sync.Mutex
	paths []string
	sess  []string
}

func (f *opencodeConformanceFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.sess = append(f.sess, r.Header.Get("x-opencode-session"))
	f.mu.Unlock()
	kind, dialectPath := "", ""
	switch r.URL.Path {
	case "/openai/v1/chat/completions":
		kind, dialectPath = "openai_chat", "/v1/chat/completions"
	case "/openai/v1/responses":
		kind, dialectPath = "openai_responses", "/v1/responses"
	case "/anthropic/v1/messages":
		kind, dialectPath = "anthropic", "/v1/messages"
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"unknown opencode path"}}`)
		return
	}
	f.inner.kind = kind
	r.URL.Path = dialectPath
	f.inner.ServeHTTP(w, r)
}

func (f *opencodeConformanceFake) seenPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *opencodeConformanceFake) seenSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sess...)
}

func opencodeGateway(t *testing.T, providerType string, fake http.Handler) *Server {
	t.Helper()
	up := httptest.NewServer(fake)
	t.Cleanup(up.Close)
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "cf", Name: "Conformance", Type: providerType, BaseURL: up.URL,
		APIKey: "test-key-1", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true}}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func TestOpencodeConformanceCells(t *testing.T) {
	// ingress client -> expected opencode upstream path
	expectedPath := map[string]string{
		"anthropic":        "/anthropic/v1/messages",
		"openai_chat":      "/openai/v1/chat/completions",
		"openai_responses": "/openai/v1/responses",
	}
	for _, providerType := range []string{"opencode-console", "opencode-go"} {
		for _, client := range conformanceClients() {
			t.Run(providerType+"/"+client.name, func(t *testing.T) {
				fake := &opencodeConformanceFake{inner: &conformanceFake{}}
				s := opencodeGateway(t, providerType, fake)

				rr := client.text(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S1 text: status=%d body=%s", rr.Code, rr.Body.String())
				}
				if text, _ := client.extract(t, rr.Body.String()); text != "plain-answer" {
					t.Fatalf("S1 text: got %q", text)
				}
				rr = client.tool(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S2 tool: status=%d body=%s", rr.Code, rr.Body.String())
				}
				if _, tool := client.extract(t, rr.Body.String()); tool != "lookup" {
					t.Fatalf("S2 tool: got tool=%q body=%s", tool, rr.Body.String())
				}
				rr = client.result(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S3 result: status=%d body=%s", rr.Code, rr.Body.String())
				}
				if text, _ := client.extract(t, rr.Body.String()); text != "final-answer" {
					t.Fatalf("S3 result: got %q", text)
				}
				saw, survived, _ := fake.inner.stats()
				if !saw || !survived {
					t.Fatalf("S3 result: upstream saw=%v survived=%v", saw, survived)
				}
				rr = client.stream(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S4 stream: status=%d body=%s", rr.Code, rr.Body.String())
				}
				assembled, terminal := client.streamText(t, rr.Body.String())
				if assembled != "Hello world" || !terminal {
					t.Fatalf("S4 stream: assembled=%q terminal=%v body=%s", assembled, terminal, rr.Body.String())
				}
				fake.inner.setFail(http.StatusTooManyRequests)
				rr = client.text(t, s)
				if rr.Code == http.StatusOK || !strings.Contains(rr.Body.String(), "error") {
					t.Fatalf("S5 error: status=%d body=%s", rr.Code, rr.Body.String())
				}
				// Every upstream request in this cell must have hit
				// exactly the opencode path for this ingress — never
				// a generic /v1/... path and never another protocol.
				for _, p := range fake.seenPaths() {
					if p != expectedPath[client.name] {
						t.Fatalf("cell addressed %q, want %q (all paths: %v)", p, expectedPath[client.name], fake.seenPaths())
					}
				}
				if providerType == "opencode-go" {
					sess := fake.seenSessions()
					if len(sess) == 0 {
						t.Fatal("go cell sent no requests")
					}
					for _, h := range sess {
						if h == "" || !strings.HasPrefix(h, "ses_") {
							t.Fatalf("go request missing session header: %q", h)
						}
						if h != sess[0] {
							t.Fatalf("go session header unstable in one cell: %v", sess)
						}
					}
				} else {
					for _, h := range fake.seenSessions() {
						if h != "" {
							t.Fatalf("console request unexpectedly carried a session header: %q", h)
						}
					}
				}
			})
		}
	}
}

// Multiple tool calls in one turn, then a tool-result continuation
// carrying both results.
func TestOpencodeMultipleToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"role":"tool"`) {
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"final-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
			return
		}
		if strings.Contains(string(body), `"tools"`) {
			_, _ = io.WriteString(w, `{"id":"c2","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"t2","type":"function","function":{"name":"lookup2","arguments":"{\"q\":\"y\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":6,"total_tokens":16}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c3","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"plain-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer srv.Close()
	cfg := config.Default()
	p := config.ProviderConfig{ID: "cf", Name: "C", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},{"type":"function","function":{"name":"lookup2","parameters":{"type":"object"}}}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("multi-tool: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("multi-tool: expected 2 tool calls, got body=%s err=%v", rr.Body.String(), err)
	}
	names := []string{resp.Choices[0].Message.ToolCalls[0].Function.Name, resp.Choices[0].Message.ToolCalls[1].Function.Name}
	if names[0] != "lookup" || names[1] != "lookup2" {
		t.Fatalf("multi-tool names = %v", names)
	}
	rr = conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"t2","type":"function","function":{"name":"lookup2","arguments":"{\"q\":\"y\"}"}}]},{"role":"tool","tool_call_id":"t1","content":"out-1"},{"role":"tool","tool_call_id":"t2","content":"out-2"}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "final-answer") {
		t.Fatalf("multi-tool continuation: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// Usage accounting comes only from fields the upstream actually
// supplies; the gateway must not invent tokens.
func TestOpencodeUsageAccounting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/anthropic/v1/messages":
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"fake","content":[{"type":"text","text":"plain-answer"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":5}}`)
		case "/openai/v1/responses":
			_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"fake","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plain-answer"}]}],"usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}}`)
		default:
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"plain-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	p := config.ProviderConfig{ID: "cf", Name: "C", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"prompt_tokens":10`) || !strings.Contains(rr.Body.String(), `"completion_tokens":5`) {
		t.Fatalf("chat usage: status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = conformancePost(t, s, "/v1/messages", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"input_tokens":11`) || !strings.Contains(rr.Body.String(), `"output_tokens":5`) {
		t.Fatalf("anthropic usage: status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = conformancePost(t, s, "/v1/responses", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"input_tokens":8`) || !strings.Contains(rr.Body.String(), `"output_tokens":4`) {
		t.Fatalf("responses usage: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// 403 entitlement: diagnostic, no in-request rotation, no credential
// cooldown, and the next request still reaches the upstream.
func TestOpencodeEntitlementDiagnosticEndToEnd(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"forbidden"}}`)
	}))
	defer srv.Close()
	cfg := config.Default()
	p := config.ProviderConfig{ID: "cf", Name: "C", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer", Enabled: true,
		Credentials: []config.CredentialConfig{{Name: "second", APIKey: "test-key-2", Enabled: true}},
		Models:      []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	for i := 0; i < 2; i++ {
		rr := conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if rr.Code == http.StatusOK {
			t.Fatalf("request %d: entitlement 403 became a 200", i)
		}
		if !strings.Contains(rr.Body.String(), "entitlement") || !strings.Contains(rr.Body.String(), "Go subscription") {
			t.Fatalf("request %d: diagnostic missing: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream hits = %d, want exactly 1 per request (no rotation, no storm)", got)
	}
	a, ok := s.reg.Get("cf")
	if !ok {
		t.Fatal("no live adapter for cf")
	}
	cs, ok := a.(providers.CredentialStateProvider)
	if !ok {
		t.Fatal("adapter does not expose credential states")
	}
	for _, st := range cs.CredentialStates() {
		if st.Cooling {
			t.Errorf("entitlement 403 cooled credential %d", st.Index)
		}
	}
}

// Bounded retries: a 429 across a 2-credential pool costs at most one
// attempt per credential, never a storm.
func TestOpencodeNoRetryStormOn429(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()
	cfg := config.Default()
	p := config.ProviderConfig{ID: "cf", Name: "C", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer", Enabled: true,
		Credentials: []config.CredentialConfig{{Name: "second", APIKey: "test-key-2", Enabled: true}},
		Models:      []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code == http.StatusOK {
		t.Fatalf("429 became a 200: %s", rr.Body.String())
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Fatalf("429 upstream hits = %d, want 2 (one per credential, no storm)", got)
	}
}

// --- admin: single-credential surfaces, G7, discovery -------------------

func TestOpencodeSingleCredentialProvider(t *testing.T) {
	p := config.ProviderConfig{APIKey: "test-key-primary",
		Credentials: []config.CredentialConfig{
			{Name: "one", APIKey: "test-key-1", Enabled: true, Email: "a@example.com"},
			{Name: "two", APIKey: "test-key-2", Enabled: false},
		}}
	if _, name, err := singleCredentialProvider(p, nil); err != nil || name != "primary" {
		t.Fatalf("nil index: name=%q err=%v", name, err)
	}
	cp, name, err := singleCredentialProvider(p, intPtr(0))
	if err != nil || name != "one" {
		t.Fatalf("index 0: name=%q err=%v", name, err)
	}
	if len(cp.Credentials) != 1 || cp.APIKey != "" || cp.Credentials[0].APIKey != "test-key-1" {
		t.Fatalf("single-credential copy wrong: %+v", cp)
	}
	if _, _, err := singleCredentialProvider(p, intPtr(2)); err == nil {
		t.Error("out-of-range index accepted")
	}
	if _, _, err := singleCredentialProvider(p, intPtr(1)); err == nil {
		t.Error("disabled credential accepted for testing")
	}
}

func intPtr(i int) *int { return &i }

func TestOpencodePoolUpdateCarriesMetadata(t *testing.T) {
	old := []config.CredentialConfig{{Name: "one", APIKey: "test-key-1", Enabled: true}}
	prio, wgt := 2, 3.5
	enabled := false
	out, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Entries: []credentialPoolEntry{
		{Index: intPtr(0), Enabled: &enabled, Email: "op@example.com", Priority: &prio, Weight: &wgt},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Enabled || out[0].Email != "op@example.com" || out[0].Priority != 2 || out[0].Weight != 3.5 || out[0].APIKey != "test-key-1" {
		t.Fatalf("pool update lost metadata/secret: %+v", out)
	}
	meta := credentialMeta(out)
	if len(meta) != 1 || meta[0]["email_masked"] != "o•••@example.com" {
		t.Fatalf("credential meta email not masked: %v", meta)
	}
	if strings.Contains(strings.Join([]string{meta[0]["preview"].(string)}, ""), "test-key-1") {
		t.Fatal("credential meta leaked a key")
	}
}

func TestOpencodeAdminProviderTestCredentialIndex(t *testing.T) {
	var auths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	body := `{"provider":{"id":"op1","name":"op1","type":"opencode-console","base_url":"` + srv.URL + `","auth_mode":"bearer","api_key":"test-key-primary","credentials":[{"name":"pool-one","api_key":"test-key-pool1","enabled":true},{"name":"pool-two","api_key":"test-key-pool2","enabled":true}],"models":[{"id":"m","model":"conf-model","enabled":true}]},"test_models":["conf-model"],"credential_index":1}`
	rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-test", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("provider-test status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Results []struct {
			Model           string `json:"model"`
			OK              bool   `json:"ok"`
			CredentialIndex *int   `json:"credential_index"`
			CredentialName  string `json:"credential_name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || len(out.Results) != 1 || !out.Results[0].OK {
		t.Fatalf("provider-test results: %+v err=%v body=%s", out, err, rr.Body.String())
	}
	if out.Results[0].CredentialIndex == nil || *out.Results[0].CredentialIndex != 1 || out.Results[0].CredentialName != "pool-two" {
		t.Fatalf("per-credential reporting wrong: %+v", out.Results[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 1 || auths[0] != "Bearer test-key-pool2" {
		t.Fatalf("provider-test used auths %v, want only the indexed credential", auths)
	}
	// Out-of-range and disabled are 400s, never key leaks.
	rr = coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-test", strings.Replace(body, `"credential_index":1`, `"credential_index":9`, 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range credential_index status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOpencodeAdminG7PrimaryReplacementPreservesPool(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	created := coverageAdminRequest(s, http.MethodPost, "/admin/api/providers", `{"provider":{"id":"op1","name":"op1","type":"opencode-console","base_url":"http://127.0.0.1:9","auth_mode":"bearer","api_key":"test-key-primary","credentials":[{"name":"pool-one","api_key":"test-key-pool1","enabled":true}],"models":[]}}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	// The UI shape for a primary-key replacement: replace_key + the new
	// primary key + credentials:[] and NO credential_pool payload.
	updated := coverageAdminRequest(s, http.MethodPut, "/admin/api/providers/op1", `{"provider":{"id":"op1","name":"op1","type":"opencode-console","base_url":"http://127.0.0.1:9","auth_mode":"bearer","api_key":"test-key-primary-v2","credentials":[],"models":[]},"replace_key":true}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", updated.Code, updated.Body.String())
	}
	got := coverageAdminRequest(s, http.MethodGet, "/admin/api/providers/op1", "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", got.Code, got.Body.String())
	}
	var detail struct {
		Provider struct {
			APIKey      string `json:"api_key"`
			Credentials []struct {
				Name string `json:"name"`
			} `json:"credentials"`
		} `json:"provider"`
		CredentialsMeta []map[string]any `json:"credentials_meta"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.CredentialsMeta) != 1 || detail.CredentialsMeta[0]["name"] != "pool-one" {
		t.Fatalf("G7: pool wiped by primary replacement: meta=%v body=%s", detail.CredentialsMeta, got.Body.String())
	}
	// An explicit pool replace with empty entries DOES clear the pool.
	cleared := coverageAdminRequest(s, http.MethodPut, "/admin/api/providers/op1", `{"provider":{"id":"op1","name":"op1","type":"opencode-console","base_url":"http://127.0.0.1:9","auth_mode":"bearer","models":[]},"credential_pool":{"replace":true,"entries":[]}}`)
	if cleared.Code != http.StatusOK {
		t.Fatalf("pool-clear PUT status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	got = coverageAdminRequest(s, http.MethodGet, "/admin/api/providers/op1", "")
	var detail2 struct {
		CredentialsMeta []map[string]any `json:"credentials_meta"`
	}
	_ = json.Unmarshal(got.Body.Bytes(), &detail2)
	if len(detail2.CredentialsMeta) != 0 {
		t.Fatalf("explicit pool clear did not clear: %v", detail2.CredentialsMeta)
	}
}

func TestOpencodeAdminDiscover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"paid-model-a"},{"id":"paid-model-b"}]}`)
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-discover", `{"provider":{"id":"con","name":"con","type":"opencode-console","base_url":"`+srv.URL+`","auth_mode":"bearer","api_key":"test-key-1"}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "paid-model-a") {
		t.Fatalf("console discover: status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-discover", `{"provider":{"id":"go","name":"go","type":"opencode-go","base_url":"`+srv.URL+`","auth_mode":"bearer","api_key":"test-key-1"}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ok":false`) {
		t.Fatalf("go discover must report unsupported, not invent models: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOpencodeConnectionTestDialects(t *testing.T) {
	var paths, auths, shapes []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		shapes = append(shapes, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/anthropic/v1/messages" {
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`)
		} else {
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
		}
	}))
	defer srv.Close()
	prov := config.ProviderConfig{ID: "op", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-primary", AuthMode: "bearer",
		Credentials: []config.CredentialConfig{{Name: "pool-one", APIKey: "test-key-pool1", Enabled: true}}}
	prov.ApplyDefaults()
	model := config.ModelConfig{ID: "m", Model: "conf-model"}
	rep := runConnectionTest(t.Context(), prov, model, "op/conf-model", "anthropic")
	if !rep.OK || rep.Endpoint != srv.URL+"/anthropic/v1/messages" {
		t.Fatalf("anthropic connection test: %+v", rep)
	}
	rep = runConnectionTest(t.Context(), prov, model, "op/conf-model", "openai")
	if !rep.OK || rep.Endpoint != srv.URL+"/openai/v1/chat/completions" {
		t.Fatalf("openai connection test: %+v", rep)
	}
	mu.Lock()
	if len(paths) != 2 || paths[0] != "/anthropic/v1/messages" || paths[1] != "/openai/v1/chat/completions" {
		mu.Unlock()
		t.Fatalf("connection-test paths = %v", paths)
	}
	if !strings.Contains(shapes[0], `"messages"`) || strings.Contains(shapes[0], `"role":"tool"`) {
		mu.Unlock()
		t.Fatalf("anthropic payload not anthropic-shaped: %s", shapes[0])
	}
	mu.Unlock()
	// Per-credential connection test uses only the indexed credential.
	rep = runConnectionTestWithCredential(t.Context(), prov, model, "op/conf-model", "openai", intPtr(0))
	if !rep.OK || rep.CredentialName != "pool-one" || rep.CredentialIndex == nil || *rep.CredentialIndex != 0 {
		t.Fatalf("per-credential connection test: %+v", rep)
	}
	if auths[2] != "Bearer test-key-pool1" {
		t.Fatalf("per-credential connection test used auth %q", auths[2])
	}
}
