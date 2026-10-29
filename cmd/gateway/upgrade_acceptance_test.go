package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Restore key custody: a backup bundle must never silently replace the
// master key at the target, and a refused restore must leave a working
// installation behind.
//
// Covered elsewhere and NOT repeated here: tampered-bundle rejection,
// overwrite refusal for the config file, rollback on install failure,
// and journal crash recovery. This test pins the key-specific edges:
//   - a keyless bundle restored with --force installs the config bytes
//     but leaves the target's different key byte-identical (so the
//     resulting key/config mismatch fails closed on load instead of
//     silently adopting the bundle's key — there is none in it);
//   - a key-included bundle restored with --force replaces the key only
//     with a loud warning, and the restored pair then loads;
//   - a restore without --force over a differing target changes nothing
//     (config, key, security store, modes) and the target still loads
//     with its own key.
//
// All credentials are synthetic canaries; all state is in TempDirs.

const upgradeRestoreSourceConfig = `{"listen":"127.0.0.1:8080","admin":{"api_key":"upgrade-restore-admin-canary","security_store_path":"security/nexaroute-security.db"},"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","enabled":false,"api_key":"upgrade-restore-provider-canary"}]}`

const upgradeRestoreTargetConfig = `{"listen":"127.0.0.1:8081","admin":{"api_key":"upgrade-target-admin-canary","security_store_path":"security/nexaroute-security.db"},"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","enabled":false,"api_key":"upgrade-target-provider-canary"}]}`

func upgradeRestoreFixture(t *testing.T, raw string) (dir, configPath string, keyBytes, configBytes []byte) {
	t.Helper()
	dir = t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	securityDir := filepath.Join(dir, "security")
	if err := os.MkdirAll(securityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := securitystore.Open(filepath.Join(securityDir, "nexaroute-security.db"), 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, action := range []string{"auth.login", "config.update"} {
		if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{
			Timestamp: now, Actor: "alice", Action: action, Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// First load performs the documented plaintext-to-encrypted migration
	// and auto-creates the sibling master key; the test then snapshots
	// both, so any later restore step can be checked byte-for-byte.
	if _, err := config.LoadBase(configPath); err != nil {
		t.Fatal(err)
	}
	keyBytes, err = os.ReadFile(configPath + ".key")
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBytes), "nxs1:") {
		t.Fatal("fixture was not migrated to encrypted storage")
	}
	return dir, configPath, keyBytes, configBytes
}

func runCode(t *testing.T, fn func([]string, *bytes.Buffer, *bytes.Buffer) int, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := fn(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestUpgradeRestoreKeyCustody(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")

	_, sourceConfig, sourceKey, sourceConfigBytes := upgradeRestoreFixture(t, upgradeRestoreSourceConfig)
	_ = sourceConfig

	bundleDir := t.TempDir()
	keylessBundle := filepath.Join(bundleDir, "keyless.tar")
	if code, _, stderr := runCode(t, func(a []string, o, e *bytes.Buffer) int { return runBackup(a, o, e) },
		[]string{"--config", sourceConfig, "--out", keylessBundle}); code != 0 {
		t.Fatalf("keyless backup exit=%d stderr=%s", code, stderr)
	}
	if contains(tarEntryNames(t, keylessBundle), "master.key") {
		t.Fatal("keyless bundle must not contain the master key")
	}
	keyedBundle := filepath.Join(bundleDir, "keyed.tar")
	if code, _, stderr := runCode(t, func(a []string, o, e *bytes.Buffer) int { return runBackup(a, o, e) },
		[]string{"--config", sourceConfig, "--out", keyedBundle, "--include-master-key"}); code != 0 {
		t.Fatalf("keyed backup exit=%d stderr=%s", code, stderr)
	}
	if !contains(tarEntryNames(t, keyedBundle), "master.key") {
		t.Fatal("opt-in bundle must contain the master key")
	}

	t.Run("keyless force restore leaves the target key untouched", func(t *testing.T) {
		_, targetConfig, targetKey, _ := upgradeRestoreFixture(t, upgradeRestoreTargetConfig)
		if bytes.Equal(targetKey, sourceKey) {
			t.Fatal("fixture keys must differ for the custody assertion to mean anything")
		}
		if code, _, stderr := runCode(t, func(a []string, o, e *bytes.Buffer) int { return runRestore(a, o, e) },
			[]string{"--bundle", keylessBundle, "--config", targetConfig, "--force"}); code != 0 {
			t.Fatalf("keyless force restore exit=%d stderr=%s", code, stderr)
		}
		restoredConfig, err := os.ReadFile(targetConfig)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restoredConfig, sourceConfigBytes) {
			t.Fatal("keyless restore did not install the bundled config bytes")
		}
		restoredKey, err := os.ReadFile(targetConfig + ".key")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restoredKey, targetKey) {
			t.Fatal("keyless restore silently replaced the target master key")
		}
		if mode := upgradeFileMode(t, targetConfig); mode != 0o600 {
			t.Fatalf("restored config mode=%o, want 0600", mode)
		}
		if mode := upgradeFileMode(t, targetConfig+".key"); mode != 0o600 {
			t.Fatalf("target key mode=%o, want 0600", mode)
		}
		// The mismatch must fail closed, not silently adopt anything:
		// the restored config cannot decrypt with the untouched key.
		if _, err := config.LoadBase(targetConfig); err == nil {
			t.Fatal("config restored without its paired key must not load")
		}
	})

	t.Run("keyed force restore warns and only then replaces the key", func(t *testing.T) {
		_, targetConfig, targetKey, _ := upgradeRestoreFixture(t, upgradeRestoreTargetConfig)
		if code, stdout, stderr := runCode(t, func(a []string, o, e *bytes.Buffer) int { return runRestore(a, o, e) },
			[]string{"--bundle", keyedBundle, "--config", targetConfig, "--force"}); code != 0 {
			t.Fatalf("keyed force restore exit=%d stderr=%s", code, stderr)
		} else if !strings.Contains(stdout+stderr, "WARNING: overwriting the existing master key") {
			t.Fatalf("key replacement must warn loudly, got stdout=%q stderr=%q", stdout, stderr)
		}
		restoredKey, err := os.ReadFile(targetConfig + ".key")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restoredKey, sourceKey) {
			t.Fatal("keyed force restore did not install the bundled key")
		}
		if bytes.Equal(restoredKey, targetKey) {
			t.Fatal("keyed force restore left the old key in place")
		}
		restored, err := config.LoadBase(targetConfig)
		if err != nil {
			t.Fatalf("restored key/config pair must load: %v", err)
		}
		if restored.Admin.APIKey != "upgrade-restore-admin-canary" {
			t.Fatalf("restored pair decrypts to the wrong admin secret: %q", restored.Admin.APIKey)
		}
	})

	t.Run("refused restore leaves a working installation", func(t *testing.T) {
		_, targetConfig, targetKey, targetConfigBytes := upgradeRestoreFixture(t, upgradeRestoreTargetConfig)
		targetStore := filepath.Join(filepath.Dir(targetConfig), "security", "nexaroute-security.db")
		storeBefore, err := os.ReadFile(targetStore)
		if err != nil {
			t.Fatal(err)
		}
		configModeBefore := upgradeFileMode(t, targetConfig)
		keyModeBefore := upgradeFileMode(t, targetConfig+".key")
		if code, _, _ := runCode(t, func(a []string, o, e *bytes.Buffer) int { return runRestore(a, o, e) },
			[]string{"--bundle", keylessBundle, "--config", targetConfig}); code != 1 {
			t.Fatalf("restore without --force over a differing target exit=%d, want 1", code)
		}
		if got, err := os.ReadFile(targetConfig); err != nil || !bytes.Equal(got, targetConfigBytes) {
			t.Fatalf("refused restore changed the config: err=%v", err)
		}
		if got, err := os.ReadFile(targetConfig + ".key"); err != nil || !bytes.Equal(got, targetKey) {
			t.Fatalf("refused restore changed the key: err=%v", err)
		}
		if got, err := os.ReadFile(targetStore); err != nil || !bytes.Equal(got, storeBefore) {
			t.Fatalf("refused restore changed the security store: err=%v", err)
		}
		if mode := upgradeFileMode(t, targetConfig); mode != configModeBefore {
			t.Fatalf("refused restore changed config mode: %o -> %o", configModeBefore, mode)
		}
		if mode := upgradeFileMode(t, targetConfig+".key"); mode != keyModeBefore {
			t.Fatalf("refused restore changed key mode: %o -> %o", keyModeBefore, mode)
		}
		restored, err := config.LoadBase(targetConfig)
		if err != nil {
			t.Fatalf("target must still load with its own key after refusal: %v", err)
		}
		if restored.Admin.APIKey != "upgrade-target-admin-canary" {
			t.Fatalf("refused restore changed the target secrets: %q", restored.Admin.APIKey)
		}
	})
}

func upgradeFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
