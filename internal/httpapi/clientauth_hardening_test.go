package httpapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestModelPreservesLargeRequestBody(t *testing.T) {
	body := []byte(`{"model":"vision-large","messages":[{"role":"user","content":"` + strings.Repeat("x", (2<<20)+256) + `"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))

	// For bodies beyond the bounded inspection prefix, a key with a model
	// allow-list must not rely on metadata that could be duplicated later.
	if got := requestModel(req); got != "" {
		t.Fatalf("requestModel() = %q, want fail-closed empty model", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if !bytes.Equal(restored, body) {
		t.Fatalf("request body changed during model inspection: got %d bytes, want %d", len(restored), len(body))
	}
}

func TestRequestTokenEstimatePreservesLargeRequestBody(t *testing.T) {
	body := []byte(`{"model":"vision-large","max_tokens":123,"messages":[{"role":"user","content":"` + strings.Repeat("x", (2<<20)+256) + `"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))

	got := requestTokenEstimate(req)
	wantAtLeast := len(body)/4 + 123
	if got < wantAtLeast {
		t.Fatalf("requestTokenEstimate() = %d, want at least %d for %d-byte body", got, wantAtLeast, len(body))
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if !bytes.Equal(restored, body) {
		t.Fatalf("request body changed during token estimation: got %d bytes, want %d", len(restored), len(body))
	}
}

func TestClientRateLimitBucketsRemainBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Server) bool
	}{
		{name: "rpm", call: func(s *Server) bool { return s.clientRPMAllow("new-client", 60) }},
		{name: "tpm", call: func(s *Server) bool { return s.clientTPMAllow("new-client", 600, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := &Server{clientBuckets: make(map[string]*clientBucket, maxClientBuckets)}
			for i := 0; i < maxClientBuckets; i++ {
				s.clientBuckets[fmt.Sprintf("occupied-%d", i)] = &clientBucket{tokens: 1, last: now}
			}
			if tc.call(s) {
				t.Fatal("new bucket unexpectedly admitted while table is full")
			}
			if got := len(s.clientBuckets); got != maxClientBuckets {
				t.Fatalf("bucket count = %d, want hard cap %d", got, maxClientBuckets)
			}
		})
	}
}

func TestClientRateLimitBucketsEvictIdleEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Server) bool
	}{
		{name: "rpm", call: func(s *Server) bool { return s.clientRPMAllow("new-client", 60) }},
		{name: "tpm", call: func(s *Server) bool { return s.clientTPMAllow("new-client", 600, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := time.Now().Add(-clientBucketIdle - time.Second)
			s := &Server{clientBuckets: make(map[string]*clientBucket, maxClientBuckets)}
			for i := 0; i < maxClientBuckets; i++ {
				s.clientBuckets[fmt.Sprintf("stale-%d", i)] = &clientBucket{tokens: 0, last: stale}
			}
			if !tc.call(s) {
				t.Fatal("request rejected even though idle buckets could be evicted")
			}
			if got := len(s.clientBuckets); got != 1 {
				t.Fatalf("bucket count after idle eviction = %d, want 1", got)
			}
		})
	}
}

func TestRequestModelRejectsDuplicateModelFields(t *testing.T) {
	body := `{"model":"allowed","model":"forbidden","messages":[]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	if got := requestModel(req); got != "" {
		t.Fatalf("requestModel() = %q, want fail-closed empty model for duplicate fields", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("request body changed: got %q, want %q", string(restored), body)
	}
}

func TestRequestTokenEstimateUsesMaximumDuplicateOutputLimit(t *testing.T) {
	body := `{"model":"m","max_tokens":2000,"max_tokens":1,"max_output_tokens":2,"messages":[]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	got := requestTokenEstimate(req)
	if got < 2000 {
		t.Fatalf("requestTokenEstimate() = %d, want >= 2000 for duplicate max_tokens", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("request body changed: got %q, want %q", string(restored), body)
	}
}

func TestClientTPMAdmissionIsolatedAndRetryDelayAccurate(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := &Server{}

	if allowed, retry, _ := s.clientTPMAllowAt("client-a", 100, 80, now); !allowed || retry != 0 {
		t.Fatalf("first client-a admission = (%t, %s), want (true, 0)", allowed, retry)
	}
	if allowed, retry, _ := s.clientTPMAllowAt("client-b", 100, 80, now); !allowed || retry != 0 {
		t.Fatalf("first client-b admission = (%t, %s), want (true, 0)", allowed, retry)
	}

	// A rejected estimate must not consume the 20 tokens remaining for client-a.
	if allowed, retry, _ := s.clientTPMAllowAt("client-a", 100, 21, now); allowed || retry != time.Second {
		t.Fatalf("client-a over-budget admission = (%t, %s), want (false, 1s)", allowed, retry)
	}
	if allowed, retry, _ := s.clientTPMAllowAt("client-a", 100, 20, now); !allowed || retry != 0 {
		t.Fatalf("client-a exact remaining admission = (%t, %s), want (true, 0)", allowed, retry)
	}
	if allowed, retry, _ := s.clientTPMAllowAt("client-b", 100, 20, now); !allowed || retry != 0 {
		t.Fatalf("client-b must retain its independent 20-token balance; got (%t, %s)", allowed, retry)
	}

	// An estimate larger than the entire TPM capacity can never fit by waiting.
	if allowed, retry, _ := s.clientTPMAllowAt("client-c", 100, 101, now); allowed || retry != 0 {
		t.Fatalf("oversized estimate admission = (%t, %s), want (false, 0)", allowed, retry)
	}
	if _, exists := s.clientBuckets[keyDigest("client-c")+"#tpm"]; exists {
		t.Fatal("oversized request unexpectedly allocated a TPM bucket")
	}
}

func TestClientTPMAdmissionIsAtomicUnderConcurrentRequests(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const capacity = 128
	const attempts = capacity * 4
	s := &Server{}
	var wg sync.WaitGroup
	var accepted atomic.Int32

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowed, _, _ := s.clientTPMAllowAt("shared-client", capacity, 1, now); allowed {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := accepted.Load(); got != capacity {
		t.Fatalf("accepted %d concurrent requests, want exactly %d", got, capacity)
	}
	if got := len(s.clientBuckets); got != 1 {
		t.Fatalf("bucket count = %d, want 1 for one client", got)
	}
}

func TestClientTPMUsageReconciliationAdjustsEstimateOnce(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id := keyDigest("reconcile-client") + "#tpm"
	s := &Server{
		clientBuckets: map[string]*clientBucket{
			id: {tokens: 20, last: now, capacity: 100, generation: 42},
		},
	}
	reservation := &clientTPMReservation{
		bucketID: id, limit: 100, estimated: 80, generation: 42,
	}

	// First authoritative usage report replaces the 80-token estimate with 40.
	s.reconcileClientTPMUsageAt(reservation, 40, now)
	if got := s.clientBuckets[id].tokens; got != 60 {
		t.Fatalf("remaining after replacing estimate with 40 actual tokens = %v, want 60", got)
	}

	// A subsequent provider-reported attempt is charged at its actual usage.
	s.reconcileClientTPMUsageAt(reservation, 20, now)
	if got := s.clientBuckets[id].tokens; got != 40 {
		t.Fatalf("remaining after additional 20-token attempt = %v, want 40", got)
	}
}

func TestClientTPMUsageReconciliationChargesUsageAboveEstimate(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id := keyDigest("debt-client") + "#tpm"
	s := &Server{
		clientBuckets: map[string]*clientBucket{
			id: {tokens: 10, last: now, capacity: 100, generation: 9},
		},
	}
	reservation := &clientTPMReservation{
		bucketID: id, limit: 100, estimated: 80, generation: 9,
	}
	s.reconcileClientTPMUsageAt(reservation, 100, now)
	if got := s.clientBuckets[id].tokens; got != -10 {
		t.Fatalf("remaining balance after 100 actual vs 80 estimated = %v, want -10 debt", got)
	}
}

func TestClientTPMUsageReconciliationIgnoresExpiredBucketGeneration(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id := keyDigest("generation-client") + "#tpm"
	s := &Server{
		clientBuckets: map[string]*clientBucket{
			id: {tokens: 10, last: now, capacity: 100, generation: 99},
		},
	}
	reservation := &clientTPMReservation{
		bucketID: id, limit: 100, estimated: 80, generation: 42,
	}
	s.reconcileClientTPMUsageAt(reservation, 40, now)
	if got := s.clientBuckets[id].tokens; got != 10 {
		t.Fatalf("balance in newer bucket generation changed to %v, want 10", got)
	}
}

func TestClientTPMConfigChangeInvalidatesOlderReservation(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id := keyDigest("changed-limit-client") + "#tpm"
	s := &Server{
		clientBuckets: map[string]*clientBucket{
			id: {tokens: 50, last: now, capacity: 100, generation: 42},
		},
	}
	oldRequest := &clientTPMReservation{
		bucketID: id, limit: 100, estimated: 50, generation: 42,
	}

	// A new request observes a TPM configuration reduction from 100 to 20.
	s.clientRL.Lock()
	b := s.clientBucketLocked(id, 20, now)
	s.clientRL.Unlock()
	if b == nil || b.capacity != 20 || b.tokens > 20 || b.generation == 42 {
		t.Fatalf("changed bucket = %#v, want capacity 20, tokens <= 20 and a fresh generation", b)
	}

	// The older response must not refund tokens into the new smaller quota.
	s.reconcileClientTPMUsageAt(oldRequest, 10, now)
	if got := s.clientBuckets[id].tokens; got != 20 {
		t.Fatalf("old reservation changed new quota balance to %v, want 20", got)
	}
}
