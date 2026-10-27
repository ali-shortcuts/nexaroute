package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func mission95Adapter(t *testing.T, srv *httptest.Server, ptype string, mutate func(*config.ProviderConfig)) *httpAdapter {
	t.Helper()
	p := config.ProviderConfig{ID: "p", Type: ptype, BaseURL: srv.URL, APIKey: "fake-adapter-key", Enabled: true}
	if mutate != nil {
		mutate(&p)
	}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mission95Serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestMission95EgressAndConcurrencyHelpers(t *testing.T) {
	prefixes := parseEgressCIDRs([]string{"10.0.0.0/8", "192.0.2.1", "not-a-cidr"})
	if len(prefixes) != 2 {
		t.Fatalf("parseEgressCIDRs=%v want 2 prefixes (invalid ignored)", prefixes)
	}
	if prefixes[0].String() != "10.0.0.0/8" || prefixes[1].String() != "192.0.2.1/32" {
		t.Fatalf("prefixes=%v want 10.0.0.0/8 and 192.0.2.1/32", prefixes)
	}
	if got := cap(newConcurrencySem(0)); got != 32 {
		t.Fatalf("sem(0) cap=%d want 32", got)
	}
	if got := cap(newConcurrencySem(-3)); got != 32 {
		t.Fatalf("sem(-3) cap=%d want 32", got)
	}
	if got := cap(newConcurrencySem(5000)); got != maxAdapterConcurrency {
		t.Fatalf("sem(5000) cap=%d want %d", got, maxAdapterConcurrency)
	}
	if got := cap(newConcurrencySem(7)); got != 7 {
		t.Fatalf("sem(7) cap=%d want 7", got)
	}
	if got := endpoint("http://h/v1/chat/completions", "/v1/chat/completions"); got != "http://h/v1/chat/completions" {
		t.Fatalf("endpoint already-suffixed=%q", got)
	}
	a := &httpAdapter{p: config.ProviderConfig{BaseURL: "https://example.invalid"}}
	if a.hostAllowed(&url.URL{}) {
		t.Fatal("empty host must not be allowed")
	}
	if _, err := newHTTPAdapterWithRetryCap(config.ProviderConfig{ID: "x", BaseURL: "://bad-url"}, time.Second, time.Minute); err == nil {
		t.Fatal("unparseable base URL must fail adapter construction")
	}
	// A hostname proxy is accepted at construction (its resolved addresses
	// are re-checked at dial time); a loopback proxy IP is blocked outright.
	proxy := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := newHTTPAdapterWithRetryCap(config.ProviderConfig{ID: "x", Type: "openai_compatible", BaseURL: "https://example.invalid", ProxyURL: "http://proxy.example.invalid:8080"}, time.Second, time.Minute); err != nil {
		t.Fatalf("hostname proxy adapter construction failed: %v", err)
	}
	if _, err := newHTTPAdapterWithRetryCap(config.ProviderConfig{ID: "x", Type: "openai_compatible", BaseURL: "https://example.invalid", ProxyURL: proxy.URL}, time.Second, time.Minute); err == nil || !strings.Contains(err.Error(), "is blocked") {
		t.Fatalf("loopback proxy err=%v want blocked", err)
	}
	// Bedrock without an access key id fails closed at construction.
	t.Setenv("MISSION95_BEDROCK_SECRET2", "fake-bedrock-secret-2")
	if _, err := newHTTPAdapterWithRetryCap(config.ProviderConfig{ID: "b", Type: "bedrock", BaseURL: "https://example.invalid", SecretKeyEnv: "MISSION95_BEDROCK_SECRET2"}, time.Second, time.Minute); err == nil || !strings.Contains(err.Error(), "access key id") {
		t.Fatalf("bedrock without access key err=%v want access-key error", err)
	}
}

func TestMission95AdapterStatsEdges(t *testing.T) {
	srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	a := mission95Adapter(t, srv, "openai_compatible", nil)
	// A cooling credential counts, and reservations beyond remaining clamp
	// the effective headroom at zero instead of going negative.
	a.creds[0].CooldownUntil = time.Now().Add(time.Hour)
	a.remainingRequests.Store(5)
	a.reservedRequests.Store(10)
	a.remainingTokens.Store(3)
	a.reservedTokens.Store(9)
	st := a.Stats()
	if st.CredentialsCooling != 1 {
		t.Fatalf("cooling=%d want 1", st.CredentialsCooling)
	}
	if st.EffectiveRemainingRequests != 0 || st.EffectiveRemainingTokens != 0 {
		t.Fatalf("effective=%d/%d want 0/0", st.EffectiveRemainingRequests, st.EffectiveRemainingTokens)
	}
	if st.RemainingRequests != 5 || st.RemainingTokens != 3 {
		t.Fatalf("remaining=%d/%d want raw 5/3", st.RemainingRequests, st.RemainingTokens)
	}
	// CredentialsMatch rejects a key list of a different length.
	if a.CredentialsMatch([]string{"fake-adapter-key", "extra"}) {
		t.Fatal("mismatched credential list length must not match")
	}
}

func TestMission95DoPathTransportEdges(t *testing.T) {
	// Cross-origin redirects are refused, never followed with credentials.
	target := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	redirector := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusFound)
	})
	a := mission95Adapter(t, redirector, "openai_compatible", nil)
	_, err := a.DoPath(context.Background(), http.MethodPost, "/v1/chat/completions", []byte(`{}`), false, nil)
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin") {
		t.Fatalf("cross-origin redirect err=%v want refusal", err)
	}
	// A redirect loop is stopped after 10 hops.
	looper := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	})
	a = mission95Adapter(t, looper, "openai_compatible", nil)
	_, err = a.DoPath(context.Background(), http.MethodPost, "/v1/chat/completions", []byte(`{}`), false, nil)
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("redirect loop err=%v want stop-after-10", err)
	}
	// An invalid method fails at request construction.
	srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	a = mission95Adapter(t, srv, "openai_compatible", nil)
	if _, err := a.DoPath(context.Background(), "bad method", "/v1/chat/completions", []byte(`{}`), false, nil); err == nil {
		t.Fatal("invalid method must fail request construction")
	}
	// A cancelled wait for the concurrency semaphore reports the context.
	a = mission95Adapter(t, srv, "openai_compatible", func(p *config.ProviderConfig) { p.MaxConcurrency = 1 })
	a.sem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = a.DoPath(ctx, http.MethodPost, "/v1/chat/completions", []byte(`{}`), false, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled semaphore wait err=%v want context.Canceled", err)
	}
	if got := a.Stats().WaitingRequests; got != 0 {
		t.Fatalf("waiting=%d want 0 after cancelled wait", got)
	}
	<-a.sem
	// A forwarded Content-Type wins over the JSON default.
	var gotCT string
	ctSrv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	})
	a = mission95Adapter(t, ctSrv, "openai_compatible", func(p *config.ProviderConfig) { p.ForwardHeaders = []string{"Content-Type"} })
	resp, err := a.DoPath(context.Background(), http.MethodPost, "/v1/chat/completions", []byte(`{}`), false, http.Header{"Content-Type": []string{"multipart/form-data; boundary=xyz"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotCT != "multipart/form-data; boundary=xyz" {
		t.Fatalf("forwarded content type=%q want multipart", gotCT)
	}
	// A request to a closed server surfaces a transport error.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	a = mission95Adapter(t, srv, "openai_compatible", nil)
	// Swap the base URL to the dead server after construction.
	a.p.BaseURL = deadURL
	if _, err := a.DoPath(context.Background(), http.MethodPost, "/v1/chat/completions", []byte(`{}`), false, nil); err == nil {
		t.Fatal("request to a closed server must fail")
	}
}

func TestMission95AuthModeInferenceAndBedrockRegion(t *testing.T) {
	cases := []struct {
		ptype  string
		header string
		want   string
	}{
		{"anthropic_compatible", "x-api-key", "fake-adapter-key"},
		{"gemini", "x-goog-api-key", "fake-adapter-key"},
		{"azure_openai", "api-key", "fake-adapter-key"},
		{"openai_compatible", "Authorization", "Bearer fake-adapter-key"},
	}
	for _, tc := range cases {
		var got string
		srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get(tc.header)
			w.WriteHeader(200)
		})
		// AuthMode is deliberately left empty so the adapter infers it
		// from the provider type.
		a := mission95Adapter(t, srv, tc.ptype, func(p *config.ProviderConfig) { p.AuthMode = "" })
		resp, err := a.DoPath(context.Background(), http.MethodPost, "/chat", []byte(`{}`), false, nil)
		if err != nil {
			t.Fatalf("%s: DoPath err=%v", tc.ptype, err)
		}
		_ = resp.Body.Close()
		if got != tc.want {
			t.Fatalf("%s: header %s=%q want %q", tc.ptype, tc.header, got, tc.want)
		}
	}
	// With no credential snapshot, the live resolved API key is used.
	srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer late-fake-key" {
			t.Errorf("late-resolved auth=%q want Bearer late-fake-key", got)
		}
		w.WriteHeader(200)
	})
	p := config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: srv.URL, APIKeyEnv: "MISSION95_LATE_KEY", Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISSION95_LATE_KEY", "late-fake-key")
	resp, err := a.DoPath(context.Background(), http.MethodPost, "/chat", []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Bedrock without a configured region signs for us-east-1.
	var gotAuth string
	bedSrv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	})
	t.Setenv("MISSION95_BEDROCK_SECRET3", "fake-bedrock-secret-3")
	bp := config.ProviderConfig{ID: "b", Type: "bedrock", BaseURL: bedSrv.URL, APIKey: "fake-access-key", SecretKeyEnv: "MISSION95_BEDROCK_SECRET3", Enabled: true}
	ba, err := newHTTPAdapter(bp, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = ba.DoPath(context.Background(), http.MethodPost, "/model/x/converse", []byte(`{"messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(gotAuth, "us-east-1") || !strings.Contains(gotAuth, "AWS4-HMAC-SHA256") {
		t.Fatalf("bedrock auth=%q want SigV4 us-east-1 scope", gotAuth)
	}
	// Bedrock streaming translates the binary event stream into SSE.
	frame := mission95EventFrame(mission95EventStrHeader(":event-type", "message"), []byte(`{"ok":true}`))
	streamSrv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(frame)
	})
	bp.BaseURL = streamSrv.URL
	ba, err = newHTTPAdapter(bp, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = ba.DoPath(context.Background(), http.MethodPost, "/model/x/converse-stream", []byte(`{"messages":[]}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"eventType":"message"`) || !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("bedrock SSE=%s want eventType message with payload", data)
	}
}

func TestMission95CredentialStateEdges(t *testing.T) {
	srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	// Seven credentials with rr=1 make the two probe positions coincide,
	// exercising the bump branch of the least-loaded choice.
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6"}
	p := config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: srv.URL, Enabled: true}
	p.Credentials = make([]config.CredentialConfig, len(keys))
	for i, k := range keys {
		p.Credentials[i] = config.CredentialConfig{APIKey: k, Enabled: true}
	}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.creds) != len(keys) {
		t.Fatalf("creds=%d want %d", len(a.creds), len(keys))
	}
	a.rr = 1
	if _, _, ok := a.reserveCredential(map[int]bool{}); !ok {
		t.Fatal("reservation with 7 available credentials must succeed")
	}
	// Fewer failures beats more failures when active counts tie.
	a2 := mission95Adapter(t, srv, "openai_compatible", nil)
	a2.creds = []credentialState{{Key: "a", Failures: 3}, {Key: "b", Failures: 1}}
	idx, _, ok := a2.reserveCredential(map[int]bool{})
	if !ok || idx != 1 {
		t.Fatalf("least-failures reservation idx=%d ok=%v want 1", idx, ok)
	}
	// Out-of-range credential operations are no-ops, never panics.
	a2.releaseCredential(-1)
	a2.releaseCredential(99)
	a2.cooldownCredential(-1, 429, "")
	a2.cooldownCredential(99, 429, "")
	// A 402 cools for an hour.
	a2.cooldownCredential(0, 402, "")
	if d := time.Until(a2.creds[0].CooldownUntil); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("402 cooldown=%v want ~1h", d)
	}
	// Cooldown increments the failure counter (3 seeded + 1 here).
	if a2.creds[0].Failures != 4 || a2.creds[0].LastStatus != 402 {
		t.Fatalf("402 state=%+v want failures=4 status=402", a2.creds[0])
	}
	// Long snippets truncate with an ellipsis.
	long := strings.Repeat("s", 1000)
	snip := a2.safeSnippet([]byte(long))
	if !strings.HasSuffix(snip, "…") || len(snip) > 772 {
		t.Fatalf("snippet len=%d suffix ellipsis=%v", len(snip), strings.HasSuffix(snip, "…"))
	}
}

func TestMission95QuotaHeaderParsing(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ratelimit-Reset-Requests", "2099-01-01T00:00:00Z")
	if v, ok := parseQuotaResetHeader(h, "x-ratelimit-reset-requests"); !ok || v <= time.Now().Unix() {
		t.Fatalf("RFC3339 reset=%d ok=%v want future unix", v, ok)
	}
	h2 := http.Header{}
	h2.Set("X-Ratelimit-Reset-Requests", "Thu, 01 Jan 2099 00:00:00 GMT")
	if v, ok := parseQuotaResetHeader(h2, "x-ratelimit-reset-requests"); !ok || v <= time.Now().Unix() {
		t.Fatalf("HTTP-date reset=%d ok=%v want future unix", v, ok)
	}
	// Retry-After bounds: a zero cap becomes 60s, an oversized fallback
	// becomes the cap, and an HTTP date beyond the cap becomes the cap.
	if got := parseRetryAfterBounded("3600", 10*time.Second, 0); got != 60*time.Second {
		t.Fatalf("zero cap retry-after=%v want 60s", got)
	}
	if got := parseRetryAfterBounded("", 120*time.Second, 60*time.Second); got != 60*time.Second {
		t.Fatalf("oversized fallback=%v want 60s", got)
	}
	if got := parseRetryAfterBounded("", 0, 60*time.Second); got != 60*time.Second {
		t.Fatalf("zero fallback=%v want 60s", got)
	}
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfterBounded(future, 10*time.Second, 60*time.Second); got != 60*time.Second {
		t.Fatalf("far-future HTTP date=%v want capped 60s", got)
	}
	near := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfterBounded(near, 10*time.Second, 60*time.Second); got < 20*time.Second || got > 30*time.Second {
		t.Fatalf("near HTTP date=%v want ~30s", got)
	}
}

func TestMission95ProbeEnvelopes(t *testing.T) {
	// Bedrock probe body + output envelope.
	var bedrockBody string
	bedSrv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bedrockBody = string(b)
		_, _ = w.Write([]byte(`{"output":{"message":{"content":[{"text":"hi"}]}},"usage":{"inputTokens":1,"outputTokens":2}}`))
	})
	t.Setenv("MISSION95_PROBE_SECRET", "fake-probe-secret")
	bp := config.ProviderConfig{ID: "b", Type: "bedrock", BaseURL: bedSrv.URL, APIKey: "fake-access-key", SecretKeyEnv: "MISSION95_PROBE_SECRET", Enabled: true}
	ba, err := newHTTPAdapter(bp, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, status, err := ba.Probe(context.Background(), "model-x", 999); err != nil || status != 200 {
		t.Fatalf("bedrock probe status=%d err=%v", status, err)
	}
	if !strings.Contains(bedrockBody, "inferenceConfig") || !strings.Contains(bedrockBody, `"maxTokens":1`) {
		t.Fatalf("bedrock probe body=%s want 1-token inferenceConfig", bedrockBody)
	}
	// Gemini probe body + candidates envelope.
	var geminiBody string
	gemSrv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		geminiBody = string(b)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`))
	})
	ga := mission95Adapter(t, gemSrv, "gemini", nil)
	if _, status, err := ga.Probe(context.Background(), "gemini-x", 999); err != nil || status != 200 {
		t.Fatalf("gemini probe status=%d err=%v", status, err)
	}
	if !strings.Contains(geminiBody, "generationConfig") || !strings.Contains(geminiBody, `"maxOutputTokens":1`) {
		t.Fatalf("gemini probe body=%s want 1-token generationConfig", geminiBody)
	}
	// Vertex probe goes through the Vertex model path.
	vertSrv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`))
	})
	va := mission95Adapter(t, vertSrv, "vertex", func(p *config.ProviderConfig) {
		p.VertexProject = "proj"
		p.VertexLocation = "us-central1"
	})
	if _, status, err := va.Probe(context.Background(), "gemini-x", 999); err != nil || status != 200 {
		t.Fatalf("vertex probe status=%d err=%v", status, err)
	}
	// Probe failure shapes.
	fail := func(handler http.HandlerFunc) (int, error) {
		srv := mission95Serve(t, handler)
		a := mission95Adapter(t, srv, "openai_compatible", nil)
		_, status, err := a.Probe(context.Background(), "m", 5)
		return status, err
	}
	if status, err := fail(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}); err == nil || status != 500 || !strings.Contains(err.Error(), "probe http 500") {
		t.Fatalf("500 probe status=%d err=%v want probe http 500", status, err)
	}
	if _, err := fail(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
	}); err == nil || !strings.Contains(err.Error(), "error envelope") {
		t.Fatalf("error-envelope probe err=%v", err)
	}
	if _, err := fail(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, (1<<20)+1))
	}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized probe err=%v want exceeds", err)
	}
	if _, err := fail(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":123}`))
	}); err == nil || !strings.Contains(err.Error(), "invalid OpenAI choices") {
		t.Fatalf("bad choices probe err=%v", err)
	}
	if _, err := fail(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{}]}`))
	}); err == nil || !strings.Contains(err.Error(), "invalid OpenAI chat-completion envelope") {
		t.Fatalf("choiceless probe err=%v", err)
	}
	// Probe against a dead server is a transport error, not a verdict.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	da := mission95Adapter(t, mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {}), "openai_compatible", nil)
	da.p.BaseURL = deadURL
	if _, _, err := da.Probe(context.Background(), "m", 5); err == nil {
		t.Fatal("probe against a dead server must fail")
	}
	// Type-specific envelope failures.
	bedFail := func(body string) error {
		srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		t.Setenv("MISSION95_PROBE_SECRET2", "fake-probe-secret-2")
		bp := config.ProviderConfig{ID: "b", Type: "bedrock", BaseURL: srv.URL, APIKey: "fake-access-key", SecretKeyEnv: "MISSION95_PROBE_SECRET2", Enabled: true}
		ba, err := newHTTPAdapter(bp, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = ba.Probe(context.Background(), "m", 5)
		return err
	}
	if err := bedFail(`{}`); err == nil || !strings.Contains(err.Error(), "no Bedrock output") {
		t.Fatalf("bedrock no-output probe err=%v", err)
	}
	anthFail := func(body string) error {
		srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		a := mission95Adapter(t, srv, "anthropic_compatible", nil)
		_, _, err := a.Probe(context.Background(), "m", 5)
		return err
	}
	if err := anthFail(`{"type":"message","role":"assistant","content":"nope"}`); err == nil || !strings.Contains(err.Error(), "invalid Anthropic content") {
		t.Fatalf("anthropic bad-content probe err=%v", err)
	}
	respFail := func(body string) error {
		srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		a := mission95Adapter(t, srv, "openai_responses", nil)
		_, _, err := a.Probe(context.Background(), "m", 5)
		return err
	}
	if err := respFail(`{}`); err == nil || !strings.Contains(err.Error(), "status/output missing") {
		t.Fatalf("responses missing-envelope probe err=%v", err)
	}
	if err := respFail(`{"status":"completed","output":123}`); err == nil || !strings.Contains(err.Error(), "invalid Responses output") {
		t.Fatalf("responses bad-output probe err=%v", err)
	}
	gemFail := func(body string) (int, error) {
		srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		a := mission95Adapter(t, srv, "gemini", nil)
		_, status, err := a.Probe(context.Background(), "m", 5)
		return status, err
	}
	// A blocked prompt (promptFeedback, no candidates) is a valid envelope.
	if status, err := gemFail(`{"promptFeedback":{"blockReason":"SAFETY"}}`); err != nil || status != 200 {
		t.Fatalf("gemini blocked-prompt probe status=%d err=%v", status, err)
	}
	if _, err := gemFail(`{"candidates":123}`); err == nil || !strings.Contains(err.Error(), "invalid Gemini candidates") {
		t.Fatalf("gemini bad-candidates probe err=%v", err)
	}
}

func TestMission95AzureDefaultVersionAndStreamErrorPaths(t *testing.T) {
	srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	a := mission95Adapter(t, srv, "azure_openai", func(p *config.ProviderConfig) { p.APIVersion = "" })
	got := a.azureDeploymentPath("my model")
	if !strings.Contains(got, "api-version=2024-10-21") || !strings.Contains(got, "deployments/my%20model") {
		t.Fatalf("azure default path=%q want default 2024-10-21 version", got)
	}
	// Stream + invalid method exercises the cancel() in the build-error branch.
	if _, err := a.DoPath(context.Background(), "bad method", "/chat", []byte(`{}`), true, nil); err == nil {
		t.Fatal("stream invalid method must fail")
	}
	// Stream + dead upstream exercises the cancel() in the Do-error branch.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	a.p.BaseURL = deadURL
	if _, err := a.DoPath(context.Background(), http.MethodPost, "/chat", []byte(`{}`), true, nil); err == nil {
		t.Fatal("stream request to a dead server must fail")
	}
}

func TestMission95GeminiProbeCandidatesMissing(t *testing.T) {
	srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	a := mission95Adapter(t, srv, "gemini", nil)
	if _, _, err := a.Probe(context.Background(), "m", 5); err == nil || !strings.Contains(err.Error(), "candidates missing") {
		t.Fatalf("gemini {} probe err=%v want candidates missing", err)
	}
}

func TestMission95ParseModelListCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i := 0; i < 10005; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":"m-%05d"}`, i)
	}
	sb.WriteString(`]}`)
	got := ParseModelList([]byte(sb.String()))
	if len(got) != 10000 {
		t.Fatalf("ParseModelList cap len=%d want 10000", len(got))
	}
	if got[0] != "m-00000" || got[len(got)-1] != "m-09999" {
		t.Fatalf("cap window first=%q last=%q want m-00000/m-09999", got[0], got[len(got)-1])
	}
}
