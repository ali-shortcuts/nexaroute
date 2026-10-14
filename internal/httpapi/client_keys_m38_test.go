package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// m38Body estimates to 119 tokens (77 bytes / 4 + max_tokens 100) under
// requestTokenEstimate; openAIOKBody reports 18 tokens of actual usage.
const m38Body = `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`

const m38Usage500Body = `{"id":"c","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":400,"completion_tokens":100,"total_tokens":500}}`

func m38Chat(t *testing.T, s *Server, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(m38Body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func m38VirtualCfg(t *testing.T, up string, id, secret string, tpm int) config.Config {
	t.Helper()
	cfg := singleProviderOpenAI(t, up, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, VirtualKeys: []config.VirtualKeyConfig{{ID: id, KeyHash: keyDigest(secret), AllowedModels: []string{"m"}, TPM: tpm}}}
	return cfg
}

// Legacy clients authenticate against stored hashes alone: the config
// never holds the plaintext secret.
func TestLegacyKeyHashOnlyAuth(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, KeyHashes: []string{config.ClientKeyDigest("legacy-secret")}}
	s := testGateway(t, cfg)
	if rr := m38Chat(t, s, "legacy-secret"); rr.Code != http.StatusOK {
		t.Fatalf("hash-matched key: status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if rr := m38Chat(t, s, "wrong-secret"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status=%d, want 401", rr.Code)
	}
	if rr := m38Chat(t, s, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status=%d, want 401", rr.Code)
	}
}

// The global client_auth.tpm binds legacy clients too, with the same
// 429 + Retry-After contract as virtual keys.
func TestGlobalTPMAppliesToLegacyKeys(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, KeyHashes: []string{config.ClientKeyDigest("legacy-secret")}, TPM: 50}
	s := testGateway(t, cfg)
	rr := m38Chat(t, s, "legacy-secret") // estimate 119 > 50
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After=%q, want no hint because the 119-token estimate exceeds the entire 50-token bucket", got)
	}
}

// Settlement refunds an over-estimate: without it the second identical
// request would exceed the remaining bucket (150-119=31 < 119); with
// the actual usage of 18 settled, ~132 remain and it is admitted.
func TestClientTPMSettleRefundsOverEstimate(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	s := testGateway(t, m38VirtualCfg(t, up.URL, "vk-refund", "nrk_refund", 150))
	if rr := m38Chat(t, s, "nrk_refund"); rr.Code != http.StatusOK {
		t.Fatalf("first request: status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if rr := m38Chat(t, s, "nrk_refund"); rr.Code != http.StatusOK {
		t.Fatalf("second request after settle refund: status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
}

// Settlement charges an under-estimate: the provider reports 500
// tokens against a 119-token reservation, draining the 300 bucket so
// the next request is rejected. Without settlement it would pass.
func TestClientTPMSettleChargesOverage(t *testing.T) {
	up := openAIUpstream(m38Usage500Body, 0, nil)
	defer up.Close()
	s := testGateway(t, m38VirtualCfg(t, up.URL, "vk-over", "nrk_over", 300))
	if rr := m38Chat(t, s, "nrk_over"); rr.Code != http.StatusOK {
		t.Fatalf("first request: status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if rr := m38Chat(t, s, "nrk_over"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request after overage settle: status=%d, want 429", rr.Code)
	}
}

// One client's settled usage never touches another client's bucket.
func TestClientTPMSettleIsolatesClients(t *testing.T) {
	up := openAIUpstream(m38Usage500Body, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, VirtualKeys: []config.VirtualKeyConfig{
		{ID: "vk-a", KeyHash: keyDigest("nrk_a"), AllowedModels: []string{"m"}, TPM: 300},
		{ID: "vk-b", KeyHash: keyDigest("nrk_b"), AllowedModels: []string{"m"}, TPM: 300},
	}}
	s := testGateway(t, cfg)
	if rr := m38Chat(t, s, "nrk_a"); rr.Code != http.StatusOK {
		t.Fatalf("client A first: status=%d, want 200", rr.Code)
	}
	if rr := m38Chat(t, s, "nrk_a"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("client A second: status=%d, want 429 (own bucket drained)", rr.Code)
	}
	if rr := m38Chat(t, s, "nrk_b"); rr.Code != http.StatusOK {
		t.Fatalf("client B first: status=%d, want 200 (unaffected by A)", rr.Code)
	}
}

// Reserve/settle arithmetic, including incremental settlement as
// streaming usage reports arrive, exercised concurrently.
func TestClientTPMReserveSettleArithmeticAndConcurrency(t *testing.T) {
	s := &Server{clientBuckets: make(map[string]*clientBucket)}
	res, ok := s.clientTPMReserve("unit-key", 1000, 900)
	if !ok || res == nil {
		t.Fatal("reserve of 900 from a 1000 bucket rejected")
	}
	s.clientTPMSettle(res, 100) // refund 800 -> ~900 available
	if _, ok := s.clientTPMReserve("unit-key", 1000, 950); ok {
		t.Fatal("reserve of 950 accepted with only ~900 available")
	}
	res2, ok := s.clientTPMReserve("unit-key", 1000, 500)
	if !ok || res2 == nil {
		t.Fatal("reserve of 500 rejected")
	}
	s.clientTPMSettle(res2, 200) // incremental report
	s.clientTPMSettle(res2, 700) // final report: net charge 700
	b := s.clientBuckets[keyDigest("unit-key")+"#tpm"]
	if b == nil {
		t.Fatal("bucket missing")
	}
	s.clientRL.Lock()
	got := b.tokens
	s.clientRL.Unlock()
	// 1000 - 100 (first request final) - 700 (second request final).
	if got < 195 || got > 205 {
		t.Fatalf("bucket tokens=%v, want ~200", got)
	}

	// Concurrency: parallel reserve/settle cycles on one client must
	// stay within bucket bounds (race detector validates the locking).
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if r, ok := s.clientTPMReserve("conc-key", 100000, 10); ok && r != nil {
					s.clientTPMSettle(r, int64(n%7))
				}
			}
		}(i)
	}
	wg.Wait()
	cb := s.clientBuckets[keyDigest("conc-key")+"#tpm"]
	if cb == nil {
		t.Fatal("concurrency bucket missing")
	}
	s.clientRL.Lock()
	ct := cb.tokens
	s.clientRL.Unlock()
	if ct < 0 || ct > 100000 {
		t.Fatalf("concurrent bucket out of bounds: %v", ct)
	}
}

func TestClientTPMRetryAfterUsesCalculatedRefill(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{
		Enabled:   true,
		KeyHashes: []string{config.ClientKeyDigest("legacy-retry")},
		TPM:       125,
	}
	s := testGateway(t, cfg)

	if rr := m38Chat(t, s, "legacy-retry"); rr.Code != http.StatusOK {
		t.Fatalf("first request: status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	limited := m38Chat(t, s, "legacy-retry")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d body=%s, want 429", limited.Code, limited.Body.String())
	}
	seconds, err := strconv.Atoi(limited.Header().Get("Retry-After"))
	if err != nil || seconds < 1 || seconds >= 60 {
		t.Fatalf("Retry-After=%q (seconds=%d, err=%v), want calculated delay between 1 and 59 seconds",
			limited.Header().Get("Retry-After"), seconds, err)
	}
}

func TestClientTPMSettleIgnoresRecreatedBucketGeneration(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := &Server{}
	res, ok, retry := s.clientTPMReserveAt("stale-key", 100, 50, now)
	if !ok || res == nil || retry != 0 {
		t.Fatalf("initial reservation = (%v, %t, %s), want successful reservation", res, ok, retry)
	}

	s.clientRL.Lock()
	delete(s.clientBuckets, res.bucketID)
	fresh := s.clientBucketLocked(res.bucketID, 100, now.Add(time.Second))
	if fresh == nil {
		s.clientRL.Unlock()
		t.Fatal("failed to create replacement bucket")
	}
	fresh.tokens = 30
	newGeneration := fresh.generation
	s.clientRL.Unlock()
	if newGeneration == res.generation {
		t.Fatal("recreated bucket reused the old generation")
	}

	s.clientTPMSettleAt(res, 10, now.Add(2*time.Second))
	s.clientRL.Lock()
	got := s.clientBuckets[res.bucketID].tokens
	s.clientRL.Unlock()
	if got != 30 {
		t.Fatalf("late settlement changed fresh bucket balance to %v, want 30", got)
	}
}

func TestClientTPMSettleIgnoresOlderQuotaAfterLimitChange(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := &Server{}
	res, ok, _ := s.clientTPMReserveAt("changed-limit-key", 100, 50, now)
	if !ok || res == nil {
		t.Fatal("initial reservation rejected")
	}

	s.clientRL.Lock()
	changed := s.clientBucketLocked(res.bucketID, 20, now.Add(time.Second))
	if changed == nil {
		s.clientRL.Unlock()
		t.Fatal("failed to apply smaller quota")
	}
	newGeneration := changed.generation
	s.clientRL.Unlock()
	if newGeneration == res.generation {
		t.Fatal("quota change did not create a new generation")
	}

	s.clientTPMSettleAt(res, 10, now.Add(2*time.Second))
	s.clientRL.Lock()
	got, capacity := s.clientBuckets[res.bucketID].tokens, s.clientBuckets[res.bucketID].capacity
	s.clientRL.Unlock()
	if got != 20 || capacity != 20 {
		t.Fatalf("late settlement changed new quota: tokens=%v capacity=%v, want 20/20", got, capacity)
	}
}

func TestClientTPMRetryAfterUsesCalculatedRefill(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{
		Enabled: true,
		KeyHashes: []string{config.ClientKeyDigest("legacy-retry")},
		TPM: 125,
	}
	s := testGateway(t, cfg)

	if rr := m38Chat(t, s, "legacy-retry"); rr.Code != http.StatusOK {
		t.Fatalf("first request: status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	limited := m38Chat(t, s, "legacy-retry")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d body=%s, want 429", limited.Code, limited.Body.String())
	}
	seconds, err := strconv.Atoi(limited.Header().Get("Retry-After"))
	if err != nil || seconds < 1 || seconds >= 60 {
		t.Fatalf("Retry-After=%q (seconds=%d, err=%v), want calculated delay between 1 and 59 seconds",
			limited.Header().Get("Retry-After"), seconds, err)
	}
}

func TestClientTPMSettleIgnoresRecreatedBucketGeneration(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := &Server{}
	res, ok, retry := s.clientTPMReserveAt("stale-key", 100, 50, now)
	if !ok || res == nil || retry != 0 {
		t.Fatalf("initial reservation = (%v, %t, %s), want successful reservation", res, ok, retry)
	}

	s.clientRL.Lock()
	delete(s.clientBuckets, res.bucketID)
	fresh := s.clientBucketLocked(res.bucketID, 100, now.Add(time.Second))
	if fresh == nil {
		s.clientRL.Unlock()
		t.Fatal("failed to create replacement bucket")
	}
	fresh.tokens = 30
	newGeneration := fresh.generation
	s.clientRL.Unlock()
	if newGeneration == res.generation {
		t.Fatal("recreated bucket reused the old generation")
	}

	s.clientTPMSettleAt(res, 10, now.Add(2*time.Second))
	s.clientRL.Lock()
	got := s.clientBuckets[res.bucketID].tokens
	s.clientRL.Unlock()
	if got != 30 {
		t.Fatalf("late settlement changed fresh bucket balance to %v, want 30", got)
	}
}

func TestClientTPMSettleIgnoresOlderQuotaAfterLimitChange(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := &Server{}
	res, ok, _ := s.clientTPMReserveAt("changed-limit-key", 100, 50, now)
	if !ok || res == nil {
		t.Fatal("initial reservation rejected")
	}

	s.clientRL.Lock()
	changed := s.clientBucketLocked(res.bucketID, 20, now.Add(time.Second))
	if changed == nil {
		s.clientRL.Unlock()
		t.Fatal("failed to apply smaller quota")
	}
	newGeneration := changed.generation
	s.clientRL.Unlock()
	if newGeneration == res.generation {
		t.Fatal("quota change did not create a new generation")
	}

	s.clientTPMSettleAt(res, 10, now.Add(2*time.Second))
	s.clientRL.Lock()
	got, capacity := s.clientBuckets[res.bucketID].tokens, s.clientBuckets[res.bucketID].capacity
	s.clientRL.Unlock()
	if got != 20 || capacity != 20 {
		t.Fatalf("late settlement changed new quota: tokens=%v capacity=%v, want 20/20", got, capacity)
	}
}
