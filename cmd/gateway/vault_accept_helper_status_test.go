package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The harness HTTP client must never follow a redirect: Go's default
// redirect policy forwards every header it is given, including
// X-Vault-Token, to the redirect target. newVaultAcceptClient refuses
// redirects (http.ErrUseLastResponse); this test pins that behavior at
// the vaultHTTP level.
func TestVaultAcceptHTTPDoesNotFollowRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if got := r.Header.Get("X-Vault-Token"); got != "" {
			t.Errorf("redirect target received X-Vault-Token header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/secret/metadata/x", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	status, _, err := vaultHTTP(context.Background(), newVaultAcceptClient(10*time.Second), redirect.URL, "test-token", http.MethodGet, "secret/metadata/x", nil)
	if err != nil {
		t.Fatalf("vaultHTTP error = %v, want the 307 response returned", err)
	}
	if status != http.StatusTemporaryRedirect {
		t.Fatalf("vaultHTTP status = %d, want 307 (redirect refused, not followed)", status)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0 (token must never reach a redirect target)", got)
	}
}

// The real helper script must verify the HTTP status of every Vault
// request. curl -sf fails on 4xx/5xx but returns success for 3xx, so a
// redirecting Vault answering a PUT with 307 used to make the helper
// report success even though nothing was written. GET responses are
// additionally capped at 1 MiB.
func TestVaultHelperRedirectAndSizeBounds(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "key-provider-vault-kv.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("helper script not present at %s: %v", script, err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ctx := context.Background()

	t.Run("put to a redirecting server fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:9/v1/secret/data/x", http.StatusTemporaryRedirect)
		}))
		defer srv.Close()
		if _, err := runVaultAcceptHelper(ctx, script, "put", "secret/data/x", key, srv.URL, "tok", 30); err == nil {
			t.Fatal("helper put to a 307 server must fail, but it reported success")
		}
	})

	t.Run("get from a redirecting server fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:9/v1/secret/data/x", http.StatusTemporaryRedirect)
		}))
		defer srv.Close()
		if _, err := runVaultAcceptHelperGet(ctx, script, "secret/data/x", srv.URL, "tok", 30); err == nil {
			t.Fatal("helper get from a 307 server must fail, but it reported success")
		}
	})

	t.Run("put returning 204 succeeds", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("put method = %s, want POST", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()
		if _, err := runVaultAcceptHelper(ctx, script, "put", "secret/data/x", key, srv.URL, "tok", 30); err != nil {
			t.Fatalf("helper put to a 204 server error = %v, want success", err)
		}
	})

	t.Run("oversized get response fails", func(t *testing.T) {
		body := fmt.Sprintf(`{"data":{"data":{"key_b64":%q,"padding":%q}}}`,
			"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", strings.Repeat("x", 2<<20))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		defer srv.Close()
		if _, err := runVaultAcceptHelperGet(ctx, script, "secret/data/x", srv.URL, "tok", 30); err == nil {
			t.Fatal("helper get with a >1MiB response must fail, but it returned a key")
		}
	})
}
