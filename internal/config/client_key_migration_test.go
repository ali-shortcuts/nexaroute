package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M3-8: a runtime Load of a config holding legacy plaintext client keys
// migrates them to SHA-256 digests and persists the migration, so the
// file no longer contains usable client credentials — while the same
// secrets keep authenticating (against the digests).
func TestLoadPersistsClientKeyHashMigration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8080","client_auth":{"enabled":true,"keys":["alpha-key-123","beta-key-456"]}}`
	if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ClientAuth.Keys) != 0 {
		t.Fatalf("plaintext keys retained in loaded config: %v", cfg.ClientAuth.Keys)
	}
	want := []string{ClientKeyDigest("alpha-key-123"), ClientKeyDigest("beta-key-456")}
	if len(cfg.ClientAuth.KeyHashes) != 2 || cfg.ClientAuth.KeyHashes[0] != want[0] || cfg.ClientAuth.KeyHashes[1] != want[1] {
		t.Fatalf("key hashes = %v, want %v", cfg.ClientAuth.KeyHashes, want)
	}
	if cfg.ClientKeyMigrationPending() != 0 {
		t.Fatal("migration still pending after Load persisted it")
	}
	disk, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"alpha-key-123", "beta-key-456"} {
		if strings.Contains(string(disk), plain) {
			t.Fatalf("plaintext client key %q still in config file", plain)
		}
	}
	if !strings.Contains(string(disk), "key_hashes") {
		t.Fatal("persisted config does not carry key_hashes")
	}
	again, err := LoadBase(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.ClientAuth.KeyHashes) != 2 || again.ClientKeyMigrationPending() != 0 {
		t.Fatalf("reload not stable: hashes=%v pending=%d", again.ClientAuth.KeyHashes, again.ClientKeyMigrationPending())
	}
}

// LoadBase converts in memory and marks the migration pending; it does
// not itself persist the hash migration (the runtime Load does). The
// pre-existing nxs1 secrets migration may still encrypt the file, but
// after any LoadBase the file must not hold the plaintext key and the
// model must carry only the digest.
func TestLoadBaseConvertsInMemoryAndKeepsNoPlaintext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8080","client_auth":{"enabled":true,"keys":["alpha-key-123"]}}`
	if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBase(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ClientAuth.KeyHashes) != 1 || cfg.ClientKeyMigrationPending() != 1 {
		t.Fatalf("in-memory migration wrong: hashes=%v pending=%d", cfg.ClientAuth.KeyHashes, cfg.ClientKeyMigrationPending())
	}
	if len(cfg.ClientAuth.Keys) != 0 {
		t.Fatalf("plaintext keys retained in model: %v", cfg.ClientAuth.Keys)
	}
	disk, _ := os.ReadFile(p)
	if strings.Contains(string(disk), "alpha-key-123") {
		t.Fatal("plaintext client key present in config file after LoadBase")
	}
}

// No save path may persist plaintext client keys, even when a caller
// constructs the Config programmatically with plaintext keys.
func TestSaveAtomicNeverPersistsPlaintextClientKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = []string{"gamma-key-789"}
	if err := SaveAtomic(p, cfg); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), "gamma-key-789") {
		t.Fatal("plaintext client key persisted by SaveAtomic")
	}
	loaded, err := LoadBase(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ClientAuth.Keys) != 0 || len(loaded.ClientAuth.KeyHashes) != 1 ||
		loaded.ClientAuth.KeyHashes[0] != ClientKeyDigest("gamma-key-789") {
		t.Fatalf("saved config lost the key: keys=%v hashes=%v", loaded.ClientAuth.Keys, loaded.ClientAuth.KeyHashes)
	}
}

func TestValidateClientKeyHashesAndTPM(t *testing.T) {
	cfg := Default()
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.KeyHashes = []string{ClientKeyDigest("some-key-123")}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("hashes-only client auth must validate: %v", err)
	}
	cfg.ClientAuth.KeyHashes = []string{"not-a-digest"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("malformed key_hashes entry accepted")
	}
	cfg.ClientAuth.KeyHashes = []string{ClientKeyDigest("some-key-123")}
	cfg.ClientAuth.TPM = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative client_auth.tpm accepted")
	}
	cfg.ClientAuth.TPM = 2000000000
	if err := cfg.Validate(); err == nil {
		t.Fatal("out-of-range client_auth.tpm accepted")
	}
}
