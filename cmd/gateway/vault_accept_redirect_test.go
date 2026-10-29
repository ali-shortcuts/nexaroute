package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestVaultAcceptClientRefusesRedirects is the regression test for the
// redirect hardening found by the independent PR #722 review: the
// previous vault-accept client shape, http.Client{Timeout} only,
// followed cross-host redirects and forwarded X-Vault-Token to the
// redirect target (verified with a two-server httptest before the fix).
// The redirect must now surface as a 3xx status, the target must never
// be hit, and the token must never reach it.
func TestVaultAcceptClientRefusesRedirects(t *testing.T) {
	var redirectTargetHits atomic.Int32
	var tokenSeenByTarget atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHits.Add(1)
		tokenSeenByTarget.Store(r.Header.Get("X-Vault-Token"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/sys/seal-status", http.StatusFound)
	}))
	defer origin.Close()

	client := newVaultAcceptClient(5 * time.Second)
	status, _, err := vaultHTTP(context.Background(), client, origin.URL, "s.test-token", http.MethodGet, "sys/seal-status", nil)
	if err != nil {
		t.Fatalf("vaultHTTP returned error: %v", err)
	}
	if status != http.StatusFound {
		t.Fatalf("status = %d, want %d (redirect must surface, not be followed)", status, http.StatusFound)
	}
	if got := redirectTargetHits.Load(); got != 0 {
		t.Fatalf("redirect target was hit %d times, want 0", got)
	}
	if tok, _ := tokenSeenByTarget.Load().(string); tok != "" {
		t.Fatalf("redirect target saw token %q, want empty", tok)
	}
}
