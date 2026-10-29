package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Fake keys only. No live OpenCode call is ever made by these tests:
// every upstream is an httptest server.

// --- path construction -------------------------------------------------

func TestOpencodeEndpointConstruction(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://opencode.ai/inference", "/openai/v1/chat/completions", "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/inference/", "/openai/v1/chat/completions", "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/inference/go", "/openai/v1/responses", "https://opencode.ai/inference/go/openai/v1/responses"},
		{"https://opencode.ai/inference/go/", "/anthropic/v1/messages", "https://opencode.ai/inference/go/anthropic/v1/messages"},
		// Base already containing the protocol segment is idempotent.
		{"https://opencode.ai/inference/openai/v1/chat/completions", "/openai/v1/chat/completions", "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/inference", "/v1/models", "https://opencode.ai/inference/v1/models"},
	}
	for _, c := range cases {
		if got := endpoint(c.base, c.path); got != c.want {
			t.Errorf("endpoint(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

func TestOpencodePathForIngress(t *testing.T) {
	cases := map[string]string{
		IngressOpenAIChat:      config.OpencodeChatPath,
		IngressOpenAIResponses: config.OpencodeResponsesPath,
		IngressAnthropic:       config.OpencodeMessagesPath,
	}
	for kind, want := range cases {
		got, ok := OpencodePathForIngress(kind)
		if !ok || got != want {
			t.Errorf("OpencodePathForIngress(%q) = %q, %v; want %q, true", kind, got, ok, want)
		}
	}
	if _, ok := OpencodePathForIngress("gemini"); ok {
		t.Error("unknown ingress kind unexpectedly mapped")
	}
}

func TestOpencodeApplyDefaults(t *testing.T) {
	goProv := config.ProviderConfig{ID: "go", Type: "opencode-go", BaseURL: "https://opencode.ai/inference/go"}
	goProv.ApplyDefaults()
	if goProv.ChatPath != config.OpencodeChatPath || goProv.ResponsesPath != config.OpencodeResponsesPath || goProv.MessagesPath != config.OpencodeMessagesPath {
		t.Errorf("go paths = %q %q %q", goProv.ChatPath, goProv.ResponsesPath, goProv.MessagesPath)
	}
	if goProv.ModelsPath != "" {
		t.Errorf("go ModelsPath = %q, want empty (no documented discovery)", goProv.ModelsPath)
	}
	con := config.ProviderConfig{ID: "con", Type: "opencode-console", BaseURL: "https://opencode.ai/inference"}
	con.ApplyDefaults()
	if con.ModelsPath != config.OpencodeModelsPath {
		t.Errorf("console ModelsPath = %q, want %q", con.ModelsPath, config.OpencodeModelsPath)
	}
	// An explicitly configured non-default path is kept, not forced.
	custom := config.ProviderConfig{ID: "c", Type: "opencode-go", BaseURL: "https://example.invalid", ChatPath: "/custom/chat"}
	custom.ApplyDefaults()
	if custom.ChatPath != "/custom/chat" {
		t.Errorf("custom ChatPath overwritten: %q", custom.ChatPath)
	}
	// A stale generic path from before these types existed is repaired.
	stale := config.ProviderConfig{ID: "s", Type: "opencode-console", BaseURL: "https://opencode.ai/inference", ChatPath: "/v1/chat/completions"}
	stale.ApplyDefaults()
	if stale.ChatPath != config.OpencodeChatPath {
		t.Errorf("stale generic ChatPath not repaired: %q", stale.ChatPath)
	}
}

func TestOpencodePresets(t *testing.T) {
	presets := Presets()
	byID := map[string]Preset{}
	for _, p := range presets {
		byID[p.ID] = p
	}
	con, ok := byID["opencode-console"]
	if !ok {
		t.Fatal("opencode-console preset missing")
	}
	if con.BaseURL != "https://opencode.ai/inference" || con.ChatPath != config.OpencodeChatPath || con.ModelsPath != config.OpencodeModelsPath || !con.SupportsDiscovery {
		t.Errorf("console preset wrong: %+v", con)
	}
	foundModelsList := false
	for _, c := range con.Capabilities {
		if c == CapabilityModelsList {
			foundModelsList = true
		}
	}
	if !foundModelsList {
		t.Errorf("console preset missing models_list capability: %v", con.Capabilities)
	}
	goP, ok := byID["opencode-go"]
	if !ok {
		t.Fatal("opencode-go preset missing")
	}
	if goP.BaseURL != "https://opencode.ai/inference/go" || goP.SupportsDiscovery {
		t.Errorf("go preset wrong: %+v", goP)
	}
	for _, c := range goP.Capabilities {
		if c == CapabilityModelsList {
			t.Errorf("go preset must not claim models_list: %v", goP.Capabilities)
		}
	}
	if len(presets) != 33 {
		t.Errorf("preset count = %d, want 33 (31 + 2 opencode)", len(presets))
	}
}

// --- session header ----------------------------------------------------

func opencodeFakeServer(t *testing.T, paths *[]string, headers *[]http.Header, keys *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		*headers = append(*headers, r.Header.Clone())
		*keys = append(*keys, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{}}`)
	}))
}

func doOpencode(t *testing.T, a *httpAdapter, ctx context.Context, path string) {
	t.Helper()
	resp, err := a.DoPath(ctx, http.MethodPost, path, []byte(`{"model":"m1","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
}

func TestOpencodeGoSessionHeader(t *testing.T) {
	var paths []string
	var headers []http.Header
	var keys []string
	srv := opencodeFakeServer(t, &paths, &headers, &keys)
	defer srv.Close()
	p := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer"}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctxA := WithSessionKey(context.Background(), "conversation-aaa")
	ctxB := WithSessionKey(context.Background(), "conversation-bbb")
	doOpencode(t, a, ctxA, config.OpencodeChatPath)
	doOpencode(t, a, ctxA, config.OpencodeChatPath)
	doOpencode(t, a, ctxB, config.OpencodeChatPath)
	if len(headers) != 3 {
		t.Fatalf("requests = %d", len(headers))
	}
	h1, h2, h3 := headers[0].Get("x-opencode-session"), headers[1].Get("x-opencode-session"), headers[2].Get("x-opencode-session")
	if h1 == "" || !strings.HasPrefix(h1, "ses_") {
		t.Fatalf("missing/malformed session header: %q", h1)
	}
	if h1 != h2 {
		t.Errorf("same conversation produced different headers: %q vs %q", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("different conversations produced the same header: %q", h1)
	}
	if paths[0] != "/openai/v1/chat/completions" {
		t.Errorf("chat path = %q", paths[0])
	}
	// Fallback determinism: no session key -> provider|model derivation.
	var fbHeaders []http.Header
	var fbPaths []string
	var fbKeys []string
	srv2 := opencodeFakeServer(t, &fbPaths, &fbHeaders, &fbKeys)
	defer srv2.Close()
	p2 := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: srv2.URL, APIKey: "test-key-1", AuthMode: "bearer"}
	a2, err := newHTTPAdapter(p2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	doOpencode(t, a2, context.Background(), config.OpencodeChatPath)
	doOpencode(t, a2, context.Background(), config.OpencodeChatPath)
	if fbHeaders[0].Get("x-opencode-session") != fbHeaders[1].Get("x-opencode-session") {
		t.Error("fallback session header not deterministic")
	}
	if fbHeaders[0].Get("x-opencode-session") != DeriveOpencodeSessionValue(p2, "", "m1") {
		t.Errorf("fallback header = %q, want derived %q", fbHeaders[0].Get("x-opencode-session"), DeriveOpencodeSessionValue(p2, "", "m1"))
	}
}

func TestOpencodeSessionHeaderClientValueWinsAndConsoleGetsNone(t *testing.T) {
	var headers []http.Header
	var paths []string
	var keys []string
	srv := opencodeFakeServer(t, &paths, &headers, &keys)
	defer srv.Close()
	// Go with a client-supplied header via ForwardHeaders: unchanged.
	p := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer", ForwardHeaders: []string{"x-opencode-session"}}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fwd := http.Header{}
	fwd.Set("x-opencode-session", "ses_client_supplied_value")
	resp, err := a.DoPath(WithSessionKey(context.Background(), "conversation-aaa"), http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"m1"}`), false, fwd)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if headers[0].Get("x-opencode-session") != "ses_client_supplied_value" {
		t.Errorf("client header overwritten: %q", headers[0].Get("x-opencode-session"))
	}
	// Console: no gateway-derived header even with a session key.
	var conHeaders []http.Header
	var conPaths []string
	var conKeys []string
	srv2 := opencodeFakeServer(t, &conPaths, &conHeaders, &conKeys)
	defer srv2.Close()
	pc := config.ProviderConfig{ID: "con1", Type: "opencode-console", BaseURL: srv2.URL, APIKey: "test-key-1", AuthMode: "bearer"}
	ac, err := newHTTPAdapter(pc, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	doOpencode(t, ac, WithSessionKey(context.Background(), "conversation-aaa"), config.OpencodeChatPath)
	if conHeaders[0].Get("x-opencode-session") != "" {
		t.Errorf("console unexpectedly got a session header: %q", conHeaders[0].Get("x-opencode-session"))
	}
	// Generic provider: no header either.
	var genHeaders []http.Header
	var genPaths []string
	var genKeys []string
	srv3 := opencodeFakeServer(t, &genPaths, &genHeaders, &genKeys)
	defer srv3.Close()
	pg := config.ProviderConfig{ID: "gen1", Type: "openai_compatible", BaseURL: srv3.URL, APIKey: "test-key-1", AuthMode: "bearer"}
	ag, err := newHTTPAdapter(pg, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	doOpencode(t, ag, WithSessionKey(context.Background(), "conversation-aaa"), "/v1/chat/completions")
	if genHeaders[0].Get("x-opencode-session") != "" {
		t.Errorf("generic provider unexpectedly got a session header: %q", genHeaders[0].Get("x-opencode-session"))
	}
}

// --- selection: priority, weight, affinity, legacy exactness ----------

func TestCredentialPriorityTier(t *testing.T) {
	a := &httpAdapter{creds: []credentialState{
		{Key: "low-prio", Priority: 1, Weight: 1},
		{Key: "high-prio", Priority: 0, Weight: 1},
		{Key: "high-prio-2", Priority: 0, Weight: 1},
	}}
	for i := 0; i < 6; i++ {
		idx, key, ok := a.reserveCredential(nil)
		if !ok {
			t.Fatal("no credential selected")
		}
		if key == "low-prio" {
			t.Fatalf("lower-priority tier selected at iteration %d (idx %d)", i, idx)
		}
		a.releaseCredential(idx)
	}
}

func TestCredentialWeightLoad(t *testing.T) {
	a := &httpAdapter{creds: []credentialState{
		{Key: "light", Weight: 1, Active: 4},
		{Key: "heavy", Weight: 4, Active: 4},
	}}
	// rr=0 seeds candidates (0,1); effective loads are 4 vs 1, so the
	// heavier-weighted credential wins despite equal Active counts.
	idx, key, ok := a.reserveCredential(nil)
	if !ok || key != "heavy" {
		t.Fatalf("weighted selection idx=%d key=%q ok=%v, want heavy", idx, key, ok)
	}
	sel, ok := a.LastCredentialSelection()
	if !ok || sel.Reason != "least-loaded" || sel.Tier != 0 {
		t.Fatalf("selection explanation = %+v, %v", sel, ok)
	}
}

func TestCredentialLegacySelectionUnchanged(t *testing.T) {
	// All Priority 0, Weight 1: the tournament must behave exactly as
	// before (alternating least-loaded picks on a fresh adapter).
	a := &httpAdapter{creds: []credentialState{{Key: "a", Weight: 1}, {Key: "b", Weight: 1}}}
	first, _, ok := a.reserveCredential(nil)
	if !ok || first != 0 {
		t.Fatalf("first pick = %d, ok=%v, want 0", first, ok)
	}
	second, _, ok := a.reserveCredential(nil)
	if !ok || second != 1 {
		t.Fatalf("second pick = %d, ok=%v, want 1 (least-loaded alternation)", second, ok)
	}
	a.releaseCredential(first)
	a.releaseCredential(second)
}

func TestCredentialSessionAffinityAndFallback(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[],"usage":{}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "aff", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer",
		Credentials: []config.CredentialConfig{{Name: "second", APIKey: "test-key-2", Enabled: true}}}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ha := a
	ctx := WithSessionKey(context.Background(), "session-xyz")
	for i := 0; i < 3; i++ {
		resp, err := ha.DoPath(ctx, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"m"}`), false, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("session affinity not stable: %v", keys)
	}
	if sel, ok := ha.LastCredentialSelection(); !ok || sel.Reason != "affinity" {
		t.Fatalf("second+ selections should explain affinity, got %+v %v", sel, ok)
	}
	// Cool the pinned credential: the next session request falls back
	// to the other credential instead of waiting out the cooldown.
	ha.credMu.RLock()
	pinned := -1
	for i, c := range ha.creds {
		if c.Key == strings.TrimPrefix(keys[0], "Bearer ") {
			pinned = i
		}
	}
	ha.credMu.RUnlock()
	if pinned < 0 {
		t.Fatal("pinned credential not found")
	}
	ha.cooldownCredential(pinned, 429, "")
	resp, err := ha.DoPath(ctx, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"m"}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if keys[3] == keys[0] {
		t.Errorf("session request reused a cooling credential: %v", keys)
	}
}

func TestCredentialCooldownExpiryAutoRecovery(t *testing.T) {
	a := &httpAdapter{creds: []credentialState{{Key: "k", Weight: 1}}}
	a.cooldownCredential(0, 429, "")
	if _, _, ok := a.reserveCredential(nil); ok {
		t.Fatal("cooling credential was selected")
	}
	// Expiry restores availability with no operator action, and the
	// config entry is untouched (Enabled is operator state, never
	// flipped by cooldown).
	a.credMu.Lock()
	a.creds[0].CooldownUntil = time.Now().Add(-time.Minute)
	a.credMu.Unlock()
	idx, _, ok := a.reserveCredential(nil)
	if !ok || idx != 0 {
		t.Fatalf("expired cooldown did not auto-recover: ok=%v idx=%d", ok, idx)
	}
}

// --- 403 entitlement vs generic 403 ------------------------------------

func TestOpencode403NoCooldownNoRotation(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"message":"forbidden"}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "op", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer",
		Credentials: []config.CredentialConfig{{Name: "second", APIKey: "test-key-2", Enabled: true}}}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ha := a
	resp, err := ha.DoPath(context.Background(), http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"restricted-model"}`), false, nil)
	if err != nil {
		t.Fatalf("opencode 403 should return the response, not an error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("opencode 403 rotated across credentials: %d upstream hits", hits.Load())
	}
	states := ha.CredentialStates()
	for _, st := range states {
		if st.Cooling {
			t.Errorf("opencode 403 cooled credential %d", st.Index)
		}
		if st.Failures != 0 {
			t.Errorf("opencode 403 counted a failure on credential %d", st.Index)
		}
	}
	if states[0].LastStatus != http.StatusForbidden {
		t.Errorf("used credential last_status = %d, want 403 recorded", states[0].LastStatus)
	}
}

func TestGeneric403StillCools(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"message":"forbidden"}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "gen", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer",
		Credentials: []config.CredentialConfig{{Name: "second", APIKey: "test-key-2", Enabled: true}}}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ha := a
	resp, err := ha.DoPath(context.Background(), http.MethodPost, "/v1/chat/completions", []byte(`{"model":"m"}`), false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if hits.Load() != 2 {
		t.Fatalf("generic 403 failover hits = %d, want 2 (both credentials tried)", hits.Load())
	}
	for _, st := range ha.CredentialStates() {
		if !st.Cooling || st.Failures != 1 {
			t.Errorf("generic 403 state wrong: %+v", st)
		}
	}
}

func TestOpencode403ModelScopedNotPoisoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "restricted-model" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"message":"forbidden"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[],"usage":{}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "op", Type: "opencode-go", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer"}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ha := a
	resp, err := ha.DoPath(context.Background(), http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"restricted-model"}`), false, nil)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restricted model: err=%v status=%d", err, resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp, err = ha.DoPath(context.Background(), http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"allowed-model"}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("allowed model on the same key after a 403: status=%d (key poisoned)", resp.StatusCode)
	}
}

func TestCredential401DoesNotDisableOrDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid key"}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "op", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer",
		Credentials: []config.CredentialConfig{{Name: "second", APIKey: "test-key-2", Enabled: true}}}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ha := a
	resp, err := ha.DoPath(context.Background(), http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"m"}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	// 401 cools (generic auth failure for opencode is NOT the
	// model-entitlement case, which is 403-only) but never deletes or
	// disables the config entry.
	if len(p.Credentials) != 1 || !p.Credentials[0].Enabled {
		t.Errorf("401 mutated the config entry: %+v", p.Credentials)
	}
	if len(ha.CredentialStates()) != 2 {
		t.Errorf("401 removed an adapter credential: %d states", len(ha.CredentialStates()))
	}
}

// --- Retry-After ---------------------------------------------------------

func TestRetryAfterBothForms(t *testing.T) {
	if got := parseRetryAfterBounded("5", 60*time.Second, 60*time.Second); got != 5*time.Second {
		t.Errorf("delta-seconds: %v", got)
	}
	if got := parseRetryAfterBounded("3600", 60*time.Second, 60*time.Second); got != 60*time.Second {
		t.Errorf("delta-seconds over cap: %v", got)
	}
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfterBounded(future, 60*time.Second, 60*time.Second); got < 5*time.Second || got > 10*time.Second {
		t.Errorf("HTTP-date: %v", got)
	}
	farFuture := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfterBounded(farFuture, 60*time.Second, 60*time.Second); got != 60*time.Second {
		t.Errorf("HTTP-date over cap: %v", got)
	}
	if got := parseRetryAfterBounded("garbage", 60*time.Second, 60*time.Second); got != 60*time.Second {
		t.Errorf("garbage fallback: %v", got)
	}
	if got := parseRetryAfterBounded("", 60*time.Second, 60*time.Second); got != 60*time.Second {
		t.Errorf("empty fallback: %v", got)
	}
	// Cooldown honours the parsed value.
	a := &httpAdapter{creds: []credentialState{{Key: "k", Weight: 1}}, retryAfterCap: 60 * time.Second}
	a.cooldownCredential(0, 429, "2")
	remaining := time.Until(a.creds[0].CooldownUntil)
	if remaining < time.Second || remaining > 3*time.Second {
		t.Errorf("429 cooldown remaining = %v, want ~2s", remaining)
	}
}

// --- redaction -----------------------------------------------------------

func TestCredentialStatesRedactedAndMaskEmail(t *testing.T) {
	if got := MaskEmail("operator@example.com"); got != "o•••@example.com" {
		t.Errorf("MaskEmail = %q", got)
	}
	if got := MaskEmail(""); got != "" {
		t.Errorf("MaskEmail empty = %q", got)
	}
	if got := MaskEmail("not-an-email"); got != "••••" {
		t.Errorf("MaskEmail invalid = %q", got)
	}
	a := &httpAdapter{creds: []credentialState{
		{Key: "test-key-secret", Name: "acct", PoolIndex: 0, Priority: 0, Weight: 1},
	}}
	b, _ := json.Marshal(a.CredentialStates())
	if strings.Contains(string(b), "test-key-secret") {
		t.Errorf("credential state leaked a key: %s", b)
	}
}

// --- discovery -----------------------------------------------------------

func TestOpencodeDiscoveryConsoleAndGo(t *testing.T) {
	if !SupportsDiscovery(config.ProviderConfig{Type: "opencode-console", ModelsPath: "/v1/models"}) {
		t.Error("console should support discovery")
	}
	if SupportsDiscovery(config.ProviderConfig{Type: "opencode-go", ModelsPath: "/v1/models"}) {
		t.Error("go must not support discovery even with a models path set")
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("discovery path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"paid-model-a"},{"id":"paid-model-b"}]}`)
	}))
	defer srv.Close()
	models, status, err := DiscoverModels(context.Background(), config.ProviderConfig{ID: "con", Type: "opencode-console", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer"})
	if err != nil || status != 200 || len(models) != 2 {
		t.Fatalf("console discovery: models=%v status=%d err=%v", models, status, err)
	}
	// Go discovery makes no network call at all.
	hits.Store(0)
	_, _, err = DiscoverModels(context.Background(), config.ProviderConfig{ID: "go", Type: "opencode-go", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer"})
	if err == nil {
		t.Fatal("go discovery unexpectedly succeeded")
	}
	if hits.Load() != 0 {
		t.Errorf("go discovery made %d network calls", hits.Load())
	}
}

// --- consolidated session contract (canonical #694 + #695) -------------

// Eligible-call gating, invalid-input fall-through, caller-scope
// isolation, and no-leak rules for x-opencode-session. Every case runs
// against a capturing fake upstream: the header the fake actually
// received is the assertion, never the adapter's intent.
func TestOpencodeSessionGatingScopeAndLeak(t *testing.T) {
	newAdapter := func(t *testing.T, p config.ProviderConfig) (*httpAdapter, *[]http.Header, *[]string) {
		t.Helper()
		var headers []http.Header
		var paths []string
		var keys []string
		srv := opencodeFakeServer(t, &paths, &headers, &keys)
		t.Cleanup(srv.Close)
		p.BaseURL = srv.URL
		a, err := newHTTPAdapter(p, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return a, &headers, &paths
	}
	call := func(t *testing.T, a *httpAdapter, ctx context.Context, method, path string, fwd http.Header) {
		t.Helper()
		resp, err := a.DoPath(ctx, method, path, []byte(`{"model":"m1","messages":[]}`), false, fwd)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	goCfg := func() config.ProviderConfig {
		return config.ProviderConfig{ID: "go1", Type: "opencode-go", APIKey: "test-key-1", AuthMode: "bearer"}
	}

	t.Run("eligible inference paths get header, ineligible get none", func(t *testing.T) {
		a, headers, _ := newAdapter(t, goCfg())
		ctx := WithSessionKey(context.Background(), "conversation-aaa")
		for _, path := range []string{config.OpencodeChatPath, config.OpencodeResponsesPath, config.OpencodeMessagesPath} {
			call(t, a, ctx, http.MethodPost, path, nil)
		}
		for _, h := range *headers {
			if h.Get("x-opencode-session") == "" {
				t.Fatalf("eligible path got no session header")
			}
		}
		a2, headers2, _ := newAdapter(t, goCfg())
		calls := []struct {
			method, path string
		}{
			{http.MethodGet, "/v1/models"},
			{http.MethodPost, "/v1/embeddings"},
			{http.MethodPost, "/v1/messages/count_tokens"},
			{http.MethodPost, "/v1/images/generations"},
			{http.MethodPost, "/v1/audio/speech"},
			{http.MethodPost, "/v1/audio/transcriptions"},
			{http.MethodPost, "/v1/rerank"},
		}
		for _, c := range calls {
			call(t, a2, ctx, c.method, c.path, nil)
		}
		for i, h := range *headers2 {
			if h.Get("x-opencode-session") != "" {
				t.Fatalf("ineligible call %d (%s %s) leaked session header %q", i, calls[i].method, calls[i].path, h.Get("x-opencode-session"))
			}
		}
	})

	t.Run("invalid client value falls through to derived, never upstream", func(t *testing.T) {
		p := goCfg()
		p.ForwardHeaders = []string{"x-opencode-session"}
		a, headers, _ := newAdapter(t, p)
		for _, bad := range []string{"bad\r\ninjected: 1", "bad\nvalue", strings.Repeat("x", 257)} {
			fwd := http.Header{}
			fwd.Set("x-opencode-session", bad)
			call(t, a, WithSessionKey(context.Background(), "conversation-aaa"), http.MethodPost, config.OpencodeChatPath, fwd)
		}
		for i, h := range *headers {
			got := h.Get("x-opencode-session")
			if got == "" || !strings.HasPrefix(got, "ses_") {
				t.Fatalf("invalid input %d did not fall through to a derived value, got %q", i, got)
			}
			if strings.Contains(got, "\r") || strings.Contains(got, "\n") {
				t.Fatalf("control bytes reached upstream in %q", got)
			}
		}
	})

	t.Run("caller scope isolates derived values", func(t *testing.T) {
		a, headers, _ := newAdapter(t, goCfg())
		ctxA := WithClientScope(WithSessionKey(context.Background(), "same-key"), "vk-alpha")
		ctxB := WithClientScope(WithSessionKey(context.Background(), "same-key"), "vk-beta")
		call(t, a, ctxA, http.MethodPost, config.OpencodeChatPath, nil)
		call(t, a, ctxB, http.MethodPost, config.OpencodeChatPath, nil)
		call(t, a, ctxA, http.MethodPost, config.OpencodeChatPath, nil)
		hA1, hB, hA2 := (*headers)[0].Get("x-opencode-session"), (*headers)[1].Get("x-opencode-session"), (*headers)[2].Get("x-opencode-session")
		if hA1 == "" || hA1 == hB {
			t.Fatalf("same session key under different scopes must differ: %q vs %q", hA1, hB)
		}
		if hA1 != hA2 {
			t.Fatalf("same scope+session must be stable: %q vs %q", hA1, hA2)
		}
		if strings.Contains(hA1, "vk-alpha") || strings.Contains(hB, "vk-beta") {
			t.Fatalf("scope must be hashed in, never plaintext in the header: %q %q", hA1, hB)
		}
		// Fallback (no session key) is also scope-isolated and deterministic.
		if DeriveOpencodeSessionValueScoped(goCfg(), "vk-alpha", "", "m1") == DeriveOpencodeSessionValueScoped(goCfg(), "vk-beta", "", "m1") {
			t.Fatal("scoped fallback must differ per scope")
		}
		if DeriveOpencodeSessionValueScoped(goCfg(), "vk-alpha", "", "m1") != DeriveOpencodeSessionValueScoped(goCfg(), "vk-alpha", "", "m1") {
			t.Fatal("scoped fallback must be deterministic")
		}
	})

	t.Run("unrelated providers never receive the header, even when forwarded", func(t *testing.T) {
		p := config.ProviderConfig{ID: "gen1", Type: "openai_compatible", APIKey: "test-key-1", AuthMode: "bearer", ForwardHeaders: []string{"x-opencode-session"}}
		a, headers, _ := newAdapter(t, p)
		fwd := http.Header{}
		fwd.Set("x-opencode-session", "ses_client_supplied_value")
		call(t, a, WithSessionKey(context.Background(), "conversation-aaa"), http.MethodPost, "/v1/chat/completions", fwd)
		if got := (*headers)[0].Get("x-opencode-session"); got != "" {
			t.Fatalf("generic provider received session header %q", got)
		}
	})

	t.Run("console forwards valid client value on inference, never derives, none on discovery", func(t *testing.T) {
		p := config.ProviderConfig{ID: "con1", Type: "opencode-console", APIKey: "test-key-1", AuthMode: "bearer", ForwardHeaders: []string{"x-opencode-session"}}
		a, headers, _ := newAdapter(t, p)
		fwd := http.Header{}
		fwd.Set("x-opencode-session", "ses_client_supplied_value")
		call(t, a, WithSessionKey(context.Background(), "conversation-aaa"), http.MethodPost, config.OpencodeMessagesPath, fwd)
		if got := (*headers)[0].Get("x-opencode-session"); got != "ses_client_supplied_value" {
			t.Fatalf("console inference must forward valid client value unchanged, got %q", got)
		}
		call(t, a, WithSessionKey(context.Background(), "conversation-aaa"), http.MethodGet, "/v1/models", fwd)
		if got := (*headers)[1].Get("x-opencode-session"); got != "" {
			t.Fatalf("console discovery leaked session header %q", got)
		}
		// Invalid client value to Console is deleted, never derived.
		fwdBad := http.Header{}
		fwdBad.Set("x-opencode-session", "bad\r\nvalue")
		call(t, a, WithSessionKey(context.Background(), "conversation-aaa"), http.MethodPost, config.OpencodeMessagesPath, fwdBad)
		if got := (*headers)[2].Get("x-opencode-session"); got != "" {
			t.Fatalf("console must not forward invalid value or derive one, got %q", got)
		}
	})
}
