package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Wave C-6 (part 6): backup staging branches (security-store
// snapshot, control-plane state, master-key opt-in) and a full
// interrupted-restore recovery through `restore --recover`.

func backupConfigWithSecurity(t *testing.T, dir string) (cfgPath, storePath string) {
	t.Helper()
	cfgPath = filepath.Join(dir, "nexaroute.json")
	cfg := config.Default()
	cfg.Admin.SecurityStoreBackend = "bbolt"
	storePath = filepath.Join(dir, "security", "nexaroute-security.db")
	cfg.Admin.SecurityStorePath = storePath
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, storePath
}

// Backup stages a live security store's snapshot when present: the
// bundle must contain the security entry and the manifest must not
// claim absence.
func TestBackupStagesSecurityStoreSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfgPath, storePath := backupConfigWithSecurity(t, dir)
	if err := os.MkdirAll(filepath.Dir(storePath), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := openTestSecurityStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "b.tar")
	var out, errBuf bytes.Buffer
	if code := runBackup([]string{"--config", cfgPath, "--out", bundle}, &out, &errBuf); code != 0 {
		t.Fatalf("backup: exit=%d (%s)", code, errBuf.String())
	}
	stage, manifest, _, err := readAndVerifyBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	if strings.Contains(manifest.Notes, "security store") {
		t.Fatalf("manifest notes claim security-store absence: %q", manifest.Notes)
	}
	found := false
	for _, f := range manifest.Files {
		if f.Name == backupEntrySecurity {
			found = true
		}
	}
	if !found {
		t.Fatalf("security entry missing from manifest: %+v", manifest.Files)
	}
}

// A backup stages the file-backend control-plane state and the
// master key (fingerprint always; bytes only with --include-master-key).
func TestBackupStagesControlPlaneAndKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nexaroute.json")
	cfg := config.Default()
	cfg.ControlPlane = config.ControlPlaneConfig{Enabled: true, Backend: "file"}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cpPath := cfgPath + ".controlplane.json"
	if err := os.WriteFile(cpPath, []byte(`{"schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := cfgPath + ".key"
	keyBytes := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(keyPath, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "b.tar")
	var out, errBuf bytes.Buffer
	if code := runBackup([]string{"--config", cfgPath, "--out", bundle}, &out, &errBuf); code != 0 {
		t.Fatalf("backup: exit=%d (%s)", code, errBuf.String())
	}
	stage, manifest, _, err := readAndVerifyBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	sum := sha256.Sum256(keyBytes)
	if manifest.MasterKeySHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("key fingerprint: %q", manifest.MasterKeySHA256)
	}
	if manifest.MasterKeyIncluded {
		t.Fatal("key bytes included without opt-in")
	}
	bundle2 := filepath.Join(dir, "b2.tar")
	if code := runBackup([]string{"--config", cfgPath, "--out", bundle2, "--include-master-key"}, &out, &errBuf); code != 0 {
		t.Fatalf("backup --include-master-key: exit=%d (%s)", code, errBuf.String())
	}
	stage2, manifest2, _, err := readAndVerifyBundle(bundle2)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage2)
	if !manifest2.MasterKeyIncluded {
		t.Fatal("key bytes missing with opt-in")
	}
}

// NOTE: createBackup's "inspect security store" fail-closed branch
// (stat error other than NotExist) is not exercised: as root, the
// only such failure is ENOTDIR, which refuseOutputCollision's
// canonicalPath already rejects earlier (covered by
// TestBackupRefusesOutputCollision); permission-based failures are
// not reproducible with root privileges. The fail-closed ordering
// is intentional and documented in the code.

// buildInterruptedJournal fabricates a well-formed interrupted
// journal: one regular-file entry whose installed content differs
// from the saved pre-restore copy.
func buildInterruptedJournal(t *testing.T, base string) (journalDir, target string) {
	t.Helper()
	journalDir = filepath.Join(base, "nexaroute-restore-journal-deadbeef01")
	target = filepath.Join(base, "victim.json")
	oldContent := []byte(`{"v":1}`)
	newContent := []byte(`{"v":2}`)
	if err := os.MkdirAll(journalDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, newContent, 0600); err != nil {
		t.Fatal(err)
	}
	savedName := "entry-0"
	if err := os.WriteFile(filepath.Join(journalDir, savedName), oldContent, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(oldContent)
	m := &restoreJournalManifest{
		Format: restoreJournalFormat,
		ID:     "deadbeef01",
		State:  "installing",
		Entries: []restoreJournalEntry{{
			Name:        backupEntryConfig,
			Target:      target,
			Existed:     true,
			Mode:        0600,
			Saved:       savedName,
			SavedSHA256: hex.EncodeToString(sum[:]),
			Installed:   true,
		}},
	}
	if err := writeJournalManifest(journalDir, m); err != nil {
		t.Fatal(err)
	}
	return journalDir, target
}

// `restore --recover` rolls an interrupted transaction back: the
// destination regains its pre-restore content and the journal is
// removed.
func TestRestoreRecoverRollsBackInterruptedJournal(t *testing.T) {
	dir := t.TempDir()
	journalDir, target := buildInterruptedJournal(t, dir)
	var out, errBuf bytes.Buffer
	if code := runRestore([]string{"--recover", journalDir}, &out, &errBuf); code != 0 {
		t.Fatalf("recover: exit=%d (%s)", code, errBuf.String())
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != `{"v":1}` {
		t.Fatalf("target after rollback: %q %v", got, err)
	}
	if _, err := os.Stat(journalDir); !os.IsNotExist(err) {
		t.Fatalf("journal not removed: %v", err)
	}
	if !strings.Contains(out.String(), "rolled back") {
		t.Fatalf("stdout: %q", out.String())
	}
}

// A tampered journal (saved copy content no longer matches the
// recorded SHA-256) must fail validation with nothing changed.
func TestRestoreRecoverRefusesTamperedJournal(t *testing.T) {
	dir := t.TempDir()
	journalDir, target := buildInterruptedJournal(t, dir)
	if err := os.WriteFile(filepath.Join(journalDir, "entry-0"), []byte(`{"v":9}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runRestore([]string{"--recover", journalDir}, &out, &errBuf); code != 1 {
		t.Fatalf("tampered journal: exit=%d, want 1", code)
	}
	if got := errBuf.String(); !strings.Contains(got, "failed validation") {
		t.Fatalf("stderr: %q", got)
	}
	// The destination is untouched and the journal preserved.
	got, _ := os.ReadFile(target)
	if string(got) != `{"v":2}` {
		t.Fatalf("target mutated by failed recovery: %q", got)
	}
	if _, err := os.Stat(journalDir); err != nil {
		t.Fatalf("journal not preserved: %v", err)
	}
}

// checkControlPlane: the redis backend case routes through
// checkExternal with the redis env var name.
func TestDoctorCheckControlPlaneRedisUnset(t *testing.T) {
	const env = "NEXAROUTE_WAVEC_UNSET_CP_REDIS"
	os.Unsetenv(env)
	cfg := config.Default()
	cfg.ControlPlane = config.ControlPlaneConfig{Enabled: true, Backend: "redis", RedisURLEnv: env}
	var w bytes.Buffer
	d := &doctor{w: &w, cfg: &cfg}
	d.checkControlPlane("config.json")
	if d.fails != 1 || !strings.Contains(w.String(), "FAIL control_plane_redis:") {
		t.Fatalf("%s", w.String())
	}
}
