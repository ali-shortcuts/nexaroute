package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// fullDrillFixture builds a self-contained installation whose backup
// exercises every verification check: a valid config, a chained
// security store, a file-backend control-plane state, and a master key.
func fullDrillFixture(t *testing.T) (dir, configPath string) {
	t.Helper()
	dir, configPath = opsFixture(t)
	rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
	})
	if err := os.WriteFile(configPath+".key", bytes.Repeat([]byte{0x11}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"schema": 1, "snapshots": {}}`)
	if err := os.WriteFile(configPath+".controlplane.json", state, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, configPath
}

func loadVerifyReport(t *testing.T, path string) restoreVerifyReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report restoreVerifyReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("report does not parse: %v", err)
	}
	if report.Format != restoreVerifyFormat {
		t.Fatalf("report format=%q, want %q", report.Format, restoreVerifyFormat)
	}
	return report
}

func verifyCheckStatus(report restoreVerifyReport, name string) string {
	for _, c := range report.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

func TestRestoreVerifyIsolatedFullDrill(t *testing.T) {
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	_, configPath := fullDrillFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	into := filepath.Join(t.TempDir(), "restored")
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{
		"--bundle", bundle, "--config", configPath, "--verify",
		"--into", into, "--report", reportPath, "--max-age", "24h",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("verify exit=%d stderr=%s\n%s", code, stderr.String(), stdout.String())
	}
	report := loadVerifyReport(t, reportPath)
	if report.Status != statusPass {
		t.Fatalf("report status=%q, want PASS: %+v", report.Status, report.Checks)
	}
	for _, want := range []string{"bundle_integrity", "config_decryption", "route_usability", "master_key_pairing", "audit_chain", "control_plane_state", "isolated_materialization"} {
		if got := verifyCheckStatus(report, want); got != statusPass {
			t.Fatalf("check %s status=%q, want PASS (%+v)", want, got, report.Checks)
		}
	}
	if report.BundleSHA256 == "" || report.BundleBytes == 0 {
		t.Fatal("report must record the bundle hash and size")
	}
	// The disposable directory holds the restored bytes; production
	// targets were not touched.
	for _, name := range []string{"config.json", "security.db", "controlplane.json"} {
		if _, err := os.Stat(filepath.Join(into, name)); err != nil {
			t.Fatalf("materialized %s missing: %v", name, err)
		}
	}
	// The report never carries secret material.
	raw, _ := os.ReadFile(reportPath)
	if bytes.Contains(raw, []byte("nxs1:")) {
		t.Fatal("the verification report must not contain ciphertext")
	}
}

func TestRestoreVerifyFailsClosedOnWrongKey(t *testing.T) {
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	key := bytes.Repeat([]byte{0x42}, 32)
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
	srcDir := t.TempDir()
	configPath := filepath.Join(srcDir, "config.json")
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

	// A wrong key IS available: verification must FAIL, not pass or block.
	wrong := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x99}, 32))
	t.Setenv("NEXAROUTE_MASTER_KEY", wrong)
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--verify", "--report", reportPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("verify(wrong key) exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	report := loadVerifyReport(t, reportPath)
	if got := verifyCheckStatus(report, "config_decryption"); got != statusFail {
		t.Fatalf("config_decryption status=%q, want FAIL", got)
	}
	if report.Status != statusFail {
		t.Fatalf("aggregate status=%q, want FAIL", report.Status)
	}
	if _, err := os.Stat(targetConfig); !os.IsNotExist(err) {
		t.Fatal("verification must never write to the restore target")
	}
}

func TestRestoreVerifyReportsMissingKeyAsBlocked(t *testing.T) {
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	plainCfg := config.Default()
	plainCfg.Providers = append(plainCfg.Providers, config.ProviderConfig{
		ID: "p1", Type: "openai_compatible", BaseURL: "https://example.invalid/v1",
		APIKey: "sk-test-secret-value", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true}},
	})
	plain, _ := json.Marshal(plainCfg)
	encrypted, _, err := secrets.TransformConfig(plain, key, true)
	if err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	configPath := filepath.Join(srcDir, "config.json")
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
	// Fresh host: no bundled key, no env, no host key file.
	reportPath := filepath.Join(t.TempDir(), "report.json")
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--verify", "--report", reportPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("verify(missing key) exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	report := loadVerifyReport(t, reportPath)
	if got := verifyCheckStatus(report, "config_decryption"); got != statusBlocked {
		t.Fatalf("config_decryption status=%q, want BLOCKED", got)
	}
	if report.Status != statusBlocked {
		t.Fatalf("aggregate status=%q, want BLOCKED", report.Status)
	}
}

func TestRestoreVerifyCorruptBundle(t *testing.T) {
	_, configPath := fullDrillFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(raw, []byte("127.0.0.1:8080"))
	if idx < 0 {
		t.Fatal("fixture config payload not found in bundle")
	}
	raw[idx] = '9'
	corrupt := filepath.Join(t.TempDir(), "corrupt.tar")
	if err := os.WriteFile(corrupt, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", corrupt, "--config", configPath, "--verify", "--report", reportPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("verify(corrupt) exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	report := loadVerifyReport(t, reportPath)
	if got := verifyCheckStatus(report, "bundle_integrity"); got != statusFail {
		t.Fatalf("bundle_integrity status=%q, want FAIL", got)
	}
}

func TestRestoreVerifyStaleBundle(t *testing.T) {
	_, configPath := fullDrillFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", configPath, "--verify", "--max-age", "1ns"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("verify(stale) exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "FAIL bundle_freshness") {
		t.Fatalf("stale bundle must FAIL freshness:\n%s", stdout.String())
	}
}

func TestRestoreVerifyExternalStoresBlocked(t *testing.T) {
	dir, configPath := opsFixture(t)
	rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "postgres"
		cfg.ControlPlane.PostgresDSNEnv = "NEXAROUTE_TEST_CP_DSN"
	})
	_ = dir
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", configPath, "--verify"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("verify(external) exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "BLOCKED external_store_ownership") {
		t.Fatalf("external stores must be explicitly BLOCKED:\n%s", stdout.String())
	}
}

func TestRestoreVerifyControlPlaneOldSchema(t *testing.T) {
	cfgBytes := bundleConfigBytes(t, func(cfg *config.Config) {
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
	})
	bundle := writeTestBundle(t, map[string][]byte{
		backupEntryConfig:  cfgBytes,
		backupEntryControl: []byte(`{"schema": 0, "snapshots": {}}`),
	})
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--bundle", bundle, "--config", filepath.Join(t.TempDir(), "config.json"), "--verify"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("verify(old schema) exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "FAIL control_plane_state") {
		t.Fatalf("old control-plane schema must FAIL:\n%s", stdout.String())
	}
}

func TestRestoreVerifyRejectsNonEmptyInto(t *testing.T) {
	_, configPath := fullDrillFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	into := t.TempDir()
	if err := os.WriteFile(filepath.Join(into, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runRestore([]string{"--bundle", bundle, "--config", configPath, "--verify", "--into", into}, &stdout, &stderr); code != 2 {
		t.Fatalf("verify(non-empty --into) exit=%d, want 2", code)
	}
	if _, err := os.Stat(filepath.Join(into, "keep")); err != nil {
		t.Fatal("a refused --into must not modify the directory")
	}
}

func TestRestoreVerifyRequiresBundle(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runRestore([]string{"--verify", "--config", defaultConfigPath()}, &stdout, &stderr); code != 2 {
		t.Fatalf("verify without --bundle exit=%d, want 2", code)
	}
}

func TestDoctorBackupPreflight(t *testing.T) {
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	_, configPath := fullDrillFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	var stdout, stderr bytes.Buffer
	if code := runDoctor([]string{"--config", configPath, "--bundle", bundle, "--max-age", "24h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor exit=%d, want 0\n%s", code, stdout.String())
	}
	for _, want := range []string{"PASS backup_integrity:", "PASS backup_freshness:", "PASS backup_key_pairing:", "PASS backup_migration_compat:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}

	// A corrupt bundle must FAIL the integrity preflight.
	corrupt := filepath.Join(t.TempDir(), "corrupt.tar")
	if err := os.WriteFile(corrupt, []byte("not a tar"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := runDoctor([]string{"--config", configPath, "--bundle", corrupt}, &stdout, &stderr); code != 1 {
		t.Fatalf("doctor(corrupt bundle) exit=%d, want 1\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "FAIL backup_integrity:") {
		t.Fatalf("doctor must FAIL the corrupt bundle:\n%s", stdout.String())
	}
}

// TestIssue438Evidence is an opt-in drill that writes a real,
// machine-readable verification report to the path named by
// NEXAROUTE_EVIDENCE_REPORT. It is skipped in normal test runs and is
// used to produce committed operational evidence reproducibly.
func TestIssue438Evidence(t *testing.T) {
	out := os.Getenv("NEXAROUTE_EVIDENCE_REPORT")
	if out == "" {
		t.Skip("set NEXAROUTE_EVIDENCE_REPORT to emit an isolated restore-verify report")
	}
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	_, configPath := fullDrillFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	// Materialize inside the repository (next to the report) so the
	// committed evidence never references a temporary path.
	into := filepath.Join(filepath.Dir(out), "issue438-drill-scratch")
	_ = os.RemoveAll(into)
	defer func() { _ = os.RemoveAll(into) }()
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{
		"--bundle", bundle, "--config", configPath, "--verify",
		"--into", into, "--report", out, "--max-age", "24h",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("evidence drill exit=%d stderr=%s\n%s", code, stderr.String(), stdout.String())
	}
	report := loadVerifyReport(t, out)
	if report.Status != statusPass {
		t.Fatalf("evidence drill status=%q, want PASS", report.Status)
	}
}

func TestDoctorWarnsOnPendingKeyRotation(t *testing.T) {
	_, configPath := opsFixture(t)
	if err := os.WriteFile(configPath+".key", bytes.Repeat([]byte{0x11}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath+".key.next", bytes.Repeat([]byte{0x22}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runDoctor([]string{"--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor exit=%d, want 0 (pending rotation is a WARN)\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "WARN key_rotation:") {
		t.Fatalf("doctor must warn about a staged replacement key:\n%s", stdout.String())
	}
}
