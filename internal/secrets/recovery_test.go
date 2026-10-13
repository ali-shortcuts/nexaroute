package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Direct branch coverage for recoverMissingRotationKey, which Key() consults
// only when the master key file itself is missing.

func TestRecoverMissingRotationKeyNoStagedKey(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	keyPath := configPath + ".key"
	if err := os.WriteFile(configPath, []byte(`{"x":"nxs1:abc"}`), 0600); err != nil {
		t.Fatal(err)
	}
	key, ok, err := recoverMissingRotationKey(configPath, keyPath)
	if err != nil || ok || key != nil {
		t.Fatalf("expected (nil,false,nil) without staged key, got (%v,%v,%v)", key, ok, err)
	}
}

func TestRecoverMissingRotationKeyPlaintextConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	keyPath := configPath + ".key"
	newKey := []byte("abcdefghijklmnopqrstuvwxyzABCDEF")
	// Plaintext config (no encrypted values): staged key must NOT be promoted.
	if err := os.WriteFile(configPath, []byte(`{"admin":{"api_key":"plain"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".next", newKey, 0600); err != nil {
		t.Fatal(err)
	}
	key, ok, err := recoverMissingRotationKey(configPath, keyPath)
	if err != nil || ok || key != nil {
		t.Fatalf("expected no recovery for plaintext config, got ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath + ".next"); err != nil {
		t.Fatalf("staged key should remain untouched: %v", err)
	}
}

func TestRecoverMissingRotationKeyPromotes(t *testing.T) {
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
	key, ok, err := recoverMissingRotationKey(configPath, keyPath)
	if err != nil || !ok || string(key) != string(newKey) {
		t.Fatalf("expected staged key recovery, got ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("staged key should have been promoted to key path: %v", err)
	}
	if _, err := os.Stat(keyPath + ".next"); !os.IsNotExist(err) {
		t.Fatalf("staged key should be gone after promotion: %v", err)
	}
}

func TestRecoverMissingRotationKeyWrongStagedKey(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	keyPath := configPath + ".key"
	realKey := []byte("01234567890123456789012345678901")
	wrongKey := []byte("abcdefghijklmnopqrstuvwxyzABCDEF")
	raw, _, err := TransformConfig([]byte(`{"providers":[{"id":"p","api_key":"canary"}]}`), realKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".next", wrongKey, 0600); err != nil {
		t.Fatal(err)
	}
	key, ok, err := recoverMissingRotationKey(configPath, keyPath)
	if err != nil || ok || key != nil {
		t.Fatalf("expected no recovery with wrong staged key, got ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath + ".next"); err != nil {
		t.Fatalf("wrong staged key must not be consumed: %v", err)
	}
}

func TestRecoverMissingRotationKeyMissingConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "nonexistent.json")
	keyPath := configPath + ".key"
	newKey := []byte("abcdefghijklmnopqrstuvwxyzABCDEF")
	if err := os.WriteFile(keyPath+".next", newKey, 0600); err != nil {
		t.Fatal(err)
	}
	key, ok, err := recoverMissingRotationKey(configPath, keyPath)
	if err != nil || ok || key != nil {
		t.Fatalf("expected (nil,false,nil) for missing config, got ok=%v err=%v", ok, err)
	}
}
