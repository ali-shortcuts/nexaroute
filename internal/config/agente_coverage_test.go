package config

// Agent E coverage tests for internal/config: the remaining uncovered
// blocks in config.go at baseline main 58576e62 (23 statements). Every
// test below drives a real behavioral branch with a specific expected
// error; fake values are obviously fake test material, never real
// credentials, and all files live under t.TempDir().

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// agentEFakeKey is the obviously-fake 32-byte test master key material
// written to <config>.key by these tests. It is not a real credential.
var agentEFakeKey = []byte("01234567890123456789012345678901")

// agentERawMode forces the raw (non-Transit) cipher path regardless of
// ambient environment.
func agentERawMode(t *testing.T) {
	t.Helper()
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "")
}

// agentEWriteKeyFile creates a 0600 key file next to path holding the
// fake test key, so LoadCipher never has to generate one.
func agentEWriteKeyFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path+".key", agentEFakeKey, 0600); err != nil {
		t.Fatal(err)
	}
}

// agentELimitFileSize ignores SIGXFSZ (so an over-limit write fails with
// EFBIG instead of killing the test process) and caps RLIMIT_FSIZE at
// limit bytes for the rest of the test, restoring the old limit on
// return. Tests in this package run sequentially, so the brief window
// is contained to the call under test.
func agentELimitFileSize(t *testing.T, limit uint64) {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	})
}

// TestAgentEValidateNexaflowErrorPropagates covers config.go:2127-2128:
// validateAdminPlane must surface a validateNexaflow failure through
// Config.Validate instead of swallowing it.
func TestAgentEValidateNexaflowErrorPropagates(t *testing.T) {
	c := Default()
	c.Nexaflow.Enabled = true
	c.Nexaflow.StoreBackend = "bogus-backend"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "nexaflow.store_backend must be file or memory") {
		t.Fatalf("Validate() = %v, want store_backend error", err)
	}
}

// TestAgentEValidateNexaflowStorePathTooLong covers config.go:2231-2232:
// an over-long store_path is rejected even though it is non-empty.
func TestAgentEValidateNexaflowStorePathTooLong(t *testing.T) {
	c := Default()
	c.Nexaflow.Enabled = true
	c.Nexaflow.StoreBackend = "file"
	c.Nexaflow.StorePath = strings.Repeat("x", 4097)
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "nexaflow.store_path is invalid or too long") {
		t.Fatalf("Validate() = %v, want store_path length error", err)
	}
}

// TestAgentEValidateNexaflowGatewayBaseURLTooLong covers
// config.go:2237-2238: an over-long gateway_base_url is rejected before
// any loopback URL parsing.
func TestAgentEValidateNexaflowGatewayBaseURLTooLong(t *testing.T) {
	c := Default()
	c.Nexaflow.Enabled = true
	c.Nexaflow.StoreBackend = "file"
	c.Nexaflow.StorePath = "store.db"
	c.Nexaflow.GatewayBaseURL = strings.Repeat("x", 2049)
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "nexaflow.gateway_base_url is invalid or too long") {
		t.Fatalf("Validate() = %v, want gateway_base_url length error", err)
	}
}

// TestAgentEMigrationBackupWriteFailure covers config.go:1301-1302 (and
// the writeAtomic write error at 4108-4109): when the encrypted
// migration backup cannot be written, LoadBase fails closed with the
// backup error and leaves the original file untouched.
func TestAgentEMigrationBackupWriteFailure(t *testing.T) {
	agentERawMode(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	agentEWriteKeyFile(t, path)
	raw := `{"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"fake-plaintext-key","enabled":false}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	// A 64-byte file-size limit is far below the sealed backup, so the
	// backup write fails; nothing else in the load path writes files.
	agentELimitFileSize(t, 64)
	_, err := LoadBase(path)
	if err == nil || !strings.Contains(err.Error(), "cannot persist encrypted migration backup") {
		t.Fatalf("LoadBase() = %v, want migration backup error", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != raw {
		t.Fatalf("failed migration rewrote the config file: %s", after)
	}
}

// TestAgentEMigrationMainWriteFailure covers config.go:1304-1305: the
// backup is written, then the atomic rewrite of the config itself
// fails. The two writes differ in size — the sealed backup is ~4/3 of
// the compact raw document while the re-encrypted document is the
// pretty-printed config with one envelope per secret field — so a
// file-size limit between the two sizes lets the backup succeed and
// the main write fail. Sizes are measured in-test, not assumed.
func TestAgentEMigrationMainWriteFailure(t *testing.T) {
	agentERawMode(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	agentEWriteKeyFile(t, path)
	var b strings.Builder
	b.WriteString(`{"providers":[`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"p%02d","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"x","enabled":false}`, i)
	}
	b.WriteString(`]}`)
	raw := []byte(b.String())
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.LoadCipher(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := secrets.EncryptedBackupWith(cipher, raw)
	if err != nil {
		t.Fatal(err)
	}
	enc, migrated, err := secrets.TransformConfigWith(context.Background(), cipher, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if migrated != 100 {
		t.Fatalf("migrated = %d, want 100 plaintext secret fields", migrated)
	}
	if len(enc)+1 <= len(backup) {
		t.Fatalf("test sizing assumption broken: encrypted doc %d bytes must exceed backup %d bytes", len(enc)+1, len(backup))
	}
	// Limit exactly at the backup size: the backup write reaches the
	// limit without exceeding it, the larger main write exceeds it.
	agentELimitFileSize(t, uint64(len(backup)))
	_, err = LoadBase(path)
	if err == nil || !strings.Contains(err.Error(), "cannot atomically migrate configuration secrets") {
		t.Fatalf("LoadBase() = %v, want atomic migration error", err)
	}
	backups, _ := filepath.Glob(path + ".*.enc.bak")
	if len(backups) != 1 {
		t.Fatalf("expected the encrypted backup to have been written before the failure, got %v", backups)
	}
}

// TestAgentELoadClientKeyMigrationPersistFailure covers
// config.go:1347-1348: Load must fail closed when the hashed client
// keys cannot be persisted. The first LoadBase (unlimited) migrates
// the file to sealed form without writing the hash migration; the
// second load runs under a tiny file-size limit so LoadBase's
// no-write decrypt path succeeds while SaveAtomic's write fails.
func TestAgentELoadClientKeyMigrationPersistFailure(t *testing.T) {
	agentERawMode(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	agentEWriteKeyFile(t, path)
	raw := `{"listen":"127.0.0.1:8080","client_auth":{"enabled":true,"keys":["fake-client-key-123"]}}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := LoadBase(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.ClientKeyMigrationPending() != 1 {
		t.Fatalf("pending = %d, want 1 after LoadBase", first.ClientKeyMigrationPending())
	}
	agentELimitFileSize(t, 64)
	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "cannot persist migrated client key hashes") {
		t.Fatalf("Load() = %v, want client key hash persistence error", err)
	}
}

// TestAgentESaveAtomicMarshalNaN covers config.go:3984-3985: a config
// that passes Validate but cannot be marshaled (a NaN virtual-key
// budget slips past the `< 0 || > 1e9` range check, since both
// comparisons are false for NaN) makes SaveAtomic return the marshal
// error instead of writing anything.
func TestAgentESaveAtomicMarshalNaN(t *testing.T) {
	agentERawMode(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Default()
	c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{
		ID:        "vk1",
		KeyHash:   ClientKeyDigest("fake-virtual-key-material"),
		BudgetUSD: math.NaN(),
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("NaN-budget config must pass Validate (NaN defeats the range check): %v", err)
	}
	err := SaveAtomic(path, c)
	if err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Fatalf("SaveAtomic() = %v, want JSON NaN marshal error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("SaveAtomic wrote a file despite the marshal error: %v", statErr)
	}
}

// agentEFailingTransit returns a Vault Transit endpoint that rejects
// every operation, to drive Transit encrypt failures.
func agentEFailingTransit(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "transit unavailable", http.StatusInternalServerError)
	}))
}

// TestAgentETransitSaveStoredPlaintextDecryptFailure covers
// config.go:4007-4008: in Transit mode, SaveAtomic must fail closed
// when the stored file's secret fields cannot be decrypted — here the
// stored file still holds a plaintext secret, which Transit mode
// refuses to read.
func TestAgentETransitSaveStoredPlaintextDecryptFailure(t *testing.T) {
	fake := &fakeTransit{entries: map[string]fakeTransitEntry{}, currentVersion: 1}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	enableTransit(t, srv)
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	stored := `{"providers":[{"id":"p","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"fake-stored-plaintext","enabled":false}]}`
	if err := os.WriteFile(path, []byte(stored), 0600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Providers = []ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "fake-in-memory-key", Enabled: false}}
	err := SaveAtomic(path, c)
	if err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("SaveAtomic() = %v, want plaintext-in-Transit decrypt error", err)
	}
}

// TestAgentETransitSaveChangedFieldReplacesEnvelope covers
// config.go:4022-4022: the envelope-preservation callback keeps an
// unchanged nxs2 envelope but must return the fresh plaintext for a
// changed field, so the second save re-seals it under Transit.
func TestAgentETransitSaveChangedFieldReplacesEnvelope(t *testing.T) {
	fake := &fakeTransit{entries: map[string]fakeTransitEntry{}, currentVersion: 1}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	enableTransit(t, srv)
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Default()
	c.Providers = []ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "fake-first-canary", Enabled: false}}
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	encryptCalls := fake.encryptCalls
	c.Providers[0].APIKey = "fake-second-canary"
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	if fake.encryptCalls == encryptCalls {
		t.Fatal("changed secret field did not trigger a fresh Transit seal")
	}
	loaded, err := LoadBase(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers) != 1 || loaded.Providers[0].APIKey != "fake-second-canary" {
		t.Fatalf("reloaded provider = %+v, want the changed key", loaded.Providers)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fake-second-canary") || strings.Contains(string(raw), "fake-first-canary") {
		t.Fatalf("stored config leaks a plaintext secret: %s", raw)
	}
}

// TestAgentETransitSaveEncryptFailure covers config.go:4031-4032: when
// the Transit provider rejects the encrypt call, SaveAtomic fails
// closed with the generic encrypt error and writes nothing.
func TestAgentETransitSaveEncryptFailure(t *testing.T) {
	srv := agentEFailingTransit(t)
	defer srv.Close()
	enableTransit(t, srv)
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Default()
	c.Providers = []ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "fake-transit-canary", Enabled: false}}
	err := SaveAtomic(path, c)
	if err == nil || !strings.Contains(err.Error(), "cannot encrypt configuration secrets") {
		t.Fatalf("SaveAtomic() = %v, want encrypt error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("SaveAtomic wrote a file despite the Transit encrypt failure: %v", statErr)
	}
}
