package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// Upgrade acceptance: representative prior-installation config shapes must
// load on the current tree, preserve every synthetic secret, and never
// silently replace the master key.
//
// The shapes below are the ones an upgrade actually meets in the field:
//   - plaintext-legacy: a pre-encryption config (no nxs1 envelopes at all);
//   - nxs1-prior-release: the exact envelope format shipped by v0.21.0
//     through v0.24.0 (verified identical across those tags: Prefix
//     "nxs1:", AES-256-GCM envelope, key-id binding) — here produced with
//     a synthetic key through the same TransformConfig path, so the test
//     pins the format the upgrade must still read, not one release's bytes;
//   - legacy-public-model: routing.public_model, migrated to a virtual
//     endpoint with a warning, never silently dropped;
//   - minimal-older: a config that predates newer optional sections, which
//     must boot on defaults instead of failing on missing fields.
//
// This matrix deliberately does NOT duplicate the single-shape tests
// (plaintext migration idempotency, public-model warning, strict refusal):
// it pins the cross-shape upgrade contract in one place, plus the key
// custody and failure-preservation properties no earlier test asserted.

const upgradePlaintextLegacy = `{"listen":"127.0.0.1:8080","admin":{"api_key":"upgrade-admin-canary"},"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","enabled":false,"api_key":"upgrade-provider-canary","credentials":[{"api_key":"upgrade-pool-canary"}]}],"client_auth":{"keys":["upgrade-client-canary"]}}`

var upgradeSyntheticKey = []byte("01234567890123456789012345678901")

func clearUpgradeKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	// Strict mode must be explicitly "false", not empty: strictConfigEnabled
	// parses the variable when present, and "" fails ParseBool.
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "false")
}

func upgradeSecretValues(t *testing.T, cfg Config) (admin, provider, pool string, clientHashes int) {
	t.Helper()
	admin = cfg.Admin.APIKey
	if len(cfg.Providers) > 0 {
		provider = cfg.Providers[0].APIKey
		if len(cfg.Providers[0].Credentials) > 0 {
			pool = cfg.Providers[0].Credentials[0].APIKey
		}
	}
	return admin, provider, pool, len(cfg.ClientAuth.KeyHashes)
}

func TestUpgradePriorShapesLoadAndPreserveSecrets(t *testing.T) {
	clearUpgradeKeyEnv(t)

	t.Run("plaintext-legacy migrates and keeps secrets", func(t *testing.T) {
		clearUpgradeKeyEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(upgradePlaintextLegacy), 0o600); err != nil {
			t.Fatal(err)
		}
		// Load (not just LoadBase) exercises the client-key persist path:
		// plaintext client keys must be hashed on disk, never kept usable.
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		admin, provider, pool, hashes := upgradeSecretValues(t, cfg)
		if admin != "upgrade-admin-canary" || provider != "upgrade-provider-canary" || pool != "upgrade-pool-canary" {
			t.Fatalf("migration changed secret values: admin=%q provider=%q pool=%q", admin, provider, pool)
		}
		if len(cfg.ClientAuth.Keys) != 0 || hashes != 1 || cfg.ClientAuth.KeyHashes[0] != ClientKeyDigest("upgrade-client-canary") {
			t.Fatalf("client key was not migrated to its digest: keys=%d hashes=%v", len(cfg.ClientAuth.Keys), cfg.ClientAuth.KeyHashes)
		}
		disk, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"upgrade-admin-canary", "upgrade-provider-canary", "upgrade-pool-canary", "upgrade-client-canary"} {
			if strings.Contains(string(disk), canary) {
				t.Fatalf("plaintext canary %q left on disk after migration", canary)
			}
		}
		if !strings.Contains(string(disk), secrets.Prefix) {
			t.Fatal("migrated config carries no nxs1 ciphertext")
		}
		if mode := fileMode(t, path); mode != 0o600 {
			t.Fatalf("migrated config mode=%o, want 0600", mode)
		}
		for _, w := range cfg.Warnings {
			if strings.Contains(w, "upgrade-") {
				t.Fatalf("warning leaks a secret value: %q", w)
			}
		}
	})

	t.Run("nxs1-prior-release loads unchanged", func(t *testing.T) {
		clearUpgradeKeyEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		encrypted, _, err := secrets.TransformConfig([]byte(upgradePlaintextLegacy), upgradeSyntheticKey, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encrypted, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		keyPath := path + ".key"
		if err := os.WriteFile(keyPath, upgradeSyntheticKey, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadBase(path)
		if err != nil {
			t.Fatal(err)
		}
		admin, provider, pool, hashes := upgradeSecretValues(t, cfg)
		if admin != "upgrade-admin-canary" || provider != "upgrade-provider-canary" || pool != "upgrade-pool-canary" {
			t.Fatalf("nxs1 load changed secret values: admin=%q provider=%q pool=%q", admin, provider, pool)
		}
		if len(cfg.ClientAuth.Keys) != 0 || hashes != 1 {
			t.Fatalf("nxs1 client key was not hashed in memory: keys=%d hashes=%d", len(cfg.ClientAuth.Keys), hashes)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("loading an already-encrypted prior-release config rewrote it; an upgrade read must be stable")
		}
		backs, err := filepath.Glob(path + ".*.enc.bak")
		if err != nil || len(backs) != 0 {
			t.Fatalf("already-encrypted config gained migration backups: %v err=%v", backs, err)
		}
		if keyAfter, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(keyAfter, upgradeSyntheticKey) {
			t.Fatalf("nxs1 load changed the master key: err=%v", err)
		}
	})

	t.Run("legacy-public-model migrates with warning", func(t *testing.T) {
		clearUpgradeKeyEnv(t)
		path := writeConfig(t, `{"listen": "127.0.0.1:8080", "routing": {"public_model": "legacy-gpt"}, `+testProviders+`}`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.VirtualEndpoints) != 1 || cfg.VirtualEndpoints[0].PublicModel != "legacy-gpt" {
			t.Fatalf("legacy public_model was dropped or changed: endpoints=%v", cfg.VirtualEndpoints)
		}
		if !hasWarningContaining(cfg.Warnings, "routing.public_model") {
			t.Fatalf("legacy migration warning missing, warnings=%v", cfg.Warnings)
		}
	})

	t.Run("minimal-older config boots on defaults", func(t *testing.T) {
		clearUpgradeKeyEnv(t)
		path := writeConfig(t, `{"listen": "127.0.0.1:8080"}`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("minimal older config must load on defaults: %v", err)
		}
		if cfg.Listen != "127.0.0.1:8080" {
			t.Fatalf("listen changed: %q", cfg.Listen)
		}
	})
}

// The master key is custody, not a replaceable default: no migration,
// reload, or client-key persist step may rewrite it, and the encrypted
// migration backup must decrypt back to exactly the original plaintext.
func TestUpgradeMigrationPreservesMasterKeyAndBackupDecrypts(t *testing.T) {
	clearUpgradeKeyEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Deliberately 0644 on disk: migration must tighten the config to 0600
	// via its atomic rewrite, while leaving the key file byte-identical.
	if err := os.WriteFile(path, []byte(upgradePlaintextLegacy), 0o644); err != nil {
		t.Fatal(err)
	}
	keyPath := path + ".key"
	if err := os.WriteFile(keyPath, upgradeSyntheticKey, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBase(path)
	if err != nil {
		t.Fatal(err)
	}
	admin, provider, pool, _ := upgradeSecretValues(t, cfg)
	if admin != "upgrade-admin-canary" || provider != "upgrade-provider-canary" || pool != "upgrade-pool-canary" {
		t.Fatalf("migration changed secret values: %q %q %q", admin, provider, pool)
	}
	if keyAfter, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(keyAfter, upgradeSyntheticKey) {
		t.Fatalf("plaintext migration rewrote the master key: err=%v", err)
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("migrated config mode=%o, want 0600", mode)
	}
	if mode := fileMode(t, keyPath); mode != 0o600 {
		t.Fatalf("master key mode=%o, want 0600", mode)
	}
	backs, err := filepath.Glob(path + ".*.enc.bak")
	if err != nil || len(backs) != 1 {
		t.Fatalf("migration backup count=%d err=%v, want exactly 1", len(backs), err)
	}
	if mode := fileMode(t, backs[0]); mode != 0o600 {
		t.Fatalf("migration backup mode=%o, want 0600", mode)
	}
	raw, err := os.ReadFile(backs[0])
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := secrets.DecryptBackup(upgradeSyntheticKey, raw)
	if err != nil {
		t.Fatalf("migration backup does not decrypt with the same key: %v", err)
	}
	if string(decrypted) != upgradePlaintextLegacy {
		t.Fatalf("migration backup does not round-trip the original plaintext:\n%s", decrypted)
	}
	// Idempotent reload and the client-key persist path must both keep the
	// same key bytes: a second load, and a Load that persists hashed
	// client keys, are upgrades too, and neither may rotate the key.
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(path); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(migrated, again) {
		t.Fatal("second load rewrote an already-migrated config")
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if keyAfter, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(keyAfter, upgradeSyntheticKey) {
		t.Fatalf("reload/persist rewrote the master key: err=%v", err)
	}
	backs, err = filepath.Glob(path + ".*.enc.bak")
	if err != nil || len(backs) != 1 {
		t.Fatalf("reload/persist created extra migration backups: %v err=%v", backs, err)
	}
}

// A failed migration must be a no-op on disk: the original config, the
// key, and the absence of a migration backup are all preserved, so the
// operator can fix the cause and retry instead of recovering a half
// migrated installation. The bad-key-mode case is deterministic even
// when the tests run as root, because the mode check is in code, not
// enforced by the filesystem.
func TestUpgradeMigrationFailurePreservesOriginalAndKey(t *testing.T) {
	clearUpgradeKeyEnv(t)

	t.Run("key file with wrong mode", func(t *testing.T) {
		clearUpgradeKeyEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(upgradePlaintextLegacy), 0o600); err != nil {
			t.Fatal(err)
		}
		keyPath := path + ".key"
		if err := os.WriteFile(keyPath, upgradeSyntheticKey, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBase(path); err == nil {
			t.Fatal("migration with a 0644 key file must fail closed")
		} else if !strings.Contains(err.Error(), "0600") {
			t.Fatalf("key-mode error must name the required mode, got: %v", err)
		}
		assertUpgradeUntouched(t, path, keyPath, upgradeSyntheticKey, upgradePlaintextLegacy)
		// The failure was non-destructive: fixing the mode retries cleanly.
		if err := os.Chmod(keyPath, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadBase(path)
		if err != nil {
			t.Fatalf("retry after fixing key mode must succeed: %v", err)
		}
		if admin, _, _, _ := upgradeSecretValues(t, cfg); admin != "upgrade-admin-canary" {
			t.Fatalf("retry changed the admin secret: %q", admin)
		}
	})

	t.Run("encrypted config with the wrong key", func(t *testing.T) {
		clearUpgradeKeyEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		encrypted, _, err := secrets.TransformConfig([]byte(upgradePlaintextLegacy), upgradeSyntheticKey, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encrypted, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		wrongKey := []byte("98765432109876543210987654321098")
		keyPath := path + ".key"
		if err := os.WriteFile(keyPath, wrongKey, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBase(path); err == nil {
			t.Fatal("decryption with the wrong key must fail closed")
		}
		assertUpgradeUntouched(t, path, keyPath, wrongKey, string(before))
	})
}

func assertUpgradeUntouched(t *testing.T, path, keyPath string, wantKey []byte, wantConfig string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != wantConfig {
		t.Fatalf("failed migration changed the config: err=%v", err)
	}
	if got, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(got, wantKey) {
		t.Fatalf("failed migration changed the master key: err=%v", err)
	}
	backs, err := filepath.Glob(path + ".*.enc.bak")
	if err != nil || len(backs) != 0 {
		t.Fatalf("failed migration created a backup: %v err=%v", backs, err)
	}
	if _, err := os.Stat(keyPath + ".next"); !os.IsNotExist(err) {
		t.Fatalf("failed migration staged a key: %v", err)
	}
	if _, err := os.Stat(keyPath + ".previous"); !os.IsNotExist(err) {
		t.Fatalf("failed migration retained a previous key: %v", err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
