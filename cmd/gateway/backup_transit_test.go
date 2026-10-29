package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// TestBackupRestoreTransitEndToEnd pins ADR 0007 §7.6 (F10): an nxs2
// config backs up with a Transit manifest (no raw key, no .key entry),
// restores only with Vault Transit available, and never stages a .key
// file for an nxs2 config.
func TestBackupRestoreTransitEndToEnd(t *testing.T) {
	fake := &cliFakeTransit{entries: map[string]cliTransitEntry{}, currentVersion: 1}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	enableCLITransit(t, srv)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	cipher, err := secrets.LoadCipher(configPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"backup-canary","enabled":false}]}`)
	enc, _, err := secrets.TransformConfigWith(context.Background(), cipher, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(enc, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(dir, "bundle.tar")
	if err := createBackup(configPath, bundle, false, false, io.Discard, liveBackupOptions{}); err != nil {
		t.Fatal(err)
	}
	// --include-master-key is rejected for Transit configs.
	if err := createBackup(configPath, filepath.Join(dir, "bundle2.tar"), true, false, io.Discard, liveBackupOptions{}); err == nil || !strings.Contains(err.Error(), "--include-master-key is rejected") {
		t.Fatalf("include-master-key on nxs2 must be rejected: %v", err)
	}
	// Read the bundle back: manifest facts, and no master.key entry.
	payloads, manifest := readTestBundle(t, bundle)
	if len(manifest.EnvelopeVersions) != 1 || manifest.EnvelopeVersions[0] != "nxs2" {
		t.Fatalf("manifest envelope_versions must be [nxs2]: %+v", manifest)
	}
	if manifest.TransitKeyRefSHA256 != cipher.KeyRefHash() {
		t.Fatalf("manifest key ref %q != cipher key ref %q", manifest.TransitKeyRefSHA256, cipher.KeyRefHash())
	}
	if len(manifest.TransitKeyVersions) != 1 || manifest.TransitKeyVersions[0] != 1 {
		t.Fatalf("manifest transit_key_versions must be [1]: %+v", manifest)
	}
	if _, ok := payloads[backupEntryKey]; ok {
		t.Fatal("Transit bundle carries a master.key entry")
	}
	nxs2, ok := payloads[backupEntryConfig]
	if !ok || !strings.Contains(string(nxs2), "nxs2:") {
		t.Fatal("bundle config is not nxs2")
	}

	// Restore verification with Transit available: PASS, decrypted.
	stage, err := os.MkdirTemp("", "nexaroute-verify-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	cfg, status, detail := verifyConfigDecryption(stage, payloads, configPath)
	if status != statusPass {
		t.Fatalf("restore verify with Transit must PASS, got %s (%s)", status, detail)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].APIKey != "backup-canary" {
		t.Fatalf("verified config did not decrypt: %+v", cfg.Providers)
	}

	// A bundle carrying a master.key alongside nxs2 is refused outright.
	bad := map[string][]byte{backupEntryConfig: nxs2, backupEntryKey: []byte("stale")}
	if _, _, err := validateBundledConfig(stage, bad); err == nil || !strings.Contains(err.Error(), "never carries a raw key") {
		t.Fatalf("bundled .key with nxs2 must be refused: %v", err)
	}
	// Keyless restore is never justified for nxs2.
	if keylessRestoreJustified(map[string][]byte{backupEntryConfig: nxs2}) {
		t.Fatal("keyless restore justified for nxs2")
	}
}

func TestRestoreVerifyTransitBlockedWithoutVault(t *testing.T) {
	fake := &cliFakeTransit{entries: map[string]cliTransitEntry{}, currentVersion: 1}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	enableCLITransit(t, srv)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	cipher, err := secrets.LoadCipher(configPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"blocked-canary","enabled":false}]}`)
	enc, _, err := secrets.TransformConfigWith(context.Background(), cipher, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	payloads := map[string][]byte{backupEntryConfig: enc}

	// Transit no longer configured on this host: BLOCKED, never PASS,
	// and no .key file is staged for the nxs2 config.
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "")
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("VAULT_TOKEN", "")
	stage, err := os.MkdirTemp("", "nexaroute-verify-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	_, status, _ := verifyConfigDecryption(stage, payloads, configPath)
	if status != statusBlocked {
		t.Fatalf("nxs2 verify without Transit must be BLOCKED, got %s", status)
	}
	stagedKey := filepath.Join(stage, "verify-config", "config.json.key")
	if _, err := os.Stat(stagedKey); !os.IsNotExist(err) {
		t.Fatalf("nxs2 verification staged a .key file: %v", err)
	}
}

func readTestBundle(t *testing.T, bundle string) (map[string][]byte, backupManifest) {
	t.Helper()
	f, err := os.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	payloads := map[string][]byte{}
	var manifest backupManifest
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		payloads[hdr.Name] = data
		if hdr.Name == backupEntryManifest {
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, ok := payloads[backupEntryManifest]; !ok {
		t.Fatal("bundle has no manifest")
	}
	return payloads, manifest
}
