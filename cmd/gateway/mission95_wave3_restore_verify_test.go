package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// wave3BucketlessStore creates a bbolt file with no buckets: opening
// it succeeds, but audit verification finds no audit trail.
func wave3BucketlessStore(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWave3CheckBundledAuditChain(t *testing.T) {
	t.Run("temp stage failure", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", blocker)
		check := checkBundledAuditChain(map[string][]byte{backupEntrySecurity: []byte("x")})
		if check.Status != statusFail || check.Detail != "cannot stage the bundled security store for verification" {
			t.Fatalf("check=%+v, want FAIL 'cannot stage the bundled security store for verification'", check)
		}
	})
	t.Run("garbage store does not open", func(t *testing.T) {
		check := checkBundledAuditChain(map[string][]byte{backupEntrySecurity: []byte("garbage, not a store")})
		if check.Status != statusFail || !strings.Contains(check.Detail, "does not open") {
			t.Fatalf("check=%+v, want FAIL 'does not open'", check)
		}
	})
	t.Run("bucketless store fails verification", func(t *testing.T) {
		check := checkBundledAuditChain(map[string][]byte{backupEntrySecurity: wave3BucketlessStore(t)})
		if check.Status != statusFail || !strings.Contains(check.Detail, "FAILED verification (2 problem(s))") {
			t.Fatalf("check=%+v, want FAIL 'FAILED verification (2 problem(s))'", check)
		}
	})
}

func TestWave3CheckControlPlaneStateInvalidJSON(t *testing.T) {
	check := checkControlPlaneState(map[string][]byte{backupEntryControl: []byte("not json")})
	if check.Status != statusFail || check.Detail != "bundled control-plane state is not valid JSON" {
		t.Fatalf("check=%+v, want FAIL 'bundled control-plane state is not valid JSON'", check)
	}
}

func TestWave3CheckExternalOwnership(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config.Config)
		want string
	}{
		{"control plane redis", func(c *config.Config) {
			c.ControlPlane.Enabled = true
			c.ControlPlane.Backend = "redis"
		}, "control-plane redis"},
		{"security store postgres", func(c *config.Config) {
			c.Admin.SecurityStoreBackend = "postgres"
		}, "security store postgres"},
		{"security store redis", func(c *config.Config) {
			c.Admin.SecurityStoreBackend = "redis"
		}, "security store redis"},
		{"video enabled", func(c *config.Config) {
			c.Video.Enabled = true
		}, "video assets/jobs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mut(&cfg)
			check := checkExternalOwnership(cfg, statusPass)
			if check.Status != statusBlocked || !strings.Contains(check.Detail, tc.want) {
				t.Fatalf("check=%+v, want BLOCKED containing %q", check, tc.want)
			}
		})
	}
}

func TestWave3PrepareDisposableDir(t *testing.T) {
	t.Run("empty existing dir is fine", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "empty")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := prepareDisposableDir(dir); err != nil {
			t.Fatalf("err=%v, want nil", err)
		}
	})
	t.Run("path under regular file", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := prepareDisposableDir(filepath.Join(blocker, "into"))
		if err == nil || !strings.Contains(err.Error(), "inspect --into directory") {
			t.Fatalf("err=%v, want 'inspect --into directory'", err)
		}
	})
	t.Run("symlink is not a real directory", func(t *testing.T) {
		realDir := filepath.Join(t.TempDir(), "real")
		if err := os.MkdirAll(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		err := prepareDisposableDir(link)
		if err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("err=%v, want 'not a real directory'", err)
		}
	})
	t.Run("regular file is not a real directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := prepareDisposableDir(file)
		if err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("err=%v, want 'not a real directory'", err)
		}
	})
}

func TestWave3MaterializeBundleWriteError(t *testing.T) {
	into := filepath.Join(t.TempDir(), "into")
	if err := os.MkdirAll(into, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(into, "config.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := materializeBundle(into, map[string][]byte{backupEntryConfig: []byte("config-bytes")})
	if err == nil || !strings.Contains(err.Error(), "materialize config.json") {
		t.Fatalf("err=%v, want 'materialize config.json'", err)
	}
}

func TestWave3VerifyConfigDecryption(t *testing.T) {
	t.Run("stage under regular file", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, status, detail := verifyConfigDecryption(blocker, map[string][]byte{backupEntryConfig: []byte("{}")}, filepath.Join(t.TempDir(), "config.json"))
		if status != statusFail || detail != "cannot stage bundled config for verification" {
			t.Fatalf("status=%q detail=%q, want FAIL 'cannot stage bundled config for verification'", status, detail)
		}
	})
	t.Run("staged config path is a directory", func(t *testing.T) {
		stage := filepath.Join(t.TempDir(), "stage")
		if err := os.MkdirAll(filepath.Join(stage, "verify-config", "config.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, status, detail := verifyConfigDecryption(stage, map[string][]byte{backupEntryConfig: []byte("{}")}, filepath.Join(t.TempDir(), "config.json"))
		if status != statusFail || detail != "cannot stage bundled config for verification" {
			t.Fatalf("status=%q detail=%q, want FAIL 'cannot stage bundled config for verification'", status, detail)
		}
	})
	t.Run("bundled key path is a directory", func(t *testing.T) {
		stage := filepath.Join(t.TempDir(), "stage")
		if err := os.MkdirAll(filepath.Join(stage, "verify-config", "config.json.key"), 0o700); err != nil {
			t.Fatal(err)
		}
		key := make([]byte, 32)
		_, status, detail := verifyConfigDecryption(stage, map[string][]byte{
			backupEntryConfig: []byte("{}"), backupEntryKey: key,
		}, filepath.Join(t.TempDir(), "config.json"))
		if status != statusFail || detail != "cannot stage bundled master key for verification" {
			t.Fatalf("status=%q detail=%q, want FAIL 'cannot stage bundled master key for verification'", status, detail)
		}
	})
	t.Run("host key path is a directory", func(t *testing.T) {
		stage := filepath.Join(t.TempDir(), "stage")
		if err := os.MkdirAll(filepath.Join(stage, "verify-config", "config.json.key"), 0o700); err != nil {
			t.Fatal(err)
		}
		hostConfig := filepath.Join(t.TempDir(), "config.json")
		key := make([]byte, 32)
		for i := range key {
			key[i] = 0x33
		}
		if err := os.WriteFile(hostConfig+".key", key, 0o600); err != nil {
			t.Fatal(err)
		}
		_, status, detail := verifyConfigDecryption(stage, map[string][]byte{
			backupEntryConfig: []byte("{}"),
		}, hostConfig)
		if status != statusFail || detail != "cannot stage the host master key for verification" {
			t.Fatalf("status=%q detail=%q, want FAIL 'cannot stage the host master key for verification'", status, detail)
		}
	})
	t.Run("encrypted config decrypts with bundled key", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers = []config.ProviderConfig{{
			ID: "demo", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1",
			APIKey: "wave3-plain-key", AuthMode: "bearer", Enabled: true,
		}}
		plain, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		key := make([]byte, 32)
		for i := range key {
			key[i] = 0x44
		}
		encrypted, _, err := secrets.TransformConfig(plain, key, true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encrypted), "nxs1:") {
			t.Fatal("encrypted fixture has no nxs1 envelope; the test would not exercise the encrypted branch")
		}
		stage := filepath.Join(t.TempDir(), "stage")
		_, status, detail := verifyConfigDecryption(stage, map[string][]byte{
			backupEntryConfig: encrypted, backupEntryKey: key,
		}, filepath.Join(t.TempDir(), "config.json"))
		if status != statusPass || detail != "encrypted values decrypted and config validated" {
			t.Fatalf("status=%q detail=%q, want PASS 'encrypted values decrypted and config validated'", status, detail)
		}
	})
}
