package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

const paywallCompletion = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The account behind this API key doesn't have enough credits. Please top up, then try again."},"finish_reason":"stop"}]}`

const cleanCompletion = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`

func testProviderConfig(url, key string, pool ...config.CredentialConfig) config.ProviderConfig {
	return config.ProviderConfig{
		ID: "p", Name: "p", Type: "openai_compatible", BaseURL: url,
		APIKey: key, Credentials: pool, AuthMode: "bearer",
		Enabled: true, MaxConcurrency: 4,
	}
}

func TestAdapterCoolsKeyOn200Paywall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, paywallCompletion)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(testProviderConfig(srv.URL, "dead-key"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x"}`), false, nil)
	if err != nil {
		t.Fatalf("single-key logical error must return the replayed response, got err %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != paywallCompletion {
		t.Fatalf("peek must replay the body untouched, got %q", body)
	}
	if st := a.Stats(); st.CredentialsCooling != 1 {
		t.Fatalf("quota paywall must cool the key, stats=%+v", st)
	}
	// The dead key stays cooled: the next request fails fast instead of
	// hammering a key with no credits.
	if _, err := a.Do(context.Background(), []byte(`{"model":"x"}`), false, nil); err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("expected cooling-down error, got %v", err)
	}
}

func TestAdapterRotatesKeyOn200Paywall(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, k)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if k == "Bearer dead" {
			_, _ = io.WriteString(w, paywallCompletion)
			return
		}
		_, _ = io.WriteString(w, cleanCompletion)
	}))
	defer srv.Close()
	p := testProviderConfig(srv.URL, "dead", config.CredentialConfig{Name: "good", APIKey: "good", Enabled: true})
	a, err := newHTTPAdapter(p, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x"}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != cleanCompletion {
		t.Fatalf("expected rotation to the good key, got %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "Bearer dead" || seen[1] != "Bearer good" {
		t.Fatalf("unexpected credential order: %#v", seen)
	}
	if st := a.Stats(); st.CredentialsCooling != 1 {
		t.Fatalf("dead key must be cooling, stats=%+v", st)
	}
}

func TestAdapter429QuotaEarnsLongCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(testProviderConfig(srv.URL, "k"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	a.credMu.RLock()
	left := time.Until(a.creds[0].CooldownUntil)
	a.credMu.RUnlock()
	if left < 30*time.Minute {
		t.Fatalf("quota 429 must cool ~1h like 402, got %v", left)
	}
}

func TestAdapter429ThrottleKeepsShortCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached, please try again.","code":"rate_limit_exceeded"}}`)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(testProviderConfig(srv.URL, "k"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	a.credMu.RLock()
	left := time.Until(a.creds[0].CooldownUntil)
	a.credMu.RUnlock()
	if left <= 0 || left > 61*time.Second {
		t.Fatalf("plain throttle must keep the short Retry-After cooldown, got %v", left)
	}
}

func TestAdapterDeploymentScopedLogicalErrorSkipsKeyCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"error"}]}`)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(testProviderConfig(srv.URL, "k"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a.noteCredentialFailure(0, 200) // Failures=1: a success mark would reset it.
	resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st := a.Stats(); st.CredentialsCooling != 0 {
		t.Fatalf("deployment-scoped logical error must not cool the key, stats=%+v", st)
	}
	a.credMu.RLock()
	failures := a.creds[0].Failures
	a.credMu.RUnlock()
	if failures != 2 {
		t.Fatalf("logical error must record a credential failure (not success), failures=%d", failures)
	}
}

func TestAdapterSSEWatcherCoolsKeyOnErrorChunk(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: {\"error\":{\"message\":\"Insufficient credits.\",\"code\":402}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()
	p := testProviderConfig(srv.URL, "k")
	p.StreamIdleTimeoutSeconds = 30
	a, err := newHTTPAdapter(p, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != stream {
		t.Fatalf("watcher must pass bytes through untouched, got %q", body)
	}
	if st := a.Stats(); st.CredentialsCooling != 1 {
		t.Fatalf("mid-stream quota error must cool the key, stats=%+v", st)
	}
}

func TestAdapterSSEWatcherLeavesCleanStreamAlone(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()
	p := testProviderConfig(srv.URL, "k")
	p.StreamIdleTimeoutSeconds = 30
	a, err := newHTTPAdapter(p, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if st := a.Stats(); st.CredentialsCooling != 0 {
		t.Fatalf("clean stream must not cool the key, stats=%+v", st)
	}
	a.credMu.RLock()
	failures := a.creds[0].Failures
	a.credMu.RUnlock()
	if failures != 0 {
		t.Fatalf("clean stream must keep zero credential failures, got %d", failures)
	}
}

func TestAdapterPeekPreservesLargeBody(t *testing.T) {
	big := `{"choices":[{"message":{"role":"assistant","content":"` + strings.Repeat("x", 200<<10) + `"},"finish_reason":"stop"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(testProviderConfig(srv.URL, "k"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != big {
		t.Fatalf("large body corrupted by peek: %d vs %d bytes", len(body), len(big))
	}
}

func TestProbeDetectsPaywallContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, paywallCompletion)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(testProviderConfig(srv.URL, "k"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = a.Probe(context.Background(), "m", 16)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("probe must fail on paywall content, got %v", err)
	}
}

func TestProbeLabelsNon2xxClass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = io.WriteString(w, `{"error":{"message":"Insufficient Balance","code":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(testProviderConfig(srv.URL, "k"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, status, err := a.Probe(context.Background(), "m", 1)
	if err == nil || status != 402 || !strings.Contains(err.Error(), "(quota)") {
		t.Fatalf("probe must label the quota class, got status=%d err=%v", status, err)
	}
}
