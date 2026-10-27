package main

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

type cliFakeTransit struct {
	mu   sync.Mutex
	keys map[string]string
}

func (f *cliFakeTransit) handler(w http.ResponseWriter, r *http.Request) {
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
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		plain, _ := base64.RawURLEncoding.DecodeString(strings.SplitN(ct, ":", 3)[2])
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(plain)}})
	case strings.HasPrefix(op, "rewrap/"):
		ct := req["ciphertext"].(string)
		plain, _ := base64.RawURLEncoding.DecodeString(strings.SplitN(ct, ":", 3)[2])
		next := "vault:v2:" + base64.RawURLEncoding.EncodeToString(plain)
		f.keys[next] = req["associated_data"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": next}})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func TestSecretsMigrateAndRewrapCommands(t *testing.T) {
	fake := &cliFakeTransit{keys: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "vault-transit")
	t.Setenv("NEXAROUTE_TRANSIT_ADDR", srv.URL)
	t.Setenv("NEXAROUTE_TRANSIT_TOKEN", "token")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacyKey := []byte("01234567890123456789012345678901")
	legacyPath := filepath.Join(dir, "legacy.key")
	if err := os.WriteFile(legacyPath, legacyKey, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"cli-canary","enabled":false}]}`), legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = runSecrets([]string{"migrate-envelope", "--config", path, "--legacy-key-file", legacyPath}); err != nil {
		t.Fatal(err)
	}
	migrated, _ := os.ReadFile(path)
	if strings.Contains(string(migrated), "nxs1:") || !strings.Contains(string(migrated), "nxs2:") {
		t.Fatalf("config not migrated: %s", migrated)
	}
	if _, err = os.Stat(path + ".nxs2-migration"); !os.IsNotExist(err) {
		t.Fatalf("migration journal not removed: %v", err)
	}
	if err = runSecrets([]string{"verify", "--config", path}); err != nil {
		t.Fatal(err)
	}
	if err = runSecrets([]string{"rewrap", "--config", path}); err != nil {
		t.Fatal(err)
	}
	if err = runSecrets([]string{"verify", "--config", path}); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".*.enc.bak")
	if len(backups) < 2 {
		t.Fatalf("expected migration and rewrap backups, got %v", backups)
	}
}

func TestSecretsMigrateRequiresProviderAndLegacyKey(t *testing.T) {
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"api_key":"x"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSecrets([]string{"migrate-envelope", "--config", path}); err == nil {
		t.Fatal("migrate-envelope without Transit provider succeeded")
	}
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "vault-transit")
	t.Setenv("NEXAROUTE_TRANSIT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("NEXAROUTE_TRANSIT_TOKEN", "token")
	if err := runSecrets([]string{"migrate-envelope", "--config", path}); err == nil {
		t.Fatal("migrate-envelope without legacy key succeeded")
	}
	for _, tc := range []struct {
		file string
		b64  string
	}{
		{"", "short"},
		{"", base64.StdEncoding.EncodeToString(make([]byte, 31))},
	} {
		if _, err := loadLegacyKey(tc.file, tc.b64); err == nil {
			t.Fatal("invalid legacy key accepted")
		}
	}
}
