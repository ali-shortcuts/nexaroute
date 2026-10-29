package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// liveSnapshotServer serves one security snapshot the way the real
// gateway endpoint does, from an already-open store handle. mode
// selects the behavior under test; only "ok" streams real bytes.
func liveSnapshotServer(t *testing.T, store *securitystore.Store, wantKey, mode string, tampered []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/api/security-snapshot" {
			http.NotFound(w, r)
			return
		}
		if wantKey != "" && r.Header.Get("x-admin-key") != wantKey {
			http.Error(w, `{"error":"admin authentication required"}`, http.StatusUnauthorized)
			return
		}
		switch mode {
		case "notfound":
			http.NotFound(w, r)
		case "unauthorized":
			http.Error(w, `{"error":"admin authentication required"}`, http.StatusUnauthorized)
		case "broken":
			http.Error(w, `{"error":"audit chain verification failed"}`, http.StatusServiceUnavailable)
		case "tampered":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tampered)
		default:
			report, err := store.VerifyAuditChain(r.Context())
			if err != nil || !report.Verified {
				http.Error(w, `{"error":"audit chain verification failed"}`, http.StatusServiceUnavailable)
				return
			}
			var buf bytes.Buffer
			if err := store.BackupTo(r.Context(), &buf); err != nil {
				http.Error(w, `{"error":"snapshot failed"}`, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(buf.Bytes())
		}
	}))
}

func openFixtureStore(t *testing.T, dir string) *securitystore.Store {
	t.Helper()
	store, err := securitystore.Open(fixtureSecurityPath(dir), 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func readBundleEntry(t *testing.T, bundlePath, name string) []byte {
	t.Helper()
	fh, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	tr := tar.NewReader(fh)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("bundle entry %q not found", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == name {
			return raw
		}
	}
}

func readBundleManifest(t *testing.T, bundlePath string) backupManifest {
	t.Helper()
	var manifest backupManifest
	if err := json.Unmarshal(readBundleEntry(t, bundlePath, backupEntryManifest), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestLiveBackupFromRunningGatewayVerifies(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	out := filepath.Join(t.TempDir(), "live.tar")
	code, stderr := runBackupCode(t, configPath, out, "--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key")
	if code != 0 {
		t.Fatalf("live backup exit=%d stderr=%s, want 0", code, stderr)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("live bundle mode/stat: %v %v, want 0600", info, err)
	}
	manifest := readBundleManifest(t, out)
	if !strings.Contains(manifest.Notes, "taken live from the running gateway") {
		t.Fatalf("live bundle manifest must record live provenance, got notes %q", manifest.Notes)
	}
	secBytes := readBundleEntry(t, out, backupEntrySecurity)
	if len(secBytes) == 0 {
		t.Fatal("live bundle has an empty security entry")
	}
	// The live bundle must pass the same isolated verification as a
	// stopped bundle, including its audit chain.
	drill := filepath.Join(t.TempDir(), "drill")
	if err := os.MkdirAll(drill, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stderr = runRestoreCode(t, out, configPath, "--verify", "--into", drill)
	if code != 0 {
		t.Fatalf("live bundle restore --verify exit=%d stderr=%s, want 0", code, stderr)
	}
}

func TestLiveBackupConcurrentWritesStayVerifiable(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	out := filepath.Join(t.TempDir(), "live-concurrent.tar")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		now := time.Now().UTC()
		for i := 0; i < 50; i++ {
			_ = store.AppendAudit(context.Background(), securitystore.AuditEvent{Timestamp: now, Actor: "writer", Action: "concurrent", Outcome: "success", Status: 200})
		}
	}()
	code, stderr := runBackupCode(t, configPath, out, "--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key")
	wg.Wait()
	if code != 0 {
		t.Fatalf("live backup during concurrent writes exit=%d stderr=%s, want 0", code, stderr)
	}
	// The concurrent writes must still be possible afterwards: the
	// refused-or-taken snapshot never wedges the live store.
	if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{Timestamp: time.Now().UTC(), Actor: "after", Action: "still-writable", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("live store must stay writable after a live backup: %v", err)
	}
	drill := filepath.Join(t.TempDir(), "drill")
	if err := os.MkdirAll(drill, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, stderr := runRestoreCode(t, out, configPath, "--verify", "--into", drill); code != 0 {
		t.Fatalf("concurrent live bundle restore --verify exit=%d stderr=%s, want 0", code, stderr)
	}
}

func TestLiveBackupGatewayFailuresAreFinal(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	cases := []struct {
		name, mode, want string
	}{
		{"old gateway has no endpoint", "notfound", "too old"},
		{"wrong admin key", "unauthorized", "admin authentication"},
		{"gateway refuses a broken chain", "broken", "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := liveSnapshotServer(t, store, "test-admin-key", tc.mode, nil)
			defer ts.Close()
			out := filepath.Join(t.TempDir(), "must-not-exist.tar")
			code, stderr := runBackupCode(t, configPath, out, "--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key")
			if code == 0 {
				t.Fatalf("live backup against %s must fail, got exit 0", tc.mode)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("live failure stderr %q must contain %q", stderr, tc.want)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("failed live backup must not leave a bundle at %s", out)
			}
		})
	}
}

func TestLiveBackupVerifiesReceivedSnapshotChainItself(t *testing.T) {
	dir, configPath := opsFixture(t)
	// A tampered copy of the fixture store: the server streams it
	// with 200, so only the CLI's own chain verification can refuse
	// it (the gateway's in-process gate is bypassed by construction
	// here, exactly the case the second check exists for).
	tamperedPath := filepath.Join(t.TempDir(), "security.db")
	raw, err := os.ReadFile(fixtureSecurityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tamperedPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Dir(tamperedPath), 0o700)
	tamperAuditStoreFile(t, tamperedPath)
	tamperedBytes, err := os.ReadFile(tamperedPath)
	if err != nil {
		t.Fatal(err)
	}
	ts := liveSnapshotServer(t, nil, "", "tampered", tamperedBytes)
	defer ts.Close()
	out := filepath.Join(t.TempDir(), "must-not-exist.tar")
	code, stderr := runBackupCode(t, configPath, out, "--live", "--admin-url", ts.URL)
	if code == 0 {
		t.Fatal("live backup of a chain-broken snapshot must fail")
	}
	if !strings.Contains(stderr, "audit chain") {
		t.Fatalf("tampered live snapshot stderr %q must name the audit chain", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("failed live backup must not leave a bundle")
	}
}

func TestStoppedBackupAgainstLiveGatewayFailsWithActionableError(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	before, err := os.ReadFile(fixtureSecurityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "must-not-exist.tar")
	code, stderr := runBackupCode(t, configPath, out)
	if code == 0 {
		t.Fatal("stopped backup against a lock-held store must fail")
	}
	if !strings.Contains(stderr, "locked by another process") || !strings.Contains(stderr, "--live") {
		t.Fatalf("locked backup stderr %q must name the lock and --live", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("failed stopped backup must not leave a bundle")
	}
	after, err := os.ReadFile(fixtureSecurityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused backup must leave the live store byte-identical")
	}
	if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{Timestamp: time.Now().UTC(), Actor: "after", Action: "still-writable", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("live store must stay writable after a refused backup: %v", err)
	}
}

func TestStoppedBackupRefusesWrongStorePermissions(t *testing.T) {
	dir, configPath := opsFixture(t)
	if err := os.Chmod(fixtureSecurityPath(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "must-not-exist.tar")
	code, stderr := runBackupCode(t, configPath, out)
	if code == 0 {
		t.Fatal("backup of a 0644 store must fail")
	}
	if !strings.Contains(stderr, "0600") {
		t.Fatalf("wrong-permission stderr %q must demand 0600", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("failed backup must not leave a bundle")
	}
}

func TestInterruptedBackupLeavesNoPartialBundle(t *testing.T) {
	_, configPath := opsFixture(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(blocker, "b.tar")
	code, _ := runBackupCode(t, configPath, out)
	if code == 0 {
		t.Fatal("backup into a non-directory parent must fail")
	}
	// ENOTDIR (not just ENOENT) is the expected lookup failure here:
	// the point is that no bundle file exists at --out.
	if _, err := os.Lstat(out); err == nil {
		t.Fatal("interrupted backup must not leave a bundle at --out")
	}
}

func TestLiveBundleRestorePreservesDifferentDestinationKey(t *testing.T) {
	dir, configPath := opsFixture(t)
	keyBytes := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(configPath+".key", keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	bundle := filepath.Join(t.TempDir(), "live.tar")
	if code, stderr := runBackupCode(t, configPath, bundle, "--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key"); code != 0 {
		t.Fatalf("live backup exit=%d stderr=%s", code, stderr)
	}
	if got := readBundleManifest(t, bundle).MasterKeySHA256; got != sha256Hex(keyBytes) {
		t.Fatalf("live bundle fingerprint %q, want key fingerprint", got)
	}

	// Destination with a DIFFERENT key: --verify must fail closed on
	// the pairing, and a --force restore of the keyless bundle must
	// leave that key byte-identical (a keyless bundle never carries
	// a key to install).
	destDir := t.TempDir()
	destConfig := filepath.Join(destDir, "config.json")
	cfgBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destConfig, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	otherKey := []byte("fedcba9876543210fedcba9876543210")
	if err := os.WriteFile(destConfig+".key", otherKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := runRestoreCode(t, bundle, destConfig, "--verify"); code == 0 {
		t.Fatal("restore --verify against a different-key destination must fail")
	}
	if got, err := os.ReadFile(destConfig + ".key"); err != nil || !bytes.Equal(got, otherKey) {
		t.Fatalf("failed verify must leave the destination key untouched, got %v %v", got, err)
	}
	if code, stderr := runRestoreCode(t, bundle, destConfig, "--force"); code != 0 {
		t.Fatalf("keyless --force restore exit=%d stderr=%s, want 0", code, stderr)
	}
	if got, err := os.ReadFile(destConfig + ".key"); err != nil || !bytes.Equal(got, otherKey) {
		t.Fatalf("keyless restore must never overwrite a different destination key, got %v %v", got, err)
	}
}

func TestCorruptLiveBundleSecurityEntryFailsClosed(t *testing.T) {
	dir, configPath := opsFixture(t)
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	bundle := filepath.Join(t.TempDir(), "live.tar")
	if code, stderr := runBackupCode(t, configPath, bundle, "--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key"); code != 0 {
		t.Fatalf("live backup exit=%d stderr=%s", code, stderr)
	}
	cfgBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	secBytes := readBundleEntry(t, bundle, backupEntrySecurity)
	// Two effective corruptions (a mid-file flip alone proved
	// ineffective: it can land in free space the chain never reads,
	// and such a bundle legitimately verifies). The manifest hashes
	// are recomputed by writeTestBundle in both cases, so only the
	// store's own integrity can refuse them.
	corruptions := map[string][]byte{}
	// (a) Content tamper: rewrite an audit record below the store,
	// which the chain must detect.
	tamperedPath := filepath.Join(t.TempDir(), "security.db")
	if err := os.WriteFile(tamperedPath, secBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperAuditStoreFile(t, tamperedPath)
	tampered, err := os.ReadFile(tamperedPath)
	if err != nil {
		t.Fatal(err)
	}
	corruptions["content-tampered"] = tampered
	// (b) Header damage: flip a byte in the bbolt meta pages, so the
	// store must fail to open at all. Both metas must be damaged:
	// flipping only meta0 is recoverable by design (bbolt falls back
	// to meta1), and such a bundle legitimately verifies.
	header := bytes.Clone(secBytes)
	pageSize := int(binary.LittleEndian.Uint32(secBytes[8:12]))
	if pageSize >= 512 && pageSize <= 65536 && len(header) > 16+pageSize {
		header[16] ^= 0xff
		header[16+pageSize] ^= 0xff
	} else {
		header = header[:64]
	}
	corruptions["header-damaged"] = header
	for name, corrupt := range corruptions {
		bad := writeTestBundle(t, map[string][]byte{backupEntryConfig: cfgBytes, backupEntrySecurity: corrupt})
		if code, _ := runRestoreCode(t, bad, configPath, "--verify"); code == 0 {
			t.Fatalf("corrupt security entry (%s) must fail restore --verify", name)
		}
		targetDir := t.TempDir()
		targetConfig := filepath.Join(targetDir, "config.json")
		if code, _ := runRestoreCode(t, bad, targetConfig, "--force"); code == 0 {
			t.Fatalf("corrupt security entry (%s) must fail restore install", name)
		}
		if _, err := os.Stat(targetConfig); !os.IsNotExist(err) {
			t.Fatalf("failed corrupt restore (%s) must write nothing to the target", name)
		}
	}
}

// A live-taken bundle must install into a fresh disposable instance
// and reproduce the pairing there: the installed config is the
// bundled config byte for byte, the installed security store reopens
// with a verified chain and the seeded records, the destination key
// (carried separately, as the documented custody model requires) is
// never overwritten by the keyless bundle, and the installed instance
// then verifies against the same bundle.
func TestLiveBundleInstallIntoFreshInstanceReproducesPairing(t *testing.T) {
	dir, configPath := opsFixture(t)
	origConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(configPath+".key", keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	store := openFixtureStore(t, dir)
	ts := liveSnapshotServer(t, store, "test-admin-key", "ok", nil)
	defer ts.Close()
	bundle := filepath.Join(t.TempDir(), "live.tar")
	if code, stderr := runBackupCode(t, configPath, bundle, "--live", "--admin-url", ts.URL, "--admin-key", "test-admin-key"); code != 0 {
		t.Fatalf("live backup exit=%d stderr=%s", code, stderr)
	}
	if got := readBundleManifest(t, bundle).MasterKeySHA256; got != sha256Hex(keyBytes) {
		t.Fatalf("live bundle fingerprint %q, want key fingerprint", got)
	}

	fresh := t.TempDir()
	destConfig := filepath.Join(fresh, "config.json")
	if err := os.WriteFile(destConfig+".key", keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stderr := runRestoreCode(t, bundle, destConfig, "--force"); code != 0 {
		t.Fatalf("live bundle fresh install exit=%d stderr=%s", code, stderr)
	}
	installedConfig, err := os.ReadFile(destConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installedConfig, origConfig) {
		t.Fatal("installed config differs from the backed-up config")
	}
	if got, err := os.ReadFile(destConfig + ".key"); err != nil || !bytes.Equal(got, keyBytes) {
		t.Fatalf("fresh install must leave the destination key untouched, got %v %v", got, err)
	}
	installedStorePath := filepath.Join(fresh, "security", "nexaroute-security.db")
	ro, err := securitystore.OpenReadOnly(installedStorePath)
	if err != nil {
		t.Fatalf("installed security store does not open: %v", err)
	}
	report, err := ro.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("installed store chain not verified: %+v", report)
	}
	records, err := ro.AuditRecords(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 2 {
		t.Fatalf("installed store audit records = %d, want >= 2 seeded records", len(records))
	}
	_ = ro.Close()
	if code, stderr := runRestoreCode(t, bundle, destConfig, "--verify"); code != 0 {
		t.Fatalf("live bundle restore --verify on fresh instance exit=%d stderr=%s", code, stderr)
	}
}

func TestResolveLiveAdminURL(t *testing.T) {
	cases := []struct {
		flag, listen, want string
		wantErr            bool
	}{
		{"https://example.test:8443/", "127.0.0.1:8080", "https://example.test:8443", false},
		{"", "127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"", ":8080", "http://127.0.0.1:8080", false},
		{"", "", "", true},
		{"", "/tmp/gateway.sock", "", true},
	}
	for _, tc := range cases {
		got, err := resolveLiveAdminURL(tc.flag, tc.listen)
		if tc.wantErr && err == nil {
			t.Fatalf("resolveLiveAdminURL(%q, %q) must fail", tc.flag, tc.listen)
		}
		if !tc.wantErr && (err != nil || got != tc.want) {
			t.Fatalf("resolveLiveAdminURL(%q, %q) = %q, %v, want %q", tc.flag, tc.listen, got, err, tc.want)
		}
	}
}

func TestFetchLiveSecuritySnapshotEnforcesEntryLimit(t *testing.T) {
	old := maxBackupEntryBytes
	maxBackupEntryBytes = 16
	defer func() { maxBackupEntryBytes = old }()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64))
	}))
	defer ts.Close()
	if _, err := fetchLiveSecuritySnapshot(ts.URL, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized live snapshot must be refused, got err=%v", err)
	}
}
