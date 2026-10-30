package providers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestContinue95OpencodeKindAndContextBranches(t *testing.T) {
	if got := OpencodeCanonicalKind(IngressOpenAIResponses); got != "openai_responses" {
		t.Errorf("responses kind = %q", got)
	}
	if got := OpencodeCanonicalKind(IngressAnthropic); got != "anthropic" {
		t.Errorf("anthropic kind = %q", got)
	}
	if got := OpencodeCanonicalKind("anything-else"); got != "openai_chat" {
		t.Errorf("default kind = %q", got)
	}
	// Nil-context guards.
	ctx := WithSessionKey(nil, "sess-1")
	if v, ok := SessionKeyFromContext(ctx); !ok || v != "sess-1" {
		t.Errorf("session key round-trip = %q, %v", v, ok)
	}
	if _, ok := SessionKeyFromContext(nil); ok {
		t.Error("nil context should yield no session key")
	}
	if _, ok := SessionKeyFromContext(context.Background()); ok {
		t.Error("empty context should yield no session key")
	}
	sctx := WithClientScope(nil, "tenant-1")
	if v, ok := ClientScopeFromContext(sctx); !ok || v != "tenant-1" {
		t.Errorf("client scope round-trip = %q, %v", v, ok)
	}
	if _, ok := ClientScopeFromContext(nil); ok {
		t.Error("nil context should yield no client scope")
	}
	if _, ok := ClientScopeFromContext(context.Background()); ok {
		t.Error("empty context should yield no client scope")
	}
	// Session hash is deterministic and non-empty.
	if h := SessionHash("sess-1"); h == "" || h == SessionHash("sess-2") {
		t.Error("session hash should be a non-empty deterministic digest")
	}
}

func TestContinue95OpencodeSessionDerivation(t *testing.T) {
	p := config.ProviderConfig{ID: "prov-1", OpencodeSessionSalt: "custom-salt"}
	if got := DeriveOpencodeSessionValueScoped(p, "scope-1", "", "model-1"); !strings.HasPrefix(got, "ses_") {
		t.Errorf("scoped empty-session derivation = %q", got)
	}
	if got := DeriveOpencodeSessionValueCachedScoped(p, "scope-1", "", "model-1"); !strings.HasPrefix(got, "ses_") {
		t.Errorf("cached scoped empty-session derivation = %q", got)
	}
	if got := DeriveOpencodeSessionValueCached(p, "sess-1", "model-1"); !strings.HasPrefix(got, "ses_") {
		t.Errorf("cached derivation = %q", got)
	}
	// Scoped and unscoped derivations differ, and the custom salt is
	// actually used (default-salt derivation differs).
	pDefault := config.ProviderConfig{ID: "prov-1"}
	if DeriveOpencodeSessionValueScoped(p, "", "sess-1", "model-1") == DeriveOpencodeSessionValueScoped(pDefault, "", "sess-1", "model-1") {
		t.Error("custom salt should change the derivation")
	}
	if DeriveOpencodeSessionValueScoped(p, "scope-1", "sess-1", "model-1") == DeriveOpencodeSessionValueScoped(p, "", "sess-1", "model-1") {
		t.Error("scope should change the derivation")
	}
	if got := modelFromPayload([]byte(`not json`)); got != "" {
		t.Errorf("modelFromPayload on invalid JSON = %q, want empty", got)
	}
	if got := modelFromPayload([]byte(`{"model": "m-9"}`)); got != "m-9" {
		t.Errorf("modelFromPayload = %q, want m-9", got)
	}
}

func TestContinue95OpencodeSessionCacheEviction(t *testing.T) {
	p := config.ProviderConfig{ID: "prov-1"}
	now := time.Now()
	// Expired-entry eviction: fill the cache past the cap with expired
	// entries, then derive a fresh value.
	opencodeSessionCache.mu.Lock()
	opencodeSessionCache.entries = map[string]sessionValueCacheEntry{}
	opencodeSessionCache.order = nil
	for i := 0; i < sessionValueCacheMax; i++ {
		k := "expired-" + string(rune('a'+i%26)) + "-" + strings.Repeat("x", 0) + itoaContinue95(i)
		opencodeSessionCache.entries[k] = sessionValueCacheEntry{value: "v", expires: now.Add(-time.Hour)}
		opencodeSessionCache.order = append(opencodeSessionCache.order, k)
	}
	opencodeSessionCache.mu.Unlock()
	got := DeriveOpencodeSessionValueCachedScoped(p, "", "fresh-key", "model-1")
	if !strings.HasPrefix(got, "ses_") {
		t.Errorf("derivation after expired eviction = %q", got)
	}
	opencodeSessionCache.mu.Lock()
	expiredLeft := 0
	for _, e := range opencodeSessionCache.entries {
		if e.expires.Before(now) {
			expiredLeft++
		}
	}
	opencodeSessionCache.mu.Unlock()
	if expiredLeft != 0 {
		t.Errorf("expired entries should have been evicted, %d remain", expiredLeft)
	}
	// Oldest-entry eviction: fill with live entries, then derive one more.
	opencodeSessionCache.mu.Lock()
	opencodeSessionCache.entries = map[string]sessionValueCacheEntry{}
	opencodeSessionCache.order = nil
	for i := 0; i < sessionValueCacheMax; i++ {
		k := "live-" + itoaContinue95(i)
		opencodeSessionCache.entries[k] = sessionValueCacheEntry{value: "v", expires: now.Add(time.Hour)}
		opencodeSessionCache.order = append(opencodeSessionCache.order, k)
	}
	opencodeSessionCache.mu.Unlock()
	got = DeriveOpencodeSessionValueCachedScoped(p, "", "another-fresh-key", "model-1")
	if !strings.HasPrefix(got, "ses_") {
		t.Errorf("derivation after oldest eviction = %q", got)
	}
	opencodeSessionCache.mu.Lock()
	n := len(opencodeSessionCache.entries)
	opencodeSessionCache.mu.Unlock()
	if n != sessionValueCacheMax {
		t.Errorf("cache size after oldest eviction = %d, want %d", n, sessionValueCacheMax)
	}
	// Restore a clean cache so later tests in the package start fresh.
	opencodeSessionCache.mu.Lock()
	opencodeSessionCache.entries = map[string]sessionValueCacheEntry{}
	opencodeSessionCache.order = nil
	opencodeSessionCache.mu.Unlock()
	// Cached value is reused on a second call.
	first := DeriveOpencodeSessionValueCachedScoped(p, "", "repeat-key", "model-1")
	second := DeriveOpencodeSessionValueCachedScoped(p, "", "repeat-key", "model-1")
	if first != second {
		t.Errorf("cached derivation not reused: %q vs %q", first, second)
	}
	opencodeSessionCache.mu.Lock()
	opencodeSessionCache.entries = map[string]sessionValueCacheEntry{}
	opencodeSessionCache.order = nil
	opencodeSessionCache.mu.Unlock()
}

func itoaContinue95(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestContinue95SessionPinEviction(t *testing.T) {
	now := time.Now()
	a := &httpAdapter{}
	// Fill to the cap with a mix of expired and live pins.
	a.sessionPins = map[string]sessionPin{}
	for i := 0; i < sessionPinMax; i++ {
		exp := now.Add(time.Hour)
		if i%2 == 0 {
			exp = now.Add(-time.Hour)
		}
		a.sessionPins["pin-"+itoaContinue95(i)] = sessionPin{credIdx: i % 3, expires: exp}
	}
	a.setSessionPinLocked("new-session", 2, now)
	// 512 expired pins evicted, 512 live pins remain, plus the new one.
	if len(a.sessionPins) != sessionPinMax/2+1 {
		t.Errorf("pin count after expired eviction = %d, want %d", len(a.sessionPins), sessionPinMax/2+1)
	}
	for k, pin := range a.sessionPins {
		if k != "new-session" && pin.expires.Before(now) {
			t.Errorf("expired pin %s survived eviction", k)
		}
	}
	if pin, ok := a.sessionPins["new-session"]; !ok || pin.credIdx != 2 {
		t.Errorf("new pin = %+v, %v", pin, ok)
	}
	// All-live pins at the cap: one oldest pin must be evicted.
	a.sessionPins = map[string]sessionPin{}
	for i := 0; i < sessionPinMax; i++ {
		a.sessionPins["live-"+itoaContinue95(i)] = sessionPin{credIdx: 0, expires: now.Add(time.Hour)}
	}
	a.setSessionPinLocked("new-session-2", 1, now)
	if len(a.sessionPins) != sessionPinMax {
		t.Errorf("all-live pin count = %d, want %d", len(a.sessionPins), sessionPinMax)
	}
	if _, ok := a.sessionPins["new-session-2"]; !ok {
		t.Error("new pin missing after all-live eviction")
	}
}

func TestContinue95CredentialLessLoaded(t *testing.T) {
	cases := []struct {
		a, b credentialState
		want bool
	}{
		{credentialState{Active: 1}, credentialState{Active: 2}, true},
		{credentialState{Active: 2}, credentialState{Active: 1}, false},
		{credentialState{Active: 1, Failures: 0}, credentialState{Active: 1, Failures: 3}, true},
		{credentialState{Active: 1, Failures: 3}, credentialState{Active: 1, Failures: 0}, false},
		{credentialState{Active: 1, LastStatus: 0}, credentialState{Active: 1, LastStatus: 429}, true},
		{credentialState{Active: 1, LastStatus: 429}, credentialState{Active: 1, LastStatus: 0}, false},
		{credentialState{Active: 1, LastStatus: 429}, credentialState{Active: 1, LastStatus: 500}, false},
	}
	for i, tc := range cases {
		if got := credentialLessLoaded(tc.a, tc.b); got != tc.want {
			t.Errorf("case %d: credentialLessLoaded = %v, want %v", i, got, tc.want)
		}
	}
}

func TestContinue95AdapterCheckRedirectEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "k", AuthMode: "bearer", Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.c == nil || a.c.CheckRedirect == nil {
		t.Fatal("adapter client or CheckRedirect is nil")
	}
	if err := a.c.CheckRedirect(req, nil); err != nil {
		t.Errorf("CheckRedirect with no prior requests should allow, got %v", err)
	}
	if err := a.streamC.CheckRedirect(req, nil); err != nil {
		t.Errorf("stream CheckRedirect with no prior requests should allow, got %v", err)
	}
}

func TestContinue95EventStreamFillDone(t *testing.T) {
	r := newEventStreamSSEReader(io.NopCloser(bytes.NewReader(nil)))
	r.fill()
	if !r.done {
		t.Error("fill on an empty stream should mark done")
	}
	r.fill() // second fill must return immediately via the done guard
	if !r.done {
		t.Error("second fill should keep done set")
	}
	if _, err := r.Read(make([]byte, 16)); !isEOFContinue95(err) {
		t.Errorf("read after done = %v, want EOF", err)
	}
}

func isEOFContinue95(err error) bool {
	return err == io.EOF
}
