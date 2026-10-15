package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
