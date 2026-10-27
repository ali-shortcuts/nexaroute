package config

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// fakeTransit is a minimal protocol-faithful Vault Transit endpoint used to
// exercise the real config load/save path with nxs2 envelopes.
type fakeTransit struct {
	mu   sync.Mutex
	keys map[string]string // ciphertext -> associated data
}

func (f *fakeTransit) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	op := strings.TrimPrefix(r.URL.Path, "/v1/transit/")
	switch {
	case strings.HasPrefix(op, "encrypt/"):
		plain, _ := base64.StdEncoding.DecodeString(req["plaintext"].(string))
		ct := "vault:v1:" + base64.RawURLEncoding.EncodeToString(plain)
		f.keys[ct] = req["associated_data"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct}})
	case strings.HasPrefix(op, "decrypt/"):
		ct := req["ciphertext"].(string)
		if f.keys[ct] != req["associated_data"].(string) {
			http.Error(w, "denied", http.StatusBadRequest)
			return
		}
		plain, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ct, "vault:v1:"))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(plain)}})
	case strings.HasPrefix(op, "rewrap/"):
		ct := req["ciphertext"].(string)
		plain, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ct, "vault:v1:"))
		next := "vault:v2:" + base64.RawURLEncoding.EncodeToString(plain)
		f.keys[next] = req["associated_data"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": next}})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func enableTransit(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "vault-transit")
	t.Setenv("NEXAROUTE_TRANSIT_ADDR", srv.URL)
	t.Setenv("NEXAROUTE_TRANSIT_TOKEN", "test-token")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
}

func TestConfigLoadSaveWithVaultTransitEnvelope(t *testing.T) {
	fake := &fakeTransit{keys: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	enableTransit(t, srv)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Default()
	c.Providers = []ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "transit-canary", Enabled: false}}
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), secrets.EnvelopePrefix) || strings.Contains(string(raw), "transit-canary") {
		t.Fatalf("config was not stored as nxs2: %s", raw)
	}
	if _, err = os.Stat(path + ".key"); !os.IsNotExist(err) {
		t.Fatalf("Transit mode created a local key file: %v", err)
	}
	loaded, err := LoadBase(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers) != 1 || loaded.Providers[0].APIKey != "transit-canary" {
		t.Fatalf("unexpected loaded provider: %+v", loaded.Providers)
	}
	if _, err = Load(path); err != nil {
		t.Fatal(err)
	}

	legacyKey := []byte("01234567890123456789012345678901")
	legacy, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"legacy-canary","enabled":false}]}`), legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, "legacy.json")
	if err = os.WriteFile(legacyPath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadBase(legacyPath); err == nil || !strings.Contains(err.Error(), "nxs1") {
		t.Fatalf("nxs1 config was not rejected in Transit mode: %v", err)
	}
	if _, err = secrets.MigrateToEnvelopeFile(t.Context(), legacyPath, legacyKey, mustEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	migrated, err := LoadBase(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated.Providers) != 1 || migrated.Providers[0].APIKey != "legacy-canary" {
		t.Fatalf("unexpected migrated provider: %+v", migrated.Providers)
	}
	backups, _ := filepath.Glob(legacyPath + ".*.enc.bak")
	if len(backups) != 1 {
		t.Fatalf("expected migration backup, got %v", backups)
	}
}

func mustEnvelope(t *testing.T) secrets.EnvelopeKeyProvider {
	t.Helper()
	env, enabled, err := secrets.ConfiguredEnvelope()
	if err != nil || !enabled {
		t.Fatalf("envelope not configured: %v", err)
	}
	return env
}

func TestConfigTransitMissingProviderFailsClosed(t *testing.T) {
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "vault-transit")
	t.Setenv("NEXAROUTE_TRANSIT_ADDR", "http://127.0.0.1:1")
	t.Setenv("NEXAROUTE_TRANSIT_TOKEN", "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacyKey := []byte("01234567890123456789012345678901")
	raw, _, err := secrets.TransformConfig([]byte(`{"admin":{"api_key":"canary"}}`), legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadBase(path); err == nil {
		t.Fatal("unreachable Transit accepted an nxs2 config")
	}
	// A plaintext config must not silently create a local key in Transit mode
	// when the provider is unreachable; the error is surfaced instead.
	plainPath := filepath.Join(dir, "plain.json")
	if err = os.WriteFile(plainPath, []byte(`{"admin":{"api_key":"plain"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadBase(plainPath); err == nil {
		t.Fatal("unreachable Transit accepted plaintext encryption")
	}
	if _, err = os.Stat(plainPath + ".key"); !os.IsNotExist(err) {
		t.Fatalf("Transit mode created a local fallback key: %v", err)
	}
}
