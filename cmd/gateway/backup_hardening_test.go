package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// writeTestBundle builds a format-valid bundle by hand (correct
// manifest hashes) around arbitrary payloads, so tests can create
// bundles createBackup would refuse to produce.
func writeTestBundle(t *testing.T, payloads map[string][]byte) string {
	t.Helper()
	manifest := backupManifest{Format: backupFormat, CreatedAt: time.Now().UTC(), GatewayVersion: "test"}
	for _, name := range []string{backupEntryConfig, backupEntrySecurity, backupEntryControl, backupEntryKey} {
		if data, ok := payloads[name]; ok {
			manifest.Files = append(manifest.Files, backupManifestFile{Name: name, SHA256: sha256Hex(data), Size: int64(len(data))})
		}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.tar")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(fh)
	write := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write(backupEntryManifest, manifestBytes)
	for _, f := range manifest.Files {
		write(f.Name, payloads[f.Name])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// rewriteFixtureConfig loads the fixture config, mutates it through
// the typed struct and writes it back.
func rewriteFixtureConfig(t *testing.T, configPath string, mutate func(*config.Config)) {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	mutate(&cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runBackupCode(t *testing.T, configPath, out string, extra ...string) (int, string) {
	t.Helper()
	args := []string{"--config", configPath, "--out", out}
	args = append(args, extra...)
	var stdout, stderr bytes.Buffer
	code := runBackup(args, &stdout, &stderr)
	return code, stderr.String()
}

// --- Fix 1: unreadable sources must fail the backup, not vanish ---

func TestBackupFailsOnUnreadableSources(t *testing.T) {
	t.Run("security store path under a regular file", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.Admin.SecurityStorePath = filepath.Join(blocker, "store.db")
		})
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 1 {
			t.Fatalf("backup exit=%d, want 1 (stderr=%s)", code, stderr)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("no bundle may be written when a source is unreadable")
		}
	})

	t.Run("control-plane state path under a regular file", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
			cfg.ControlPlane.StatePath = filepath.Join(blocker, "cp.json")
		})
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 1 {
			t.Fatalf("backup exit=%d, want 1 (stderr=%s)", code, stderr)
		}
	})

	t.Run("master key path is a directory", func(t *testing.T) {
		_, configPath := opsFixture(t)
		if err := os.Mkdir(configPath+".key", 0o700); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 1 {
			t.Fatalf("backup exit=%d, want 1 (stderr=%s)", code, stderr)
		}
	})

	t.Run("genuinely absent sources still back up", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		data, err := json.MarshalIndent(config.Default(), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 0 {
			t.Fatalf("backup exit=%d, want 0 (stderr=%s)", code, stderr)
		}
		names := tarEntryNames(t, out)
		if len(names) != 2 || !contains(names, backupEntryManifest) || !contains(names, backupEntryConfig) {
			t.Fatalf("entries=%v, want manifest+config only", names)
		}
	})
}

// --- Fix 4: output collisions are refused, sources untouched ---

func TestBackupRefusesOutputCollisions(t *testing.T) {
	setup := func(t *testing.T) (dir, configPath string, sources map[string][]byte) {
		dir, configPath = opsFixture(t)
		if err := os.WriteFile(configPath+".key", []byte("key-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
		})
		if err := os.WriteFile(configPath+".controlplane.json", []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		sources = map[string][]byte{}
		for _, p := range []string{
			configPath, configPath + ".key", configPath + ".controlplane.json",
			filepath.Join(dir, "security", "nexaroute-security.db"),
		} {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			sources[p] = b
		}
		return dir, configPath, sources
	}
	assertUnchanged := func(t *testing.T, sources map[string][]byte) {
		t.Helper()
		for p, want := range sources {
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("source %s unreadable after refused backup: %v", p, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("source %s was modified by a refused backup", p)
			}
		}
	}

	t.Run("exact source paths", func(t *testing.T) {
		_, configPath, sources := setup(t)
		for _, out := range []string{
			configPath, configPath + ".key", configPath + ".controlplane.json",
			filepath.Join(filepath.Dir(configPath), "security", "nexaroute-security.db"),
		} {
			if code, stderr := runBackupCode(t, configPath, out); code != 1 {
				t.Fatalf("out=%s backup exit=%d, want 1 (stderr=%s)", out, code, stderr)
			}
		}
		assertUnchanged(t, sources)
	})

	t.Run("symlink to the config", func(t *testing.T) {
		dir, configPath, sources := setup(t)
		link := filepath.Join(dir, "link.tar")
		if err := os.Symlink(configPath, link); err != nil {
			t.Fatal(err)
		}
		if code, _ := runBackupCode(t, configPath, link); code != 1 {
			t.Fatalf("symlink collision exit=%d, want 1", code)
		}
		assertUnchanged(t, sources)
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the symlink itself must survive a refused backup")
		}
	})

	t.Run("hard link to the config", func(t *testing.T) {
		dir, configPath, sources := setup(t)
		hard := filepath.Join(dir, "hard.tar")
		if err := os.Link(configPath, hard); err != nil {
			t.Fatal(err)
		}
		if code, _ := runBackupCode(t, configPath, hard); code != 1 {
			t.Fatalf("hardlink collision exit=%d, want 1", code)
		}
		assertUnchanged(t, sources)
	})

	t.Run("dotdot spelling of the config path", func(t *testing.T) {
		dir, configPath, sources := setup(t)
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "sub", "..", "config.json")
		if code, _ := runBackupCode(t, configPath, out); code != 1 {
			t.Fatalf("dotdot collision exit=%d, want 1", code)
		}
		assertUnchanged(t, sources)
	})
}

// --- Fix 2: restore rolls back on install failure, at every stage ---

func TestRestoreRollsBackOnInstallFailure(t *testing.T) {
	_, configPath := opsFixture(t)
	if err := os.WriteFile(configPath+".key", []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle, "--include-master-key"); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}

	// Install order is config, security store, master key. Inject a
	// failure at each stage in turn and prove the target ends up
	// EXACTLY as it was before the restore.
	for _, failAt := range []int{1, 2, 3} {
		t.Run("fail at install step", func(t *testing.T) {
			targetDir := t.TempDir()
			targetConfig := filepath.Join(targetDir, "config.json")
			preConfig := []byte("PRE-EXISTING CONFIG")
			preDB := []byte("PRE-EXISTING DB")
			if err := os.WriteFile(targetConfig, preConfig, 0o600); err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(targetDir, "security", "nexaroute-security.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dbPath, preDB, 0o600); err != nil {
				t.Fatal(err)
			}

			calls := 0
			original := restoreWriteFile
			restoreWriteFile = func(path string, data []byte, perm os.FileMode) error {
				calls++
				if calls == failAt {
					return errors.New("injected install failure")
				}
				return original(path, data, perm)
			}
			defer func() { restoreWriteFile = original }()

			var stdout, stderr bytes.Buffer
			code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("failAt=%d restore exit=%d, want 1 (stderr=%s)", failAt, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "rolled back") {
				t.Fatalf("failAt=%d: error must report the rollback: %s", failAt, stderr.String())
			}
			if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, preConfig) {
				t.Fatalf("failAt=%d: config not rolled back: %q", failAt, got)
			}
			if got, _ := os.ReadFile(dbPath); !bytes.Equal(got, preDB) {
				t.Fatalf("failAt=%d: security store not rolled back: %q", failAt, got)
			}
			if _, err := os.Stat(targetConfig + ".key"); !os.IsNotExist(err) {
				t.Fatalf("failAt=%d: key file must not exist after rollback (err=%v)", failAt, err)
			}
			entries, err := os.ReadDir(targetDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".nexaroute-") {
					t.Fatalf("failAt=%d: leftover temp file %s in target dir", failAt, e.Name())
				}
			}
		})
	}
}

// --- Fix 3: semantic validation cannot be bypassed; keyless path is narrow ---

func TestRestoreRejectsSemanticallyInvalidConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.MaxAttempts = 999999 // valid JSON, fails application validation
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bundle := writeTestBundle(t, map[string][]byte{backupEntryConfig: data})
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer
	if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 1 {
		t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if _, err := os.Stat(targetConfig); !os.IsNotExist(err) {
		t.Fatal("a semantically invalid config must never be installed")
	}
}

func TestRestoreKeylessEncryptedConfig(t *testing.T) {
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	// Give the config a real secret value so encryption applies.
	plainCfg := config.Default()
	plainCfg.Providers = append(plainCfg.Providers, config.ProviderConfig{
		ID: "p1", Type: "openai_compatible", BaseURL: "https://example.invalid/v1",
		APIKey: "sk-test-secret-value", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true}},
	})
	plain, err := json.Marshal(plainCfg)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, _, err := secrets.TransformConfig(plain, key, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encrypted, []byte(secrets.Prefix)) {
		t.Fatal("fixture config was not encrypted")
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath+".key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}

	// Keyless restore onto a fresh target: the key neither travelled
	// with the bundle nor exists in the environment — the identified
	// missing-key path installs with a warning.
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer
	if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 0 {
		t.Fatalf("keyless restore exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "keyless restore") {
		t.Fatalf("keyless restore must warn loudly:\n%s", stdout.String())
	}
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, encrypted) {
		t.Fatal("restored encrypted config differs from the bundled bytes")
	}

	// Same bundle, but a (wrong) key IS available from the
	// environment: the loader failure is then a decryption failure,
	// not an identified missing key — restore must refuse and write
	// nothing.
	wrong := bytes.Repeat([]byte{0x99}, 32)
	t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(wrong))
	target2 := filepath.Join(t.TempDir(), "config.json")
	stdout.Reset()
	stderr.Reset()
	if code := runRestore([]string{"--bundle", bundle, "--config", target2}, &stdout, &stderr); code != 1 {
		t.Fatalf("wrong-env-key restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if _, err := os.Stat(target2); !os.IsNotExist(err) {
		t.Fatal("nothing may be written when an available key fails to decrypt")
	}
}

// --- Round 3 Fix 1: restore target collisions + inconsistent bundles ---

func bundleConfigBytes(t *testing.T, mutate func(*config.Config)) []byte {
	t.Helper()
	cfg := config.Default()
	mutate(&cfg)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRestoreRejectsCollidingTargets(t *testing.T) {
	assertRefusedAndUnchanged := func(t *testing.T, bundle string, targetConfig string, sentinels map[string][]byte) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr); code != 1 {
			t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "collide") {
			t.Fatalf("refusal must name the collision: %s", stderr.String())
		}
		for path, want := range sentinels {
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("target %s changed by a refused restore (err=%v)", path, err)
			}
		}
	}

	t.Run("security store path resolves onto the config", func(t *testing.T) {
		cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
			cfg.Admin.SecurityStorePath = "config.json"
		})
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig: cfgBytes, backupEntrySecurity: []byte("db-bytes"),
		})
		targetDir := t.TempDir()
		targetConfig := filepath.Join(targetDir, "config.json")
		sentinel := []byte("SENTINEL CONFIG")
		if err := os.WriteFile(targetConfig, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefusedAndUnchanged(t, bundle, targetConfig, map[string][]byte{targetConfig: sentinel})
	})

	t.Run("control-plane state path resolves onto the config", func(t *testing.T) {
		cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
			cfg.ControlPlane.StatePath = "config.json"
		})
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig: cfgBytes, backupEntryControl: []byte("{}"),
		})
		targetDir := t.TempDir()
		targetConfig := filepath.Join(targetDir, "config.json")
		sentinel := []byte("SENTINEL CONFIG")
		if err := os.WriteFile(targetConfig, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefusedAndUnchanged(t, bundle, targetConfig, map[string][]byte{targetConfig: sentinel})
	})

	t.Run("security store path resolves onto the master key", func(t *testing.T) {
		cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
			cfg.Admin.SecurityStorePath = "config.json.key"
		})
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig: cfgBytes, backupEntrySecurity: []byte("db-bytes"),
			backupEntryKey: []byte("key-bytes"),
		})
		targetDir := t.TempDir()
		targetConfig := filepath.Join(targetDir, "config.json")
		sentinel := []byte("SENTINEL CONFIG")
		if err := os.WriteFile(targetConfig, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefusedAndUnchanged(t, bundle, targetConfig, map[string][]byte{targetConfig: sentinel})
		if _, err := os.Stat(targetConfig + ".key"); !os.IsNotExist(err) {
			t.Fatal("no key file may be installed by a refused restore")
		}
	})

	t.Run("security target is a symlink to the config", func(t *testing.T) {
		cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {})
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig: cfgBytes, backupEntrySecurity: []byte("db-bytes"),
		})
		targetDir := t.TempDir()
		targetConfig := filepath.Join(targetDir, "config.json")
		sentinel := []byte("SENTINEL CONFIG")
		if err := os.WriteFile(targetConfig, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		secDir := filepath.Join(targetDir, "security")
		if err := os.MkdirAll(secDir, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(secDir, "nexaroute-security.db")
		if err := os.Symlink(targetConfig, link); err != nil {
			t.Fatal(err)
		}
		assertRefusedAndUnchanged(t, bundle, targetConfig, map[string][]byte{targetConfig: sentinel})
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the target symlink must survive a refused restore")
		}
	})

	t.Run("security payload with a non-bbolt bundled backend", func(t *testing.T) {
		cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
			cfg.Admin.SecurityStoreBackend = "redis"
		})
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig: cfgBytes, backupEntrySecurity: []byte("db-bytes"),
		})
		targetConfig := filepath.Join(t.TempDir(), "config.json")
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 1 {
			t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if _, err := os.Stat(targetConfig); !os.IsNotExist(err) {
			t.Fatal("an inconsistent bundle must not install anything")
		}
	})

	t.Run("control payload without an enabled file backend", func(t *testing.T) {
		cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {})
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig: cfgBytes, backupEntryControl: []byte("{}"),
		})
		targetConfig := filepath.Join(t.TempDir(), "config.json")
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 1 {
			t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if _, err := os.Stat(targetConfig); !os.IsNotExist(err) {
			t.Fatal("an inconsistent bundle must not install anything")
		}
	})
}

// --- Round 3 Fix 2: journal survives an incomplete rollback ---

func TestRestorePreservesJournalOnIncompleteRollback(t *testing.T) {
	_, configPath := opsFixture(t)
	if err := os.WriteFile(configPath+".key", []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle, "--include-master-key"); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}

	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	preConfig := []byte("PRE-EXISTING CONFIG")
	preDB := []byte("PRE-EXISTING DB")
	if err := os.WriteFile(targetConfig, preConfig, 0o640); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(targetDir, "security", "nexaroute-security.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, preDB, 0o600); err != nil {
		t.Fatal(err)
	}

	// Installs: config, security, key. Fail the KEY install (stage 3)
	// so both earlier files need rollback; then fail the rollback of
	// the security store specifically. Config rollback still succeeds.
	installCalls := 0
	origInstall := restoreWriteFile
	restoreWriteFile = func(path string, data []byte, perm os.FileMode) error {
		installCalls++
		if installCalls == 3 {
			return errors.New("injected install failure at key stage")
		}
		return origInstall(path, data, perm)
	}
	defer func() { restoreWriteFile = origInstall }()
	origRollback := restoreRollbackWriteFile
	restoreRollbackWriteFile = func(path string, data []byte, perm os.FileMode) error {
		if path == dbPath {
			return errors.New("injected rollback failure")
		}
		return origRollback(path, data, perm)
	}
	defer func() { restoreRollbackWriteFile = origRollback }()

	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "rollback INCOMPLETE") {
		t.Fatalf("error must report the incomplete rollback: %s", errText)
	}
	// Extract and verify the preserved journal.
	const marker = "restore journal at "
	idx := strings.Index(errText, marker)
	if idx < 0 {
		t.Fatalf("error must report the journal path: %s", errText)
	}
	journalPath := strings.Fields(errText[idx+len(marker):])[0]
	journalPath = strings.TrimRight(journalPath, "—")
	defer func() { _ = os.RemoveAll(journalPath) }()
	info, err := os.Stat(journalPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("journal must survive an incomplete rollback: stat err=%v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("journal dir mode=%04o, want 0700", info.Mode().Perm())
	}
	entries, err := os.ReadDir(journalPath)
	if err != nil || len(entries) == 0 {
		t.Fatalf("journal must hold recovery copies: entries=%v err=%v", entries, err)
	}
	foundDB := false
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(journalPath, e.Name()))
		if err == nil && bytes.Equal(data, preDB) {
			foundDB = true
		}
		if err == nil && bytes.Equal(data, preConfig) {
			// present as well; fine either way
		}
	}
	if !foundDB {
		t.Fatal("journal must preserve the pre-restore security store bytes")
	}
	// The config WAS rolled back (its rollback step succeeded), with
	// its original 0640 mode preserved by the journal metadata.
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, preConfig) {
		t.Fatal("config target must be rolled back to its pre-restore bytes")
	}
	if info, err := os.Stat(targetConfig); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("rolled-back config mode=%v err=%v, want 0640 preserved", info.Mode().Perm(), err)
	}
	// The security store still holds the BUNDLED bytes (install
	// succeeded, rollback failed) — exactly the mixed state the error
	// reports, recoverable from the preserved journal.
	bundleDB, _ := os.ReadFile(dbPath)
	if bytes.Equal(bundleDB, preDB) {
		t.Fatal("security target should still hold installed bytes after its rollback failed")
	}
}

// --- Round 3 Fix 3: doctor absence vs real errors ---

func TestDoctorFailsOnInaccessiblePaths(t *testing.T) {
	t.Run("security store path under a regular file", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.Admin.SecurityStorePath = filepath.Join(blocker, "store.db")
		})
		var stdout, stderr bytes.Buffer
		if code := runDoctor([]string{"--config", configPath}, &stdout, &stderr); code != 1 {
			t.Fatalf("doctor exit=%d, want 1\n%s", code, stdout.String())
		}
		if !strings.Contains(stdout.String(), "FAIL security_store:") {
			t.Fatalf("doctor must FAIL the security store check:\n%s", stdout.String())
		}
	})

	t.Run("control-plane state path under a regular file", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
			cfg.ControlPlane.StatePath = filepath.Join(blocker, "cp.json")
		})
		var stdout, stderr bytes.Buffer
		if code := runDoctor([]string{"--config", configPath}, &stdout, &stderr); code != 1 {
			t.Fatalf("doctor exit=%d, want 1\n%s", code, stdout.String())
		}
		if !strings.Contains(stdout.String(), "FAIL control_plane:") {
			t.Fatalf("doctor must FAIL the control plane check:\n%s", stdout.String())
		}
	})

	t.Run("control-plane state path is a directory", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		stateDir := filepath.Join(dir, "cpdir")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
			cfg.ControlPlane.StatePath = stateDir
		})
		var stdout, stderr bytes.Buffer
		if code := runDoctor([]string{"--config", configPath}, &stdout, &stderr); code != 1 {
			t.Fatalf("doctor exit=%d, want 1\n%s", code, stdout.String())
		}
		if !strings.Contains(stdout.String(), "FAIL control_plane:") {
			t.Fatalf("doctor must FAIL the control plane check:\n%s", stdout.String())
		}
	})

	t.Run("absent state still warns without failing", func(t *testing.T) {
		_, configPath := opsFixture(t)
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
			cfg.ControlPlane.StatePath = "never-created.json"
		})
		var stdout, stderr bytes.Buffer
		if code := runDoctor([]string{"--config", configPath}, &stdout, &stderr); code != 0 {
			t.Fatalf("doctor exit=%d, want 0 (absence is a WARN)\n%s", code, stdout.String())
		}
		if !strings.Contains(stdout.String(), "WARN control_plane:") {
			t.Fatalf("absent state must stay a WARN:\n%s", stdout.String())
		}
	})
}

// --- Round 3 Fix 4: backup creation enforces the bundle limits ---

func TestBackupEnforcesLimitsDuringCreation(t *testing.T) {
	setLimits := func(entry, total int64) {
		oldEntry, oldTotal := maxBackupEntryBytes, maxBackupTotalBytes
		maxBackupEntryBytes, maxBackupTotalBytes = entry, total
		t.Cleanup(func() { maxBackupEntryBytes, maxBackupTotalBytes = oldEntry, oldTotal })
	}

	t.Run("entry overflow", func(t *testing.T) {
		_, configPath := opsFixture(t)
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatal(err)
		}
		// Above the config (first entry stages fine) but below the
		// security snapshot: creation must fail on a later entry.
		setLimits(info.Size()+128, 1<<40)
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 1 {
			t.Fatalf("backup exit=%d, want 1 (stderr=%s)", code, stderr)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("no bundle may be produced when an entry overflows")
		}
	})

	t.Run("aggregate overflow", func(t *testing.T) {
		_, configPath := opsFixture(t)
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatal(err)
		}
		setLimits(1<<40, info.Size()+16)
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 1 {
			t.Fatalf("backup exit=%d, want 1 (stderr=%s)", code, stderr)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("no bundle may be produced when the aggregate overflows")
		}
	})
}

// --- Round 4 Blocker A: symlinked ancestor + nonexistent descendants ---
//
// canonicalPath previously resolved only the immediate parent of a
// missing path. With an existing symlinked ancestor and one or more
// nonexistent deeper components, two spellings of the SAME eventual
// file produced different "canonical" strings and the collision
// check passed — the second payload then overwrote the first.

func TestCanonicalPathResolvesMissingDescendants(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	pairs := [][2]string{
		{filepath.Join(link, "new", "config.json"), filepath.Join(real, "new", "config.json")},
		{filepath.Join(link, "a", "b", "config.json"), filepath.Join(real, "a", "b", "config.json")},
		{filepath.Join(link, "config.json"), filepath.Join(real, "config.json")},
	}
	for _, pair := range pairs {
		a, err := canonicalPath(pair[0])
		if err != nil {
			t.Fatalf("canonicalPath(%s): %v", pair[0], err)
		}
		b, err := canonicalPath(pair[1])
		if err != nil {
			t.Fatalf("canonicalPath(%s): %v", pair[1], err)
		}
		if a != b {
			t.Fatalf("aliases canonicalize differently:\n %s -> %s\n %s -> %s", pair[0], a, pair[1], b)
		}
	}
	// A dangling symlink destination resolves through its target.
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(real, "future.json"), dangling); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalPath(dangling)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "future.json"); got != want {
		t.Fatalf("dangling symlink canonical=%s, want %s", got, want)
	}
	// A path under a regular file is an error, never a silent
	// "canonical" fallback.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalPath(filepath.Join(blocker, "child.json")); err == nil {
		t.Fatal("canonicalPath under a regular file must error, not fall back")
	}
}

func TestRestoreRejectsSymlinkedAncestorAlias(t *testing.T) {
	for _, deep := range []string{"new", filepath.Join("a", "b")} {
		t.Run(deep, func(t *testing.T) {
			root := t.TempDir()
			real := filepath.Join(root, "real")
			if err := os.MkdirAll(real, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			viaLink := filepath.Join(link, deep, "config.json")
			viaReal := filepath.Join(real, deep, "config.json")
			cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
				cfg.Admin.SecurityStorePath = viaLink
			})
			bundle := writeTestBundle(t, map[string][]byte{
				backupEntryConfig: cfgBytes, backupEntrySecurity: []byte("db-bytes"),
			})
			var stdout, stderr bytes.Buffer
			code := runRestore([]string{"--bundle", bundle, "--config", viaReal}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("restore exit=%d, want 1 (collision) stderr=%s stdout=%s", code, stderr.String(), stdout.String())
			}
			if !strings.Contains(stderr.String(), "collide") {
				t.Fatalf("refusal must name the collision: %s", stderr.String())
			}
			if _, err := os.Stat(viaReal); !os.IsNotExist(err) {
				t.Fatal("no restore target may be created by a refused restore")
			}
			if _, err := os.Stat(filepath.Join(real, deep)); !os.IsNotExist(err) {
				t.Fatal("no destination directory may be created by a refused restore")
			}
		})
	}
}

// --- Round 4 Blocker B: crash recovery from the durable journal ---
//
// Helper-process pattern: the test binary re-executes itself with
// NR_CRASH_HELPER=1; the helper runs a real restore whose install
// seam aborts the process with os.Exit AFTER the Nth install — no
// defers run, the in-process rollback never fires, exactly like a
// kill or crash. The parent then recovers from the journal the dead
// process left behind and verifies every destination's prior state.

func TestRestoreCrashRecovery(t *testing.T) {
	if os.Getenv("NR_CRASH_HELPER") == "1" {
		after, _ := strconv.Atoi(os.Getenv("NR_CRASH_AFTER"))
		calls := 0
		orig := restoreWriteFile
		restoreWriteFile = func(path string, data []byte, perm os.FileMode) error {
			err := orig(path, data, perm)
			if err == nil {
				calls++
				if calls == after {
					os.Exit(42) // abrupt termination mid-transaction
				}
			}
			return err
		}
		var stdout, stderr bytes.Buffer
		code := runRestore([]string{"--bundle", os.Getenv("NR_CRASH_BUNDLE"), "--config", os.Getenv("NR_CRASH_CONFIG"), "--force"}, &stdout, &stderr)
		os.Exit(code)
	}

	// Source bundle: config + security store + master key.
	_, configPath := opsFixture(t)
	if err := os.WriteFile(configPath+".key", []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle, "--include-master-key"); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	bundleConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// Targets with three different prior states: an existing config
	// (mode 0640), an existing SYMLINK as the security store, and an
	// absent master key.
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	preConfig := []byte("PRE-CRASH CONFIG")
	if err := os.WriteFile(targetConfig, preConfig, 0o640); err != nil {
		t.Fatal(err)
	}
	pointee := filepath.Join(targetDir, "pointee.db")
	pointeeBytes := []byte("POINTEE BYTES - NOT PART OF THE RESTORE")
	if err := os.WriteFile(pointee, pointeeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	secDir := filepath.Join(targetDir, "security")
	if err := os.MkdirAll(secDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(secDir, "nexaroute-security.db")
	if err := os.Symlink(pointee, dbPath); err != nil {
		t.Fatal(err)
	}

	// Crash after the 2nd install (config + security store landed;
	// the key install never runs).
	cmd := exec.Command(os.Args[0], "-test.run", "^TestRestoreCrashRecovery$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"NR_CRASH_HELPER=1", "NR_CRASH_AFTER=2",
		"NR_CRASH_BUNDLE="+bundle, "NR_CRASH_CONFIG="+targetConfig,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("helper must die with code 42 (crash simulation); err=%v output=%s", err, out)
	}

	// Post-crash state: config replaced, symlink replaced by a
	// regular file, key still absent, journal left behind.
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, bundleConfig) {
		t.Fatal("post-crash config must hold the installed bundle bytes")
	}
	if info, err := os.Lstat(dbPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("post-crash security target must be a regular file (symlink replaced); err=%v", err)
	}

	var journalDir string
	for _, dir := range findInterruptedRestoreJournals() {
		if m, err := readJournalManifest(dir); err == nil && len(m.Entries) > 0 && strings.HasPrefix(m.Entries[0].Target, targetDir) {
			journalDir = dir
			break
		}
	}
	if journalDir == "" {
		t.Fatal("the crashed restore must leave a discoverable interrupted journal")
	}

	// Recover in a fresh invocation and verify every prior state.
	var stdout, stderr bytes.Buffer
	if code := runRestore([]string{"--recover", journalDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("recover exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, preConfig) {
		t.Fatal("recovered config must hold its pre-restore bytes")
	}
	if info, err := os.Stat(targetConfig); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("recovered config mode must be the journaled 0640; err=%v", err)
	}
	if info, err := os.Lstat(dbPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("recovered security target must be a symlink again")
	}
	if target, err := os.Readlink(dbPath); err != nil || target != pointee {
		t.Fatalf("recovered symlink target=%q err=%v, want %q", target, err, pointee)
	}
	if got, _ := os.ReadFile(pointee); !bytes.Equal(got, pointeeBytes) {
		t.Fatal("the symlink's pointee must never be touched")
	}
	if _, err := os.Stat(targetConfig + ".key"); !os.IsNotExist(err) {
		t.Fatal("a destination absent before the transaction must be absent after recovery")
	}
	if _, err := os.Stat(journalDir); !os.IsNotExist(err) {
		t.Fatal("a fully recovered journal must be removed")
	}
}

func TestRestoreRecoverGuards(t *testing.T) {
	writeJournal := func(t *testing.T, m *restoreJournalManifest) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "nexaroute-restore-journal-test")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJournalManifest(dir, m); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("completed journal is cleaned, never rolled back", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "config.json")
		sentinelBytes := []byte("COMPLETED TX STATE - MUST SURVIVE")
		if err := os.WriteFile(sentinel, sentinelBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		dir := writeJournal(t, &restoreJournalManifest{
			Format: restoreJournalFormat, ID: "d00d1e7a", State: "completed",
			Entries: []restoreJournalEntry{{Name: backupEntryConfig, Target: sentinel, Existed: true, Mode: 0o600, Saved: "entry-0", SavedSHA256: sha256Hex([]byte("STALE PRE-STATE")), Installed: true}},
		})
		if err := os.WriteFile(filepath.Join(dir, "entry-0"), []byte("STALE PRE-STATE"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--recover", dir}, &stdout, &stderr); code != 0 {
			t.Fatalf("recover(completed) exit=%d, want 0 (stderr=%s)", code, stderr.String())
		}
		if got, _ := os.ReadFile(sentinel); !bytes.Equal(got, sentinelBytes) {
			t.Fatal("a completed transaction must never be rolled back")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("a completed journal must be cleaned up")
		}
	})

	t.Run("unknown journal format is refused and preserved", func(t *testing.T) {
		dir := writeJournal(t, &restoreJournalManifest{
			Format: restoreJournalFormat, ID: "x", State: "installing",
		})
		// Corrupt the format field to an unknown future version.
		manifestPath := filepath.Join(dir, "manifest.json")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.Replace(raw, []byte(restoreJournalFormat), []byte("nexaroute-restore-journal/99"), 1)
		if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--recover", dir}, &stdout, &stderr); code != 1 {
			t.Fatalf("recover(unknown format) exit=%d, want 1", code)
		}
		if _, err := os.Stat(manifestPath); err != nil {
			t.Fatal("an unknown-format journal must be preserved, never replayed or deleted")
		}
	})
}

// --- Round 5 Finding 1: journal preparation durability failures ---

func journalDirsInTemp(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "nexaroute-restore-journal-") {
			out[e.Name()] = true
		}
	}
	return out
}

// A failed saved-copy write/sync must abort BEFORE any destination is
// modified, and the unusable journal must be cleaned up.
func TestRestoreAbortsWhenJournalCopyFails(t *testing.T) {
	_, configPath := opsFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	pre := []byte("PRE-EXISTING - MUST SURVIVE")
	if err := os.WriteFile(targetConfig, pre, 0o600); err != nil {
		t.Fatal(err)
	}

	orig := writeJournalSavedCopy
	writeJournalSavedCopy = func(string, []byte) error { return errors.New("injected saved-copy write failure") }
	defer func() { writeJournalSavedCopy = orig }()

	before := journalDirsInTemp(t)
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, pre) {
		t.Fatal("no destination may be modified when journal preparation fails")
	}
	for name := range journalDirsInTemp(t) {
		if !before[name] {
			t.Fatalf("failed journal preparation must clean up its journal; leftover: %s", name)
		}
	}
}

// If every install landed but the "completed" state cannot be
// persisted, restore must NOT report success and must NOT delete the
// journal: it stays "installing" on disk (recoverable = rollback).
func TestRestoreCompletedPersistFailure(t *testing.T) {
	_, configPath := opsFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	bundleConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	pre := []byte("PRE-RESTORE BYTES")
	if err := os.WriteFile(targetConfig, pre, 0o600); err != nil {
		t.Fatal(err)
	}

	orig := persistJournalManifest
	persistJournalManifest = func(dir string, m *restoreJournalManifest) error {
		if m.State == "completed" {
			return errors.New("injected completion persistence failure")
		}
		return orig(dir, m)
	}
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr)
	persistJournalManifest = orig

	if code != 1 {
		t.Fatalf("restore exit=%d, want 1 when completion cannot be persisted (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not be marked completed") {
		t.Fatalf("stderr must explain the ambiguous completion, got: %s", stderr.String())
	}
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, bundleConfig) {
		t.Fatal("installs DID land; the destination must hold the restored bytes")
	}
	// Locate the preserved journal and check its on-disk state.
	var journalDir string
	for _, dir := range findInterruptedRestoreJournals() {
		if m, err := readJournalManifest(dir); err == nil && len(m.Entries) > 0 && m.Entries[0].Target == targetConfig {
			journalDir = dir
			break
		}
	}
	if journalDir == "" {
		t.Fatal("the journal must be preserved when completion cannot be persisted")
	}
	m, err := readJournalManifest(journalDir)
	if err != nil || m.State != "installing" {
		t.Fatalf("on-disk journal state=%q err=%v, want \"installing\"", m.State, err)
	}
	// The operator's documented option: roll the transaction back.
	stdout.Reset()
	stderr.Reset()
	if code := runRestore([]string{"--recover", journalDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("recover exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, pre) {
		t.Fatal("recovery after a failed completion must restore the pre-restore bytes")
	}
}

// --- Round 5 Finding 2: strict journal validation (adversarial) ---

func fabricateJournal(t *testing.T, m *restoreJournalManifest, saved map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "nexaroute-restore-journal-fab")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range saved {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJournalManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	return dir
}

func validInstallingJournal(target string, pre []byte) (*restoreJournalManifest, map[string][]byte) {
	return &restoreJournalManifest{
		Format: restoreJournalFormat, ID: "abcdef0123456789", State: "installing",
		Entries: []restoreJournalEntry{{Name: backupEntryConfig, Target: target, Existed: true, Mode: 0o600, Saved: "entry-0", SavedSHA256: sha256Hex(pre), Installed: true}},
	}, map[string][]byte{"entry-0": pre}
}

func TestRestoreRecoverRejectsInvalidJournals(t *testing.T) {
	sentinel := []byte("SENTINEL DESTINATION - MUST NOT CHANGE")
	pre := []byte("journaled pre-state")

	setup := func(t *testing.T) (target string) {
		target = filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(target, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		return target
	}
	assertRejected := func(t *testing.T, dir, target string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--recover", dir}, &stdout, &stderr); code != 1 {
			t.Fatalf("recover exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if got, _ := os.ReadFile(target); !bytes.Equal(got, sentinel) {
			t.Fatal("a rejected journal must not modify any destination")
		}
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
			t.Fatal("a rejected journal must be preserved")
		}
	}

	t.Run("unknown state", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		m.State = "bogus"
		assertRejected(t, fabricateJournal(t, m, saved), target)
	})
	t.Run("empty state", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		m.State = ""
		assertRejected(t, fabricateJournal(t, m, saved), target)
	})
	t.Run("malformed manifest JSON", func(t *testing.T) {
		target := setup(t)
		dir := filepath.Join(t.TempDir(), "nexaroute-restore-journal-fab")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertRejected(t, dir, target)
	})
	t.Run("missing transaction ID", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		m.ID = ""
		assertRejected(t, fabricateJournal(t, m, saved), target)
	})
	t.Run("zero entries", func(t *testing.T) {
		target := setup(t)
		m, _ := validInstallingJournal(target, pre)
		m.Entries = nil
		assertRejected(t, fabricateJournal(t, m, nil), target)
	})
	t.Run("saved name traversal", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		m.Entries[0].Saved = "../escape"
		assertRejected(t, fabricateJournal(t, m, saved), target)
	})
	t.Run("saved name absolute", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		m.Entries[0].Saved = "/tmp/entry-0"
		assertRejected(t, fabricateJournal(t, m, saved), target)
	})
	t.Run("saved copy is a symlink", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		dir := fabricateJournal(t, m, saved)
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("VICTIM"), 0o600); err != nil {
			t.Fatal(err)
		}
		savedPath := filepath.Join(dir, "entry-0")
		if err := os.Remove(savedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, savedPath); err != nil {
			t.Fatal(err)
		}
		assertRejected(t, dir, target)
		if got, _ := os.ReadFile(victim); string(got) != "VICTIM" {
			t.Fatal("the symlinked victim must be untouched")
		}
	})
	t.Run("saved copy is a directory", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		dir := fabricateJournal(t, m, saved)
		savedPath := filepath.Join(dir, "entry-0")
		if err := os.Remove(savedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(savedPath, 0o700); err != nil {
			t.Fatal(err)
		}
		assertRejected(t, dir, target)
	})
	t.Run("duplicate targets", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		m.Entries = append(m.Entries, restoreJournalEntry{Name: backupEntrySecurity, Target: target, Existed: true, Mode: 0o600, Saved: "entry-1", SavedSHA256: sha256Hex(pre)})
		saved["entry-1"] = pre
		assertRejected(t, fabricateJournal(t, m, saved), target)
	})
	t.Run("aliased targets via symlink", func(t *testing.T) {
		target := setup(t)
		alias := filepath.Join(t.TempDir(), "alias.json")
		if err := os.Symlink(target, alias); err != nil {
			t.Fatal(err)
		}
		m, saved := validInstallingJournal(target, pre)
		m.Entries = append(m.Entries, restoreJournalEntry{Name: backupEntrySecurity, Target: alias, Existed: true, Mode: 0o600, Saved: "entry-1", SavedSHA256: sha256Hex(pre)})
		saved["entry-1"] = pre
		assertRejected(t, fabricateJournal(t, m, saved), target)
		if link, err := os.Lstat(alias); err != nil || link.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the alias symlink itself must be untouched")
		}
	})
	t.Run("missing saved copy", func(t *testing.T) {
		target := setup(t)
		m, _ := validInstallingJournal(target, pre)
		assertRejected(t, fabricateJournal(t, m, nil), target)
	})
	t.Run("tampered saved copy fails checksum", func(t *testing.T) {
		target := setup(t)
		m, _ := validInstallingJournal(target, pre)
		assertRejected(t, fabricateJournal(t, m, map[string][]byte{"entry-0": []byte("TAMPERED BYTES")}), target)
	})
	t.Run("absent entry with saved metadata", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		m.Entries[0].Existed = false // contradicts Saved/SavedSHA256/Mode
		assertRejected(t, fabricateJournal(t, m, saved), target)
	})
	t.Run("journal dir permissions too broad", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		dir := fabricateJournal(t, m, saved)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		assertRejected(t, dir, target)
	})
	t.Run("journal dir passed through a symlink", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		realDir := fabricateJournal(t, m, saved)
		link := filepath.Join(t.TempDir(), "journal-link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--recover", link}, &stdout, &stderr); code != 1 {
			t.Fatalf("recover via symlinked journal dir exit=%d, want 1", code)
		}
		if got, _ := os.ReadFile(target); !bytes.Equal(got, sentinel) {
			t.Fatal("destination must be unchanged")
		}
		if _, err := os.Stat(filepath.Join(realDir, "manifest.json")); err != nil {
			t.Fatal("the real journal must be preserved")
		}
	})
	t.Run("manifest is a symlink", func(t *testing.T) {
		target := setup(t)
		m, saved := validInstallingJournal(target, pre)
		dir := fabricateJournal(t, m, saved)
		manPath := filepath.Join(dir, "manifest.json")
		raw, err := os.ReadFile(manPath)
		if err != nil {
			t.Fatal(err)
		}
		realMan := filepath.Join(t.TempDir(), "real-manifest.json")
		if err := os.WriteFile(realMan, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(manPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realMan, manPath); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--recover", dir}, &stdout, &stderr); code != 1 {
			t.Fatalf("recover with symlinked manifest exit=%d, want 1", code)
		}
		if got, _ := os.ReadFile(target); !bytes.Equal(got, sentinel) {
			t.Fatal("destination must be unchanged")
		}
		if _, err := os.Lstat(manPath); err != nil {
			t.Fatal("the journal must be preserved")
		}
	})
}

// Recovery that fails midway must preserve the journal, name the
// failed destination, and still recover the others.
func TestRestoreRecoverFailurePreservesJournal(t *testing.T) {
	dir1 := t.TempDir()
	targetA := filepath.Join(dir1, "config.json")
	targetB := filepath.Join(dir1, "security.db")
	preA, preB := []byte("PRE A"), []byte("PRE B")
	for _, target := range []string{targetA, targetB} {
		if err := os.WriteFile(target, []byte("INSTALLED STATE"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := &restoreJournalManifest{
		Format: restoreJournalFormat, ID: "0123456789abcdef", State: "installing",
		Entries: []restoreJournalEntry{
			{Name: backupEntryConfig, Target: targetA, Existed: true, Mode: 0o600, Saved: "entry-0", SavedSHA256: sha256Hex(preA), Installed: true},
			{Name: backupEntrySecurity, Target: targetB, Existed: true, Mode: 0o600, Saved: "entry-1", SavedSHA256: sha256Hex(preB), Installed: true},
		},
	}
	journalDir := fabricateJournal(t, m, map[string][]byte{"entry-0": preA, "entry-1": preB})

	orig := restoreRollbackWriteFile
	restoreRollbackWriteFile = func(path string, data []byte, perm os.FileMode) error {
		if path == targetB {
			return errors.New("injected recovery write failure")
		}
		return orig(path, data, perm)
	}
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--recover", journalDir}, &stdout, &stderr)
	restoreRollbackWriteFile = orig

	if code != 1 {
		t.Fatalf("recover exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), targetB) {
		t.Fatalf("error must name the failed destination %s; got: %s", targetB, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(journalDir, "manifest.json")); err != nil {
		t.Fatal("an incomplete recovery must preserve the journal")
	}
	if got, _ := os.ReadFile(targetA); !bytes.Equal(got, preA) {
		t.Fatal("the recoverable destination must still be recovered")
	}
	if got, _ := os.ReadFile(targetB); string(got) != "INSTALLED STATE" {
		t.Fatal("the failed destination must be left as it was")
	}
}

// --- Round 6: destination directory-sync failures ---
//
// A failed directory sync after a rename does NOT mean the mutation
// did not happen. These tests inject sync failures at each stage and
// pin the semantics: the landed mutation counts, rollback/recovery
// scope includes it, and the journal survives whenever the outcome
// is not durably established.

// failSyncFor makes syncDirFn fail for one directory: times == 1
// fails the next call only, times < 0 fails every call.
func failSyncFor(t *testing.T, dir string, times int) {
	t.Helper()
	orig := syncDirFn
	remaining := times
	syncDirFn = func(d string) error {
		if d == dir && remaining != 0 {
			if remaining > 0 {
				remaining--
			}
			return errors.New("injected directory sync failure")
		}
		return orig(d)
	}
	t.Cleanup(func() { syncDirFn = orig })
}

func twoEntryRestoreFixture(t *testing.T) (bundle, targetConfig, targetDB string, preConfig, preDB []byte) {
	t.Helper()
	_, configPath := opsFixture(t)
	bundle = filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	targetDir := t.TempDir()
	targetConfig = filepath.Join(targetDir, "config.json")
	preConfig = []byte("PRE CONFIG")
	if err := os.WriteFile(targetConfig, preConfig, 0o640); err != nil {
		t.Fatal(err)
	}
	secDir := filepath.Join(targetDir, "security")
	if err := os.MkdirAll(secDir, 0o700); err != nil {
		t.Fatal(err)
	}
	targetDB = filepath.Join(secDir, "nexaroute-security.db")
	preDB = []byte("PRE DB")
	if err := os.WriteFile(targetDB, preDB, 0o600); err != nil {
		t.Fatal(err)
	}
	return bundle, targetConfig, targetDB, preConfig, preDB
}

func TestRestoreDirSyncFailureAfterInstall(t *testing.T) {
	t.Run("transient failure: landed install is rolled back too", func(t *testing.T) {
		bundle, targetConfig, targetDB, preConfig, preDB := twoEntryRestoreFixture(t)
		// Fail only the FIRST sync of the security directory (the
		// second install's durability confirmation).
		failSyncFor(t, filepath.Dir(targetDB), 1)
		var stdout, stderr bytes.Buffer
		code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "syncing its directory failed") || !strings.Contains(stderr.String(), "rolled back") {
			t.Fatalf("stderr must report the sync failure and the rollback, got: %s", stderr.String())
		}
		if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, preConfig) {
			t.Fatal("config must be rolled back")
		}
		if got, _ := os.ReadFile(targetDB); !bytes.Equal(got, preDB) {
			t.Fatal("the install whose rename LANDED before the failed sync must be rolled back, not skipped")
		}
		// Only a journal whose manifest targets THIS transaction's files
		// can be this transaction's leak. Scanning the shared temp dir by
		// name alone also flags journals owned by concurrent test
		// processes (the round-2 flake-hunt failure on this subtest,
		// #404) — foreign journals are not this rollback's residue. The
		// persistent-failure subtest below already scopes by target the
		// same way.
		for _, dir := range findInterruptedRestoreJournals() {
			m, err := readJournalManifest(dir)
			if err != nil {
				continue
			}
			for _, e := range m.Entries {
				if e.Target == targetConfig || e.Target == targetDB {
					t.Fatalf("a fully rolled-back transaction must not leave a journal: %s (target %s)", dir, e.Target)
				}
			}
		}
	})

	t.Run("persistent failure: rollback lands but cannot confirm, journal preserved", func(t *testing.T) {
		bundle, targetConfig, targetDB, preConfig, preDB := twoEntryRestoreFixture(t)
		failSyncFor(t, filepath.Dir(targetDB), -1)
		var stdout, stderr bytes.Buffer
		code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "rollback INCOMPLETE") || !strings.Contains(stderr.String(), targetDB) {
			t.Fatalf("stderr must report incomplete rollback naming %s, got: %s", targetDB, stderr.String())
		}
		// The rollback mutations DID land (bytes are pre-state); only
		// their durability is unconfirmed — hence the preserved journal.
		if got, _ := os.ReadFile(targetDB); !bytes.Equal(got, preDB) {
			t.Fatal("rollback mutation must have landed even though its sync failed")
		}
		if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, preConfig) {
			t.Fatal("config must be rolled back")
		}
		found := false
		for _, dir := range findInterruptedRestoreJournals() {
			if m, err := readJournalManifest(dir); err == nil && len(m.Entries) > 0 && m.Entries[0].Target == targetConfig {
				found = true
			}
		}
		if !found {
			t.Fatal("the journal must be preserved when a rollback sync cannot confirm durability")
		}
	})
}

// An install-stage WRITE failure whose rollback hits a sync failure:
// rollback is incomplete (journal preserved) even though the rollback
// write itself landed.
func TestRestoreRollbackDirSyncFailure(t *testing.T) {
	bundle, targetConfig, _, preConfig, _ := twoEntryRestoreFixture(t)
	targetDir := filepath.Dir(targetConfig)

	armed := false
	origWrite := restoreWriteFile
	calls := 0
	restoreWriteFile = func(path string, data []byte, perm os.FileMode) error {
		calls++
		if calls == 2 {
			armed = true // arm the sync failure only once rollback begins
			return errors.New("injected second-install failure")
		}
		return origWrite(path, data, perm)
	}
	defer func() { restoreWriteFile = origWrite }()

	origSync := syncDirFn
	syncDirFn = func(d string) error {
		if armed && d == targetDir {
			return errors.New("injected directory sync failure")
		}
		return origSync(d)
	}
	defer func() { syncDirFn = origSync }()

	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "rollback INCOMPLETE") || !strings.Contains(stderr.String(), targetConfig) {
		t.Fatalf("stderr must report incomplete rollback naming %s, got: %s", targetConfig, stderr.String())
	}
	if got, _ := os.ReadFile(targetConfig); !bytes.Equal(got, preConfig) {
		t.Fatal("the rollback write landed; only its directory sync failed")
	}
	found := false
	for _, dir := range findInterruptedRestoreJournals() {
		if m, err := readJournalManifest(dir); err == nil && len(m.Entries) > 0 && m.Entries[0].Target == targetConfig {
			found = true
		}
	}
	if !found {
		t.Fatal("journal must be preserved when rollback cannot confirm durability")
	}
}

// A recovery whose per-destination sync fails: the destination's
// bytes ARE restored (the mutation landed) but recovery reports it
// as unconfirmed, preserves the journal, and a retry succeeds.
func TestRestoreRecoverDirSyncFailure(t *testing.T) {
	targetDir := t.TempDir()
	targetA := filepath.Join(targetDir, "config.json")
	targetB := filepath.Join(targetDir, "security.db")
	preA, preB := []byte("PRE A"), []byte("PRE B")
	for _, target := range []string{targetA, targetB} {
		if err := os.WriteFile(target, []byte("INSTALLED STATE"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := &restoreJournalManifest{
		Format: restoreJournalFormat, ID: "0123456789abcdef", State: "installing",
		Entries: []restoreJournalEntry{
			{Name: backupEntryConfig, Target: targetA, Existed: true, Mode: 0o600, Saved: "entry-0", SavedSHA256: sha256Hex(preA), Installed: true},
			{Name: backupEntrySecurity, Target: targetB, Existed: true, Mode: 0o600, Saved: "entry-1", SavedSHA256: sha256Hex(preB), Installed: true},
		},
	}
	journalDir := fabricateJournal(t, m, map[string][]byte{"entry-0": preA, "entry-1": preB})

	// Both targets share one parent directory here; fail its syncs
	// during the first recovery attempt.
	failSyncFor(t, targetDir, -1)
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--recover", journalDir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("recover exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), targetA) {
		t.Fatalf("error must name the unconfirmed destinations, got: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(journalDir, "manifest.json")); err != nil {
		t.Fatal("journal must be preserved when recovery syncs fail")
	}
	if got, _ := os.ReadFile(targetA); !bytes.Equal(got, preA) {
		t.Fatal("recovery mutation landed even though its sync failed")
	}

	// Retry with sync healthy: idempotent, completes, journal removed.
	syncDirFn = syncDir
	stdout.Reset()
	stderr.Reset()
	if code := runRestore([]string{"--recover", journalDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("retry recover exit=%d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got, _ := os.ReadFile(targetB); !bytes.Equal(got, preB) {
		t.Fatal("retry must leave every destination at its pre-restore state")
	}
	if _, err := os.Stat(journalDir); !os.IsNotExist(err) {
		t.Fatal("a fully confirmed recovery must remove the journal")
	}
}

// --- Fix 5: bundle payload bounds ---

func TestRestoreRejectsOversizedBundles(t *testing.T) {
	setLimits := func(entry, total int64) {
		oldEntry, oldTotal := maxBackupEntryBytes, maxBackupTotalBytes
		maxBackupEntryBytes, maxBackupTotalBytes = entry, total
		t.Cleanup(func() { maxBackupEntryBytes, maxBackupTotalBytes = oldEntry, oldTotal })
	}

	t.Run("single entry over the per-entry limit", func(t *testing.T) {
		setLimits(1024, 1<<40)
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig:   []byte("{}"),
			backupEntrySecurity: bytes.Repeat([]byte{0x01}, 1500),
		})
		target := filepath.Join(t.TempDir(), "config.json")
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--bundle", bundle, "--config", target}, &stdout, &stderr); code != 1 {
			t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("oversized bundle must not install anything")
		}
	})

	t.Run("aggregate over the total limit", func(t *testing.T) {
		setLimits(1<<20, 2048)
		bundle := writeTestBundle(t, map[string][]byte{
			backupEntryConfig:   []byte("{}"),
			backupEntrySecurity: bytes.Repeat([]byte{0x01}, 900),
			backupEntryControl:  bytes.Repeat([]byte{0x02}, 900),
		})
		target := filepath.Join(t.TempDir(), "config.json")
		var stdout, stderr bytes.Buffer
		if code := runRestore([]string{"--bundle", bundle, "--config", target}, &stdout, &stderr); code != 1 {
			t.Fatalf("restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("over-aggregate bundle must not install anything")
		}
	})
}
