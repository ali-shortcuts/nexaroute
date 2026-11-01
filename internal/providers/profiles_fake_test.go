package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Fake-upstream protocol tests (Task 5). No live provider is called: the
// profile's own path/auth mapping is pointed at a local httptest server
// and the test asserts the exact request the gateway sends — URL path,
// method, auth header, body — plus the response envelope the adapter
// returns. This tests NexaRoute's wire shape for the profile, NOT the
// upstream provider, and must never be quoted as provider verification.

type fakeCapture struct {
	method string
	path   string
	auth   string
	apiKey string
	ver    string
	body   []byte
}

func runFakeProfile(t *testing.T, profileID, body, respBody string) fakeCapture {
	t.Helper()
	p, ok := ProfileByID(profileID)
	if !ok {
		t.Fatalf("unknown profile %q", profileID)
	}
	cfg, err := p.ToProviderConfig("")
	if err != nil {
		// Templated profiles (Cloudflare) are substituted to a fake host.
		sub := strings.ReplaceAll(strings.ReplaceAll(p.BaseURL, "{account_id}", "ACCT"), "{gateway_id}", "GTW")
		cfg, err = p.ToProviderConfigWithBase(sub, "")
		if err != nil {
			t.Fatalf("profile %q config: %v", profileID, err)
		}
	}
	var cap fakeCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap = fakeCapture{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("x-api-key"), ver: r.Header.Get("anthropic-version"), body: b}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()
	cfg.BaseURL = srv.URL
	cfg.APIKey = "fake-test-key"
	cfg.APIKeyEnv = ""
	a, err := NewAdapter(cfg, 5*time.Second)
	if err != nil {
		t.Fatalf("NewAdapter for %q: %v", profileID, err)
	}
	defer CloseIdleConnections(a)
	ctx := context.Background()
	resp, err := a.DoPath(ctx, http.MethodPost, profileMainPath(p), []byte(body), false, nil)
	if err != nil {
		t.Fatalf("DoPath for %q: %v", profileID, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("profile %q fake upstream status %d: %s", profileID, resp.StatusCode, got)
	}
	return cap
}

func TestFakeUpstreamChatProfile(t *testing.T) {
	cap := runFakeProfile(t, "ashna-chat",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	if cap.method != http.MethodPost || cap.path != "/chat/completions" {
		t.Errorf("ashna-chat request = %s %s, want POST /chat/completions", cap.method, cap.path)
	}
	if cap.auth != "Bearer fake-test-key" {
		t.Errorf("ashna-chat auth = %q, want Bearer fake-test-key", cap.auth)
	}
	var body map[string]any
	if err := json.Unmarshal(cap.body, &body); err != nil || body["model"] != "m" {
		t.Errorf("ashna-chat body = %s", cap.body)
	}
}

func TestFakeUpstreamMessagesProfile(t *testing.T) {
	cap := runFakeProfile(t, "ashna-messages",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		`{"id":"msg-1","type":"message","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	if cap.method != http.MethodPost || cap.path != "/v1/messages" {
		t.Errorf("ashna-messages request = %s %s, want POST /v1/messages", cap.method, cap.path)
	}
	if cap.apiKey != "fake-test-key" {
		t.Errorf("ashna-messages x-api-key = %q, want fake-test-key", cap.apiKey)
	}
	if cap.ver != "2023-06-01" {
		t.Errorf("ashna-messages anthropic-version = %q, want 2023-06-01", cap.ver)
	}
}

func TestFakeUpstreamResponsesProfile(t *testing.T) {
	cap := runFakeProfile(t, "groq-responses",
		`{"model":"m","input":"hi"}`,
		`{"id":"resp-1","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	if cap.method != http.MethodPost || cap.path != "/v1/responses" {
		t.Errorf("groq-responses request = %s %s, want POST /v1/responses", cap.method, cap.path)
	}
	if cap.auth != "Bearer fake-test-key" {
		t.Errorf("groq-responses auth = %q, want Bearer fake-test-key", cap.auth)
	}
}

func TestFakeUpstreamTemplatedProfileSubstituted(t *testing.T) {
	// Cloudflare compat: the template must be substituted before any
	// request, and the substituted profile then sends chat to the
	// compat path with a Bearer token.
	cap := runFakeProfile(t, "cloudflare-compat-chat",
		`{"model":"openai/gpt-x","messages":[{"role":"user","content":"hi"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
	if cap.method != http.MethodPost || cap.path != "/chat/completions" {
		t.Errorf("cloudflare-compat-chat request = %s %s, want POST /chat/completions", cap.method, cap.path)
	}
	if cap.auth != "Bearer fake-test-key" {
		t.Errorf("cloudflare-compat-chat auth = %q, want Bearer fake-test-key", cap.auth)
	}
}

func TestFakeUpstreamOrcaRouterMessages(t *testing.T) {
	cap := runFakeProfile(t, "orcarouter-messages",
		`{"model":"anthropic/claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		`{"content":[{"type":"text","text":"hello"}]}`)
	if cap.path != "/v1/messages" || cap.apiKey != "fake-test-key" {
		t.Errorf("orcarouter-messages request = %s apiKey=%q, want /v1/messages with x-api-key", cap.path, cap.apiKey)
	}
}
