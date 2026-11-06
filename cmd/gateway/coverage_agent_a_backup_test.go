package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// ---------------------------------------------------------------------------
// resolveLiveAdminURL (direct unit tests; the createBackup error return
// for it was removed as dead — LoadBase validates Listen first).
// ---------------------------------------------------------------------------

func TestCoverageAgentAResolveLiveAdminURL(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		listen  string
		want    string
		wantErr string
	}{
		{"flag wins and trims slashes", "https://admin.example:8443///", "127.0.0.1:8080", "https://admin.example:8443", ""},
		{"flag wins with whitespace", "  https://admin.example:8443/ ", "127.0.0.1:8080", "https://admin.example:8443", ""},
		{"empty listen errors", "", "", "", "live backup requires --admin-url"},
		{"scheme listen returned as-is", "", "https://gateway.example:8443/v1/", "https://gateway.example:8443/v1", ""},
		{"slash listen errors", "", "/tmp/gateway.sock", "", "cannot derive an admin URL"},
		{"bare port maps to loopback", "", ":9090", "http://127.0.0.1:9090", ""},
		{"wildcard v4 maps to loopback", "", "0.0.0.0:9090", "http://127.0.0.1:9090", ""},
		{"wildcard v6 maps to loopback", "", "[::]:9090", "http://127.0.0.1:9090", ""},
		{"plain host fallthrough", "", "myhost", "http://myhost", ""},
		{"bare colon with extra colons", "", ":9090:extra", "http://127.0.0.1:9090:extra", ""},
		{"host port passthrough", "", "gateway.internal:8080", "http://gateway.internal:8080", ""},
		{"loopback host port passthrough", "", "127.0.0.1:8080", "http://127.0.0.1:8080", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLiveAdminURL(tc.flag, tc.listen)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveLiveAdminURL(%q, %q) err=%v, want containing %q", tc.flag, tc.listen, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLiveAdminURL(%q, %q) err=%v, want %q", tc.flag, tc.listen, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("resolveLiveAdminURL(%q, %q)=%q, want %q", tc.flag, tc.listen, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// createBackup error returns.
// ---------------------------------------------------------------------------

// TestCoverageAgentACreateBackupSecurityStoreStatError pins the
// non-NotExist Stat branch via the backupSecurityStoreStat seam. A
// filesystem trigger does not work here: refuseOutputCollision
// canonicalizes the same path first and fails on exactly the conditions
// (ENOTDIR, ELOOP, ...) that would make this Stat fail with anything
// other than NotExist, so the branch is only reachable through a
// concurrent replacement (a race) — and it stays fail-closed.
func TestCoverageAgentACreateBackupSecurityStoreStatError(t *testing.T) {
	_, configPath := opsFixture(t)
	old := backupSecurityStoreStat
	backupSecurityStoreStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat boom") }
	t.Cleanup(func() { backupSecurityStoreStat = old })
	err := createBackup(configPath, filepath.Join(t.TempDir(), "b.tar"), false, false, io.Discard, liveBackupOptions{})
	if err == nil || !strings.Contains(err.Error(), "inspect security store") {
		t.Fatalf("createBackup err=%v, want an 'inspect security store' error", err)
	}
}

// TestCoverageAgentACreateBackupSecurityStorePathDeletedCWD pins the
// resolveSecurityStorePath error return in createBackup: with the
// working directory deleted, a relative config path still resolves
// through the deleted cwd's inode (so the config loads), but
// filepath.Abs — which needs getcwd — fails when resolving the default
// relative security-store path.
func TestCoverageAgentACreateBackupSecurityStorePathDeletedCWD(t *testing.T) {
	base := t.TempDir()
	otherDir := filepath.Join(base, "other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A valid config with the default (empty) security-store path, so
	// the store resolves to the relative "security/..." default.
	data, err := json.MarshalIndent(config.Default(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	absConfig := filepath.Join(otherDir, "config.json")
	if err := os.WriteFile(absConfig, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	doomed := filepath.Join(base, "doomed")
	if err := os.MkdirAll(doomed, 0o755); err != nil {
		t.Fatal(err)
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(doomed); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(doomed); err != nil {
		t.Fatal(err)
	}
	// Relative config path: ".." still resolves from the deleted cwd's
	// inode to base, so the config itself loads.
	relConfig := filepath.Join("..", "other", "config.json")
	berr := createBackup(relConfig, filepath.Join(base, "out.tar"), false, false, io.Discard, liveBackupOptions{})
	if err := os.Chdir(origWD); err != nil {
		t.Fatal(err)
	}
	if berr == nil || !strings.Contains(berr.Error(), "no such file or directory") {
		t.Fatalf("createBackup err=%v, want a getcwd failure ('no such file or directory')", berr)
	}
}

// TestCoverageAgentACreateBackupAddWriteError pins the add() WriteFile
// branch: with a tiny file-size limit the config entry cannot be
// staged (reads are unaffected by RLIMIT_FSIZE; only writes fail).
func TestCoverageAgentACreateBackupAddWriteError(t *testing.T) {
	_, configPath := opsFixture(t)
	limitFileSize(t, 100)
	err := createBackup(configPath, filepath.Join(t.TempDir(), "b.tar"), false, false, io.Discard, liveBackupOptions{})
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("createBackup err=%v, want 'file too large'", err)
	}
}

// TestCoverageAgentACreateBackupLiveProbeWriteError pins the live
// snapshot staging WriteFile branch: the limit lets the (small) config
// entry stage but not the (multi-KB) security snapshot.
func TestCoverageAgentACreateBackupLiveProbeWriteError(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	limitFileSize(t, uint64(len(configBytes))+100)
	code, stderr := runBackupCode(t, configPath, filepath.Join(t.TempDir(), "live.tar"),
		"--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key")
	if code != 1 || !strings.Contains(stderr, "stage live security snapshot for verification") {
		t.Fatalf("live backup code=%d stderr=%s, want the probe-staging error", code, stderr)
	}
}

// TestCoverageAgentACreateBackupLiveVerifyError pins the live
// VerifyAuditChain error branch via the backupVerifyContext seam:
// VerifyAuditChain only errors on a cancelled context (or a bbolt View
// failure, which cannot happen on a just-opened store).
func TestCoverageAgentACreateBackupLiveVerifyError(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	old := backupVerifyContext
	backupVerifyContext = func() (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		return c, cancel
	}
	t.Cleanup(func() { backupVerifyContext = old })
	code, stderr := runBackupCode(t, configPath, filepath.Join(t.TempDir(), "live.tar"),
		"--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key")
	if code != 1 || !strings.Contains(stderr, "verify live security snapshot chain") {
		t.Fatalf("live backup code=%d stderr=%s, want the live verify error", code, stderr)
	}
}

// TestCoverageAgentACreateBackupOfflineVerifyError pins the offline
// VerifyAuditChain error branch the same way.
func TestCoverageAgentACreateBackupOfflineVerifyError(t *testing.T) {
	_, configPath := opsFixture(t)
	old := backupVerifyContext
	backupVerifyContext = func() (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		return c, cancel
	}
	t.Cleanup(func() { backupVerifyContext = old })
	code, stderr := runBackupCode(t, configPath, filepath.Join(t.TempDir(), "b.tar"))
	if code != 1 || !strings.Contains(stderr, "verify audit chain for backup") {
		t.Fatalf("offline backup code=%d stderr=%s, want the offline verify error", code, stderr)
	}
}

// TestCoverageAgentACreateBackupLiveAddAggregateError pins the live
// add() aggregate-limit return: the config stages, but the security
// snapshot pushes the staged total past the swapped aggregate limit.
func TestCoverageAgentACreateBackupLiveAddAggregateError(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldLimit := maxBackupTotalBytes
	maxBackupTotalBytes = int64(len(configBytes)) + 100
	t.Cleanup(func() { maxBackupTotalBytes = oldLimit })
	code, stderr := runBackupCode(t, configPath, filepath.Join(t.TempDir(), "live.tar"),
		"--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key")
	if code != 1 || !strings.Contains(stderr, "aggregate bundle limit") {
		t.Fatalf("live backup code=%d stderr=%s, want the aggregate-limit error", code, stderr)
	}
}

// ---------------------------------------------------------------------------
// canonicalPath: absolute symlink target resets the resolved prefix.
// ---------------------------------------------------------------------------

func TestCoverageAgentACanonicalPathAbsoluteSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalPath(filepath.Join(link, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(real, "f.txt") {
		t.Fatalf("canonicalPath=%q, want %q", got, filepath.Join(real, "f.txt"))
	}
}

// ---------------------------------------------------------------------------
// writeBundle seams.
// ---------------------------------------------------------------------------

// covcliBundleStage builds a minimal stage dir holding one file, and
// returns the manifest file list describing it.
func covcliBundleStage(t *testing.T, name string, data []byte) (stage string, files []backupManifestFile) {
	t.Helper()
	stage = t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return stage, []backupManifestFile{{Name: name, SHA256: sha256Hex(data), Size: int64(len(data))}}
}

func TestCoverageAgentAWriteBundleChmodError(t *testing.T) {
	stage, files := covcliBundleStage(t, "config.json", []byte(`{"listen":"127.0.0.1:8080"}`))
	old := bundleTempChmod
	bundleTempChmod = func(f *os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { bundleTempChmod = old })
	out := filepath.Join(t.TempDir(), "b.tar")
	if err := writeBundle(out, []byte(`{"format":"x"}`), files, stage); err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("writeBundle err=%v, want 'chmod boom'", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when chmod fails")
	}
}

func TestCoverageAgentAWriteBundleManifestWriteError(t *testing.T) {
	stage, files := covcliBundleStage(t, "config.json", []byte(`{"listen":"127.0.0.1:8080"}`))
	limitFileSize(t, 10)
	out := filepath.Join(t.TempDir(), "b.tar")
	if err := writeBundle(out, []byte(`{"format":"nexaroute-backup/1"}`), files, stage); err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("writeBundle err=%v, want 'file too large'", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when the manifest write fails")
	}
}

func TestCoverageAgentAWriteBundleSyncError(t *testing.T) {
	stage, files := covcliBundleStage(t, "config.json", []byte(`{"listen":"127.0.0.1:8080"}`))
	old := bundleTempSync
	bundleTempSync = func(f *os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { bundleTempSync = old })
	out := filepath.Join(t.TempDir(), "b.tar")
	if err := writeBundle(out, []byte(`{"format":"x"}`), files, stage); err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("writeBundle err=%v, want 'sync boom'", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when sync fails")
	}
}

func TestCoverageAgentAWriteBundleCloseError(t *testing.T) {
	stage, files := covcliBundleStage(t, "config.json", []byte(`{"listen":"127.0.0.1:8080"}`))
	old := bundleTempClose
	bundleTempClose = func(f *os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { bundleTempClose = old })
	out := filepath.Join(t.TempDir(), "b.tar")
	if err := writeBundle(out, []byte(`{"format":"x"}`), files, stage); err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("writeBundle err=%v, want 'close boom'", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when close fails")
	}
}

// ---------------------------------------------------------------------------
// writeSavedCopyDurable seams.
// ---------------------------------------------------------------------------

func TestCoverageAgentAWriteSavedCopyChmodError(t *testing.T) {
	old := savedCopyChmod
	savedCopyChmod = func(f *os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { savedCopyChmod = old })
	if err := writeSavedCopyDurable(filepath.Join(t.TempDir(), "entry-0"), []byte("saved")); err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("writeSavedCopyDurable err=%v, want 'chmod boom'", err)
	}
}

func TestCoverageAgentAWriteSavedCopySyncError(t *testing.T) {
	old := savedCopySync
	savedCopySync = func(f *os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { savedCopySync = old })
	if err := writeSavedCopyDurable(filepath.Join(t.TempDir(), "entry-0"), []byte("saved")); err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("writeSavedCopyDurable err=%v, want 'sync boom'", err)
	}
}

// ---------------------------------------------------------------------------
// writeJournalManifest.
// ---------------------------------------------------------------------------

func covcliJournalManifest(state string, entries []restoreJournalEntry) *restoreJournalManifest {
	return &restoreJournalManifest{
		Format:    restoreJournalFormat,
		ID:        "abcdef0123456789",
		CreatedAt: time.Now().UTC(),
		State:     state,
		Entries:   entries,
	}
}

func TestCoverageAgentAWriteJournalManifestWriteError(t *testing.T) {
	limitFileSize(t, 10)
	if err := writeJournalManifest(t.TempDir(), covcliJournalManifest("prepared", nil)); err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("writeJournalManifest err=%v, want 'file too large'", err)
	}
}

func TestCoverageAgentAWriteJournalManifestChmodError(t *testing.T) {
	old := journalTempChmod
	journalTempChmod = func(f *os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { journalTempChmod = old })
	if err := writeJournalManifest(t.TempDir(), covcliJournalManifest("prepared", nil)); err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("writeJournalManifest err=%v, want 'chmod boom'", err)
	}
}

func TestCoverageAgentAWriteJournalManifestSyncError(t *testing.T) {
	old := journalTempSync
	journalTempSync = func(f *os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { journalTempSync = old })
	if err := writeJournalManifest(t.TempDir(), covcliJournalManifest("prepared", nil)); err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("writeJournalManifest err=%v, want 'sync boom'", err)
	}
}

func TestCoverageAgentAWriteJournalManifestCloseError(t *testing.T) {
	old := journalTempClose
	journalTempClose = func(f *os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { journalTempClose = old })
	if err := writeJournalManifest(t.TempDir(), covcliJournalManifest("prepared", nil)); err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("writeJournalManifest err=%v, want 'close boom'", err)
	}
}

// ---------------------------------------------------------------------------
// writeFileAtomic.
// ---------------------------------------------------------------------------

func TestCoverageAgentAWriteFileAtomicCreateTempError(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "sys")
	if err := os.Symlink("/sys", link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(link, "f.json"), []byte("{}"), 0o600); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("writeFileAtomic err=%v, want a read-only filesystem error", err)
	}
}

func TestCoverageAgentAWriteFileAtomicChmodError(t *testing.T) {
	old := atomicTempChmod
	atomicTempChmod = func(f *os.File, perm os.FileMode) error { return errors.New("chmod boom") }
	t.Cleanup(func() { atomicTempChmod = old })
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "f.json"), []byte("{}"), 0o600); err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("writeFileAtomic err=%v, want 'chmod boom'", err)
	}
}

func TestCoverageAgentAWriteFileAtomicSyncError(t *testing.T) {
	old := atomicTempSync
	atomicTempSync = func(f *os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { atomicTempSync = old })
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "f.json"), []byte("{}"), 0o600); err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("writeFileAtomic err=%v, want 'sync boom'", err)
	}
}

func TestCoverageAgentAWriteFileAtomicCloseError(t *testing.T) {
	old := atomicTempClose
	atomicTempClose = func(f *os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { atomicTempClose = old })
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "f.json"), []byte("{}"), 0o600); err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("writeFileAtomic err=%v, want 'close boom'", err)
	}
}

// ---------------------------------------------------------------------------
// validateJournal / recoverRestoreJournal.
// ---------------------------------------------------------------------------

// covcliWriteJournal writes a journal directory (0700) with the given
// manifest (0600) and saved copies (0600), and returns its path.
func covcliWriteJournal(t *testing.T, m *restoreJournalManifest, saved map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	// validateJournal requires exactly 0700; TempDir's mode is not
	// guaranteed to be that tight, so pin it explicitly.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range saved {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCoverageAgentAValidateJournalSavedCopyMissing(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config.json")
	m := covcliJournalManifest("prepared", []restoreJournalEntry{{
		Name: "config.json", Target: target,
		Existed: true, Mode: 0o600, Saved: "entry-0", SavedSHA256: sha256Hex([]byte("original-bytes")),
	}})
	dir := covcliWriteJournal(t, m, nil) // the saved copy is absent
	if err := validateJournal(dir, m); err == nil || !strings.Contains(err.Error(), "saved copy") {
		t.Fatalf("validateJournal err=%v, want a saved-copy error", err)
	}
}

func TestCoverageAgentARecoverCompletedJournalRemoveError(t *testing.T) {
	content := []byte("original-bytes")
	target := filepath.Join(t.TempDir(), "config.json")
	m := covcliJournalManifest("completed", []restoreJournalEntry{{
		Name: "config.json", Target: target,
		Existed: true, Mode: 0o600, Saved: "entry-0", SavedSHA256: sha256Hex(content),
	}})
	dir := covcliWriteJournal(t, m, map[string][]byte{"entry-0": content})
	old := removeRestoreJournalDir
	removeRestoreJournalDir = func(string) error { return errors.New("remove boom") }
	t.Cleanup(func() { removeRestoreJournalDir = old })
	var stdout bytes.Buffer
	if err := recoverRestoreJournal(dir, &stdout); err == nil || !strings.Contains(err.Error(), "clean up completed restore journal") {
		t.Fatalf("recoverRestoreJournal err=%v, want the completed-cleanup error", err)
	}
}

func TestCoverageAgentARecoverRollbackJournalRemoveError(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config.json") // absent: recovery just removes it
	m := covcliJournalManifest("prepared", []restoreJournalEntry{{
		Name: "config.json", Target: target, Existed: false,
	}})
	dir := covcliWriteJournal(t, m, nil)
	old := removeRestoreJournalDir
	removeRestoreJournalDir = func(string) error { return errors.New("remove boom") }
	t.Cleanup(func() { removeRestoreJournalDir = old })
	var stdout bytes.Buffer
	if err := recoverRestoreJournal(dir, &stdout); err == nil || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("recoverRestoreJournal err=%v, want the post-rollback removal error", err)
	}
}

// ---------------------------------------------------------------------------
// installWithRollback failure paths.
// ---------------------------------------------------------------------------

func TestCoverageAgentAInstallMkdirTempError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "config.json")
	t.Setenv("TMPDIR", filepath.Join(blocker, "sub"))
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("{}")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "create restore journal") {
		t.Fatalf("installWithRollback err=%v, want a journal-creation error", err)
	}
}

func TestCoverageAgentAInstallFailPrepRemoveError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPersist := persistJournalManifest
	persistJournalManifest = func(string, *restoreJournalManifest) error { return errors.New("persist boom") }
	t.Cleanup(func() { persistJournalManifest = oldPersist })
	oldRemove := removeRestoreJournalDir
	removeRestoreJournalDir = func(string) error { return errors.New("remove boom") }
	t.Cleanup(func() { removeRestoreJournalDir = oldRemove })
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("new")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "additionally the incomplete journal") || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("installWithRollback err=%v, want the failPrep removal error", err)
	}
}

func TestCoverageAgentAInstallPersistPreparedError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPersist := persistJournalManifest
	persistJournalManifest = func(string, *restoreJournalManifest) error { return errors.New("persist boom") }
	t.Cleanup(func() { persistJournalManifest = oldPersist })
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("new")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "write restore journal") {
		t.Fatalf("installWithRollback err=%v, want the prepared-manifest error", err)
	}
}

func TestCoverageAgentAInstallReadlinkError(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "config.json")
	if err := os.Symlink(real, target); err != nil {
		t.Fatal(err)
	}
	old := installReadlink
	installReadlink = func(string) (string, error) { return "", errors.New("readlink boom") }
	t.Cleanup(func() { installReadlink = old })
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("new")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "journal restore target") || !strings.Contains(err.Error(), "readlink boom") {
		t.Fatalf("installWithRollback err=%v, want the journaling readlink error", err)
	}
}

func TestCoverageAgentAInstallReadFileError(t *testing.T) {
	// /proc/self/mem is a regular file whose reads fail with EIO, so
	// journaling it fails deterministically without touching any real
	// destination (failPrep aborts before the first install).
	err := installWithRollback(
		[]installItem{{name: "config.json", target: "/proc/self/mem"}},
		map[string][]byte{"config.json": []byte("new")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "journal restore target") || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("installWithRollback err=%v, want the journaling read error", err)
	}
}

func TestCoverageAgentAInstallUnsupportedType(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("new")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("installWithRollback err=%v, want the unsupported-type error", err)
	}
}

// ---------------------------------------------------------------------------
// restoreBundle bundled-audit staging.
// ---------------------------------------------------------------------------

// covcliRestoreBundle builds a fixture backup (config + security store)
// and returns the bundle path and the config path it was made from.
func covcliRestoreBundle(t *testing.T) (bundle, configPath string) {
	t.Helper()
	_, configPath = opsFixture(t)
	bundle = filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("fixture backup code=%d stderr=%s", code, stderr)
	}
	return bundle, configPath
}

func TestCoverageAgentARestoreBundleAuditMkdirError(t *testing.T) {
	bundle, configPath := covcliRestoreBundle(t)
	old := restoreAuditProbeMkdirAll
	restoreAuditProbeMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir boom") }
	t.Cleanup(func() { restoreAuditProbeMkdirAll = old })
	var stdout bytes.Buffer
	err := restoreBundle(bundle, configPath, true, false, &stdout)
	if err == nil || !strings.Contains(err.Error(), "stage bundled security store for audit verification") {
		t.Fatalf("restoreBundle err=%v, want the audit-staging error", err)
	}
}

func TestCoverageAgentARestoreBundleAuditWriteError(t *testing.T) {
	bundle, configPath := covcliRestoreBundle(t)
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// The validate step materializes the (small) config before the audit
	// staging writes the (multi-KB) security snapshot, so a limit just
	// above the config size lets validation pass and fails the staging.
	limitFileSize(t, uint64(len(configBytes))+100)
	var stdout bytes.Buffer
	err = restoreBundle(bundle, configPath, true, false, &stdout)
	if err == nil || !strings.Contains(err.Error(), "stage bundled security store for audit verification") {
		t.Fatalf("restoreBundle err=%v, want the audit-staging write error", err)
	}
}

func TestCoverageAgentARestoreBundleAuditVerifyError(t *testing.T) {
	bundle, configPath := covcliRestoreBundle(t)
	old := restoreBundleAuditContext
	restoreBundleAuditContext = func() context.Context {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		return c
	}
	t.Cleanup(func() { restoreBundleAuditContext = old })
	var stdout bytes.Buffer
	err := restoreBundle(bundle, configPath, true, false, &stdout)
	if err == nil || !strings.Contains(err.Error(), "verify bundled audit chain") {
		t.Fatalf("restoreBundle err=%v, want the audit verify error", err)
	}
}

// ---------------------------------------------------------------------------
// Remaining backup.go seams: canonicalPath Readlink, validateJournal
// saved-copy read, installWithRollback journal syncs and journal-ID RNG.
// ---------------------------------------------------------------------------

func TestCoverageAgentACanonicalPathReadlinkError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	old := canonicalReadlink
	canonicalReadlink = func(string) (string, error) { return "", errors.New("readlink boom") }
	t.Cleanup(func() { canonicalReadlink = old })
	if _, err := canonicalPath(link); err == nil || !strings.Contains(err.Error(), "readlink boom") {
		t.Fatalf("canonicalPath err=%v, want the injected readlink error", err)
	}
}

func TestCoverageAgentAValidateJournalReadSavedCopyError(t *testing.T) {
	content := []byte("original-bytes")
	target := filepath.Join(t.TempDir(), "config.json")
	m := covcliJournalManifest("prepared", []restoreJournalEntry{{
		Name: "config.json", Target: target,
		Existed: true, Mode: 0o600, Saved: "entry-0", SavedSHA256: sha256Hex(content),
	}})
	dir := covcliWriteJournal(t, m, map[string][]byte{"entry-0": content})
	old := validateJournalReadSavedCopy
	validateJournalReadSavedCopy = func(string, string) ([]byte, error) {
		return nil, errors.New("readback boom")
	}
	t.Cleanup(func() { validateJournalReadSavedCopy = old })
	if err := validateJournal(dir, m); err == nil || !strings.Contains(err.Error(), "saved copy") || !strings.Contains(err.Error(), "readback boom") {
		t.Fatalf("validateJournal err=%v, want the injected saved-copy read error", err)
	}
}

func TestCoverageAgentAInstallJournalParentSyncError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	old := journalSyncDir
	journalSyncDir = func(string) error { return errors.New("sync boom") }
	t.Cleanup(func() { journalSyncDir = old })
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("{}")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "create restore journal") || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("installWithRollback err=%v, want the journal-parent sync error", err)
	}
}

func TestCoverageAgentAInstallJournalDirSyncError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := journalSyncDir
	calls := 0
	journalSyncDir = func(string) error {
		calls++
		if calls == 1 {
			return nil // the journal-parent sync before relying on the journal
		}
		return errors.New("sync boom")
	}
	t.Cleanup(func() { journalSyncDir = old })
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("replacement")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "journal restore target") || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("installWithRollback err=%v, want the journal-directory sync error", err)
	}
	// The pre-existing target must be untouched: the failure fired before
	// any install, and failPrep removed the incomplete journal.
	if got, err := os.ReadFile(target); err != nil || string(got) != "original" {
		t.Fatalf("target after failed install: %q err=%v, want %q", got, err, "original")
	}
}

func TestCoverageAgentAInstallJournalRandError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	old := installJournalRandRead
	installJournalRandRead = func([]byte) (int, error) { return 0, errors.New("rand boom") }
	t.Cleanup(func() { installJournalRandRead = old })
	err := installWithRollback(
		[]installItem{{name: "config.json", target: target}},
		map[string][]byte{"config.json": []byte("{}")},
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "create restore journal") || !strings.Contains(err.Error(), "rand boom") {
		t.Fatalf("installWithRollback err=%v, want the journal-ID RNG error", err)
	}
}
