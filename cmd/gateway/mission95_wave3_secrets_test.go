package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// wave3SecretsConfig writes a plaintext config plus a 32-byte .key
// file, clearing external key-provider env vars so rotation uses the
// auto-managed file key.
func wave3SecretsConfig(t *testing.T, mutate func(*config.Config)) (string, []byte) {
	t.Helper()
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x55
	}
	if err := os.WriteFile(cfgPath+".key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, key
}

func TestWave3RunSecrets(t *testing.T) {
	t.Run("bad flag", func(t *testing.T) {
		if err := runSecrets([]string{"verify", "--bogus"}); err == nil {
			t.Fatal("bad flag must error")
		}
	})
	t.Run("verify with invalid config", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, []byte("{invalid json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runSecrets([]string{"verify", "--config", cfgPath}); err == nil {
			t.Fatal("verify of an invalid config must error")
		}
	})
	// NOTE: the verify loop's client_auth.keys reporting branch
	// (secrets.go verify, "OK client_auth.keys[i]") is deliberately
	// NOT tested: LoadBase migrates plaintext client keys into
	// KeyHashes and nils Keys before verify ever sees the config
	// (config.MigrateClientKeysToHashes), so the branch is
	// unreachable through the CLI path — defense in depth for
	// hand-built Config values only. The existing
	// TestSecretsVerifyReportsAllKeyMaterial records the same
	// conclusion. The verify paths that ARE reachable (provider,
	// credential, decision-provider, admin keys) are covered there.
	t.Run("decrypt garbage envelope fails", func(t *testing.T) {
		cfgPath, before := func() (string, []byte) {
			p, _ := wave3SecretsConfig(t, func(cfg *config.Config) {
				cfg.Providers = []config.ProviderConfig{{
					ID: "demo", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1",
					APIKey: "nxs1:garbage!!!", AuthMode: "bearer", Enabled: true,
				}}
			})
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			return p, raw
		}()
		if err := runSecrets([]string{"decrypt", "--to-stdout", "--allow-plaintext", "--config", cfgPath}); err == nil {
			t.Fatal("decrypt of a garbage envelope must error")
		}
		after, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("a failed decrypt must not modify the config")
		}
	})
	t.Run("rotate missing config fails", func(t *testing.T) {
		t.Setenv("NEXAROUTE_MASTER_KEY", "")
		t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
		t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
		if err := runSecrets([]string{"rotate", "--config", filepath.Join(t.TempDir(), "missing.json")}); err == nil {
			t.Fatal("rotate of a missing config must error")
		}
	})
	t.Run("rotate with directory key fails", func(t *testing.T) {
		cfgPath, _ := wave3SecretsConfig(t, nil)
		if err := os.Remove(cfgPath + ".key"); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cfgPath+".key", 0o700); err != nil {
			t.Fatal(err)
		}
		err := runSecrets([]string{"rotate", "--config", cfgPath})
		if err == nil || !strings.Contains(err.Error(), "master key") {
			t.Fatalf("err=%v, want a master-key error", err)
		}
	})
	t.Run("rotate garbage envelope fails without changes", func(t *testing.T) {
		cfgPath, _ := wave3SecretsConfig(t, func(cfg *config.Config) {
			cfg.Providers = []config.ProviderConfig{{
				ID: "demo", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1",
				APIKey: "nxs1:garbage!!!", AuthMode: "bearer", Enabled: true,
			}}
		})
		before, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		keyBefore, err := os.ReadFile(cfgPath + ".key")
		if err != nil {
			t.Fatal(err)
		}
		if err := runSecrets([]string{"rotate", "--config", cfgPath}); err == nil {
			t.Fatal("rotate of a garbage envelope must error")
		}
		after, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("a failed rotate must not modify the config")
		}
		keyAfter, err := os.ReadFile(cfgPath + ".key")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(keyBefore, keyAfter) {
			t.Fatal("a failed rotate must not modify the key")
		}
	})
	t.Run("rotate previous copy failure retains config and key", func(t *testing.T) {
		cfgPath, _ := wave3SecretsConfig(t, nil)
		if err := os.MkdirAll(cfgPath+".key.previous", 0o700); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		keyBefore, err := os.ReadFile(cfgPath + ".key")
		if err != nil {
			t.Fatal(err)
		}
		err = runSecrets([]string{"rotate", "--config", cfgPath})
		if err == nil || !strings.Contains(err.Error(), "cannot retain recovery copy") {
			t.Fatalf("err=%v, want 'cannot retain recovery copy'", err)
		}
		after, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("a failed rotate must not modify the config")
		}
		keyAfter, err := os.ReadFile(cfgPath + ".key")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(keyBefore, keyAfter) {
			t.Fatal("a failed rotate must not modify the key")
		}
	})
	// NOTE: the writeRotateFile(.next) failure branch (secrets.go
	// rotate, "cannot stage replacement master key") is deliberately
	// NOT tested: secrets.Key inspects a pre-existing .next file
	// before any rotation step, so a .next directory fails at Key()
	// ("cannot inspect staged master key") instead. Reaching the
	// staged-write failure needs .next to appear as a directory
	// between Key() and the write — a TOCTOU, not a deterministic
	// test. The .previous failure above IS deterministic because
	// Key() never inspects .previous.
}

func TestWave3WriteRotateFile(t *testing.T) {
	t.Run("create temp under regular file fails", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeRotateFile(filepath.Join(blocker, "x.key"), []byte("x")); err == nil {
			t.Fatal("create temp under a regular file must fail")
		}
	})
	t.Run("rename onto directory fails", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "target-dir")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeRotateFile(dir, []byte("x")); err == nil {
			t.Fatal("rename onto an existing directory must fail")
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("directory target must survive untouched (info=%v err=%v)", info, err)
		}
	})
}
