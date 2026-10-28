package main

import (
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

	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// cliFakeTransit mirrors the config-package fake: versioned DEK store,
// encrypt/decrypt/rewrap only, no associated data, identical ciphertext
// when rewrapping an already-latest key.
type cliFakeTransit struct {
	mu             sync.Mutex
	entries        map[string]cliTransitEntry
	currentVersion int
}

type cliTransitEntry struct {
	dek     []byte
	version int
}

func (f *cliFakeTransit) rotate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.currentVersion++
}

func (f *cliFakeTransit) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	op := strings.TrimPrefix(r.URL.Path, "/v1/transit/")
	lookup := func(ct string) (cliTransitEntry, bool) {
		parts := strings.SplitN(ct, ":", 3)
		if len(parts) != 3 {
			return cliTransitEntry{}, false
		}
		ent, ok := f.entries[parts[2]]
		return ent, ok
	}
	switch {
	case strings.HasPrefix(op, "encrypt/"):
		plain, err := base64.StdEncoding.DecodeString(req["plaintext"].(string))
		if err != nil || len(plain) != 32 {
			http.Error(w, "bad dek", http.StatusBadRequest)
			return
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("entry-%d", len(f.entries)+1)))
		f.entries[id] = cliTransitEntry{dek: plain, version: f.currentVersion}
		ct := fmt.Sprintf("vault:v%d:%s", f.currentVersion, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.currentVersion}})
	case strings.HasPrefix(op, "decrypt/"):
		ent, ok := lookup(req["ciphertext"].(string))
		if !ok {
			http.Error(w, "unknown ciphertext", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(ent.dek)}})
	case strings.HasPrefix(op, "rewrap/"):
		ct := req["ciphertext"].(string)
		ent, ok := lookup(ct)
		if !ok {
			http.Error(w, "unknown ciphertext", http.StatusBadRequest)
			return
		}
		if ent.version == f.currentVersion {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.currentVersion}})
			return
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("entry-%d", len(f.entries)+1)))
		f.entries[id] = cliTransitEntry{dek: ent.dek, version: f.currentVersion}
		ct = fmt.Sprintf("vault:v%d:%s", f.currentVersion, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.currentVersion}})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func enableCLITransit(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "token")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
}

func TestSecretsMigrateAndRewrapCommands(t *testing.T) {
	fake := &cliFakeTransit{entries: map[string]cliTransitEntry{}, currentVersion: 1}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	enableCLITransit(t, srv)

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
	if err = runSecrets([]string{"migrate-to-transit", "--config", path, "--legacy-key-file", legacyPath}); err != nil {
		t.Fatal(err)
	}
	migrated, _ := os.ReadFile(path)
	if strings.Contains(string(migrated), "nxs1:") || !strings.Contains(string(migrated), "nxs2:") {
		t.Fatalf("config not migrated: %s", migrated)
	}
	// No migration journal exists anymore (§7.2 uses a verbatim backup).
	if journals, _ := filepath.Glob(path + ".nxs2-migration*"); len(journals) != 0 {
		t.Fatalf("migration journal present: %v", journals)
	}
	if backups, _ := filepath.Glob(path + ".*.pre-transit.bak"); len(backups) != 1 {
		t.Fatalf("expected one verbatim migration backup, got %v", backups)
	}
	if err = runSecrets([]string{"verify", "--config", path}); err != nil {
		t.Fatal(err)
	}
	// Rewrap at the latest version is a no-op: nothing written, no backup.
	before, _ := os.ReadFile(path)
	if err = runSecrets([]string{"rewrap-transit", "--config", path}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("already-latest rewrap rewrote the config")
	}
	if backups, _ := filepath.Glob(path + ".*.pre-rewrap.bak"); len(backups) != 0 {
		t.Fatalf("already-latest rewrap created a backup: %v", backups)
	}
	// After rotation the rewrap moves versions, keeps a verbatim backup,
	// and the result still verifies.
	fake.rotate()
	if err = runSecrets([]string{"rewrap-transit", "--config", path}); err != nil {
		t.Fatal(err)
	}
	rewrapped, _ := os.ReadFile(path)
	if !strings.Contains(string(rewrapped), "nxs2:") {
		t.Fatalf("rewrapped config lost its envelopes: %s", rewrapped)
	}
	rewrapBackups, _ := filepath.Glob(path + ".*.pre-rewrap.bak")
	if len(rewrapBackups) != 1 {
		t.Fatalf("expected one verbatim rewrap backup, got %v", rewrapBackups)
	}
	backup, _ := os.ReadFile(rewrapBackups[0])
	if string(backup) != string(before) {
		t.Fatal("rewrap backup is not the verbatim pre-rewrap bytes")
	}
	if err = runSecrets([]string{"verify", "--config", path}); err != nil {
		t.Fatal(err)
	}
	// Dry runs write nothing.
	if err = runSecrets([]string{"migrate-to-transit", "--config", path, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if err = runSecrets([]string{"rewrap-transit", "--config", path, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	dryAfter, _ := os.ReadFile(path)
	if string(dryAfter) != string(rewrapped) {
		t.Fatal("dry run modified the config")
	}
	// Rotate refuses Transit mode (§7.4).
	if err = runSecrets([]string{"rotate", "--config", path}); err == nil || !strings.Contains(err.Error(), "refuses Vault Transit mode") {
		t.Fatalf("rotate in Transit mode must refuse: %v", err)
	}
}

func TestSecretsMigrateRequiresProviderAndLegacyKey(t *testing.T) {
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"api_key":"x"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSecrets([]string{"migrate-to-transit", "--config", path}); err == nil {
		t.Fatal("migrate-to-transit without Transit provider succeeded")
	}
	// Transit selected but the fixture is nxs1 and no legacy key is
	// supplied: the legacy key is demanded (F11), before any Vault call.
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("VAULT_TOKEN", "token")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
	legacyKey := []byte("01234567890123456789012345678901")
	nxs1, _, err := secrets.TransformConfig([]byte(`{"admin":{"api_key":"canary"}}`), legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	nxs1Path := filepath.Join(dir, "nxs1.json")
	if err := os.WriteFile(nxs1Path, nxs1, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSecrets([]string{"migrate-to-transit", "--config", nxs1Path}); err == nil {
		t.Fatal("migrate-to-transit without legacy key succeeded")
	}
	// A plaintext fixture fails with the plaintext message, not for want
	// of a legacy key (the key is only demanded when nxs1 is present).
	if err := runSecrets([]string{"migrate-to-transit", "--config", path}); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("plaintext migration must fail with the plaintext message: %v", err)
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
