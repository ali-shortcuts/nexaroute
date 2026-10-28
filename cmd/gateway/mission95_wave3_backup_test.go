package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// wave3Restore runs runRestore with captured output.
func wave3Restore(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runRestore(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// wave3SetLimits lowers the bundle size limits for one test, restoring
// them afterwards (same pattern as the existing limit tests).
func wave3SetLimits(t *testing.T, entry, total int64) {
	t.Helper()
	oldEntry, oldTotal := maxBackupEntryBytes, maxBackupTotalBytes
	maxBackupEntryBytes, maxBackupTotalBytes = entry, total
	t.Cleanup(func() { maxBackupEntryBytes, maxBackupTotalBytes = oldEntry, oldTotal })
}

func TestWave3CreateBackupInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte("{invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "b.tar")
	if code, stderr := runBackupCode(t, cfgPath, out); code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	} else if !strings.Contains(stderr, "load config") {
		t.Fatalf("stderr=%q, want 'load config'", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when the config does not load")
	}
}

func TestWave3CreateBackupRelativeControlPath(t *testing.T) {
	dir, configPath := opsFixture(t)
	rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
		cfg.ControlPlane.StatePath = "relative-state.json"
	})
	if err := os.WriteFile(filepath.Join(dir, "relative-state.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, out); code != 0 {
		t.Fatalf("exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	names := tarEntryNames(t, out)
	found := false
	for _, n := range names {
		if n == backupEntryControl {
			found = true
		}
	}
	if !found {
		t.Fatalf("bundle entries=%v, want controlplane.json for a relative state path", names)
	}
}

func TestWave3CreateBackupTempDirFailure(t *testing.T) {
	dir, configPath := opsFixture(t)
	_ = dir
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar")
	t.Setenv("TMPDIR", blocker)
	if code, stderr := runBackupCode(t, configPath, out); code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when no temp dir can be created")
	}
}

func TestWave3CreateBackupAggregateOverflowAtConfig(t *testing.T) {
	_, configPath := opsFixture(t)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	wave3SetLimits(t, 1<<30, int64(len(raw))-1)
	out := filepath.Join(t.TempDir(), "b.tar")
	code, stderr := runBackupCode(t, configPath, out)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "aggregate") {
		t.Fatalf("stderr=%q, want 'aggregate'", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written on aggregate overflow")
	}
}

func TestWave3CreateBackupAggregateOverflowAtControl(t *testing.T) {
	_, configPath := opsFixture(t)
	rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
		cfg.ControlPlane.StatePath = ""
	})
	statePath := configPath + ".controlplane.json"
	if err := os.WriteFile(statePath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first.tar")
	if code, stderr := runBackupCode(t, configPath, first); code != 0 {
		t.Fatalf("first backup exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	manifest := wave3BundleManifest(t, first)
	budget := wave3ManifestEntrySize(t, manifest, backupEntryConfig) + wave3ManifestEntrySize(t, manifest, backupEntrySecurity)
	wave3SetLimits(t, 1<<30, budget)
	second := filepath.Join(t.TempDir(), "second.tar")
	code, stderr := runBackupCode(t, configPath, second)
	if code != 1 {
		t.Fatalf("second backup exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "aggregate") {
		t.Fatalf("stderr=%q, want 'aggregate'", stderr)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written on aggregate overflow")
	}
}

func TestWave3CreateBackupAggregateOverflowAtKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(config.Default(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x11
	}
	if err := os.WriteFile(cfgPath+".key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	wave3SetLimits(t, 1<<30, int64(len(raw)))
	out := filepath.Join(dir, "b.tar")
	code, stderr := runBackupCode(t, cfgPath, out, "--include-master-key")
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "aggregate") {
		t.Fatalf("stderr=%q, want 'aggregate'", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written on aggregate overflow")
	}
}

func TestWave3CreateBackupOutputUnwritable(t *testing.T) {
	_, configPath := opsFixture(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stderr := runBackupCode(t, configPath, filepath.Join(blocker, "b.tar"))
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "backup failed") {
		t.Fatalf("stderr=%q, want 'backup failed'", stderr)
	}
}

func TestWave3CreateBackupUnreadableSecurityStore(t *testing.T) {
	dir, configPath := opsFixture(t)
	if err := os.WriteFile(fixtureSecurityPath(dir), []byte("this is not a bbolt database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar")
	code, stderr := runBackupCode(t, configPath, out)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "open security store") {
		t.Fatalf("stderr=%q, want 'open security store'", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when the security store does not open")
	}
}

func TestWave3CreateBackupControlStateIsDirectory(t *testing.T) {
	dir, configPath := opsFixture(t)
	stateDir := filepath.Join(dir, "state-dir")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
		cfg.ControlPlane.StatePath = stateDir
	})
	out := filepath.Join(t.TempDir(), "b.tar")
	code, stderr := runBackupCode(t, configPath, out)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "read control-plane state") {
		t.Fatalf("stderr=%q, want 'read control-plane state'", stderr)
	}
}

func TestWave3RestoreBundleSecurityBackendRefused(t *testing.T) {
	dir, _ := opsFixture(t)
	storeBytes, err := os.ReadFile(fixtureSecurityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
		cfg.Admin.SecurityStoreBackend = "postgres"
		// Validation requires a URL for postgres backends; no
		// connection is attempted because the restore refuses the
		// backend before opening any store.
		cfg.Admin.SecurityStoreURL = "postgres://127.0.0.1:5432/nexaroute"
	})
	bundle := writeTestBundle(t, map[string][]byte{
		backupEntryConfig: cfgBytes, backupEntrySecurity: storeBytes,
	})
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	code, stdout, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force"})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%s stderr=%s)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "cannot restore") || !strings.Contains(stderr, "postgres") {
		t.Fatalf("stderr=%q, want 'cannot restore' + 'postgres'", stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(targetConfig), "security", "nexaroute-security.db")); !os.IsNotExist(err) {
		t.Fatal("no store may be installed by a refused restore")
	}
}

func TestWave3RestoreBundleTargetCanonicalError(t *testing.T) {
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {})
	bundle := writeTestBundle(t, map[string][]byte{backupEntryConfig: cfgBytes})
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", filepath.Join(blocker, "config.json"), "--force"})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "resolve restore target") {
		t.Fatalf("stderr=%q, want 'resolve restore target'", stderr)
	}
}

func TestWave3RestoreBundleSameFileTargets(t *testing.T) {
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	sentinel := []byte("SENTINEL CONFIG")
	if err := os.WriteFile(targetConfig, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	controlTarget := filepath.Join(targetDir, "state.json")
	if err := os.Link(targetConfig, controlTarget); err != nil {
		t.Fatal(err)
	}
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
		cfg.ControlPlane.StatePath = controlTarget
	})
	bundle := writeTestBundle(t, map[string][]byte{
		backupEntryConfig: cfgBytes, backupEntryControl: []byte(`{"version":1}`),
	})
	code, _, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force"})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "collide") || !strings.Contains(stderr, "same file") {
		t.Fatalf("stderr=%q, want 'collide' + 'same file'", stderr)
	}
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, sentinel) {
		t.Fatal("a refused restore must not modify its targets")
	}
}

func TestWave3RestoreBundleTargetIsDirectory(t *testing.T) {
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {})
	bundle := writeTestBundle(t, map[string][]byte{backupEntryConfig: cfgBytes})
	targetDir := filepath.Join(t.TempDir(), "config.json")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetDir, "--force"})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "inspect restore target") {
		t.Fatalf("stderr=%q, want 'inspect restore target'", stderr)
	}
}

func TestWave3RestoreBundleKeyOverwriteWarning(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	for i := range key1 {
		key1[i] = 0x11
		key2[i] = 0x22
	}
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {})
	bundle := writeTestBundle(t, map[string][]byte{
		backupEntryConfig: cfgBytes, backupEntryKey: key1,
	})
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	if err := os.WriteFile(targetConfig, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetConfig+".key", key2, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force"})
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (stdout=%s stderr=%s)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "overwriting the existing master key") {
		t.Fatalf("stdout=%q, want 'overwriting the existing master key'", stdout)
	}
	if got, err := os.ReadFile(targetConfig + ".key"); err != nil || !bytes.Equal(got, key1) {
		t.Fatalf("target key=%x err=%v, want the bundled key", got, err)
	}
}

func TestWave3RestoreBundleGarbageSecurityStore(t *testing.T) {
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {})
	bundle := writeTestBundle(t, map[string][]byte{
		backupEntryConfig: cfgBytes, backupEntrySecurity: []byte("garbage, not a store"),
	})
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	code, _, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force"})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "open bundled security store") {
		t.Fatalf("stderr=%q, want 'open bundled security store'", stderr)
	}
}

func TestWave3RestoreBundleSidecarWriteFailure(t *testing.T) {
	dir, configPath := opsFixture(t)
	bundle := tamperedBundle(t, dir, configPath)
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	sidecar := filepath.Join(targetDir, "security", "nexaroute-security.db.restore-override.json")
	if err := os.MkdirAll(sidecar, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force", "--allow-broken-audit-chain"})
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (stdout=%s stderr=%s)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "could not write the audit-chain override record") {
		t.Fatalf("stdout=%q, want 'could not write the audit-chain override record'", stdout)
	}
}

func TestWave3RestoreBundleKeyFingerprintMismatch(t *testing.T) {
	_, configPath := opsFixture(t)
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	for i := range key1 {
		key1[i] = 0x11
		key2[i] = 0x22
	}
	if err := os.WriteFile(configPath+".key", key1, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	payloads := wave3BundlePayloads(t, bundle)
	if _, ok := payloads[backupEntryKey]; ok {
		t.Fatal("bundle must not include the key for the fingerprint test")
	}
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	if err := os.WriteFile(targetConfig, payloads[backupEntryConfig], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetConfig+".key", key2, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force"})
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (stdout=%s stderr=%s)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "does not match the backup's fingerprint") {
		t.Fatalf("stdout=%q, want 'does not match the backup's fingerprint'", stdout)
	}
}

func TestWave3RestoreBundleEmptyTargetPath(t *testing.T) {
	// The CLI never passes an empty config path; this direct call
	// covers the resolveSecurityStorePath failure inside restoreBundle.
	dir, _ := opsFixture(t)
	storeBytes, err := os.ReadFile(fixtureSecurityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {})
	bundle := writeTestBundle(t, map[string][]byte{
		backupEntryConfig: cfgBytes, backupEntrySecurity: storeBytes,
	})
	if err := restoreBundle(bundle, "", false, false, io.Discard); err == nil || !strings.Contains(err.Error(), "security store path is required") {
		t.Fatalf("err=%v, want 'security store path is required'", err)
	}
}

func TestWave3RestoreIdempotentSecondRun(t *testing.T) {
	_, configPath := opsFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	code, _, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force"})
	if code != 0 {
		t.Fatalf("first restore exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	code, stdout, stderr := wave3Restore(t, []string{"--bundle", bundle, "--config", targetConfig, "--force"})
	if code != 0 {
		t.Fatalf("second restore exit=%d, want 0 (stdout=%s stderr=%s)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unchanged") {
		t.Fatalf("stdout=%q, want 'unchanged' on the idempotent second run", stdout)
	}
}

func TestWave3InstallWithRollbackEmptyIsNoop(t *testing.T) {
	if err := installWithRollback(nil, nil, io.Discard); err != nil {
		t.Fatalf("empty installs err=%v, want nil", err)
	}
}

func TestWave3InstallWithRollbackTempFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", blocker)
	// No new temp dir may be created after TMPDIR is pointed at a
	// regular file; the journal creation fails before any target is
	// touched, so a literal target path suffices here.
	err := installWithRollback(
		[]installItem{{backupEntryConfig, "/wave3-no-such-dir/config.json"}},
		map[string][]byte{backupEntryConfig: []byte("x")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "create restore journal") {
		t.Fatalf("err=%v, want 'create restore journal'", err)
	}
}

func TestWave3InstallWithRollbackDirectoryTarget(t *testing.T) {
	targetDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(targetDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	dirTarget := filepath.Join(targetDir, "sub")
	err := installWithRollback(
		[]installItem{{backupEntryConfig, dirTarget}},
		map[string][]byte{backupEntryConfig: []byte("new")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("err=%v, want 'unsupported type'", err)
	}
	if info, err := os.Stat(dirTarget); err != nil || !info.IsDir() {
		t.Fatalf("directory target must survive untouched (info=%v err=%v)", info, err)
	}
}

func TestWave3InstallWithRollbackLstatError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installWithRollback(
		[]installItem{{backupEntryConfig, filepath.Join(blocker, "config.json")}},
		map[string][]byte{backupEntryConfig: []byte("new")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "journal restore target") {
		t.Fatalf("err=%v, want 'journal restore target'", err)
	}
}

func TestWave3InstallWithRollbackPersistPreparedFails(t *testing.T) {
	orig := persistJournalManifest
	defer func() { persistJournalManifest = orig }()
	persistJournalManifest = func(string, *restoreJournalManifest) error {
		return errors.New("wave3 injected persist failure")
	}
	target := filepath.Join(t.TempDir(), "config.json")
	pre := []byte("pre-restore bytes")
	if err := os.WriteFile(target, pre, 0o600); err != nil {
		t.Fatal(err)
	}
	err := installWithRollback(
		[]installItem{{backupEntryConfig, target}},
		map[string][]byte{backupEntryConfig: []byte("new bytes")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "write restore journal") {
		t.Fatalf("err=%v, want 'write restore journal'", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, pre) {
		t.Fatal("a failed prepared persist must leave the target untouched")
	}
}

func TestWave3InstallWithRollbackPersistInstallingFails(t *testing.T) {
	orig := persistJournalManifest
	defer func() { persistJournalManifest = orig }()
	calls := 0
	persistJournalManifest = func(dir string, m *restoreJournalManifest) error {
		calls++
		if calls >= 2 {
			return errors.New("wave3 injected persist failure")
		}
		return orig(dir, m)
	}
	target := filepath.Join(t.TempDir(), "config.json")
	pre := []byte("pre-restore bytes")
	if err := os.WriteFile(target, pre, 0o600); err != nil {
		t.Fatal(err)
	}
	err := installWithRollback(
		[]installItem{{backupEntryConfig, target}},
		map[string][]byte{backupEntryConfig: []byte("new bytes")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "write restore journal") {
		t.Fatalf("err=%v, want 'write restore journal'", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, pre) {
		t.Fatal("a failed installing persist must leave the target untouched")
	}
}

func TestWave3InstallWithRollbackPersistProgressFails(t *testing.T) {
	orig := persistJournalManifest
	defer func() { persistJournalManifest = orig }()
	calls := 0
	persistJournalManifest = func(dir string, m *restoreJournalManifest) error {
		calls++
		if calls >= 3 {
			return errors.New("wave3 injected persist failure")
		}
		return orig(dir, m)
	}
	target := filepath.Join(t.TempDir(), "config.json")
	pre := []byte("pre-restore bytes")
	if err := os.WriteFile(target, pre, 0o600); err != nil {
		t.Fatal(err)
	}
	err := installWithRollback(
		[]installItem{{backupEntryConfig, target}},
		map[string][]byte{backupEntryConfig: []byte("new bytes")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "could not update the restore journal") {
		t.Fatalf("err=%v, want 'could not update the restore journal'", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, []byte("new bytes")) {
		t.Fatal("the install landed before the progress persist failed, so the target must hold the new bytes")
	}
	// The journal must be preserved and recoverable: locate it by
	// its entry target, then roll back through the real CLI path.
	var journalDir string
	for _, dir := range findInterruptedRestoreJournals() {
		m, err := readJournalManifest(dir)
		if err != nil {
			continue
		}
		if len(m.Entries) == 1 && m.Entries[0].Target == target {
			journalDir = dir
		}
	}
	if journalDir == "" {
		t.Fatal("no preserved journal found for the failed transaction")
	}
	m, err := readJournalManifest(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "installing" {
		t.Fatalf("journal state=%q, want installing", m.State)
	}
	var stdout, stderr bytes.Buffer
	if code := runRestore([]string{"--recover", journalDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("recover exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, pre) {
		t.Fatal("recovery must restore the pre-restore bytes")
	}
	if _, err := os.Stat(journalDir); !os.IsNotExist(err) {
		t.Fatal("a recovered journal must be removed")
	}
}

func TestWave3InstallWithRollbackDeletedCWD(t *testing.T) {
	savedWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldPWD, pwdSet := os.LookupEnv("PWD")
	defer func() {
		_ = os.Chdir(savedWd)
		if pwdSet {
			_ = os.Setenv("PWD", oldPWD)
		} else {
			_ = os.Unsetenv("PWD")
		}
	}()
	_ = os.Unsetenv("PWD")
	cwd, err := os.MkdirTemp("", "wave3-deleted-cwd-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cwd); err != nil {
		t.Skipf("cannot remove the current working directory on this platform: %v", err)
	}
	err = installWithRollback(
		[]installItem{{backupEntryConfig, "relative-config.json"}},
		map[string][]byte{backupEntryConfig: []byte("new")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "journal restore target") {
		t.Fatalf("err=%v, want 'journal restore target'", err)
	}
}

func TestWave3RollbackInstalledBranches(t *testing.T) {
	journalDir := filepath.Join(t.TempDir(), "journal")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("link target restored", func(t *testing.T) {
		linkPath := filepath.Join(t.TempDir(), "link")
		if err := os.WriteFile(linkPath, []byte("installed file"), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest := &restoreJournalManifest{
			Format: restoreJournalFormat, ID: "abcdef0123456789", State: "installing",
			Entries: []restoreJournalEntry{{Name: backupEntryConfig, Target: linkPath, Existed: true, LinkTo: "somewhere"}},
		}
		failures := rollbackInstalled(journalDir, manifest, []installItem{{backupEntryConfig, linkPath}}, 1)
		if len(failures) != 0 {
			t.Fatalf("failures=%v, want none", failures)
		}
		got, err := os.Readlink(linkPath)
		if err != nil || got != "somewhere" {
			t.Fatalf("link target=%q err=%v, want symlink to somewhere", got, err)
		}
	})
	t.Run("absent entry stays absent", func(t *testing.T) {
		absentPath := filepath.Join(t.TempDir(), "absent.json")
		manifest := &restoreJournalManifest{
			Format: restoreJournalFormat, ID: "abcdef0123456789", State: "installing",
			Entries: []restoreJournalEntry{{Name: backupEntryConfig, Target: absentPath, Existed: false}},
		}
		failures := rollbackInstalled(journalDir, manifest, []installItem{{backupEntryConfig, absentPath}}, 1)
		if len(failures) != 0 {
			t.Fatalf("failures=%v, want none", failures)
		}
		if _, err := os.Stat(absentPath); !os.IsNotExist(err) {
			t.Fatal("an absent entry must stay absent after rollback")
		}
	})
}

func TestWave3ValidateJournal(t *testing.T) {
	// The dir-permissions (0755) case is already covered by existing
	// tests and is not repeated here.
	target := filepath.Join(t.TempDir(), "config.json")
	pre := []byte("journaled pre-state")
	mkValid := func() (*restoreJournalManifest, map[string][]byte) {
		return validInstallingJournal(target, pre)
	}
	mutate := func(t *testing.T, fn func(*restoreJournalManifest)) (string, *restoreJournalManifest) {
		t.Helper()
		m, saved := mkValid()
		dir := fabricateJournal(t, m, saved)
		fn(m)
		if err := writeJournalManifest(dir, m); err != nil {
			t.Fatal(err)
		}
		return dir, m
	}
	assertErr := func(t *testing.T, dir string, m *restoreJournalManifest, want string) {
		t.Helper()
		err := validateJournal(dir, m)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want substring %q", err, want)
		}
	}

	t.Run("missing journal dir", func(t *testing.T) {
		m, _ := mkValid()
		assertErr(t, filepath.Join(t.TempDir(), "missing"), m, "stat journal directory")
	})
	t.Run("missing manifest", func(t *testing.T) {
		m, _ := mkValid()
		dir := filepath.Join(t.TempDir(), "journal")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		assertErr(t, dir, m, "stat journal manifest")
	})
	t.Run("manifest grants group access", func(t *testing.T) {
		m, saved := mkValid()
		dir := fabricateJournal(t, m, saved)
		if err := os.Chmod(filepath.Join(dir, "manifest.json"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertErr(t, dir, m, "grant group/other access")
	})
	t.Run("bad format", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Format = "bogus/1" })
		assertErr(t, dir, m, "unsupported journal format")
	})
	t.Run("non hex id", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.ID = "zzzzzzzz" })
		assertErr(t, dir, m, "is not hexadecimal")
	})
	t.Run("unknown entry name", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Entries[0].Name = "bogus" })
		assertErr(t, dir, m, "unknown name")
	})
	t.Run("duplicate entry names", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) {
			m.Entries = append(m.Entries, m.Entries[0])
		})
		assertErr(t, dir, m, "duplicate entries")
	})
	t.Run("relative target", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Entries[0].Target = "relative/path.json" })
		assertErr(t, dir, m, "not a valid absolute path")
	})
	t.Run("target under regular file", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Entries[0].Target = filepath.Join(blocker, "config.json") })
		assertErr(t, dir, m, "cannot be canonicalized")
	})
	t.Run("duplicate canonical targets via symlink", func(t *testing.T) {
		realPath := filepath.Join(t.TempDir(), "real.json")
		if err := os.WriteFile(realPath, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		linkPath := filepath.Join(t.TempDir(), "link.json")
		if err := os.Symlink(realPath, linkPath); err != nil {
			t.Fatal(err)
		}
		m, saved := mkValid()
		m.Entries = []restoreJournalEntry{
			{Name: backupEntryConfig, Target: linkPath},
			{Name: backupEntrySecurity, Target: realPath},
		}
		dir := fabricateJournal(t, m, saved)
		assertErr(t, dir, m, "target the same destination")
	})
	t.Run("absent entry carrying metadata", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) {
			m.Entries[0].Existed = false
			m.Entries[0].Mode = 0
			m.Entries[0].LinkTo = ""
			m.Entries[0].SavedSHA256 = ""
			// Saved alone still counts as saved-state metadata.
		})
		assertErr(t, dir, m, "carries saved-state metadata")
	})
	t.Run("symlink entry carrying saved copy", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) {
			m.Entries[0].LinkTo = "somewhere"
		})
		assertErr(t, dir, m, "carries a saved copy")
	})
	t.Run("symlink entry with NUL link target", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) {
			m.Entries[0].LinkTo = "a\x00b"
			m.Entries[0].Saved = ""
			m.Entries[0].SavedSHA256 = ""
		})
		assertErr(t, dir, m, "invalid recorded link target")
	})
	t.Run("bad saved copy name", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Entries[0].Saved = "evil" })
		assertErr(t, dir, m, "invalid saved-copy name")
	})
	t.Run("mode not a permission mask", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Entries[0].Mode = 0o10000 })
		assertErr(t, dir, m, "not a permission mask")
	})
	t.Run("checksum too short", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Entries[0].SavedSHA256 = "abc" })
		assertErr(t, dir, m, "missing its saved-copy checksum")
	})
	t.Run("checksum not hex", func(t *testing.T) {
		dir, m := mutate(t, func(m *restoreJournalManifest) { m.Entries[0].SavedSHA256 = strings.Repeat("z", 64) })
		assertErr(t, dir, m, "malformed saved-copy checksum")
	})
	t.Run("saved copy grants group access", func(t *testing.T) {
		m, saved := mkValid()
		dir := fabricateJournal(t, m, saved)
		if err := os.Chmod(filepath.Join(dir, "entry-0"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertErr(t, dir, m, "grant group/other access")
	})
	t.Run("saved copy exceeds entry limit", func(t *testing.T) {
		wave3SetLimits(t, 10, 1<<30)
		m, saved := mkValid()
		dir := fabricateJournal(t, m, saved)
		assertErr(t, dir, m, "exceeds the entry size limit")
	})
	t.Run("journal dir owned by another user", func(t *testing.T) {
		m, saved := mkValid()
		dir := fabricateJournal(t, m, saved)
		if err := os.Chown(dir, 1234, 1234); err != nil {
			t.Skipf("platform forbids chown to another uid: %v", err)
		}
		assertErr(t, dir, m, "not the current user")
	})
	t.Run("manifest owned by another user", func(t *testing.T) {
		m, saved := mkValid()
		dir := fabricateJournal(t, m, saved)
		if err := os.Chown(filepath.Join(dir, "manifest.json"), 1234, 1234); err != nil {
			t.Skipf("platform forbids chown to another uid: %v", err)
		}
		assertErr(t, dir, m, "not the current user")
	})
}
