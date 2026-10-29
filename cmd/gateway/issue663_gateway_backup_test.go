package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Issue #663 (gateway scope): behavior tests for the backup/restore
// branches that the ba5ff2f coverprofile shows as uncovered. Every
// test below asserts a real outcome (bytes, modes, error text, or
// rollback state); none exists only to move a percentage.

func issue663GatewayWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func issue663GatewayLiveServer(t *testing.T, body []byte, status int, seenKey *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seenKey != nil {
			*seenKey = r.Header.Get("x-admin-key")
		}
		if r.URL.Path != "/admin/api/security-snapshot" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

// issue663GatewayStoreBytes builds a small security store with two
// chained audit events and returns its on-disk bytes. When tamper is
// true the first event is rewritten below the adapter so the chain
// no longer verifies.
func issue663GatewayStoreBytes(t *testing.T, tamper bool) []byte {
	t.Helper()
	dir := t.TempDir()
	// The store directory must be 0700: securitystore.Open enforces
	// it, and a bare t.TempDir() root is not guaranteed to qualify.
	storeDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(storeDir, "security.db")
	store, err := securitystore.Open(path, 100, 365)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{
			Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if tamper {
		tamperAuditStoreFile(t, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestIssue663GatewayRunBackupFlagAndLoadErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runBackup([]string{"--bogus-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad flag exit=%d want 2", code)
	}
	dir := t.TempDir()
	badConfig := filepath.Join(dir, "bad.json")
	issue663GatewayWrite(t, badConfig, []byte(`{"listen": 123}`))
	if err := createBackup(badConfig, filepath.Join(dir, "b.tar"), false, false, io.Discard, liveBackupOptions{}); err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("type-broken config must fail at load: %v", err)
	}
}

func TestIssue663GatewayCreateBackupAggregateLimit(t *testing.T) {
	old := maxBackupTotalBytes
	maxBackupTotalBytes = 10
	defer func() { maxBackupTotalBytes = old }()
	dir := t.TempDir()
	src := backupConfig(t, dir)
	if err := createBackup(src, filepath.Join(dir, "b.tar"), false, false, io.Discard, liveBackupOptions{}); err == nil || !strings.Contains(err.Error(), "aggregate bundle limit") {
		t.Fatalf("tiny aggregate limit must be refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.tar")); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written when staging exceeds the aggregate limit")
	}
}

func TestIssue663GatewayCreateBackupMasterKeyUnreadable(t *testing.T) {
	dir := t.TempDir()
	src := backupConfig(t, dir)
	if err := os.MkdirAll(src+".key", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createBackup(src, filepath.Join(dir, "b.tar"), false, false, io.Discard, liveBackupOptions{}); err == nil || !strings.Contains(err.Error(), "read master key file") {
		t.Fatalf("directory in place of the key file must fail: %v", err)
	}
}

func TestIssue663GatewayLiveBackupUnreachableIsFinal(t *testing.T) {
	dir := t.TempDir()
	src := backupConfig(t, dir)
	live := liveBackupOptions{enabled: true, adminURL: "http://127.0.0.1:1", adminKey: "test-key"}
	if err := createBackup(src, filepath.Join(dir, "b.tar"), false, false, io.Discard, live); err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("unreachable admin must fail finally: %v", err)
	}
}

func TestIssue663GatewayLiveBackupSuccessAndAuthHeader(t *testing.T) {
	var seenKey string
	srv := issue663GatewayLiveServer(t, issue663GatewayStoreBytes(t, false), http.StatusOK, &seenKey)
	defer srv.Close()
	dir := t.TempDir()
	src := backupConfig(t, dir)
	var stdout bytes.Buffer
	live := liveBackupOptions{enabled: true, adminURL: srv.URL, adminKey: "operator-key"}
	if err := createBackup(src, filepath.Join(dir, "b.tar"), false, false, &stdout, live); err != nil {
		t.Fatalf("live backup: %v", err)
	}
	if seenKey != "operator-key" {
		t.Fatalf("admin key header=%q want operator-key", seenKey)
	}
	if !strings.Contains(stdout.String(), "backup written to") {
		t.Fatalf("stdout missing completion line: %s", stdout.String())
	}
	_, manifest, _, err := readAndVerifyBundle(filepath.Join(dir, "b.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest.Notes, "taken live from the running gateway") {
		t.Fatalf("manifest notes must record the live snapshot: %q", manifest.Notes)
	}
	found := false
	for _, f := range manifest.Files {
		if f.Name == backupEntrySecurity {
			found = true
		}
	}
	if !found {
		t.Fatalf("live bundle lacks the security entry: %+v", manifest.Files)
	}
}

func TestIssue663GatewayLiveBackupProbeOpenFailure(t *testing.T) {
	srv := issue663GatewayLiveServer(t, []byte("not a bbolt store"), http.StatusOK, nil)
	defer srv.Close()
	dir := t.TempDir()
	src := backupConfig(t, dir)
	live := liveBackupOptions{enabled: true, adminURL: srv.URL}
	if err := createBackup(src, filepath.Join(dir, "b.tar"), false, false, io.Discard, live); err == nil || !strings.Contains(err.Error(), "open live security snapshot for verification") {
		t.Fatalf("garbage live snapshot must fail at probe open: %v", err)
	}
}

func TestIssue663GatewayLiveBackupBrokenChain(t *testing.T) {
	srv := issue663GatewayLiveServer(t, issue663GatewayStoreBytes(t, true), http.StatusOK, nil)
	defer srv.Close()
	dir := t.TempDir()
	src := backupConfig(t, dir)
	live := liveBackupOptions{enabled: true, adminURL: srv.URL}
	if err := createBackup(src, filepath.Join(dir, "b.tar"), false, false, io.Discard, live); err == nil || !strings.Contains(err.Error(), "audit chain verification failed") {
		t.Fatalf("tampered live snapshot must be refused without override: %v", err)
	}
	var stdout bytes.Buffer
	if err := createBackup(src, filepath.Join(dir, "b2.tar"), false, true, &stdout, live); err != nil {
		t.Fatalf("override live backup: %v", err)
	}
	if !strings.Contains(stdout.String(), "WARNING: live security snapshot audit chain FAILED verification") {
		t.Fatalf("override must warn loudly: %s", stdout.String())
	}
	_, manifest, _, err := readAndVerifyBundle(filepath.Join(dir, "b2.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest.Notes, "FAILED verification at backup time") {
		t.Fatalf("override must be recorded in the manifest: %q", manifest.Notes)
	}
}

func TestIssue663GatewayStoppedBackupBrokenChain(t *testing.T) {
	dir := t.TempDir()
	cfgPath, storePath := backupConfigWithSecurity(t, dir)
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := openTestSecurityStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{
			Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tamperAuditStoreFile(t, storePath)
	if err := createBackup(cfgPath, filepath.Join(dir, "b.tar"), false, false, io.Discard, liveBackupOptions{}); err == nil || !strings.Contains(err.Error(), "audit chain verification failed") {
		t.Fatalf("tampered stopped store must be refused without override: %v", err)
	}
	var stdout bytes.Buffer
	if err := createBackup(cfgPath, filepath.Join(dir, "b2.tar"), false, true, &stdout, liveBackupOptions{}); err != nil {
		t.Fatalf("override stopped backup: %v", err)
	}
	if !strings.Contains(stdout.String(), "WARNING: audit chain FAILED verification") {
		t.Fatalf("override must warn loudly: %s", stdout.String())
	}
}

func TestIssue663GatewayResolveLiveAdminURLSchemeListen(t *testing.T) {
	got, err := resolveLiveAdminURL("", "http://127.0.0.1:9000/")
	if err != nil || got != "http://127.0.0.1:9000" {
		t.Fatalf("scheme listen = %q, %v", got, err)
	}
}

func TestIssue663GatewayFetchLiveSnapshotStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, "bad key", "gateway admin authentication"},
		{http.StatusForbidden, "forbidden", "gateway admin authentication"},
		{http.StatusNotFound, "", "too old to offer live snapshots"},
		{http.StatusNotImplemented, "", "not supported by the gateway's security store backend"},
		{http.StatusServiceUnavailable, "store busy", "gateway refused the live snapshot (HTTP 503): store busy"},
		{http.StatusTeapot, "weird", "live snapshot request failed (HTTP 418): weird"},
	}
	for _, tc := range cases {
		srv := issue663GatewayLiveServer(t, []byte(tc.body), tc.status, nil)
		_, err := fetchLiveSecuritySnapshot(srv.URL, "k")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: err=%v want %q", tc.status, err, tc.want)
		}
	}
	// Success returns the exact bytes and sends a trimmed key header.
	var seenKey string
	srv := issue663GatewayLiveServer(t, []byte("snapshot-bytes"), http.StatusOK, &seenKey)
	defer srv.Close()
	got, err := fetchLiveSecuritySnapshot(srv.URL, "  spaced-key  ")
	if err != nil || string(got) != "snapshot-bytes" || seenKey != "spaced-key" {
		t.Fatalf("success: got=%q err=%v key=%q", got, err, seenKey)
	}
	// Unreachable admin and an unparseable URL are both final errors.
	if _, err := fetchLiveSecuritySnapshot("http://127.0.0.1:1", ""); err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("unreachable: %v", err)
	}
	if _, err := fetchLiveSecuritySnapshot("http://exa mple.invalid", ""); err == nil || !strings.Contains(err.Error(), "build live snapshot request") {
		t.Fatalf("bad URL: %v", err)
	}
}

func TestIssue663GatewayCanonicalPath(t *testing.T) {
	if _, err := canonicalPath(""); err == nil || !strings.Contains(err.Error(), "empty path") {
		t.Fatalf("empty path: %v", err)
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	// Relative spellings resolve against the working directory.
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// NOTE: filepath.Join would Clean "." and ".." away before the
	// function ever sees them, so these paths are concatenated.
	got, err := canonicalPath("real" + string(os.PathSeparator) + "." + string(os.PathSeparator) + "file")
	if err != nil || got != filepath.Join(real, "file") {
		t.Fatalf("dot component: %q %v", got, err)
	}
	// ".." applies physically across a symlink: a/link/.. is the
	// parent of the link target, not the parent of the link.
	sub := filepath.Join(dir, "a")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(sub, "link")); err != nil {
		t.Fatal(err)
	}
	got, err = canonicalPath(filepath.Join(sub, "link") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "sibling")
	if err != nil || got != filepath.Join(dir, "sibling") {
		t.Fatalf("physical dotdot: %q %v want %q", got, err, filepath.Join(dir, "sibling"))
	}
	// A symlinked parent with a missing tail canonicalizes through
	// the link, and a dangling final link resolves through its target.
	got, err = canonicalPath(filepath.Join(sub, "link", "new", "file"))
	if err != nil || got != filepath.Join(real, "new", "file") {
		t.Fatalf("missing tail through link: %q %v", got, err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(real, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	got, err = canonicalPath(dangling)
	if err != nil || got != filepath.Join(real, "gone") {
		t.Fatalf("dangling link: %q %v", got, err)
	}
	// A symlink loop errors instead of resolving forever.
	if err := os.Symlink(filepath.Join(dir, "loop-b"), filepath.Join(dir, "loop-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "loop-a"), filepath.Join(dir, "loop-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalPath(filepath.Join(dir, "loop-a", "x")); err == nil || !strings.Contains(err.Error(), "too many symlink resolutions") {
		t.Fatalf("loop: %v", err)
	}
	// A non-directory component is a real error, not a missing path.
	blocker := filepath.Join(dir, "blocker")
	issue663GatewayWrite(t, blocker, []byte("x"))
	if _, err := canonicalPath(filepath.Join(blocker, "x")); err == nil {
		t.Fatal("path under a regular file must fail canonicalization")
	}
}

func TestIssue663GatewayWriteBundle(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("bundle-config")
	issue663GatewayWrite(t, filepath.Join(stage, backupEntryConfig), payload)
	manifestBytes := []byte(`{"format":"nexaroute-backup/1"}`)
	files := []backupManifestFile{{Name: backupEntryConfig, SHA256: sha256Hex(payload), Size: int64(len(payload))}}
	out := filepath.Join(dir, "bundle.tar")
	if err := writeBundle(out, manifestBytes, files, stage); err != nil {
		t.Fatalf("writeBundle: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode=%v err=%v want 0600", info.Mode().Perm(), err)
	}
	fh, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	tr := tar.NewReader(fh)
	var names []string
	var first []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == backupEntryManifest {
			first = data
		}
	}
	if len(names) != 2 || names[0] != backupEntryManifest || names[1] != backupEntryConfig {
		t.Fatalf("tar entries=%v", names)
	}
	if !bytes.Equal(first, manifestBytes) {
		t.Fatalf("manifest entry differs: %q", first)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nexaroute-backup-") {
			t.Fatalf("temp bundle file leaked: %s", e.Name())
		}
	}
	// Missing staged payload, a directory payload, and a negative
	// declared size must all fail instead of writing a bad bundle.
	if err := writeBundle(filepath.Join(dir, "m.tar"), manifestBytes, files, filepath.Join(dir, "no-stage")); err == nil {
		t.Fatal("missing staged payload accepted")
	}
	stageDir := filepath.Join(dir, "stage-dir")
	if err := os.MkdirAll(filepath.Join(stageDir, backupEntryConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(filepath.Join(dir, "d.tar"), manifestBytes, files, stageDir); err == nil {
		t.Fatal("directory payload accepted")
	}
	badSize := []backupManifestFile{{Name: backupEntryConfig, SHA256: sha256Hex(payload), Size: -1}}
	if err := writeBundle(filepath.Join(dir, "n.tar"), manifestBytes, badSize, stage); err == nil {
		t.Fatal("negative entry size accepted")
	}
	// Output directory under a regular file: no temp file can be made.
	blocker := filepath.Join(dir, "blocker")
	issue663GatewayWrite(t, blocker, []byte("x"))
	if err := writeBundle(filepath.Join(blocker, "b.tar"), manifestBytes, files, stage); err == nil {
		t.Fatal("uncreatable output accepted")
	}
}

func TestIssue663GatewayWriteFileAtomicAndSavedCopy(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "file.txt")
	if err := writeFileAtomic(nested, []byte("hello"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "a")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("created parent mode=%v err=%v want 0700", info.Mode().Perm(), err)
	}
	got, err := os.ReadFile(nested)
	if err != nil || string(got) != "hello" {
		t.Fatalf("content=%q err=%v", got, err)
	}
	if info, err := os.Stat(nested); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v want 0600", info.Mode().Perm(), err)
	}
	if err := writeFileAtomic(nested, []byte("replaced"), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err = os.ReadFile(nested)
	if err != nil || string(got) != "replaced" {
		t.Fatalf("overwrite content=%q err=%v", got, err)
	}
	if info, err := os.Stat(nested); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("overwrite mode=%v err=%v want 0644", info.Mode().Perm(), err)
	}
	saved := filepath.Join(dir, "saved.bin")
	if err := writeSavedCopyDurable(saved, []byte("durably")); err != nil {
		t.Fatalf("writeSavedCopyDurable: %v", err)
	}
	got, err = os.ReadFile(saved)
	if err != nil || string(got) != "durably" {
		t.Fatalf("saved content=%q err=%v", got, err)
	}
	if info, err := os.Stat(saved); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved mode=%v err=%v want 0600", info.Mode().Perm(), err)
	}
	// /dev/full fails every write with ENOSPC: the write branch,
	// not the open branch, must report it.
	if err := writeSavedCopyDurable("/dev/full", []byte("x")); err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("/dev/full write: %v", err)
	}
}

func TestIssue663GatewayJournalFiles(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "journal")
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &restoreJournalManifest{
		Format: restoreJournalFormat, ID: "abcdef0123456789", State: "prepared",
		Entries: []restoreJournalEntry{{Name: backupEntryConfig, Target: filepath.Join(dir, "c.json"), Existed: false}},
	}
	if err := writeJournalManifest(journal, m); err != nil {
		t.Fatalf("writeJournalManifest: %v", err)
	}
	info, err := os.Stat(filepath.Join(journal, "manifest.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode=%v err=%v want 0600", info.Mode().Perm(), err)
	}
	back, err := readJournalManifest(journal)
	if err != nil || back.ID != m.ID || back.State != "prepared" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	entries, err := os.ReadDir(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".manifest-") {
			t.Fatalf("temp manifest leaked: %s", e.Name())
		}
	}
	// Manifest creation fails when the journal directory cannot hold
	// a temp file, and rename fails when manifest.json is a directory.
	blocker := filepath.Join(dir, "blocker")
	issue663GatewayWrite(t, blocker, []byte("x"))
	if err := writeJournalManifest(filepath.Join(blocker, "j"), m); err == nil {
		t.Fatal("manifest under a regular file accepted")
	}
	renameDir := filepath.Join(dir, "rename-journal")
	if err := os.MkdirAll(filepath.Join(renameDir, "manifest.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJournalManifest(renameDir, m); err == nil {
		t.Fatal("rename onto a directory accepted")
	}
	// removeJournalDir removes, and reports a path it cannot remove.
	victim := filepath.Join(dir, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	issue663GatewayWrite(t, filepath.Join(victim, "f"), []byte("x"))
	if err := removeJournalDir(victim); err != nil {
		t.Fatalf("removeJournalDir: %v", err)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("journal dir survived: %v", err)
	}
	if err := removeJournalDir(filepath.Join(blocker, "x")); err == nil {
		t.Fatal("remove under a regular file accepted")
	}
	// syncDir: a real directory syncs, a missing one errors.
	if err := syncDir(dir); err != nil {
		t.Fatalf("syncDir: %v", err)
	}
	if err := syncDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("sync of a missing directory accepted")
	}
}

func TestIssue663GatewayInstallWithRollback(t *testing.T) {
	// Empty plan is a no-op.
	if err := installWithRollback(nil, map[string][]byte{}, io.Discard); err != nil {
		t.Fatalf("empty install: %v", err)
	}
	// Success: a pre-existing file is replaced and a new file is
	// created. (No global TempDir journal scan here: sibling
	// sessions share TMPDIR, so a scan cannot attribute a journal
	// directory to this test.)
	dir := t.TempDir()
	oldTarget := filepath.Join(dir, "old.json")
	issue663GatewayWrite(t, oldTarget, []byte("old"))
	if err := os.Chmod(oldTarget, 0o644); err != nil {
		t.Fatal(err)
	}
	newTarget := filepath.Join(dir, "new.json")
	var stdout bytes.Buffer
	installs := []installItem{{backupEntryConfig, oldTarget}, {backupEntryKey, newTarget}}
	payloads := map[string][]byte{backupEntryConfig: []byte("new-config"), backupEntryKey: []byte("new-key")}
	if err := installWithRollback(installs, payloads, &stdout); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(oldTarget)
	if err != nil || string(got) != "new-config" {
		t.Fatalf("replaced content=%q err=%v", got, err)
	}
	if info, err := os.Stat(oldTarget); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("replaced mode=%v err=%v want 0600", info.Mode().Perm(), err)
	}
	got, err = os.ReadFile(newTarget)
	if err != nil || string(got) != "new-key" {
		t.Fatalf("created content=%q err=%v", got, err)
	}
	if !strings.Contains(stdout.String(), "restored "+oldTarget) || !strings.Contains(stdout.String(), "restored "+newTarget) {
		t.Fatalf("stdout must name both installs: %s", stdout.String())
	}
	// A directory target is refused before anything is written.
	dirTarget := filepath.Join(dir, "dirtarget")
	if err := os.MkdirAll(dirTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "sentinel.json")
	issue663GatewayWrite(t, sentinel, []byte("keep"))
	if err := installWithRollback([]installItem{{backupEntryConfig, dirTarget}}, map[string][]byte{backupEntryConfig: []byte("x")}, io.Discard); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("directory target: %v", err)
	}
	// A target under a regular file fails journaling with no writes.
	blocker := filepath.Join(dir, "blocker")
	issue663GatewayWrite(t, blocker, []byte("x"))
	if err := installWithRollback([]installItem{{backupEntryConfig, filepath.Join(blocker, "x.json")}}, map[string][]byte{backupEntryConfig: []byte("x")}, io.Discard); err == nil || !strings.Contains(err.Error(), "journal restore target") {
		t.Fatalf("lstat failure: %v", err)
	}
}

func TestIssue663GatewayInstallRollbackOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	issue663GatewayWrite(t, first, []byte("original"))
	if err := os.Chmod(first, 0o640); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "second.json")
	oldWrite := restoreWriteFile
	defer func() { restoreWriteFile = oldWrite }()
	restoreWriteFile = func(path string, data []byte, perm os.FileMode) error {
		if path == second {
			return io.ErrUnexpectedEOF
		}
		return oldWrite(path, data, perm)
	}
	installs := []installItem{{backupEntryConfig, first}, {backupEntryKey, second}}
	payloads := map[string][]byte{backupEntryConfig: []byte("replaced"), backupEntryKey: []byte("never")}
	err := installWithRollback(installs, payloads, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("failed install must roll back: %v", err)
	}
	got, rerr := os.ReadFile(first)
	if rerr != nil || string(got) != "original" {
		t.Fatalf("rollback content=%q err=%v want original", got, rerr)
	}
	if info, err := os.Stat(first); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("rollback mode=%v err=%v want 0640", info.Mode().Perm(), err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("failed install must not leave the second file: %v", err)
	}
	// A failure on the very first install still reports rollback and
	// leaves the target untouched.
	restoreWriteFile = func(path string, data []byte, perm os.FileMode) error {
		return io.ErrUnexpectedEOF
	}
	if err := installWithRollback([]installItem{{backupEntryConfig, first}}, map[string][]byte{backupEntryConfig: []byte("x")}, io.Discard); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("first-install failure: %v", err)
	}
	got, rerr = os.ReadFile(first)
	if rerr != nil || string(got) != "original" {
		t.Fatalf("first-install failure changed the target: %q %v", got, rerr)
	}
}

func TestIssue663GatewayInstallRollbackRestoresSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	issue663GatewayWrite(t, real, []byte("linked-content"))
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "second.json")
	oldWrite := restoreWriteFile
	defer func() { restoreWriteFile = oldWrite }()
	restoreWriteFile = func(path string, data []byte, perm os.FileMode) error {
		if path == second {
			return io.ErrUnexpectedEOF
		}
		return oldWrite(path, data, perm)
	}
	installs := []installItem{{backupEntryConfig, link}, {backupEntryKey, second}}
	payloads := map[string][]byte{backupEntryConfig: []byte("replaced"), backupEntryKey: []byte("never")}
	if err := installWithRollback(installs, payloads, io.Discard); err == nil {
		t.Fatal("failing install accepted")
	}
	target, err := os.Readlink(link)
	if err != nil || target != real {
		t.Fatalf("rollback must recreate the link: %q %v", target, err)
	}
	got, err := os.ReadFile(real)
	if err != nil || string(got) != "linked-content" {
		t.Fatalf("link target content changed: %q %v", got, err)
	}
}

// issue663GatewayPreservedJournal extracts the journal directory a
// failed install reports as preserved, and asserts it is a real
// journal directory still in the "installing" state. Parsing the
// error (instead of scanning the shared TempDir) keeps the assertion
// attributable to this test even while sibling sessions run.
func issue663GatewayPreservedJournal(t *testing.T, err error, marker, terminator string) string {
	t.Helper()
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		t.Fatalf("error does not name a preserved journal (%q): %v", marker, err)
	}
	rest := msg[i+len(marker):]
	end := strings.Index(rest, terminator)
	if end < 0 {
		t.Fatalf("cannot parse journal path from error: %v", err)
	}
	dir := strings.TrimSpace(rest[:end])
	m, rerr := readJournalManifest(dir)
	if rerr != nil {
		t.Fatalf("preserved journal %s is unreadable: %v", dir, rerr)
	}
	if m.State != "installing" {
		t.Fatalf("preserved journal state=%q want installing", m.State)
	}
	return dir
}

func TestIssue663GatewayInstallJournalPersistFailures(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "c.json")
	issue663GatewayWrite(t, target, []byte("before"))
	oldPersist := persistJournalManifest
	defer func() { persistJournalManifest = oldPersist }()
	// Failure while writing the "prepared" manifest: nothing written.
	persistJournalManifest = func(string, *restoreJournalManifest) error {
		return io.ErrUnexpectedEOF
	}
	if err := installWithRollback([]installItem{{backupEntryConfig, target}}, map[string][]byte{backupEntryConfig: []byte("after")}, io.Discard); err == nil || !strings.Contains(err.Error(), "write restore journal") {
		t.Fatalf("prepared persist failure: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "before" {
		t.Fatalf("prepared failure changed the target: %q %v", got, err)
	}
	// Failure while writing the "installing" manifest: still nothing.
	calls := 0
	persistJournalManifest = func(dir string, m *restoreJournalManifest) error {
		calls++
		if calls == 2 {
			return io.ErrUnexpectedEOF
		}
		return oldPersist(dir, m)
	}
	if err := installWithRollback([]installItem{{backupEntryConfig, target}}, map[string][]byte{backupEntryConfig: []byte("after")}, io.Discard); err == nil || !strings.Contains(err.Error(), "write restore journal") {
		t.Fatalf("installing persist failure: %v", err)
	}
	got, err = os.ReadFile(target)
	if err != nil || string(got) != "before" {
		t.Fatalf("installing failure changed the target: %q %v", got, err)
	}
	// Failure while marking "completed": the files ARE installed but
	// the journal is preserved and success is never reported.
	persistJournalManifest = func(dir string, m *restoreJournalManifest) error {
		if m.State == "completed" {
			return io.ErrUnexpectedEOF
		}
		return oldPersist(dir, m)
	}
	err = installWithRollback([]installItem{{backupEntryConfig, target}}, map[string][]byte{backupEntryConfig: []byte("after")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "could not be marked completed") {
		t.Fatalf("completed persist failure: %v", err)
	}
	installErr := err
	got, err = os.ReadFile(target)
	if err != nil || string(got) != "after" {
		t.Fatalf("completed failure must still have installed: %q %v", got, err)
	}
	journal := issue663GatewayPreservedJournal(t, installErr, "journal is preserved at ", " with")
	if rerr := os.RemoveAll(journal); rerr != nil {
		t.Fatalf("cleanup preserved journal %s: %v", journal, rerr)
	}
}

func TestIssue663GatewayInstallSavedCopySeamFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "c.json")
	issue663GatewayWrite(t, target, []byte("before"))
	oldSaved := writeJournalSavedCopy
	defer func() { writeJournalSavedCopy = oldSaved }()
	writeJournalSavedCopy = func(string, []byte) error { return io.ErrUnexpectedEOF }
	if err := installWithRollback([]installItem{{backupEntryConfig, target}}, map[string][]byte{backupEntryConfig: []byte("after")}, io.Discard); err == nil || !strings.Contains(err.Error(), "journal restore target") {
		t.Fatalf("saved-copy failure: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "before" {
		t.Fatalf("saved-copy failure changed the target: %q %v", got, err)
	}
}

func TestIssue663GatewayInstallSyncFailures(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "c.json")
	issue663GatewayWrite(t, target, []byte("before"))
	oldSync := syncDirFn
	defer func() { syncDirFn = oldSync }()
	// Only the first destination sync fails: the install is rolled
	// back cleanly and the journal is removed.
	calls := 0
	syncDirFn = func(dir string) error {
		calls++
		if calls == 1 {
			return io.ErrUnexpectedEOF
		}
		return oldSync(dir)
	}
	if err := installWithRollback([]installItem{{backupEntryConfig, target}}, map[string][]byte{backupEntryConfig: []byte("after")}, io.Discard); err == nil || !strings.Contains(err.Error(), "syncing its directory failed") || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("single sync failure: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "before" {
		t.Fatalf("sync failure rollback content=%q err=%v", got, err)
	}
	// Every sync fails, including rollback syncs: rollback is
	// INCOMPLETE and the journal is preserved for a retry.
	syncDirFn = func(string) error { return io.ErrUnexpectedEOF }
	err = installWithRollback([]installItem{{backupEntryConfig, target}}, map[string][]byte{backupEntryConfig: []byte("after")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rollback INCOMPLETE") {
		t.Fatalf("permanent sync failure: %v", err)
	}
	journal := issue663GatewayPreservedJournal(t, err, "journal at ", " —")
	if rerr := os.RemoveAll(journal); rerr != nil {
		t.Fatalf("cleanup preserved journal %s: %v", journal, rerr)
	}
}

func TestIssue663GatewayValidateBundledConfigErrors(t *testing.T) {
	plain := bundleConfigBytes(t, func(*config.Config) {})
	// Stage under a regular file: the validate directory cannot be made.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	issue663GatewayWrite(t, blocker, []byte("x"))
	if _, _, err := validateBundledConfig(filepath.Join(blocker, "stage"), map[string][]byte{backupEntryConfig: plain}); err == nil {
		t.Fatal("stage under a regular file accepted")
	}
	// Staged config path is a directory: the staged write fails.
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(filepath.Join(stage, "validate", "config.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateBundledConfig(stage, map[string][]byte{backupEntryConfig: plain}); err == nil {
		t.Fatal("staged config as a directory accepted")
	}
	// Staged key path is a directory: the staged key write fails.
	stage2 := filepath.Join(dir, "stage2")
	if err := os.MkdirAll(filepath.Join(stage2, "validate", "config.json.key"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateBundledConfig(stage2, map[string][]byte{backupEntryConfig: plain, backupEntryKey: []byte("k")}); err == nil {
		t.Fatal("staged key as a directory accepted")
	}
	// Invalid JSON that still carries an nxs1 envelope marker: the
	// keyless path is taken, then the structural parse fails.
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	stage3 := filepath.Join(dir, "stage3")
	if _, _, err := validateBundledConfig(stage3, map[string][]byte{backupEntryConfig: []byte(`{"x": "nxs1:AAAA`)}); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("unparseable keyless config: %v", err)
	}
}

func TestIssue663GatewayKeylessRestoreJustified(t *testing.T) {
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	plain := bundleConfigBytes(t, func(*config.Config) {})
	if keylessRestoreJustified(map[string][]byte{backupEntryConfig: plain}) {
		t.Fatal("plaintext config must never justify a keyless restore")
	}
	plainCfg := config.Default()
	plainCfg.Providers = append(plainCfg.Providers, config.ProviderConfig{
		ID: "p1", Type: "openai_compatible", BaseURL: "https://example.invalid/v1",
		APIKey: "secret-value", Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true}},
	})
	raw, err := json.Marshal(plainCfg)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	encrypted, _, err := secrets.TransformConfig(raw, key, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encrypted, []byte(secrets.Prefix)) {
		t.Fatal("fixture was not encrypted")
	}
	encPayload := map[string][]byte{backupEntryConfig: encrypted}
	if !keylessRestoreJustified(encPayload) {
		t.Fatal("encrypted config with no key anywhere must justify a keyless restore")
	}
	withKey := map[string][]byte{backupEntryConfig: encrypted, backupEntryKey: key}
	if keylessRestoreJustified(withKey) {
		t.Fatal("a bundled key must defeat keyless justification")
	}
	// Each environment key source independently defeats it.
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "present")
			if keylessRestoreJustified(encPayload) {
				t.Fatalf("env %s must defeat keyless justification", env)
			}
		})
	}
	// The SHA-256 helper used by journals and manifests is exact.
	sum := sha256.Sum256([]byte("abc"))
	if got := sha256Hex([]byte("abc")); got != hex.EncodeToString(sum[:]) || got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sha256Hex=%q", got)
	}
}
