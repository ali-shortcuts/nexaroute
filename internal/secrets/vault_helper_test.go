package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Vault KV reference helper (scripts/key-provider-vault-kv.sh)
// implements the CommandProvider protocol over Vault's HTTP API.
// These tests run the actual script against a fake Vault and pin the
// wire behavior: request path and token header, KV v2 payload shape,
// stdout carrying ONLY the base64 key on get, and fail-closed errors
// (nonzero exit, nothing on stdout) — the exact properties the
// gateway relies on when NEXAROUTE_KEY_PROVIDER=command points at it.
type fakeVault struct {
	mu       sync.Mutex
	entry    string // stored key_b64; "" = no entry
	requests []fakeVaultRequest
	deny     bool
}

type fakeVaultRequest struct {
	method, path, token, body string
}

func (f *fakeVault) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, fakeVaultRequest{r.Method, r.URL.Path, r.Header.Get("X-Vault-Token"), string(body)})
	deny := f.deny
	entry := f.entry
	f.mu.Unlock()
	if deny || r.Header.Get("X-Vault-Token") != "test-token" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if entry == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"data":{"data":{"key_b64":%q},"metadata":{"version":1}}}`, entry)
	case http.MethodPost:
		// Extract key_b64 the way Vault would store it.
		var payload struct {
			Data struct {
				KeyB64 string `json:"key_b64"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Data.KeyB64 == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.entry = payload.Data.KeyB64
		f.mu.Unlock()
		fmt.Fprint(w, `{"data":{"version":2}}`)
	}
}

func vaultHelperPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "key-provider-vault-kv.sh")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("helper script not found at %s", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestVaultKVHelperRoundTripThroughCommandProvider(t *testing.T) {
	vault := &fakeVault{}
	ts := httptest.NewServer(http.HandlerFunc(vault.handler))
	defer ts.Close()
	t.Setenv("VAULT_ADDR", ts.URL)
	t.Setenv("VAULT_TOKEN", "test-token")

	provider := CommandProvider{Command: vaultHelperPath(t)}
	ctx := context.Background()
	ref := "secret/data/nexaroute/master-key"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	if err := provider.Store(ctx, ref, key); err != nil {
		t.Fatalf("store via vault helper: %v", err)
	}
	got, err := provider.Load(ctx, ref)
	if err != nil {
		t.Fatalf("load via vault helper: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("vault helper round trip returned a different key")
	}
	// Wire assertions: KV v2 path, token header, POST then GET.
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if len(vault.requests) != 2 {
		t.Fatalf("vault saw %d requests, want 2", len(vault.requests))
	}
	post, get := vault.requests[0], vault.requests[1]
	if post.method != http.MethodPost || post.path != "/v1/secret/data/nexaroute/master-key" || post.token != "test-token" {
		t.Errorf("POST request wrong: %+v", post)
	}
	if !strings.Contains(post.body, `"key_b64":"`+base64.StdEncoding.EncodeToString(key)+`"`) {
		t.Errorf("POST body missing the base64 key: %s", post.body)
	}
	if get.method != http.MethodGet || get.path != post.path {
		t.Errorf("GET request wrong: %+v", get)
	}
}

// A Vault that accepts the connection but never responds must not hang the
// helper: curl has no total timeout by default, and the gateway loads its
// master key with context.Background, so the helper itself must bound the
// request. VAULT_HELPER_TIMEOUT=1 keeps this test fast; the handler sleeps
// 5s, so a pass here proves the bound fires well before the server answers.
func TestVaultKVHelperSlowServerTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		fmt.Fprint(w, `{"data":{"data":{"key_b64":"AAAA"},"metadata":{"version":1}}}`)
	}))
	defer slow.Close()
	t.Setenv("VAULT_ADDR", slow.URL)
	t.Setenv("VAULT_TOKEN", "test-token")
	t.Setenv("VAULT_HELPER_TIMEOUT", "1")

	provider := CommandProvider{Command: vaultHelperPath(t)}
	start := time.Now()
	_, err := provider.Load(context.Background(), "secret/data/nexaroute/master-key")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("load from a never-responding Vault must fail")
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("slow-server load took %v, want under 4s with VAULT_HELPER_TIMEOUT=1", elapsed)
	}
}

func TestVaultKVHelperFailuresAreClosed(t *testing.T) {
	vault := &fakeVault{deny: true}
	ts := httptest.NewServer(http.HandlerFunc(vault.handler))
	defer ts.Close()
	provider := CommandProvider{Command: vaultHelperPath(t)}
	ctx := context.Background()

	t.Setenv("VAULT_ADDR", ts.URL)
	t.Setenv("VAULT_TOKEN", "wrong-token")
	if _, err := provider.Load(ctx, "secret/data/nexaroute/master-key"); err == nil {
		t.Fatal("load with a denied token must fail")
	}
	// Missing configuration fails before any request is attempted.
	os.Unsetenv("VAULT_TOKEN")
	if _, err := provider.Load(ctx, "secret/data/nexaroute/master-key"); err == nil {
		t.Fatal("load without VAULT_TOKEN must fail")
	}
}
