package secrets

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Wave-3 Key coverage: configured-provider errors, the env provider
// (missing var, bad base64, wrong length, valid key), the
// missing-rotation recovery path (promoted key and staged-key error),
// and the config-directory creation failure. The Stat-after-read,
// OpenFile-O_EXCL and persist-failure branches need a mid-call
// filesystem race or fault injection and are deliberately not faked.

func wave3IsolateKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_ENV", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_REF", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_COMMAND", "")
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
}

func TestWave3KeyUnsupportedProvider(t *testing.T) {
	wave3IsolateKeyEnv(t)
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "bogus-provider")
	if _, _, err := Key(filepath.Join(t.TempDir(), "config.yaml")); err == nil || !strings.Contains(err.Error(), "unsupported NEXAROUTE_KEY_PROVIDER") {
		t.Fatalf("unsupported provider error = %v", err)
	}
}

func TestWave3KeyEnvProviderLoadFailures(t *testing.T) {
	cases := []struct {
		name  string
		value *string
		want  string
	}{
		{"missing var", nil, ""},
		{"bad base64", strptr("!!!not-base64!!!"), ""},
		// Every provider validates key length inside Load, so a
		// wrong-length env value surfaces the provider's own error;
		// Key's length guard after Load is defense-in-depth that no
		// configured provider can reach.
		{"wrong length", strptr(base64.StdEncoding.EncodeToString(make([]byte, 16))), "must be base64-encoded 32-byte key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wave3IsolateKeyEnv(t)
			t.Setenv("NEXAROUTE_KEY_PROVIDER", "env")
			t.Setenv("NEXAROUTE_KEY_PROVIDER_ENV", "WAVE3_TEST_KEY_VAR")
			if tc.value != nil {
				t.Setenv("WAVE3_TEST_KEY_VAR", *tc.value)
			} else {
				t.Setenv("WAVE3_TEST_KEY_VAR", "")
			}
			_, _, err := Key(filepath.Join(t.TempDir(), "config.yaml"))
			if err == nil {
				t.Fatal("env provider failure must error")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func strptr(s string) *string { return &s }

func TestWave3KeyEnvProviderValidKey(t *testing.T) {
	wave3IsolateKeyEnv(t)
	key := []byte("01234567890123456789012345678901")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "env")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_ENV", "WAVE3_TEST_KEY_VAR")
	t.Setenv("WAVE3_TEST_KEY_VAR", base64.StdEncoding.EncodeToString(key))
	got, id, err := Key(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("valid env key must load: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("returned key = %q, want the env key", got)
	}
	sum := sha256.Sum256(key)
	if id != hex.EncodeToString(sum[:8]) {
		t.Fatalf("key id = %q, want hex(sha256(key)[:8])", id)
	}
}

func TestWave3KeyMissingRotationPromotesStagedKey(t *testing.T) {
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	keyPath := configPath + ".key"
	newKey := []byte("abcdefghijklmnopqrstuvwxyzABCDEF")
	raw, _, err := TransformConfig([]byte(`{"providers":[{"id":"p","api_key":"canary"}]}`), newKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), Prefix) {
		t.Fatal("fixture must contain encrypted values")
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".next", newKey, 0600); err != nil {
		t.Fatal(err)
	}
	got, id, err := Key(configPath)
	if err != nil {
		t.Fatalf("missing-rotation recovery must promote the staged key: %v", err)
	}
	if string(got) != string(newKey) {
		t.Fatalf("promoted key = %q, want staged key", got)
	}
	sum := sha256.Sum256(newKey)
	if id != hex.EncodeToString(sum[:8]) {
		t.Fatalf("key id = %q", id)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("staged key should have been promoted to the key path: %v", err)
	}
}

func TestWave3KeyMissingRotationStagedKeyError(t *testing.T) {
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	keyPath := configPath + ".key"
	raw, _, err := TransformConfig([]byte(`{"providers":[{"id":"p","api_key":"canary"}]}`), []byte("abcdefghijklmnopqrstuvwxyzABCDEF"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// A staged key with wrong permissions is not "missing": recovery
	// must fail loudly instead of silently generating a fresh key.
	if err := os.WriteFile(keyPath+".next", make([]byte, 32), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Key(configPath); err == nil || !strings.Contains(err.Error(), "cannot inspect staged master key") {
		t.Fatalf("bad staged key error = %v", err)
	}
}

func TestWave3KeyConfigDirectoryCreationFails(t *testing.T) {
	wave3IsolateKeyEnv(t)
	if st, err := os.Stat("/proc/self"); err != nil || !st.IsDir() {
		t.Skip("read-only /proc layout required for this test is unavailable")
	}
	configPath := "/proc/self/wave3-missing-dir/config.yaml"
	if _, _, err := Key(configPath); err == nil || !strings.Contains(err.Error(), "cannot create config directory for master key") {
		t.Fatalf("read-only config directory error = %v", err)
	}
}
