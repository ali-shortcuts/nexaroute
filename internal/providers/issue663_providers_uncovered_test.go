package providers

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// credentialLessLoaded orders credentials by active load, then failures,
// then by whether a credential has ever reported a status.
func TestIssue663ProvidersCredentialLessLoaded(t *testing.T) {
	less := func(a, b credentialState) bool { return credentialLessLoaded(a, b) }
	if !less(credentialState{Active: 0}, credentialState{Active: 2}) {
		t.Fatal("fewer active connections did not sort first")
	}
	if less(credentialState{Active: 2}, credentialState{Active: 0}) {
		t.Fatal("more active connections sorted first")
	}
	if !less(credentialState{Active: 1, Failures: 0}, credentialState{Active: 1, Failures: 3}) {
		t.Fatal("fewer failures did not sort first at equal active load")
	}
	if less(credentialState{Active: 1, Failures: 3}, credentialState{Active: 1, Failures: 0}) {
		t.Fatal("more failures sorted first at equal active load")
	}
	if !less(credentialState{LastStatus: 0}, credentialState{LastStatus: 429}) {
		t.Fatal("never-contacted credential did not sort before a rate-limited one")
	}
	if less(credentialState{LastStatus: 429}, credentialState{LastStatus: 0}) {
		t.Fatal("rate-limited credential sorted before a never-contacted one")
	}
	if less(credentialState{Active: 1, Failures: 1, LastStatus: 200}, credentialState{Active: 1, Failures: 1, LastStatus: 200}) {
		t.Fatal("identical credentials compared as less")
	}
}

// setSessionPinLocked sweeps expired pins at capacity and evicts one live
// pin when the map is still full after the sweep.
func TestIssue663ProvidersSetSessionPinSweep(t *testing.T) {
	mk := func() *httpAdapter {
		a, err := newHTTPAdapter(config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true}, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	now := time.Now()

	// Expired pins are swept first; if the sweep frees room, no live pin
	// is evicted.
	a := mk()
	a.sessionPins = map[string]sessionPin{}
	for i := 0; i < sessionPinMax-1; i++ {
		a.sessionPins[fmt.Sprintf("live-%d", i)] = sessionPin{credIdx: 0, expires: now.Add(time.Hour)}
	}
	a.sessionPins["expired"] = sessionPin{credIdx: 0, expires: now.Add(-time.Hour)}
	a.setSessionPinLocked("new", 1, now)
	if _, ok := a.sessionPins["expired"]; ok {
		t.Fatal("expired session pin survived the capacity sweep")
	}
	if len(a.sessionPins) != sessionPinMax {
		t.Fatalf("pin count after sweep = %d, want %d", len(a.sessionPins), sessionPinMax)
	}
	if pin, ok := a.sessionPins["new"]; !ok || pin.credIdx != 1 {
		t.Fatalf("new pin missing or wrong after sweep: %+v ok=%v", pin, ok)
	}
	live := 0
	for k := range a.sessionPins {
		if strings.HasPrefix(k, "live-") {
			live++
		}
	}
	if live != sessionPinMax-1 {
		t.Fatalf("live pins after expired-only sweep = %d, want %d (no live eviction)", live, sessionPinMax-1)
	}

	// All-live at capacity: exactly one live pin is evicted for the new one.
	b := mk()
	b.sessionPins = map[string]sessionPin{}
	for i := 0; i < sessionPinMax; i++ {
		b.sessionPins[fmt.Sprintf("live-%d", i)] = sessionPin{credIdx: 0, expires: now.Add(time.Hour)}
	}
	b.setSessionPinLocked("new", 2, now)
	if len(b.sessionPins) != sessionPinMax {
		t.Fatalf("pin count after eviction = %d, want %d", len(b.sessionPins), sessionPinMax)
	}
	if pin, ok := b.sessionPins["new"]; !ok || pin.credIdx != 2 {
		t.Fatalf("new pin missing or wrong after eviction: %+v ok=%v", pin, ok)
	}
}

// OpencodeCanonicalKind maps each ingress kind to its canonical kind.
func TestIssue663ProvidersOpencodeCanonicalKind(t *testing.T) {
	if got := OpencodeCanonicalKind(IngressOpenAIResponses); got != "openai_responses" {
		t.Fatalf("responses kind = %q, want openai_responses", got)
	}
	if got := OpencodeCanonicalKind(IngressAnthropic); got != "anthropic" {
		t.Fatalf("anthropic kind = %q, want anthropic", got)
	}
	if got := OpencodeCanonicalKind("something-else"); got != "openai_chat" {
		t.Fatalf("unknown kind = %q, want openai_chat default", got)
	}
}

// Session and scope context helpers tolerate a nil parent context.
func TestIssue663ProvidersNilContextHelpers(t *testing.T) {
	ctx := WithSessionKey(nil, "conv-1")
	if got, ok := SessionKeyFromContext(ctx); !ok || got != "conv-1" {
		t.Fatalf("session key from nil-parent context = (%q, %v), want (conv-1, true)", got, ok)
	}
	if _, ok := SessionKeyFromContext(nil); ok {
		t.Fatal("nil context reported a session key")
	}
	sctx := WithClientScope(nil, "tenant-1")
	if got, ok := ClientScopeFromContext(sctx); !ok || got != "tenant-1" {
		t.Fatalf("client scope from nil-parent context = (%q, %v), want (tenant-1, true)", got, ok)
	}
	if _, ok := ClientScopeFromContext(nil); ok {
		t.Fatal("nil context reported a client scope")
	}
}

// The configured session salt wins over the default, and the unscoped
// cached derivation matches the direct derivation.
func TestIssue663ProvidersOpencodeSessionSaltAndCache(t *testing.T) {
	p := config.ProviderConfig{ID: "p", OpencodeSessionSalt: "custom-salt"}
	if got := opencodeSessionSalt(p); got != "custom-salt" {
		t.Fatalf("session salt = %q, want custom-salt", got)
	}
	if got := opencodeSessionSalt(config.ProviderConfig{ID: "p"}); got != config.DefaultOpencodeSessionSalt {
		t.Fatalf("default session salt = %q, want %q", got, config.DefaultOpencodeSessionSalt)
	}
	cached := DeriveOpencodeSessionValueCached(p, "conv-1", "model-a")
	direct := DeriveOpencodeSessionValue(p, "conv-1", "model-a")
	if cached != direct {
		t.Fatalf("cached derivation %q != direct derivation %q", cached, direct)
	}
	// Scoped derivation without a session key falls back to provider+model.
	scoped := DeriveOpencodeSessionValueCachedScoped(p, "tenant-1", "", "model-a")
	scopedAgain := DeriveOpencodeSessionValueCachedScoped(p, "tenant-1", "", "model-a")
	if scoped == "" || scoped != scopedAgain {
		t.Fatalf("scoped fallback derivation unstable: %q vs %q", scoped, scopedAgain)
	}
	if scoped == cached {
		t.Fatal("scoped fallback derivation collided with the unscoped derivation")
	}
}

// The session-value cache sweeps expired entries at capacity and evicts
// the oldest live entry when still full.
func TestIssue663ProvidersSessionValueCacheEviction(t *testing.T) {
	p := config.ProviderConfig{ID: "p"}
	savedEntries := opencodeSessionCache.entries
	savedOrder := opencodeSessionCache.order
	defer func() {
		opencodeSessionCache.mu.Lock()
		opencodeSessionCache.entries = savedEntries
		opencodeSessionCache.order = savedOrder
		opencodeSessionCache.mu.Unlock()
	}()
	now := time.Now()

	// Expired entries are swept; no live entry is evicted.
	opencodeSessionCache.mu.Lock()
	opencodeSessionCache.entries = map[string]sessionValueCacheEntry{}
	opencodeSessionCache.order = nil
	for i := 0; i < sessionValueCacheMax-1; i++ {
		k := fmt.Sprintf("live-%d", i)
		opencodeSessionCache.entries[k] = sessionValueCacheEntry{value: "v", expires: now.Add(time.Hour)}
		opencodeSessionCache.order = append(opencodeSessionCache.order, k)
	}
	opencodeSessionCache.entries["expired"] = sessionValueCacheEntry{value: "v", expires: now.Add(-time.Hour)}
	opencodeSessionCache.order = append(opencodeSessionCache.order, "expired")
	opencodeSessionCache.mu.Unlock()
	DeriveOpencodeSessionValueCachedScoped(p, "", "issue663-sweep", "m")
	opencodeSessionCache.mu.Lock()
	_, expiredKept := opencodeSessionCache.entries["expired"]
	liveCount := len(opencodeSessionCache.entries)
	opencodeSessionCache.mu.Unlock()
	if expiredKept {
		t.Fatal("expired cache entry survived the capacity sweep")
	}
	if liveCount != sessionValueCacheMax {
		t.Fatalf("cache size after sweep = %d, want %d", liveCount, sessionValueCacheMax)
	}

	// All live at capacity: the oldest entry is evicted for the new one.
	opencodeSessionCache.mu.Lock()
	opencodeSessionCache.entries = map[string]sessionValueCacheEntry{}
	opencodeSessionCache.order = nil
	for i := 0; i < sessionValueCacheMax; i++ {
		k := fmt.Sprintf("live-%d", i)
		opencodeSessionCache.entries[k] = sessionValueCacheEntry{value: "v", expires: now.Add(time.Hour)}
		opencodeSessionCache.order = append(opencodeSessionCache.order, k)
	}
	opencodeSessionCache.mu.Unlock()
	DeriveOpencodeSessionValueCachedScoped(p, "", "issue663-evict", "m")
	opencodeSessionCache.mu.Lock()
	_, oldestKept := opencodeSessionCache.entries["live-0"]
	size := len(opencodeSessionCache.entries)
	opencodeSessionCache.mu.Unlock()
	if oldestKept {
		t.Fatal("oldest cache entry survived eviction at capacity")
	}
	if size != sessionValueCacheMax {
		t.Fatalf("cache size after eviction = %d, want %d", size, sessionValueCacheMax)
	}
}

// modelFromPayload returns empty on unparseable payloads.
func TestIssue663ProvidersModelFromPayloadInvalid(t *testing.T) {
	if got := modelFromPayload([]byte(`{bad`)); got != "" {
		t.Fatalf("model from invalid payload = %q, want empty", got)
	}
	if got := modelFromPayload([]byte(`{"model":"m1"}`)); got != "m1" {
		t.Fatalf("model from valid payload = %q, want m1", got)
	}
}

// fill on an already-drained reader returns immediately without touching
// the source again.
func TestIssue663ProvidersEventStreamFillAfterDone(t *testing.T) {
	r := newEventStreamSSEReader(io.NopCloser(strings.NewReader("")))
	r.fill()
	if !r.done {
		t.Fatal("first fill of an empty stream did not mark the reader done")
	}
	r.fill() // done guard: must return without error or state change
	if r.err != nil {
		t.Fatalf("second fill reported an error: %v", r.err)
	}
	buf := make([]byte, 16)
	if n, err := r.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("read after done = (%d, %v), want (0, EOF)", n, err)
	}
}
