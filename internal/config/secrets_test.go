package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaintextSecretMigrationEncryptedBackupAndIdempotency(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8080","admin":{"api_key":"admin-canary"},"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","enabled":false,"api_key":"provider-canary","credentials":[{"api_key":"pool-canary"}]}],"client_auth":{"keys":["client-canary"]}}`
	if e := os.WriteFile(p, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	cfg, e := LoadBase(p)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Admin.APIKey != "admin-canary" || cfg.Providers[0].APIKey != "provider-canary" || cfg.Providers[0].Credentials[0].APIKey != "pool-canary" || cfg.ClientAuth.Keys[0] != "client-canary" {
		t.Fatal("migration failed to preserve runtime values")
	}
	disk, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"admin-canary", "provider-canary", "pool-canary", "client-canary"} {
		if strings.Contains(string(disk), s) {
			t.Fatalf("plaintext %s in config", s)
		}
	}
	if !strings.Contains(string(disk), "nxs1:") {
		t.Fatal("missing ciphertext")
	}
	backs, e := filepath.Glob(p + ".*.enc.bak")
	if e != nil || len(backs) != 1 {
		t.Fatalf("encrypted backup count=%d err=%v", len(backs), e)
	}
	bb, _ := os.ReadFile(backs[0])
	if strings.Contains(string(bb), "provider-canary") {
		t.Fatal("plaintext backup")
	}
	before, _ := os.ReadFile(p)
	if _, e = LoadBase(p); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("migration was not idempotent")
	}
	again, _ := filepath.Glob(p + ".*.enc.bak")
	if len(again) != 1 {
		t.Fatalf("duplicate migration backup: %d", len(again))
	}
}
