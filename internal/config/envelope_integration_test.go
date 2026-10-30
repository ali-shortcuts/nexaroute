package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
// exercise the real config load/save path with nxs2 envelopes. It models
// the revised ADR 0007 contract: encrypt/decrypt/rewrap only, no
// associated data or context, versioned wrapped keys, and an identical
// ciphertext when rewrapping an already-latest key.
type fakeTransit struct {
	mu             sync.Mutex
	entries        map[string]fakeTransitEntry
	currentVersion int
	encryptCalls   int
}

type fakeTransitEntry struct {
	dek     []byte
	version int
}

func (f *fakeTransit) rotate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.currentVersion++
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
		f.encryptCalls++
		plain, err := base64.StdEncoding.DecodeString(req["plaintext"].(string))
		if err != nil || len(plain) != 32 {
			http.Error(w, "bad dek", http.StatusBadRequest)
			return
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("entry-%d", len(f.entries)+1)))
		f.entries[id] = fakeTransitEntry{dek: plain, version: f.currentVersion}
		ct := fmt.Sprintf("vault:v%d:%s", f.currentVersion, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.currentVersion}})
	case strings.HasPrefix(op, "decrypt/"):
		ct := req["ciphertext"].(string)
		parts := strings.SplitN(ct, ":", 3)
		if len(parts) != 3 {
			http.Error(w, "bad ciphertext", http.StatusBadRequest)
			return
		}
		ent, ok := f.entries[parts[2]]
		if !ok {
			http.Error(w, "unknown ciphertext", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(ent.dek)}})
	case strings.HasPrefix(op, "rewrap/"):
		ct := req["ciphertext"].(string)
		parts := strings.SplitN(ct, ":", 3)
		if len(parts) != 3 {
			http.Error(w, "bad ciphertext", http.StatusBadRequest)
			return
		}
		ent, ok := f.entries[parts[2]]
		if !ok {
			http.Error(w, "unknown ciphertext", http.StatusBadRequest)
			return
		}
		if ent.version == f.currentVersion {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.currentVersion}})
			return
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("entry-%d", len(f.entries)+1)))
		f.entries[id] = fakeTransitEntry{dek: ent.dek, version: f.currentVersion}
		ct = fmt.Sprintf("vault:v%d:%s", f.currentVersion, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.currentVersion}})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func enableTransit(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "test-token")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
}

func TestConfigLoadSaveWithVaultTransitEnvelope(t *testing.T) {
	fake := &fakeTransit{entries: map[string]fakeTransitEntry{}, currentVersion: 1}
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
	// §7.5 save preserves unchanged envelopes byte-for-byte: saving the
	// same plaintext again must not change the stored envelope and must
	// not call Transit encrypt again.
	storedEnvelope := string(raw)
	encryptCalls := fake.encryptCalls
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(path)
	if string(raw2) != storedEnvelope {
		t.Fatalf("unchanged save rewrote the stored envelope:\nbefore: %s\nafter: %s", storedEnvelope, raw2)
	}
	if fake.encryptCalls != encryptCalls {
		t.Fatalf("unchanged save called Transit encrypt %d times (was %d)", fake.encryptCalls, encryptCalls)
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

	// Plaintext secret fields fail closed on load in Transit mode (§7.5):
	// no silent auto-migration, no local key.
	plainPath := filepath.Join(dir, "plain.json")
	if err = os.WriteFile(plainPath, []byte(`{"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"plain-canary","enabled":false}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadBase(plainPath); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("plaintext config in Transit mode must fail with the explicit-migration message: %v", err)
	}
	if _, err = os.Stat(plainPath + ".key"); !os.IsNotExist(err) {
		t.Fatalf("plaintext load in Transit mode created a local key: %v", err)
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
	backups, _ := filepath.Glob(legacyPath + ".*.pre-transit.bak")
	if len(backups) != 1 {
		t.Fatalf("expected one verbatim migration backup, got %v", backups)
	}
	// The backup holds the verbatim pre-migration bytes.
	backup, _ := os.ReadFile(backups[0])
	if !strings.Contains(string(backup), "nxs1:") {
		t.Fatalf("migration backup is not the pre-migration bytes: %s", backup)
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
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:1")
	t.Setenv("VAULT_TOKEN", "token")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
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

func TestConfigTransitGateOffFailsClosed(t *testing.T) {
	// Provider selected but gate not exactly "true": selection fails
	// before any Vault call and before any local key is considered.
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "")
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:1")
	t.Setenv("VAULT_TOKEN", "token")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"api_key":"plain"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(path); err == nil {
		t.Fatal("gate-off Transit load succeeded")
	}
	if _, err := os.Stat(path + ".key"); !os.IsNotExist(err) {
		t.Fatalf("gate-off load created a local fallback key: %v", err)
	}
}

func TestConfigTransitForbiddenFailsClosed(t *testing.T) {
	fake := &fakeTransit{entries: map[string]fakeTransitEntry{}, currentVersion: 1}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	enableTransit(t, srv)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Default()
	c.Providers = []ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "forbidden-canary", Enabled: false}}
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer denied.Close()
	t.Setenv("VAULT_ADDR", denied.URL)
	if _, err := LoadBase(path); err == nil {
		t.Fatal("forbidden Transit accepted an nxs2 config")
	} else {
		var terr *secrets.TransitError
		if !errors.As(err, &terr) || terr.Class != secrets.ClassTransitForbidden {
			t.Fatalf("forbidden Transit must fail with transit_forbidden, got %v", err)
		}
	}
	if _, err := os.Stat(path + ".key"); !os.IsNotExist(err) {
		t.Fatalf("forbidden Transit created a local fallback key: %v", err)
	}
}
