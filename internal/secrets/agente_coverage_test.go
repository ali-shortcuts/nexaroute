package secrets

// Agent E coverage for internal/secrets (branch agents/coverage-providers-security).
//
// This file closes the remaining reachable uncovered statements of the
// package with meaningful behavioral tests. Every key, token, and wrapped
// value below is a synthetic test fixture (deterministic byte patterns or
// freshly generated random bytes that are never logged); no real secret
// material appears in any assertion or log line. Fakes are contract
// tests only: the Vault Transit provider here is an in-process fake
// envelope provider or a local httptest server, never a real Vault.
//
// Techniques used for the hard branches, all deterministic or bounded
// by retry loops that fail loudly instead of passing silently:
//   - RLIMIT_FSIZE fault injection for atomic-write failures (the
//     verbatim backup fits under the limit, the larger rewritten config
//     does not; SIGXFSZ is ignored so the write fails with EFBIG).
//   - /proc as a key-file location: procfs rejects file creation even
//     for root, so key generation fails at OpenFile with O_EXCL.
//   - atime/inode change watchers that delete a file inside the exact
//     window between two syscalls (rotation-key rename, post-write
//     verify read), plus spin-removal loops for the sub-microsecond
//     Stat-after-read races, each with an observable error message.
//   - a counting envelope provider whose unwrap budget is exhausted
//     exactly at the post-write verification step.
//
// Blocks that are NOT covered here are unreachable by construction and
// are documented in the Agent E report (defensive AES/GCM construction
// on fixed 32-byte keys, json.Marshal of all-string/int structs,
// re-checks that parseEnvelope already enforced, second decodes of the
// same bytes, visitors that never return an error, the dead return
// after the Vault retry loop, fchmod/fsync/close/flock failures with no
// deterministic trigger on local filesystems, and the key-provider
// length guard that every configured provider already enforces).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// agentESetFileSizeLimit caps the size of files this process may create
// (RLIMIT_FSIZE) for the duration of the test and restores the previous
// limit in a cleanup. Writes past the limit raise SIGXFSZ, which is
// ignored so the write fails with EFBIG (or a short write) instead of
// killing the test process. Ignoring SIGXFSZ is harmless for the rest
// of the test binary.
func agentESetFileSizeLimit(t *testing.T, limit uint64) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatalf("getrlimit RLIMIT_FSIZE: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Errorf("restoring RLIMIT_FSIZE: %v", err)
		}
	})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: old.Max}); err != nil {
		t.Fatalf("setrlimit RLIMIT_FSIZE=%d: %v", limit, err)
	}
}

// agentEAtimeNano returns the access time of path in nanoseconds, or -1.
func agentEAtimeNano(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return -1
	}
	return st.Atim.Sec*1_000_000_000 + st.Atim.Nsec
}

// agentEIno returns the inode number of path, or 0.
func agentEIno(path string) uint64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return st.Ino
}

// agentESpinRemove removes path in a tight loop until done is closed.
// It is the fault injector for sub-microsecond races between two
// syscalls in the code under test.
func agentESpinRemove(path string, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
			_ = os.Remove(path)
		}
	}
}

// agentEAtimeDelete watches path for an atime change from baseline (a
// read by the code under test) and then deletes removePath once. The
// read happens strictly after the rotation key was loaded, so the
// delete can only land inside the window before the key rename.
func agentEAtimeDelete(path string, baseline int64, removePath string, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		if at := agentEAtimeNano(path); at != -1 && at != baseline {
			_ = os.Remove(removePath)
			return
		}
	}
}

// agentEInoDelete watches path for an inode change from baseline (the
// atomic rename of the rewritten config) and then deletes path once,
// so the post-write verification read fails.
func agentEInoDelete(path string, baselineIno uint64, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		if ino := agentEIno(path); ino != 0 && ino != baselineIno {
			_ = os.Remove(path)
			return
		}
	}
}

// agentEBackupDeleter deletes any pre-migration/pre-rewrap backup file
// that appears in dir, until done is closed. It is the fault injector
// for the "rollback also failed" branches.
func agentEBackupDeleter(dir string, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".pre-transit.bak") || strings.HasSuffix(e.Name(), ".pre-rewrap.bak") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// agentELongRewrapProvider is a deterministic EnvelopeKeyProvider whose
// wrapped values embed the DEK in the ciphertext tail, so UnwrapDEK can
// recover it without server-side state. RewrapDEK returns a deliberately
// long v2 wrapped value (padded tail), so a rewrapped config is much
// larger than the original — the property the rewrap write-failure test
// needs. All values are synthetic test fixtures.
type agentELongRewrapProvider struct {
	keyRef string
}

func (p *agentELongRewrapProvider) KeyRefHash() string { return p.keyRef }

func (p *agentELongRewrapProvider) WrapDEK(_ context.Context, dek []byte) (string, int, error) {
	if len(dek) != 32 {
		return "", 0, errors.New("agentE: bad dek")
	}
	return "vault:v1:" + base64.StdEncoding.EncodeToString(dek), 1, nil
}

func (p *agentELongRewrapProvider) UnwrapDEK(_ context.Context, wrapped string) ([]byte, int, error) {
	v, err := ParseTransitCiphertextVersion(wrapped)
	if err != nil {
		return nil, 0, err
	}
	parts := strings.SplitN(wrapped, ":", 3)
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(raw) < 32 {
		return nil, 0, errors.New("agentE: bad wrapped dek")
	}
	return append([]byte(nil), raw[:32]...), v, nil
}

func (p *agentELongRewrapProvider) RewrapDEK(ctx context.Context, wrapped string) (string, int, error) {
	dek, _, err := p.UnwrapDEK(ctx, wrapped)
	if err != nil {
		return "", 0, err
	}
	padded := append(append([]byte(nil), dek...), make([]byte, 1000)...)
	return "vault:v2:" + base64.StdEncoding.EncodeToString(padded), 2, nil
}

func (p *agentELongRewrapProvider) Close() error { return nil }

// agentEManyProviderConfig builds a config document with n providers,
// each api_key sealed under key (nxs1). The large document makes the
// decrypt-verify window in key rotation wide enough for the atime
// watcher to delete the staged key before the rename.
func agentEManyProviderConfig(t *testing.T, key []byte, n int) []byte {
	t.Helper()
	provs := make([]any, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("prov-%03d", i)
		sealed, err := seal(key, AAD(id, "api_key"), []byte("synthetic-value"))
		if err != nil {
			t.Fatal(err)
		}
		provs = append(provs, map[string]any{"id": id, "api_key": sealed})
	}
	raw, err := json.Marshal(map[string]any{"providers": provs})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// agentERotationDir returns a scratch directory on /tmp for the
// rotation-race tests. The atime signal they rely on needs a
// filesystem that updates atime on read (relatime); the workspace
// filesystem is mounted noatime, so t.TempDir() (under $TMPDIR) cannot
// be used for these two tests. The fixtures are small (~100 KiB).
func agentERotationDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agentE-rotation-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
func agentEAtimeUpdates(t *testing.T, path string) bool {
	t.Helper()
	before := agentEAtimeNano(path)
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	return agentEAtimeNano(path) != before && before != -1
}

// TestAgentEDecodeStrictObjectClosingToken covers the closing-token
// error of decodeStrictObject (envelope.go:478-479). In {"a":1] the
// value scan ends at ']' so dec.More() is false, and the next Token is
// ']' rather than '}'; in {"a":1 the object is truncated. Both must be
// rejected as a malformed object.
func TestAgentEDecodeStrictObjectClosingToken(t *testing.T) {
	for _, inner := range []string{`{"a":1]`, `{"a":1`} {
		if _, err := parseEnvelope(Continue95SecretsRawEnvelope(t, inner)); err == nil || !strings.Contains(err.Error(), "malformed object") {
			t.Fatalf("inner %q: err=%v", inner, err)
		}
	}
}

// TestAgentEMigrateNestedEnvelopeVerificationMismatch covers the
// in-memory verification mismatch of MigrateConfigBytes
// (envelope.go:876-877). The legacy plaintext of the field is itself a
// valid nxs2 envelope: migration leaves it untouched (already an
// envelope), but decrypting the migrated config yields the inner
// plaintext instead of the envelope string, so the verification must
// fail closed rather than produce a config that does not round-trip.
func TestAgentEMigrateNestedEnvelopeVerificationMismatch(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("agentE-nested")
	inner := sealField(t, env, "prov-synth", "api_key", "inner-secret")
	legacy := Continue95SecretsSyntheticKey(0x2a)
	sealed, err := seal(legacy, AAD("prov-synth", "api_key"), []byte(inner))
	if err != nil {
		t.Fatal(err)
	}
	raw := Continue95SecretsProviderConfig(sealed)
	_, _, err = MigrateConfigBytes(ctx, raw, legacy, env)
	if err == nil || !strings.Contains(err.Error(), "migration in-memory verification failed: migrated config does not decrypt to the original plaintext") {
		t.Fatalf("nested-envelope migration err=%v", err)
	}
}

// TestAgentECommandProviderStartFailure covers the cmd.Start error of
// runProviderCommand (key_provider.go:55-56): a configured helper that
// does not exist must fail closed on both get and put.
func TestAgentECommandProviderStartFailure(t *testing.T) {
	ctx := context.Background()
	p := CommandProvider{Command: "agentE-no-such-helper-command"}
	if _, err := p.Load(ctx, "test-ref"); err == nil || !strings.Contains(err.Error(), "key provider command failed") {
		t.Fatalf("load with missing command err=%v", err)
	}
	if err := p.Store(ctx, "test-ref", Continue95SecretsSyntheticKey(0x31)); err == nil || !strings.Contains(err.Error(), "key provider command failed") {
		t.Fatalf("store with missing command err=%v", err)
	}
}

// TestAgentECappedBufferLimits covers both cappedBuffer branches
// (key_provider.go:100-101, 103-104): a write that exactly fills the
// remaining space is truncated to fit, and a write to an already-full
// buffer is dropped while still reporting the full length.
func TestAgentECappedBufferLimits(t *testing.T) {
	b := &cappedBuffer{limit: 4}
	if n, err := b.Write([]byte("ab")); err != nil || n != 2 {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	if n, err := b.Write([]byte("cdefgh")); err != nil || n != 2 {
		t.Fatalf("partial write n=%d err=%v", n, err)
	}
	if got := b.buf.String(); got != "abcd" {
		t.Fatalf("buffer=%q want %q", got, "abcd")
	}
	if n, err := b.Write([]byte("efgh")); err != nil || n != 4 {
		t.Fatalf("full write n=%d err=%v", n, err)
	}
	if got := b.buf.String(); got != "abcd" {
		t.Fatalf("buffer after full write=%q want %q", got, "abcd")
	}
}

// TestAgentEMigrateConfigWriteFailure covers the atomic-replace failure
// of MigrateToEnvelopeFile (migration.go:75-76) and the Write failure
// of writeFileAtomic (migration.go:341-343). The verbatim backup
// (len(raw) bytes) fits under the file-size limit; the migrated config
// is larger and its write fails, so the migration aborts with the
// backup intact and the config untouched.
func TestAgentEMigrateConfigWriteFailure(t *testing.T) {
	ctx := context.Background()
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	legacy := Continue95SecretsSyntheticKey(0x40)
	env := newFakeEnvelope("agentE-mig-write")
	legacyDoc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-migrate-key"), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	target, migrated, err := MigrateConfigBytes(ctx, legacyDoc, legacy, env)
	if err != nil || migrated != 1 {
		t.Fatalf("precompute migration: migrated=%d err=%v", migrated, err)
	}
	if len(target) <= len(legacyDoc) {
		t.Fatalf("migrated config not larger: target=%d raw=%d", len(target), len(legacyDoc))
	}
	path := filepath.Join(dir, "config.json")
	raw := append(legacyDoc, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	agentESetFileSizeLimit(t, uint64(len(raw)))
	result, err := MigrateToEnvelopeFile(ctx, path, legacy, env)
	if err == nil || !strings.Contains(err.Error(), "cannot atomically replace config during migration") {
		t.Fatalf("migration config-write failure err=%v", err)
	}
	if result.BackupPath == "" {
		t.Fatal("failed migration must still report the backup path")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("config modified by failed migration: err=%v", err)
	}
	if backup, err := os.ReadFile(result.BackupPath); err != nil || !bytes.Equal(backup, raw) {
		t.Fatalf("backup must hold the verbatim pre-migration bytes: err=%v", err)
	}
}

// TestAgentEMigrateRollbackAlsoFails covers the rollback-failure branch
// of MigrateToEnvelopeFile (migration.go:83-84). The counting provider's
// unwrap budget (2) covers the canary and the in-memory verification
// unwraps, so the post-write verification unwrap fails; the rollback
// then fails because the watcher deleted the backup file first. The
// config keeps the migrated bytes and the error names the (now
// missing) backup for manual restore.
func TestAgentEMigrateRollbackAlsoFails(t *testing.T) {
	ctx := context.Background()
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	legacy := Continue95SecretsSyntheticKey(0x41)
	legacyDoc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-migrate-key"), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	raw := append(legacyDoc, '\n')
	for attempt := 0; attempt < 20; attempt++ {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		env := &Continue95SecretsCountingProvider{base: newFakeEnvelope("agentE-mig-rollback"), wrapLimit: -1, unwrapLimit: 2}
		done := make(chan struct{})
		go agentEBackupDeleter(dir, done)
		result, err := MigrateToEnvelopeFile(ctx, path, legacy, env)
		close(done)
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "rollback also failed") {
			continue
		}
		if result.BackupPath == "" {
			t.Fatal("failed migration must still report the backup path")
		}
		if !strings.Contains(err.Error(), "restore "+result.BackupPath+" manually") {
			t.Fatalf("rollback-failure error must name the backup: err=%v", err)
		}
		if _, statErr := os.Stat(result.BackupPath); !os.IsNotExist(statErr) {
			t.Fatalf("backup must have been deleted by the watcher: stat err=%v", statErr)
		}
		if after, rerr := os.ReadFile(path); rerr != nil || !strings.Contains(string(after), "nxs2:") {
			t.Fatalf("config must still hold the migrated bytes after failed rollback: err=%v", rerr)
		}
		return
	}
	t.Fatal("migration rollback failure was not observed in 20 attempts")
}

// TestAgentERewrapConfigWriteFailure covers the atomic-replace failure
// of RewrapConfigFile (migration.go:169-170) and the Write failure of
// writeFileAtomic (migration.go:341-343). The rewrap provider returns a
// deliberately long v2 wrapped key, so the rewrapped config is much
// larger than the original and its write fails under the file-size
// limit while the verbatim backup succeeds.
func TestAgentERewrapConfigWriteFailure(t *testing.T) {
	ctx := context.Background()
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	env := &agentELongRewrapProvider{keyRef: TransitKeyRefHash("", "transit", "agentE-long-rewrap")}
	sealed, _, err := TransformConfigWith(ctx, Cipher{env: env}, Continue95SecretsProviderConfig("synthetic-rewrap-key"), true)
	if err != nil {
		t.Fatal(err)
	}
	out, n, _, err := RewrapConfigBytes(ctx, env, sealed)
	if err != nil || n != 1 {
		t.Fatalf("precompute rewrap: n=%d err=%v", n, err)
	}
	if len(out) <= len(sealed) {
		t.Fatalf("rewrapped config not larger: out=%d raw=%d", len(out), len(sealed))
	}
	path := filepath.Join(dir, "config.json")
	raw := append(sealed, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	agentESetFileSizeLimit(t, uint64(len(raw)))
	result, err := RewrapConfigFile(ctx, path, env)
	if err == nil || !strings.Contains(err.Error(), "cannot atomically replace config during rewrap") {
		t.Fatalf("rewrap config-write failure err=%v", err)
	}
	if result.BackupPath == "" {
		t.Fatal("failed rewrap must still report the backup path")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("config modified by failed rewrap: err=%v", err)
	}
	if backup, err := os.ReadFile(result.BackupPath); err != nil || !bytes.Equal(backup, raw) {
		t.Fatalf("backup must hold the verbatim pre-rewrap bytes: err=%v", err)
	}
}

// TestAgentEVerifyReadFailureRollsBack covers the ReadFile error of
// verifyFileDecryptsWithTransit (migration.go:287-288). The watcher
// deletes the config the moment its inode changes (the atomic rename
// of the rewritten config), so the post-write verification read fails
// and the migration rolls back to the verbatim backup.
func TestAgentEVerifyReadFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	legacy := Continue95SecretsSyntheticKey(0x42)
	env := newFakeEnvelope("agentE-verify-read")
	legacyDoc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-migrate-key"), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	raw := append(legacyDoc, '\n')
	path := filepath.Join(dir, "config.json")
	for attempt := 0; attempt < 50; attempt++ {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		baseline := agentEIno(path)
		done := make(chan struct{})
		go agentEInoDelete(path, baseline, done)
		_, err := MigrateToEnvelopeFile(ctx, path, legacy, env)
		close(done)
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "migration post-write verification failed (open ") {
			continue
		}
		if !strings.Contains(err.Error(), "rolled back to the pre-migration backup") {
			t.Fatalf("verify read failure did not roll back: err=%v", err)
		}
		if after, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(after, raw) {
			t.Fatalf("rollback must restore the original bytes: err=%v", rerr)
		}
		return
	}
	t.Fatal("post-write verify read failure was not observed in 50 attempts")
}

// TestAgentEKeyStatAfterReadRace covers the Stat error after a
// successful key-file read (secrets.go:56-57). A deleter removes the
// key file in a tight loop; when the removal lands between the read
// and the stat, Key fails closed with "cannot inspect master key
// file" instead of trusting the bytes it just read.
func TestAgentEKeyStatAfterReadRace(t *testing.T) {
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	keyPath := cfgPath + ".key"
	key := Continue95SecretsSyntheticKey(0x43)
	for attempt := 0; attempt < 300; attempt++ {
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go agentESpinRemove(keyPath, done)
		_, _, err := Key(cfgPath)
		close(done)
		if err != nil && strings.Contains(err.Error(), "cannot inspect master key file") {
			return
		}
	}
	t.Fatal("key-file Stat-after-read failure was not observed in 300 attempts")
}

// TestAgentEKeyCreateInProcFails covers the OpenFile failure of key
// generation (secrets.go:92-93). procfs does not support file creation
// even for root, so generating a key for a config under /proc fails at
// OpenFile with O_EXCL and reports that the key file cannot be
// created.
func TestAgentEKeyCreateInProcFails(t *testing.T) {
	wave3IsolateKeyEnv(t)
	cfgPath := fmt.Sprintf("/proc/agentE-test-%d-%d.json", os.Getpid(), time.Now().UnixNano())
	_, _, err := Key(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "cannot create master key file; set NEXAROUTE_MASTER_KEY or NEXAROUTE_MASTER_KEY_FILE") {
		t.Fatalf("proc key creation err=%v", err)
	}
}

// TestAgentEKeyPersistFailure covers the write failure of key
// generation (secrets.go:102-104). The generated key is 32 bytes; a
// 16-byte file-size limit makes the write fail, so generation reports
// "cannot persist master key" and removes the partial file.
func TestAgentEKeyPersistFailure(t *testing.T) {
	wave3IsolateKeyEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	agentESetFileSizeLimit(t, 16)
	_, _, err := Key(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "cannot persist master key") {
		t.Fatalf("key persist failure err=%v", err)
	}
	if _, statErr := os.Stat(cfgPath + ".key"); !os.IsNotExist(statErr) {
		t.Fatalf("partial key file must be removed: stat err=%v", statErr)
	}
}

// TestAgentEKeyPendingRotationRenameRace covers the rename failure of
// recoverPendingRotation (secrets.go:136-137). The config is encrypted
// with the staged key, so recovery must promote it; the watcher
// deletes the staged key the moment the config read moves its atime
// (which happens strictly after the staged key was loaded), so the
// rename fails and recovery reports it cannot be completed.
func TestAgentEKeyPendingRotationRenameRace(t *testing.T) {
	wave3IsolateKeyEnv(t)
	dir := agentERotationDir(t)
	cfgPath := filepath.Join(dir, "config.json")
	keyPath := cfgPath + ".key"
	nextPath := keyPath + ".next"
	current := Continue95SecretsSyntheticKey(0x44)
	next := Continue95SecretsSyntheticKey(0x45)
	raw := agentEManyProviderConfig(t, next, 200)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if !agentEAtimeUpdates(t, cfgPath) {
		t.Skip("filesystem does not update atime on read; cannot signal the rotation race")
	}
	for attempt := 0; attempt < 30; attempt++ {
		if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, current, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(nextPath, next, 0o600); err != nil {
			t.Fatal(err)
		}
		baseline := agentEAtimeNano(cfgPath)
		done := make(chan struct{})
		go agentEAtimeDelete(cfgPath, baseline, nextPath, done)
		_, _, err := Key(cfgPath)
		close(done)
		if err != nil && strings.Contains(err.Error(), "encrypted config uses the staged key but key recovery could not be completed") {
			return
		}
	}
	t.Fatal("pending-rotation rename failure was not observed in 30 attempts")
}

// TestAgentEKeyMissingRotationRenameRace covers the rename failure of
// recoverMissingRotationKey (secrets.go:159-160). The key file is
// missing and the config is encrypted with the staged key, so recovery
// must promote it; the watcher deletes the staged key the moment the
// config read moves its atime, so the rename fails.
func TestAgentEKeyMissingRotationRenameRace(t *testing.T) {
	wave3IsolateKeyEnv(t)
	dir := agentERotationDir(t)
	cfgPath := filepath.Join(dir, "config.json")
	keyPath := cfgPath + ".key"
	nextPath := keyPath + ".next"
	next := Continue95SecretsSyntheticKey(0x46)
	raw := agentEManyProviderConfig(t, next, 200)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if !agentEAtimeUpdates(t, cfgPath) {
		t.Skip("filesystem does not update atime on read; cannot signal the rotation race")
	}
	for attempt := 0; attempt < 30; attempt++ {
		if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(keyPath)
		if err := os.WriteFile(nextPath, next, 0o600); err != nil {
			t.Fatal(err)
		}
		baseline := agentEAtimeNano(cfgPath)
		done := make(chan struct{})
		go agentEAtimeDelete(cfgPath, baseline, nextPath, done)
		_, _, err := Key(cfgPath)
		close(done)
		if err != nil && strings.Contains(err.Error(), "encrypted config uses the staged key but key recovery could not be completed") {
			return
		}
	}
	t.Fatal("missing-rotation rename failure was not observed in 30 attempts")
}

// TestAgentEReadRotationKeyStatRace covers the Stat error after a
// successful staged-key read (secrets.go:172-173). A deleter removes
// the staged key in a tight loop; the Stat error is distinguishable
// from the read error by its "stat " operation prefix, so the test
// only passes when the Stat branch itself was taken.
func TestAgentEReadRotationKeyStatRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.next")
	key := Continue95SecretsSyntheticKey(0x47)
	for attempt := 0; attempt < 500; attempt++ {
		if err := os.WriteFile(path, key, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go agentESpinRemove(path, done)
		_, err := readRotationKey(path)
		close(done)
		if err != nil && strings.Contains(err.Error(), "stat ") {
			return
		}
	}
	t.Fatal("readRotationKey Stat-after-read failure was not observed in 500 attempts")
}

// TestAgentEVaultRetryCancelledByServer covers the context-cancellation
// check at the top of the Vault retry loop (vault_transit.go:306-307).
// The fake server flushes a 503 response and only then cancels the
// caller's context: if the client completes the round-trip, the retry
// loop must notice the already-cancelled context before sleeping for
// the backoff and report transit_unavailable without a second request.
// If the cancellation tears down the in-flight request instead, that
// attempt fails with a transport error and is retried; the test only
// passes when an attempt observes the cancellation error with exactly
// one request. A fresh server and provider per attempt keeps the
// attempts independent.
func TestAgentEVaultRetryCancelledByServer(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			cancel()
		}))
		p, err := NewVaultTransitProvider(srv.URL, "synthetic-token", "", "transit", "synth-key", 5*time.Second)
		if err != nil {
			srv.Close()
			cancel()
			t.Fatal(err)
		}
		_, _, err = p.WrapDEK(ctx, Continue95SecretsSyntheticKey(0x48))
		srv.Close()
		cancel()
		if err == nil {
			t.Fatal("cancelled retry must fail")
		}
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitUnavailable || !strings.Contains(terr.Message, "cancelled") {
			continue // transport error: the cancel tore down the request; retry
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("cancelled retry made %d requests, want 1", got)
		}
		return
	}
	t.Fatal("cancelled retry was not observed in 50 attempts")
}
