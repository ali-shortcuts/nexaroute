package httpapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
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
