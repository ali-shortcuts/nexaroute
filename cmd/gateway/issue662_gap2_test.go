package main

// Second-wave coverage for issue #662 (video runtime, providers, gateway).
//
// This file covers only blocks that were still at zero count on the
// ba5ff2f baseline after PR #668, in the areas the issue prioritizes:
// configuration/input validation, provider timeouts/errors/rate limits,
// cancellation/cleanup/retry logic, health/readiness, and fail-closed
// startup/shutdown paths. Fakes are loopback stand-ins for the LOCAL
// contract only; none of this claims a live Vault, ComfyUI, Postgres,
// Redis, or aggregator account passed anything.
//
// Deliberately NOT covered here (documented in the PR body instead):
//   - config diff/flatten JSON round-trip errors: json.Marshal of a
//     config struct cannot fail, and its output always unmarshals.
//   - doctor listen-parse failure and backup live-URL derivation: the
//     config loader already rejects non-host:port listen values, so
//     those branches are shadowed by load validation.
//   - backup security-store stat failure and create-backup Transit
//     envelope failure: the former is shadowed by the output-collision
//     check (which rejects every unstatable source first), the latter
//     needs a loadable live-Transit configuration (BLOCKED_EXTERNAL).
//   - backup 2 GiB per-entry / 4 GiB aggregate limits, stage-write and
//     tar/sync/close errors, report-write chmod/write/sync/close
//     errors: filesystem fault injection, not unit-testable here.
//   - provider-accept cancellation warning: it fires only on SIGINT /
//     SIGTERM mid-loop, which no deterministic unit test can time.
//   - poll loop-top cancellation with a prior status error: the timer
//     select always wins that race first; the cancel-during-poll and
//     cancel-during-sleep branches below are the deterministic ones.
//   - verify size/SHA mismatch: the local store's own Read enforces
//     both invariants first, so those branches are shadowed.
//   - verify no-decodable-stream: Probe already errors when no video
//     stream exists, so the empty-codec branch cannot be reached.
//   - verify stat failure and fail-closed workspace-removal failure:
//     both need a filesystem failure between two successful calls.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// ---------------------------------------------------------------------------
// config.go
// ---------------------------------------------------------------------------

func TestGap2ConfigValidateRejectsUnreadablePath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runConfig([]string{"validate", "--config", filepath.Join(t.TempDir(), "no-such-config.json")}, &stdout, &stderr)
	if code != configExitFailure || !strings.Contains(stderr.String(), "config invalid") {
		t.Fatalf("validate exit=%d stderr=%q, want failure with config-invalid diagnostic", code, stderr.String())
	}
}

func TestGap2ConfigDiffReportsRemovedKeys(t *testing.T) {
	left := config.Default()
	right := config.Default()
	right.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "https://example.invalid/v1", Enabled: true}}
	changes, err := configDiff(left, right)
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	for _, line := range changes {
		if strings.Contains(line, "removed") {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("diff = %v, want at least one removed key for the provider present only on the right", changes)
	}
}

func TestGap2ConfigDryRunPrepareFailureAndCounting(t *testing.T) {
	t.Run("bedrock without its secret env fails preparation, not load", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers = []config.ProviderConfig{{
			ID: "bedrock-missing-secret", Type: "bedrock", BaseURL: "https://example.invalid",
			Enabled: true, APIKey: "dummy-access-key", SecretKeyEnv: "GAP2_DEFINITELY_UNSET_SECRET",
		}}
		var stdout, stderr bytes.Buffer
		code := runConfig([]string{"dry-run", "--config", writeTestConfig(t, cfg)}, &stdout, &stderr)
		if code != configExitFailure || !strings.Contains(stderr.String(), "dry-run failed while preparing providers") {
			t.Fatalf("dry-run exit=%d stderr=%q, want provider-preparation failure", code, stderr.String())
		}
	})
	t.Run("enabled providers are counted, disabled ones are not", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers = []config.ProviderConfig{
			{ID: "on", Type: "openai_compatible", BaseURL: "https://example.invalid/v1", Enabled: true},
			{ID: "off", Type: "openai_compatible", BaseURL: "https://example.invalid/v1", Enabled: false},
		}
		var stdout, stderr bytes.Buffer
		code := runConfig([]string{"dry-run", "--config", writeTestConfig(t, cfg)}, &stdout, &stderr)
		if code != configExitOK || !strings.Contains(stdout.String(), "providers_prepared=1") {
			t.Fatalf("dry-run exit=%d stdout=%q stderr=%q, want providers_prepared=1", code, stdout.String(), stderr.String())
		}
	})
}

// ---------------------------------------------------------------------------
// dispatch
// ---------------------------------------------------------------------------

func TestGap2DispatchHandlesDiagnosticsAndQualityGate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code, handled := dispatchSubcommand([]string{"diagnostics"}, nil, &stdout, &stderr); !handled || code != 2 {
		t.Fatalf("diagnostics dispatch = (%d, %v), want usage exit with handled=true", code, handled)
	}
	if code, handled := dispatchSubcommand([]string{"quality-gate"}, nil, &stdout, &stderr); !handled || code != 2 {
		t.Fatalf("quality-gate dispatch = (%d, %v), want usage exit with handled=true", code, handled)
	}
	if _, handled := dispatchSubcommand([]string{"not-a-command"}, nil, &stdout, &stderr); handled {
		t.Fatal("unknown subcommand must not be handled")
	}
}

// ---------------------------------------------------------------------------
// doctor.go
// ---------------------------------------------------------------------------

func gap2DoctorReports(t *testing.T, run func(d *doctor)) string {
	t.Helper()
	var buf bytes.Buffer
	d := &doctor{w: &buf}
	run(d)
	return buf.String()
}

func TestGap2DoctorConfigAndControlPlaneFailures(t *testing.T) {
	t.Run("unreadable config fails the config check", func(t *testing.T) {
		out := gap2DoctorReports(t, func(d *doctor) {
			d.checkConfig(filepath.Join(t.TempDir(), "no-such-config.json"))
		})
		if !strings.Contains(out, "FAIL config: cannot read") {
			t.Fatalf("doctor output = %q, want FAIL config cannot-read", out)
		}
	})
	t.Run("control-plane state directory fails the read", func(t *testing.T) {
		dir := t.TempDir()
		cfg := config.Default()
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
		cfg.ControlPlane.StatePath = dir
		out := gap2DoctorReports(t, func(d *doctor) {
			d.cfg = &cfg
			d.checkControlPlane(filepath.Join(dir, "config.json"))
		})
		if !strings.Contains(out, "FAIL control_plane: cannot read file backend state") {
			t.Fatalf("doctor output = %q, want FAIL control_plane read", out)
		}
	})
	t.Run("corrupt control-plane state fails JSON parsing", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "cp.json")
		if err := os.WriteFile(state, []byte("this is not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "file"
		cfg.ControlPlane.StatePath = state
		out := gap2DoctorReports(t, func(d *doctor) {
			d.cfg = &cfg
			d.checkControlPlane(filepath.Join(dir, "config.json"))
		})
		if !strings.Contains(out, "FAIL control_plane: file backend state is corrupt") {
			t.Fatalf("doctor output = %q, want FAIL control_plane corrupt", out)
		}
	})
}

func TestGap2DoctorMasterKeyAndBundleChecks(t *testing.T) {
	t.Run("non-0600 key file fails permissions", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(configPath+".key", []byte("k"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := gap2DoctorReports(t, func(d *doctor) { d.checkMasterKey(configPath) })
		if !strings.Contains(out, "FAIL master_key_permissions") || !strings.Contains(out, "0600") {
			t.Fatalf("doctor output = %q, want FAIL master_key_permissions demanding 0600", out)
		}
	})
	t.Run("key stat under a regular file fails inspection", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		out := gap2DoctorReports(t, func(d *doctor) { d.checkMasterKey(filepath.Join(blocker, "config.json")) })
		if !strings.Contains(out, "FAIL master_key_permissions: cannot inspect") {
			t.Fatalf("doctor output = %q, want FAIL master_key_permissions cannot-inspect", out)
		}
	})
	t.Run("retained previous-rotation copy passes", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(configPath+".key", []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath+".key.previous", []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		out := gap2DoctorReports(t, func(d *doctor) { d.checkMasterKey(configPath) })
		if !strings.Contains(out, "PASS key_rotation") {
			t.Fatalf("doctor output = %q, want PASS key_rotation", out)
		}
	})
	t.Run("missing bundle fails the bundle check", func(t *testing.T) {
		out := gap2DoctorReports(t, func(d *doctor) {
			d.checkBackupBundle(filepath.Join(t.TempDir(), "no-such.tar"), filepath.Join(t.TempDir(), "config.json"), 0)
		})
		if !strings.Contains(out, "FAIL backup_bundle: cannot read") {
			t.Fatalf("doctor output = %q, want FAIL backup_bundle cannot-read", out)
		}
	})
	t.Run("group-readable bundle warns on permissions before integrity", func(t *testing.T) {
		dir := t.TempDir()
		bundle := filepath.Join(dir, "bundle.tar")
		if err := os.WriteFile(bundle, []byte("not a bundle"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := gap2DoctorReports(t, func(d *doctor) {
			d.checkBackupBundle(bundle, filepath.Join(dir, "config.json"), 0)
		})
		if !strings.Contains(out, "WARN backup_permissions") || !strings.Contains(out, "FAIL backup_integrity") {
			t.Fatalf("doctor output = %q, want WARN permissions then FAIL integrity", out)
		}
	})
}

func TestGap2ResolveSecurityStorePathRequiresAConfigPath(t *testing.T) {
	if _, err := resolveSecurityStorePath("", ""); err == nil {
		t.Fatal("empty configured path with no config path must error")
	}
	got, err := resolveSecurityStorePath("", filepath.Join("dir", "config.json"))
	if err != nil || !strings.HasSuffix(got, filepath.Join("security", "nexaroute-security.db")) {
		t.Fatalf("default store path = %q, err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// controlplane_runtime.go
// ---------------------------------------------------------------------------

type gap2FakeCPStore struct {
	gets []func() (controlplane.Snapshot, error)
	puts []func() (controlplane.Snapshot, error)
	get  int
	put  int
}

func (s *gap2FakeCPStore) Get(ctx context.Context, namespace string) (controlplane.Snapshot, error) {
	_ = ctx
	_ = namespace
	if s.get < len(s.gets) {
		fn := s.gets[s.get]
		s.get++
		return fn()
	}
	return controlplane.Snapshot{}, controlplane.ErrNotFound
}

func (s *gap2FakeCPStore) Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (controlplane.Snapshot, error) {
	_ = ctx
	_ = namespace
	_ = expectedRevision
	_ = payload
	if s.put < len(s.puts) {
		fn := s.puts[s.put]
		s.put++
		return fn()
	}
	return controlplane.Snapshot{}, controlplane.ErrConflict
}

func (s *gap2FakeCPStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	_ = ctx
	_ = namespace
	_ = expectedRevision
	return nil
}

func TestGap2ControlPlaneBreakerStore(t *testing.T) {
	if _, err := (&controlPlaneRuntime{}).BreakerStore("r1"); err == nil {
		t.Fatal("breaker store over a nil control-plane store must fail")
	}
	fake := &gap2FakeCPStore{}
	rt := &controlPlaneRuntime{store: fake}
	store, err := rt.BreakerStore("r1")
	if err != nil || store == nil {
		t.Fatalf("breaker store over a working store = %v, err=%v, want non-nil", store, err)
	}
}

func TestGap2ControlPlaneSnapshotConflictAndReloadFailure(t *testing.T) {
	fake := &gap2FakeCPStore{
		gets: []func() (controlplane.Snapshot, error){
			func() (controlplane.Snapshot, error) { return controlplane.Snapshot{}, controlplane.ErrNotFound },
			func() (controlplane.Snapshot, error) { return controlplane.Snapshot{}, errors.New("reload boom") },
		},
		puts: []func() (controlplane.Snapshot, error){
			func() (controlplane.Snapshot, error) { return controlplane.Snapshot{}, controlplane.ErrConflict },
		},
	}
	manager, err := controlplane.NewManager(fake, controlplane.LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureControlPlaneSnapshot(context.Background(), manager, "gateway/runtime", []byte("bootstrap")); err == nil || !strings.Contains(err.Error(), "revision conflict and reload failed") {
		t.Fatalf("snapshot conflict+reload error = %v, want the combined diagnostic", err)
	}
}

func TestGap2ControlPlaneCorruptFileStateFailsOpen(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "cp.json")
	if err := os.WriteFile(state, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.ControlPlaneConfig{
		Enabled: true, Backend: "file", StatePath: state, ConfigFailure: "last_known_good",
	}
	if _, err := openControlPlaneRuntime(cfg, filepath.Join(dir, "config.json")); err == nil || !strings.Contains(err.Error(), "load control-plane snapshot") {
		t.Fatalf("corrupt file state open error = %v, want load-snapshot diagnostic", err)
	}
}

func TestGap2RedisHashAdapterPropagatesDeadPortErrors(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:9", MaxRetries: 0, DialTimeout: time.Second})
	defer client.Close()
	adapter := redisHashAdapter{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := adapter.HGetAll(ctx, "k"); err == nil {
		t.Fatal("HGetAll against a dead port must fail (local contract only, not a live Redis claim)")
	}
	if _, err := adapter.Eval(ctx, "return 1", nil); err == nil {
		t.Fatal("Eval against a dead port must fail")
	}
}

// ---------------------------------------------------------------------------
// report writers: provider-accept, external-accept, canary
// ---------------------------------------------------------------------------

func gap2ReportWriteErrors(t *testing.T, write func(path string) error) {
	t.Helper()
	t.Run("directory under a regular file fails mkdir", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := write(filepath.Join(blocker, "report.json")); err == nil {
			t.Fatal("report under a regular file must fail directory creation")
		}
	})
	t.Run("read-only proc directory fails temp creation", func(t *testing.T) {
		if err := write(filepath.Join("/proc", "gap2-report.json")); err == nil {
			t.Fatal("report temp creation under /proc must fail")
		}
	})
}

func TestGap2ProviderAcceptReportWriteErrors(t *testing.T) {
	gap2ReportWriteErrors(t, func(path string) error {
		return writeProviderAcceptReport(path, providerAcceptReport{})
	})
}

func TestGap2ExternalAcceptReportWriteErrors(t *testing.T) {
	gap2ReportWriteErrors(t, func(path string) error {
		return writeExternalAcceptReport(path, externalAcceptReport{})
	})
}

func TestGap2CanaryReportWriteErrors(t *testing.T) {
	gap2ReportWriteErrors(t, func(path string) error {
		return writeCanaryReport(path, []byte("{}"))
	})
}

// ---------------------------------------------------------------------------
// external_accept helpers, audit, init
// ---------------------------------------------------------------------------

func TestGap2ExternalAcceptHelpers(t *testing.T) {
	if containsAudienceString([]string{"a", "b"}, "c") {
		t.Fatal("absent audience must not match")
	}
	if !containsAudienceString([]string{"a", "b"}, "b") {
		t.Fatal("present audience must match")
	}
	if validAcceptanceOIDCEndpoint("://bad", "https://issuer.example.test") {
		t.Fatal("unparseable endpoint must be invalid")
	}
	// NOTE: fetchOIDCJWKS's request-creation error is deliberately not
	// tested: its endpoint validation already parses the URL with the
	// same parser http.NewRequest uses, so no input can pass validation
	// and then fail construction. The branch is defensive-only.
}

func TestGap2AuditVerifyMissingStoreFailsClosed(t *testing.T) {
	path := writeTestConfig(t, config.Default())
	var stdout, stderr bytes.Buffer
	code := runAudit([]string{"verify", "--config", path}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "audit verify") {
		t.Fatalf("audit verify exit=%d stderr=%q, want non-zero with an audit diagnostic", code, stderr.String())
	}
}

func TestGap2InitWithAllFlagsProvidedWritesWithoutPrompting(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "config.json")
	var stdout, stderr bytes.Buffer
	code := runInit([]string{
		"--out", out, "--force",
		"--listen", "127.0.0.1:8080", "--provider-id", "primary", "--type", "openai_compatible",
		"--base-url", "https://api.example.invalid", "--api-key-env", "GAP2_INIT_KEY", "--model", "model-x",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("non-interactive init exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("init did not write the config: %v", err)
	}
	if strings.Contains(stdout.String(), "Provider base URL") {
		t.Fatal("fully-flagged init must not prompt")
	}
}

// ---------------------------------------------------------------------------
// shared gateway test helpers
// ---------------------------------------------------------------------------

// gap2RawTCPServer serves one truncated response (declared Content-Length
// larger than the bytes sent) and returns its base URL.
func gap2RawTCPServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer ln.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: video/mp4\r\n\r\nshort"))
	}()
	return "http://" + ln.Addr().String()
}

// gap2RunBackup runs runBackup with buffers and returns exit code,
// stdout, and stderr.
func gap2RunBackup(t *testing.T, configPath, out string, extra ...string) (int, string, string) {
	t.Helper()
	args := []string{"--config", configPath, "--out", out}
	args = append(args, extra...)
	var stdout, stderr bytes.Buffer
	code := runBackup(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// ---------------------------------------------------------------------------
// vault_accept.go: flags, validators, safe errors
// ---------------------------------------------------------------------------

func TestGap2VaultAcceptFlagValidation(t *testing.T) {
	run := func(args ...string) (int, string) {
		var stderr bytes.Buffer
		code := runVaultAccept(args, &stderr, &stderr)
		return code, stderr.String()
	}
	if code, stderr := run("--timeout", "0s"); code != 2 || !strings.Contains(stderr, "--timeout must be between") {
		t.Fatalf("timeout validation exit=%d stderr=%q", code, stderr)
	}
	if code, _ := run("--timeout", "61s"); code != 2 {
		t.Fatalf("over-max timeout exit=%d, want 2", code)
	}
	if code, stderr := run("--timeout", "10s", "--overall-timeout", "5s"); code != 2 || !strings.Contains(stderr, "--overall-timeout must be between") {
		t.Fatalf("overall-timeout validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--ref", "not-a-kv-path"); code != 2 || !strings.Contains(stderr, "--ref must be") {
		t.Fatalf("ref validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--ref", "secret/data/x/y", "--addr-env", ""); code != 2 || !strings.Contains(stderr, "--addr-env and --token-env must name") {
		t.Fatalf("env-name validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--ref", "secret/data/x/y", "--confirm-live", "--helper", filepath.Join(t.TempDir(), "no-such-helper")); code != 2 || !strings.Contains(stderr, "--helper does not point") {
		t.Fatalf("helper validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--ref", "secret/data/x/y", "--confirm-live", "--helper", vaultAcceptHelperPath(t), "--outage-addr", "ftp://example.invalid"); code != 2 || !strings.Contains(stderr, "--outage-addr") {
		t.Fatalf("outage-addr validation exit=%d stderr=%q", code, stderr)
	}
}

func TestGap2VaultAcceptDefaultOutPathAndMissingEnv(t *testing.T) {
	// Resolve the helper before changing directory: the helper path is
	// relative to the package directory.
	helper := vaultAcceptHelperPath(t)
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	code := runVaultAccept([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", helper,
		"--confirm-live",
		"--addr-env", "GAP2_VAULT_ADDR_UNSET", "--token-env", "GAP2_VAULT_TOKEN_UNSET",
	}, &stdout, &stderr)
	if code == 2 {
		t.Fatalf("missing-env run must write a BLOCKED report, not fail usage: stderr=%q", stderr.String())
	}
	entries, err := filepath.Glob("vault-acceptance-*.json")
	if err != nil || len(entries) != 1 {
		t.Fatalf("default out path produced %v, err=%v, want exactly one report", entries, err)
	}
	report := readVaultAcceptReport(t, entries[0])
	if report.Outcome != externalStatusBlocked || vaultAcceptStatus(report, "vault.config") != externalStatusBlocked {
		t.Fatalf("missing-env report outcome=%q config=%q, want blocked", report.Outcome, vaultAcceptStatus(report, "vault.config"))
	}
}

func TestGap2VaultAcceptMissingTokenNamesTheTokenEnv(t *testing.T) {
	fake := newFakeVaultAccept()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	t.Setenv("GAP2_VAULT_ADDR_SET", server.URL)
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	runVaultAccept([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", vaultAcceptHelperPath(t),
		"--confirm-live",
		"--addr-env", "GAP2_VAULT_ADDR_SET", "--token-env", "GAP2_VAULT_TOKEN_UNSET",
		"--out", out,
	}, &stdout, &stderr)
	report := readVaultAcceptReport(t, out)
	found := false
	for _, check := range report.Checks {
		if check.ID == "vault.config" && strings.Contains(check.Reason, "GAP2_VAULT_TOKEN_UNSET") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-token report checks = %+v, want vault.config naming the token env", report.Checks)
	}
}

func TestGap2VaultAcceptInvalidAddrAndDeadHealth(t *testing.T) {
	t.Run("non-HTTP(S) address fails the config check", func(t *testing.T) {
		t.Setenv("GAP2_VAULT_ADDR_BAD", "ftp://example.invalid")
		t.Setenv("GAP2_VAULT_TOKEN_BAD", "dummy-token")
		out := filepath.Join(t.TempDir(), "report.json")
		var stdout, stderr bytes.Buffer
		runVaultAccept([]string{
			"--ref", "secret/data/nexaroute/acceptance/master-key",
			"--helper", vaultAcceptHelperPath(t),
			"--confirm-live",
			"--addr-env", "GAP2_VAULT_ADDR_BAD", "--token-env", "GAP2_VAULT_TOKEN_BAD",
			"--out", out,
		}, &stdout, &stderr)
		report := readVaultAcceptReport(t, out)
		if vaultAcceptStatus(report, "vault.config") != externalStatusFail {
			t.Fatalf("bad-addr report config = %q, want fail", vaultAcceptStatus(report, "vault.config"))
		}
	})
	t.Run("dead health endpoint skips the key checks", func(t *testing.T) {
		t.Setenv("GAP2_VAULT_ADDR_DEAD", "http://127.0.0.1:9")
		t.Setenv("GAP2_VAULT_TOKEN_DEAD", "dummy-token")
		out := filepath.Join(t.TempDir(), "report.json")
		var stdout, stderr bytes.Buffer
		runVaultAccept([]string{
			"--ref", "secret/data/nexaroute/acceptance/master-key",
			"--helper", vaultAcceptHelperPath(t),
			"--confirm-live",
			"--addr-env", "GAP2_VAULT_ADDR_DEAD", "--token-env", "GAP2_VAULT_TOKEN_DEAD",
			"--out", out,
		}, &stdout, &stderr)
		report := readVaultAcceptReport(t, out)
		if vaultAcceptStatus(report, "vault.health") != externalStatusFail {
			t.Fatalf("dead-health report health = %q, want fail", vaultAcceptStatus(report, "vault.health"))
		}
		for _, id := range []string{"vault.kv_put_get", "vault.kv_rotation", "vault.wrong_token", "vault.revoked_token", "vault.outage_fail_closed"} {
			if vaultAcceptStatus(report, id) != externalStatusBlocked {
				t.Fatalf("dead-health report %s = %q, want blocked-skipped", id, vaultAcceptStatus(report, id))
			}
		}
	})
}

func TestGap2VaultAcceptValidators(t *testing.T) {
	if err := validateVaultAcceptRef(""); err == nil {
		t.Fatal("empty ref must error")
	}
	if err := validateVaultAcceptRef("/absolute/data/x"); err == nil {
		t.Fatal("absolute ref must error")
	}
	if err := validateVaultAcceptRef("secret/nodata/x"); err == nil {
		t.Fatal("ref without /data/ must error")
	}
	if err := validateVaultAcceptRef("secret/data/x/y"); err != nil {
		t.Fatalf("valid ref error = %v", err)
	}
	if _, err := validateVaultAcceptHelper(""); err == nil {
		t.Fatal("empty helper must error")
	}
	if _, err := validateVaultAcceptHelper(filepath.Join(t.TempDir(), "no-such")); err == nil {
		t.Fatal("missing helper must error")
	}
	plain := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateVaultAcceptHelper(plain); err == nil {
		t.Fatal("non-executable helper must error")
	}
	if _, err := validateVaultAcceptAddr("not a url"); err == nil {
		t.Fatal("unparseable addr must error")
	}
	if _, err := validateVaultAcceptAddr("http://user@127.0.0.1:8200"); err == nil {
		t.Fatal("userinfo addr must error")
	}
	if _, err := validateVaultAcceptAddr("http://127.0.0.1:8200/?q=1"); err == nil {
		t.Fatal("query addr must error")
	}
	if _, err := validateVaultAcceptAddr("ftp://example.invalid"); err == nil {
		t.Fatal("ftp addr must error")
	}
	if _, err := validateVaultAcceptAddr("http://203.0.113.1:8200"); err == nil {
		t.Fatal("non-loopback plain HTTP must error")
	}
	if got := vaultAcceptEvidenceClass(nil); got != "none" {
		t.Fatalf("nil URL evidence = %q, want none", got)
	}
	u, err := validateVaultAcceptAddr("https://vault.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if got := vaultAcceptEvidenceClass(u); got != "external" {
		t.Fatalf("remote URL evidence = %q, want external", got)
	}
}

func TestGap2VaultAcceptSafeError(t *testing.T) {
	if got := vaultAcceptSafeError(nil); got != "" {
		t.Fatalf("nil error = %q, want empty", got)
	}
	urlErr := &url.Error{Op: "Get", URL: "http://127.0.0.1:9", Err: errors.New("connection refused")}
	if got := vaultAcceptSafeError(urlErr); got != "connection refused" {
		t.Fatalf("url.Error unwrap = %q, want inner message", got)
	}
	if got := vaultAcceptSafeError(errors.New("   ")); got != "request failed" {
		t.Fatalf("blank error = %q, want request failed", got)
	}
	long := strings.Repeat("x", 500)
	if got := vaultAcceptSafeError(errors.New(long)); len(got) != 200 {
		t.Fatalf("long error length = %d, want 200 truncation", len(got))
	}
	if got := vaultAcceptSafeError(errors.New("  padded  ")); got != "padded" {
		t.Fatalf("padded error = %q, want trimmed", got)
	}
	if got := safeVaultAcceptString("  "); got != "unknown" {
		t.Fatalf("blank string = %q, want unknown", got)
	}
}

// ---------------------------------------------------------------------------
// vault_accept.go: helper execution
// ---------------------------------------------------------------------------

func TestGap2VaultAcceptHelperErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := runVaultAcceptHelper(ctx, filepath.Join(t.TempDir(), "no-such-helper"), "get", "secret/data/x", nil, "http://127.0.0.1:9", "tok", 30); err == nil {
		t.Fatal("missing helper must error")
	}
	if _, err := runVaultAcceptHelper(ctx, vaultAcceptHelperPath(t), "put", "secret/data/x", make([]byte, 16), "http://127.0.0.1:9", "tok", 30); err == nil || !strings.Contains(err.Error(), "not 32 bytes") {
		t.Fatalf("short-key put error = %v, want 32-byte complaint", err)
	}
	if _, err := runVaultAcceptHelperGet(ctx, filepath.Join(t.TempDir(), "no-such-helper"), "secret/data/x", "http://127.0.0.1:9", "tok", 30); err == nil {
		t.Fatal("helper get through a missing helper must error")
	}
}

// gap2VaultHelper writes a small shell helper implementing the
// key-provider contract for one behavior mode.
func gap2VaultHelper(t *testing.T, mode string) string {
	t.Helper()
	var script string
	switch mode {
	case "fail":
		script = "#!/bin/sh\nread -r line\nexit 1\n"
	case "get-fail":
		script = "#!/bin/sh\nread -r line\ncase \"$line\" in\n  \"put \"*) read -r key; exit 0;;\n  *) exit 1;;\nesac\n"
	case "get-mismatch":
		script = "#!/bin/sh\nread -r line\ncase \"$line\" in\n  \"put \"*) read -r key; exit 0;;\n  *) echo \"QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=\"; exit 0;;\nesac\n"
	case "always-ok":
		script = "#!/bin/sh\nread -r line\nexit 0\n"
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	path := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGap2VaultAcceptPutGetFailures(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Run("helper put failure fails the round-trip", func(t *testing.T) {
		check := runVaultAcceptPutGet(ctx, gap2VaultHelper(t, "fail"), "secret/data/x", key, "http://127.0.0.1:9", "tok", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "helper put failed") {
			t.Fatalf("put-fail check = %+v", check)
		}
	})
	t.Run("helper get failure after put fails the round-trip", func(t *testing.T) {
		check := runVaultAcceptPutGet(ctx, gap2VaultHelper(t, "get-fail"), "secret/data/x", key, "http://127.0.0.1:9", "tok", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "helper get failed after a successful put") {
			t.Fatalf("get-fail check = %+v", check)
		}
	})
	t.Run("mismatched key material fails the round-trip", func(t *testing.T) {
		check := runVaultAcceptPutGet(ctx, gap2VaultHelper(t, "get-mismatch"), "secret/data/x", key, "http://127.0.0.1:9", "tok", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "did not return the same 32-byte key") {
			t.Fatalf("mismatch check = %+v", check)
		}
	})
	t.Run("short key fails decryption setup", func(t *testing.T) {
		check := runVaultAcceptDecrypt(make([]byte, 16))
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot encrypt") {
			t.Fatalf("short-key decrypt check = %+v", check)
		}
	})
	t.Run("helper put failure fails rotation", func(t *testing.T) {
		check := runVaultAcceptRotation(ctx, gap2VaultHelper(t, "fail"), "secret/data/x", "http://127.0.0.1:9", "tok", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "helper put of the replacement key failed") {
			t.Fatalf("rotation-fail check = %+v", check)
		}
	})
	t.Run("helper that accepts an invalid token fails the wrong-token check", func(t *testing.T) {
		check := runVaultAcceptWrongToken(ctx, gap2VaultHelper(t, "always-ok"), "secret/data/x", "http://127.0.0.1:9", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "helper accepted an invalid token") {
			t.Fatalf("wrong-token check = %+v", check)
		}
	})
}

// ---------------------------------------------------------------------------
// vault_accept.go: check functions against fake servers
// ---------------------------------------------------------------------------

func TestGap2VaultAcceptHealthBranches(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	t.Run("HTTP 500 fails health", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		check := runVaultAcceptHealth(ctx, client, srv.URL, "tok")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "HTTP 500") {
			t.Fatalf("health 500 check = %+v", check)
		}
	})
	t.Run("invalid JSON fails health", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		check := runVaultAcceptHealth(ctx, client, srv.URL, "tok")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "not valid JSON") {
			t.Fatalf("health bad-JSON check = %+v", check)
		}
	})
	t.Run("sealed vault fails health", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.21.0","storage_type":"raft","sealed":true}`))
		}))
		defer srv.Close()
		check := runVaultAcceptHealth(ctx, client, srv.URL, "tok")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "Vault is sealed") {
			t.Fatalf("health sealed check = %+v", check)
		}
	})
	t.Run("unreachable vault fails health", func(t *testing.T) {
		check := runVaultAcceptHealth(ctx, client, "http://127.0.0.1:9", "tok")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "Vault health request failed") {
			t.Fatalf("health unreachable check = %+v", check)
		}
	})
	t.Run("unknown leader state does not fail health", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/sys/leader" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.21.0","storage_type":"raft","sealed":false}`))
		}))
		defer srv.Close()
		check := runVaultAcceptHealth(ctx, client, srv.URL, "tok")
		if check.Status != externalStatusPass || check.vault.HAKnown {
			t.Fatalf("health leader-unknown check = %+v, want pass with HAKnown=false", check)
		}
	})
}

func TestGap2VaultAcceptTokenMetadataBranches(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	t.Run("lookup denied blocks token metadata", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.lookupDenied = true
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		defer srv.Close()
		var report vaultAcceptReport
		check := runVaultAcceptTokenMetadata(ctx, client, srv.URL, fake.primaryToken, &report)
		if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "lookup-self") {
			t.Fatalf("metadata denied check = %+v", check)
		}
	})
	t.Run("HTTP 500 blocks token metadata", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		var report vaultAcceptReport
		check := runVaultAcceptTokenMetadata(ctx, client, srv.URL, "tok", &report)
		if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "HTTP 500") {
			t.Fatalf("metadata 500 check = %+v", check)
		}
	})
	t.Run("invalid JSON blocks token metadata", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		var report vaultAcceptReport
		check := runVaultAcceptTokenMetadata(ctx, client, srv.URL, "tok", &report)
		if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "not valid JSON") {
			t.Fatalf("metadata bad-JSON check = %+v", check)
		}
	})
}

func TestGap2VaultAcceptPreflightBranches(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	t.Run("occupied path refuses overwrite", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.keyB64 = base64.StdEncoding.EncodeToString(make([]byte, 32))
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		defer srv.Close()
		check := runVaultAcceptRefPreflight(ctx, client, srv.URL, fake.primaryToken, "secret/data/nexaroute/acceptance/master-key")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "already contains data") {
			t.Fatalf("preflight occupied check = %+v", check)
		}
	})
	t.Run("forbidden path fails without writing", func(t *testing.T) {
		fake := newFakeVaultAccept()
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		defer srv.Close()
		check := runVaultAcceptRefPreflight(ctx, client, srv.URL, "wrong-token", "secret/data/nexaroute/acceptance/master-key")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "HTTP 403") {
			t.Fatalf("preflight forbidden check = %+v", check)
		}
	})
	t.Run("server error fails preflight", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		check := runVaultAcceptRefPreflight(ctx, client, srv.URL, "tok", "secret/data/x")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "HTTP 500") {
			t.Fatalf("preflight 500 check = %+v", check)
		}
	})
	t.Run("unreachable vault fails preflight", func(t *testing.T) {
		check := runVaultAcceptRefPreflight(ctx, client, "http://127.0.0.1:9", "tok", "secret/data/x")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot read the selected Vault path") {
			t.Fatalf("preflight unreachable check = %+v", check)
		}
	})
}

// gap2NoopRevokeVault is a minimal Vault stand-in whose revoke-self
// endpoint acknowledges but never revokes: the acceptance check must
// detect that a "revoked" token still works.
type gap2NoopRevokeVault struct {
	primaryToken string
	keyB64       string
}

func (f *gap2NoopRevokeVault) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/auth/token/create":
		writeVaultAcceptJSON(w, map[string]any{"auth": map[string]any{"client_token": "child-noop"}})
		return
	case "/v1/auth/token/revoke-self":
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token := r.Header.Get("X-Vault-Token")
	if token != f.primaryToken && token != "child-noop" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && r.Method == http.MethodGet {
		writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{
			"data": map[string]any{"key_b64": f.keyB64}, "metadata": map[string]any{"version": 1},
		}})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestGap2VaultAcceptRevokedTokenStillWorksFails(t *testing.T) {
	ctx := context.Background()
	fake := &gap2NoopRevokeVault{
		primaryToken: "primary-token-7f31c9",
		keyB64:       base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	check := runVaultAcceptRevokedToken(ctx, &http.Client{Timeout: 5 * time.Second},
		vaultAcceptHelperPath(t), "secret/data/nexaroute/acceptance/master-key",
		srv.URL, fake.primaryToken, 5*time.Second, 30)
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "helper accepted a revoked child token") {
		t.Fatalf("revoked-token check = %+v", check)
	}
}

func TestGap2VaultAcceptOutageAndCleanupFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("helper succeeding against the outage endpoint fails the check", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.keyB64 = base64.StdEncoding.EncodeToString(make([]byte, 32))
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		defer srv.Close()
		check := runVaultAcceptOutage(ctx, vaultAcceptHelperPath(t), "secret/data/nexaroute/acceptance/master-key", srv.URL, fake.primaryToken, 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "succeeded against the deliberately unreachable outage endpoint") {
			t.Fatalf("outage check = %+v", check)
		}
	})
	t.Run("denied cleanup delete blocks cleanup", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.deleteDenied = true
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		defer srv.Close()
		check := runVaultAcceptCleanup(ctx, &http.Client{Timeout: 5 * time.Second}, srv.URL, fake.primaryToken, "secret/data/nexaroute/acceptance/master-key")
		if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "token lacks delete") {
			t.Fatalf("cleanup denied check = %+v, want blocked", check)
		}
	})
	t.Run("server error fails cleanup", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		check := runVaultAcceptCleanup(ctx, &http.Client{Timeout: 5 * time.Second}, srv.URL, "tok", "secret/data/nexaroute/acceptance/master-key")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cleanup returned HTTP 500") {
			t.Fatalf("cleanup 500 check = %+v, want fail", check)
		}
	})
	t.Run("ref without a data segment cannot derive a metadata path", func(t *testing.T) {
		check := runVaultAcceptCleanup(ctx, &http.Client{Timeout: 5 * time.Second}, "http://127.0.0.1:9", "tok", "secret/nodata/x")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot derive the KV v2 metadata path") {
			t.Fatalf("cleanup bad-ref check = %+v", check)
		}
	})
}

func TestGap2VaultAcceptPutFailureSkipsDecryptAndSkipCleanup(t *testing.T) {
	fake := newFakeVaultAccept()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", fake.primaryToken)
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	runVaultAccept([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", gap2VaultHelper(t, "fail"),
		"--confirm-live",
		"--out", out,
	}, &stdout, &stderr)
	report := readVaultAcceptReport(t, out)
	if vaultAcceptStatus(report, "vault.gateway_decrypt") != externalStatusBlocked {
		t.Fatalf("put-fail run decrypt = %q, want blocked-skipped", vaultAcceptStatus(report, "vault.gateway_decrypt"))
	}
	// The full harness now includes the expired-token check (added by
	// the vault-accept gaps work): the fixture child token must really
	// expire, or the check polls to its 10s deadline and fails. Every
	// other full-harness fixture sets childTTL for exactly this reason.
	skipCleanupFake := newFakeVaultAccept()
	skipCleanupFake.childTTL = time.Second
	code, report2, _, _, _ := runVaultAcceptFixture(t, skipCleanupFake, "--skip-cleanup")
	if code != 0 {
		t.Fatalf("skip-cleanup fixture exit=%d, want 0", code)
	}
	if vaultAcceptStatus(report2, "vault.cleanup") != externalStatusNotRun {
		t.Fatalf("skip-cleanup run cleanup = %q, want NOT_RUN", vaultAcceptStatus(report2, "vault.cleanup"))
	}
}

func TestGap2VaultAcceptFinishAndReportWriteErrors(t *testing.T) {
	t.Run("unwritable report path fails finish", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GAP2_VAULT_ADDR_UNSET2", "")
		var stdout, stderr bytes.Buffer
		code := runVaultAccept([]string{
			"--ref", "secret/data/nexaroute/acceptance/master-key",
			"--helper", vaultAcceptHelperPath(t),
			"--confirm-live",
			"--addr-env", "GAP2_VAULT_ADDR_UNSET2", "--token-env", "GAP2_VAULT_TOKEN_UNSET2",
			"--out", filepath.Join(blocker, "report.json"),
		}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "cannot write report") {
			t.Fatalf("finish write-error exit=%d stderr=%q", code, stderr.String())
		}
	})
	t.Run("report mkdir and temp creation errors", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeVaultAcceptReport(filepath.Join(blocker, "report.json"), vaultAcceptReport{}); err == nil {
			t.Fatal("report under a regular file must fail mkdir")
		}
		if err := writeVaultAcceptReport(filepath.Join("/proc", "report.json"), vaultAcceptReport{}); err == nil {
			t.Fatal("report temp creation under /proc must fail")
		}
	})
	t.Run("all-not-run report gains the not-run limitation", func(t *testing.T) {
		report := vaultAcceptReport{}
		report.Checks = append(report.Checks, vaultAcceptCheck{ID: "x", Selected: true, Status: externalStatusNotRun})
		finalizeVaultAcceptReport(&report)
		if report.Outcome != externalStatusNotRun {
			t.Fatalf("outcome = %q, want NOT_RUN", report.Outcome)
		}
		found := false
		for _, lim := range report.Limitations {
			if strings.Contains(lim, "not acceptance evidence") {
				found = true
			}
		}
		if !found {
			t.Fatalf("limitations = %v, want the not-run limitation", report.Limitations)
		}
	})
}

// ---------------------------------------------------------------------------
// comfyui_accept.go: flags, preflight, video config
// ---------------------------------------------------------------------------

func TestGap2ComfyUIAcceptFlagValidation(t *testing.T) {
	run := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := runComfyUIAccept(args, &stdout, &stderr)
		return code, stderr.String()
	}
	if code, stderr := run("--confirm-live", "--timeout", "0s"); code != 2 || !strings.Contains(stderr, "--timeout must be between") {
		t.Fatalf("timeout validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--confirm-live", "--poll-interval", "31s"); code != 2 || !strings.Contains(stderr, "--poll-interval must be between") {
		t.Fatalf("poll-interval validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--confirm-live", "--prompt", "   "); code != 2 || !strings.Contains(stderr, "--prompt must not be empty") {
		t.Fatalf("prompt validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--confirm-live", "--duration", "0", "--fps", "0"); code != 2 || !strings.Contains(stderr, "--duration must be positive") {
		t.Fatalf("duration/fps validation exit=%d stderr=%q", code, stderr)
	}
	if code, stderr := run("--confirm-live", "--config", filepath.Join(t.TempDir(), "no-such-config.json")); code != 2 || !strings.Contains(stderr, "cannot load config") {
		t.Fatalf("config-load validation exit=%d stderr=%q", code, stderr)
	}
}

func TestGap2ComfyUIAcceptDefaultOutPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	configPath := writeTestConfig(t, config.Default())
	var stdout, stderr bytes.Buffer
	code := runComfyUIAccept([]string{"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t)}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("unprovisioned run exit=%d, want 3 (blocked)", code)
	}
	entries, err := filepath.Glob(filepath.Join(".artifacts", "comfyui-acceptance", "comfyui-acceptance-*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("default out path produced %v, err=%v, want exactly one report", entries, err)
	}
}

func gap2ComfyAcceptConfig(t *testing.T, baseURL string) (config.Config, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Video = config.VideoConfig{
		Enabled: true, AuthTokenEnv: "NEXAROUTE_TEST_VIDEO_TOKEN",
		ComfyUIEnabled: true, ComfyUIBaseURL: baseURL, ComfyUIWorkflowPath: comfyAcceptWorkflow(t),
		ComfyUIModel: "local-video", ComfyUIMaxDurationSeconds: 30,
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}
	return cfg, filepath.Join(t.TempDir(), "config.json")
}

func TestGap2ComfyUIAcceptPreflightBlocks(t *testing.T) {
	run := func(cfg config.Config, configPath string) (int, comfyAcceptReport) {
		report := comfyAcceptReport{}
		code := runComfyUIAcceptChecks(&report, cfg, configPath, t.TempDir(), "ffprobe", "a red cube", 1, 24, 0, 0, 0, time.Second, 10*time.Millisecond)
		return code, report
	}
	t.Run("video disabled blocks before any contact", func(t *testing.T) {
		cfg := config.Default()
		code, report := run(cfg, "")
		if code != 3 {
			t.Fatalf("disabled preflight code=%d, want 3", code)
		}
		if status, _ := comfyAcceptCheckStatus(report, "comfyui:preflight"); status != externalStatusBlocked {
			t.Fatalf("disabled preflight status=%q, want blocked", status)
		}
		if status, _ := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusNotRun {
			t.Fatalf("disabled inference status=%q, want NOT_RUN", status)
		}
	})
	t.Run("missing target blocks before any contact", func(t *testing.T) {
		cfg := config.Default()
		cfg.Video.Enabled = true
		cfg.Video.ComfyUIEnabled = true
		code, report := run(cfg, "")
		if code != 3 {
			t.Fatalf("missing-target preflight code=%d, want 3", code)
		}
		if status, _ := comfyAcceptCheckStatus(report, "comfyui:preflight"); status != externalStatusBlocked {
			t.Fatalf("missing-target preflight status=%q, want blocked", status)
		}
	})
	t.Run("unreadable workflow blocks before any contact", func(t *testing.T) {
		cfg := config.Default()
		cfg.Video.Enabled = true
		cfg.Video.ComfyUIEnabled = true
		cfg.Video.ComfyUIBaseURL = "http://127.0.0.1:9"
		cfg.Video.ComfyUIWorkflowPath = filepath.Join(t.TempDir(), "no-such-workflow.json")
		code, report := run(cfg, "")
		if code != 3 {
			t.Fatalf("unreadable-workflow preflight code=%d, want 3", code)
		}
		if status, reason := comfyAcceptCheckStatus(report, "comfyui:preflight"); status != externalStatusBlocked || !strings.Contains(reason, "not readable") {
			t.Fatalf("unreadable-workflow preflight status=%q reason=%q", status, reason)
		}
	})
}

func TestGap2ComfyAcceptVideoConfigJoinsRelativeWorkflow(t *testing.T) {
	vc := comfyAcceptVideoConfig(config.VideoConfig{ComfyUIWorkflowPath: "wf.json"}, filepath.Join("dir", "config.json"))
	if vc.ComfyUIWorkflowPath != filepath.Join("dir", "wf.json") {
		t.Fatalf("relative workflow = %q, want dir-joined", vc.ComfyUIWorkflowPath)
	}
	vc = comfyAcceptVideoConfig(config.VideoConfig{ComfyUIWorkflowPath: "/abs/wf.json"}, filepath.Join("dir", "config.json"))
	if vc.ComfyUIWorkflowPath != "/abs/wf.json" {
		t.Fatalf("absolute workflow = %q, want unchanged", vc.ComfyUIWorkflowPath)
	}
}

// ---------------------------------------------------------------------------
// comfyui_accept.go: poll cancellation branches
// ---------------------------------------------------------------------------

func TestGap2ComfyAcceptPollCancellation(t *testing.T) {
	t.Run("cancel during status fetch wraps the last error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(5 * time.Second)
		}))
		defer srv.Close()
		cfg, _ := gap2ComfyAcceptConfig(t, srv.URL)
		provider, err := providers.NewComfyUI(comfyAcceptVideoConfig(cfg.Video, ""), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		_, _, err = pollComfyAcceptJob(ctx, provider, "prompt-x", 10*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "last status error") {
			t.Fatalf("cancel-during-fetch error = %v, want last-status-error wrap", err)
		}
	})
	t.Run("cancel during sleep after a failed poll wraps the last error", func(t *testing.T) {
		// The counter-observed cancel in the first version of this
		// test could land inside GetJob instead of the sleep, so it
		// exercised the wrong wrap. This version makes the first
		// poll fail slowly (200ms server sleep) and cancels at 1s,
		// deterministically inside the 30s timer sleep.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		cfg, _ := gap2ComfyAcceptConfig(t, srv.URL)
		provider, err := providers.NewComfyUI(comfyAcceptVideoConfig(cfg.Video, ""), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(time.Second, cancel)
		_, _, err = pollComfyAcceptJob(ctx, provider, "prompt-x", 30*time.Second)
		if err == nil || !strings.Contains(err.Error(), "last status error") {
			t.Fatalf("cancel-during-sleep error = %v, want last-status-error wrap", err)
		}
	})
}

// ---------------------------------------------------------------------------
// comfyui_accept.go: asset verification and access
// ---------------------------------------------------------------------------

func gap2ComfySinkAsset(t *testing.T, sink *storage.Local, data []byte) video.Asset {
	t.Helper()
	saved, err := sink.Put(context.Background(), video.Asset{
		Filename: "clip.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, data)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestGap2ComfyAcceptVerifyAssetBranches(t *testing.T) {
	newSink := func(t *testing.T) *storage.Local {
		t.Helper()
		sink, err := storage.NewLocal(t.TempDir(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return sink
	}
	probe := &render.Renderer{FFprobe: comfyAcceptProbe(t)}
	t.Run("non-positive size rejected", func(t *testing.T) {
		sink := newSink(t)
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, video.Asset{SizeBytes: 0}); err == nil || !strings.Contains(err.Error(), "non-positive size") {
			t.Fatalf("size-zero error = %v", err)
		}
	})
	t.Run("non-video content type rejected", func(t *testing.T) {
		sink := newSink(t)
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, video.Asset{SizeBytes: 10, ContentType: "image/png", SHA256: strings.Repeat("a", 64)}); err == nil || !strings.Contains(err.Error(), "non-video content type") {
			t.Fatalf("content-type error = %v", err)
		}
	})
	t.Run("missing SHA rejected", func(t *testing.T) {
		sink := newSink(t)
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, video.Asset{SizeBytes: 10, ContentType: "video/mp4", SHA256: "abc"}); err == nil || !strings.Contains(err.Error(), "no valid SHA-256") {
			t.Fatalf("short-SHA error = %v", err)
		}
	})
	t.Run("malformed SHA rejected", func(t *testing.T) {
		sink := newSink(t)
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, video.Asset{SizeBytes: 10, ContentType: "video/mp4", SHA256: strings.Repeat("z", 64)}); err == nil || !strings.Contains(err.Error(), "malformed SHA-256") {
			t.Fatalf("malformed-SHA error = %v", err)
		}
	})
	t.Run("unreadable asset rejected", func(t *testing.T) {
		sink := newSink(t)
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, video.Asset{
			SizeBytes: 10, ContentType: "video/mp4", SHA256: strings.Repeat("a", 64),
			URI: filepath.Join(t.TempDir(), "missing.mp4"),
		}); err == nil || !strings.Contains(err.Error(), "read back persisted asset") {
			t.Fatalf("unreadable error = %v", err)
		}
	})
	t.Run("missing provider metadata rejected", func(t *testing.T) {
		sink := newSink(t)
		saved := gap2ComfySinkAsset(t, sink, []byte("fake-mp4-bytes"))
		saved.Metadata = map[string]string{"project_id": "p", "job_id": "j", "node_id": "9", "filename": "clip.mp4"}
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, saved); err == nil || !strings.Contains(err.Error(), "provider=comfyui") {
			t.Fatalf("provider-metadata error = %v", err)
		}
	})
	t.Run("missing node_id rejected", func(t *testing.T) {
		sink := newSink(t)
		saved := gap2ComfySinkAsset(t, sink, []byte("fake-mp4-bytes"))
		saved.Metadata = map[string]string{"project_id": "p", "job_id": "j", "provider": "comfyui", "filename": "clip.mp4"}
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, saved); err == nil || !strings.Contains(err.Error(), "node_id") {
			t.Fatalf("node_id error = %v", err)
		}
	})
	t.Run("missing filename rejected", func(t *testing.T) {
		sink := newSink(t)
		saved := gap2ComfySinkAsset(t, sink, []byte("fake-mp4-bytes"))
		saved.Metadata = map[string]string{"project_id": "p", "job_id": "j", "provider": "comfyui", "node_id": "9"}
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, saved); err == nil || !strings.Contains(err.Error(), "filename") {
			t.Fatalf("filename error = %v", err)
		}
	})
	t.Run("ffprobe failure rejected", func(t *testing.T) {
		sink := newSink(t)
		saved := gap2ComfySinkAsset(t, sink, []byte("fake-mp4-bytes"))
		saved.Metadata = map[string]string{"project_id": "p", "job_id": "j", "provider": "comfyui", "node_id": "9", "filename": "clip.mp4"}
		failingProbe := &render.Renderer{FFprobe: gap2FailingProbe(t)}
		if _, err := verifyComfyAcceptAsset(context.Background(), sink, failingProbe, saved); err == nil || !strings.Contains(err.Error(), "verify playable media") {
			t.Fatalf("ffprobe error = %v", err)
		}
	})
}

// gap2FailingProbe writes an executable script that always fails, so
// ffprobe-dependent verification can be exercised without a real binary.
func gap2FailingProbe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGap2ComfyAcceptAssetAccessBranches(t *testing.T) {
	t.Run("asset outside the root escapes", func(t *testing.T) {
		runDir := t.TempDir()
		if err := verifyComfyAcceptAssetAccess(runDir, video.Asset{URI: filepath.Join(t.TempDir(), "outside.mp4")}); err == nil || !strings.Contains(err.Error(), "escapes the acceptance asset root") {
			t.Fatalf("escape error = %v", err)
		}
	})
	t.Run("unresolvable root fails resolution", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "no-such-root")
		if err := verifyComfyAcceptAssetAccess(root, video.Asset{URI: filepath.Join(root, "clip.mp4")}); err == nil || !strings.Contains(err.Error(), "resolve acceptance asset root") {
			t.Fatalf("root-resolve error = %v", err)
		}
	})
	t.Run("unresolvable asset fails resolution", func(t *testing.T) {
		runDir := t.TempDir()
		if err := verifyComfyAcceptAssetAccess(runDir, video.Asset{URI: filepath.Join(runDir, "missing.mp4")}); err == nil || !strings.Contains(err.Error(), "resolve persisted asset") {
			t.Fatalf("asset-resolve error = %v", err)
		}
	})
	t.Run("symlink escape fails resolution", func(t *testing.T) {
		runDir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.mp4")
		if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(runDir, "clip.mp4")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := verifyComfyAcceptAssetAccess(runDir, video.Asset{URI: link}); err == nil || !strings.Contains(err.Error(), "through a symlink") {
			t.Fatalf("symlink-escape error = %v", err)
		}
	})
	t.Run("directory asset is not a regular file", func(t *testing.T) {
		runDir := t.TempDir()
		if err := verifyComfyAcceptAssetAccess(runDir, video.Asset{URI: runDir}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("directory error = %v", err)
		}
	})
	t.Run("group-readable asset fails the mode check", func(t *testing.T) {
		runDir := t.TempDir()
		path := filepath.Join(runDir, "clip.mp4")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := verifyComfyAcceptAssetAccess(runDir, video.Asset{URI: path}); err == nil || !strings.Contains(err.Error(), "want owner-only 0600") {
			t.Fatalf("mode error = %v", err)
		}
	})
}

func TestGap2ComfyAcceptFailClosedWithoutPromptOrDir(t *testing.T) {
	report := &comfyAcceptReport{}
	comfyAcceptFailClosed(report, "", nil)
	if report.Cleanup == nil || report.Cleanup.RemoteStatus != externalStatusNotRun || report.Cleanup.LocalStatus != externalStatusNotRun {
		t.Fatalf("fail-closed without prompt/dir = %+v, want both NOT_RUN", report.Cleanup)
	}
	if !strings.Contains(report.Cleanup.RemoteReason, "no ComfyUI prompt ID") {
		t.Fatalf("fail-closed remote reason = %q", report.Cleanup.RemoteReason)
	}
}

func TestGap2ComfyAcceptReportWriteErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeComfyAcceptReport(filepath.Join(blocker, "report.json"), comfyAcceptReport{}); err == nil {
		t.Fatal("report under a regular file must fail mkdir")
	}
	if err := writeComfyAcceptReport(filepath.Join("/proc", "report.json"), comfyAcceptReport{}); err == nil {
		t.Fatal("report temp creation under /proc must fail")
	}
}

// ---------------------------------------------------------------------------
// comfyui_accept.go: end-to-end check flows against a fake server
// ---------------------------------------------------------------------------

func gap2ComfyServer(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-1"}`))
		case r.URL.Path == "/history/prompt-1":
			switch mode {
			case "processing":
				_, _ = w.Write([]byte(`{"prompt-1":{"status":{"status_str":"success","completed":false},"outputs":{}}}`))
			case "completed-mp4":
				_, _ = w.Write([]byte(`{"prompt-1":{"status":{"status_str":"success","completed":true},"outputs":{"9":{"videos":[{"filename":"out.mp4","subfolder":"","type":"output"}]}}}}`))
			case "completed-gif":
				_, _ = w.Write([]byte(`{"prompt-1":{"status":{"status_str":"success","completed":true},"outputs":{"9":{"gifs":[{"filename":"out.gif","subfolder":"","type":"output"}]}}}}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		case r.URL.Path == "/view":
			if mode == "view-404" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(comfyAcceptMP4())
		case r.URL.Path == "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func gap2ComfyRunChecks(t *testing.T, srv *httptest.Server, ffprobe string, timeout, pollInterval time.Duration) (int, comfyAcceptReport) {
	t.Helper()
	cfg, configPath := gap2ComfyAcceptConfig(t, srv.URL)
	assetDir := t.TempDir()
	report := newComfyAcceptReport(cfg, assetDir, ffprobe, timeout, pollInterval)
	code := runComfyUIAcceptChecks(&report, cfg, configPath, assetDir, ffprobe, "a red cube", 1, 24, 0, 0, 0, timeout, pollInterval)
	return code, report
}

func TestGap2ComfyAcceptSubmitFailureFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/system_stats" {
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
			return
		}
		http.Error(w, "submit exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()
	code, report := gap2ComfyRunChecks(t, srv, comfyAcceptProbe(t), 5*time.Second, 10*time.Millisecond)
	if code != 1 {
		t.Fatalf("submit-fail code=%d, want 1", code)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:submit"); status != externalStatusFail {
		t.Fatalf("submit status=%q, want fail", status)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("submit-fail cleanup = %+v, want local pass (workspace removed)", report.Cleanup)
	}
}

func TestGap2ComfyAcceptPollTimeoutFailsClosed(t *testing.T) {
	srv := gap2ComfyServer(t, "processing")
	defer srv.Close()
	code, report := gap2ComfyRunChecks(t, srv, comfyAcceptProbe(t), 300*time.Millisecond, 10*time.Millisecond)
	if code != 1 {
		t.Fatalf("poll-timeout code=%d, want 1", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:poll"); status != externalStatusFail || !strings.Contains(reason, "timed out") {
		t.Fatalf("poll status=%q reason=%q, want fail with timeout", status, reason)
	}
	if report.Job == nil || report.Job.State != "processing" {
		t.Fatalf("poll-timeout job = %+v, want processing state recorded", report.Job)
	}
}

func TestGap2ComfyAcceptDownloadFailureFailsClosed(t *testing.T) {
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-1"}`))
		case r.URL.Path == "/history/prompt-1":
			_, _ = w.Write([]byte(`{"prompt-1":{"status":{"status_str":"success","completed":true},"outputs":{"9":{"videos":[{"filename":"out.mp4","subfolder":"","type":"output"}]}}}}`))
		case r.URL.Path == "/view":
			http.NotFound(w, r)
		case r.URL.Path == "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv2.Close()
	code, report := gap2ComfyRunChecks(t, srv2, comfyAcceptProbe(t), 5*time.Second, 10*time.Millisecond)
	if code != 1 {
		t.Fatalf("download-fail code=%d, want 1", code)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:download"); status != externalStatusFail {
		t.Fatalf("download status=%q, want fail", status)
	}
}

func TestGap2ComfyAcceptAssetIntegrityFailureFailsClosed(t *testing.T) {
	srv := gap2ComfyServer(t, "completed-mp4")
	defer srv.Close()
	code, report := gap2ComfyRunChecks(t, srv, gap2FailingProbe(t), 5*time.Second, 10*time.Millisecond)
	if code != 1 {
		t.Fatalf("integrity-fail code=%d, want 1", code)
	}
	if status, _ := comfyAcceptCheckStatus(report, "comfyui:asset_integrity"); status != externalStatusFail {
		t.Fatalf("integrity status=%q, want fail", status)
	}
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusPass {
		t.Fatalf("integrity-fail cleanup = %+v, want local pass (workspace removed)", report.Cleanup)
	}
}

// ---------------------------------------------------------------------------
// backup.go: live snapshot failures, durable writes, canonical paths,
// bundle errors, and unreadable-source branches
// ---------------------------------------------------------------------------

func TestGap2FetchLiveSecuritySnapshotFailures(t *testing.T) {
	t.Run("control-character URL fails request construction", func(t *testing.T) {
		if _, err := fetchLiveSecuritySnapshot("http://127.0.0.1:9/\x7f", ""); err == nil || !strings.Contains(err.Error(), "build live snapshot request") {
			t.Fatalf("fetchLive error = %v, want request-construction failure", err)
		}
	})
	t.Run("dead gateway fails without fallback", func(t *testing.T) {
		if _, err := fetchLiveSecuritySnapshot("http://127.0.0.1:9", ""); err == nil || !strings.Contains(err.Error(), "cannot reach the running gateway") || !strings.Contains(err.Error(), "no fallback") {
			t.Fatalf("fetchLive error = %v, want unreachable diagnostic with no-fallback note", err)
		}
	})
	for _, tc := range []struct {
		name string
		code int
		want string
	}{
		{"501 maps to backend-unsupported", http.StatusNotImplemented, "not supported by the gateway's security store backend"},
		{"500 maps to the generic failure", http.StatusInternalServerError, "live snapshot request failed (HTTP 500)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "detail", tc.code)
			}))
			defer srv.Close()
			if _, err := fetchLiveSecuritySnapshot(srv.URL, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fetchLive error = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("truncated body fails the read", func(t *testing.T) {
		if _, err := fetchLiveSecuritySnapshot(gap2RawTCPServer(t), ""); err == nil || !strings.Contains(err.Error(), "read live security snapshot") {
			t.Fatalf("fetchLive error = %v, want read failure", err)
		}
	})
}

func TestGap2BackupDurableWritesAndCanonicalPaths(t *testing.T) {
	t.Run("saved-copy write to a full device fails", func(t *testing.T) {
		if err := writeSavedCopyDurable("/dev/full", []byte("x")); err == nil {
			t.Fatal("write to /dev/full must fail")
		}
	})
	t.Run("journal manifest rename onto a directory fails", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "manifest.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJournalManifest(dir, &restoreJournalManifest{}); err == nil {
			t.Fatal("manifest rename onto a directory must fail")
		}
	})
	t.Run("sync of a missing directory fails", func(t *testing.T) {
		if err := syncDir(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
			t.Fatal("sync of a missing directory must fail")
		}
	})
	t.Run("relative paths join the working directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		got, err := canonicalPath(filepath.Join("sub", "file"))
		if err != nil || got != filepath.Join(dir, "sub", "file") {
			t.Fatalf("relative canonical = %q, err=%v", got, err)
		}
	})
	t.Run("dot components are skipped", func(t *testing.T) {
		dir := t.TempDir()
		// NOTE: filepath.Join would clean the dot away before
		// canonicalPath ever sees it, so concatenate instead.
		got, err := canonicalPath(dir + "/./file")
		if err != nil || got != filepath.Join(dir, "file") {
			t.Fatalf("dot canonical = %q, err=%v", got, err)
		}
	})
	t.Run("dotdot components walk to the parent", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := canonicalPath(sub + "/../file")
		if err != nil || got != filepath.Join(dir, "file") {
			t.Fatalf("dotdot canonical = %q, err=%v", got, err)
		}
	})
	t.Run("negative-size bundle entry fails the tar header", func(t *testing.T) {
		stage := t.TempDir()
		if err := os.WriteFile(filepath.Join(stage, "x"), []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "b.tar")
		err := writeBundle(out, []byte("{}"), []backupManifestFile{{Name: "x", Size: -1}}, stage)
		if err == nil || !strings.Contains(err.Error(), "negative size") {
			t.Fatalf("writeBundle error = %v, want negative-size failure", err)
		}
	})
}

func TestGap2BackupUnreadableKeyAndControlPlaneState(t *testing.T) {
	t.Run("key file as a directory fails the read", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		if err := os.Mkdir(configPath+".key", 0o700); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 1 || !strings.Contains(stderr, "read master key file") {
			t.Fatalf("key-dir backup exit=%d stderr=%q, want master-key read failure", code, stderr)
		}
		_ = dir
	})
	t.Run("control-plane state as a directory fails the read", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		cpDir := filepath.Join(dir, "cpstate")
		if err := os.Mkdir(cpDir, 0o700); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
			cfg.ControlPlane.StatePath = cpDir
		})
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 1 || !strings.Contains(stderr, "read control-plane state") {
			t.Fatalf("cp-dir backup exit=%d stderr=%q, want control-plane read failure", code, stderr)
		}
	})
}

func TestGap2BackupLiveSnapshotVerificationPaths(t *testing.T) {
	t.Run("garbage live snapshot fails probe opening", func(t *testing.T) {
		_, configPath := opsFixture(t)
		ts := liveSnapshotServer(t, nil, "", "tampered", []byte("not a bbolt store"))
		defer ts.Close()
		out := filepath.Join(t.TempDir(), "live.tar")
		code, _, stderr := gap2RunBackup(t, configPath, out, "--live", "--admin-url", ts.URL)
		if code != 1 || !strings.Contains(stderr, "open live security snapshot for verification") {
			t.Fatalf("garbage live backup exit=%d stderr=%q, want probe-open failure", code, stderr)
		}
	})
	t.Run("tampered live snapshot with override warns and succeeds", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		storePath := fixtureSecurityPath(dir)
		tamperAuditStoreFile(t, storePath)
		tampered, err := os.ReadFile(storePath)
		if err != nil {
			t.Fatal(err)
		}
		ts := liveSnapshotServer(t, nil, "", "tampered", tampered)
		defer ts.Close()
		out := filepath.Join(t.TempDir(), "live.tar")
		code, stdout, stderr := gap2RunBackup(t, configPath, out, "--live", "--admin-url", ts.URL, "--allow-broken-audit-chain")
		if code != 0 {
			t.Fatalf("override live backup exit=%d stdout=%q stderr=%q, want 0", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "WARNING: live security snapshot audit chain FAILED verification") {
			t.Fatalf("override live backup stdout=%q, want the broken-chain WARNING", stdout)
		}
		manifest := readBundleManifest(t, out)
		if !strings.Contains(manifest.Notes, "FAILED verification") {
			t.Fatalf("override live manifest notes=%q, want the FAILED-verification record", manifest.Notes)
		}
	})
}

// ---------------------------------------------------------------------------
// Wave 2: blocks that needed exact block-range mapping to target.
// ---------------------------------------------------------------------------

// Additional vault helper modes for wave 2.
func gap2VaultHelper2(t *testing.T, mode string) string {
	t.Helper()
	var script string
	switch mode {
	case "put-noisy":
		script = "#!/bin/sh\nread -r line\ncase \"$line\" in\n  \"put \"*) read -r key; echo \"leaked-on-put\"; exit 0;;\n  *) exit 1;;\nesac\n"
	case "get-garbage":
		script = "#!/bin/sh\nread -r line\ncase \"$line\" in\n  \"put \"*) read -r key; exit 0;;\n  *) echo \"not-base64-at-all\"; exit 0;;\nesac\n"
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	path := filepath.Join(t.TempDir(), "helper2.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGap2VaultAcceptFlagParseError(t *testing.T) {
	var stderr bytes.Buffer
	code := runVaultAccept([]string{"--no-such-flag"}, &stderr, &stderr)
	if code != 2 {
		t.Fatalf("unknown-flag exit=%d, want 2", code)
	}
}

func TestGap2VaultAcceptTokenLookupRequestError(t *testing.T) {
	check := runVaultAcceptTokenMetadata(context.Background(), &http.Client{Timeout: 5 * time.Second}, "http://127.0.0.1:9", "tok", &vaultAcceptReport{})
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "Vault token lookup failed") {
		t.Fatalf("lookup request-error check = %+v", check)
	}
}

func TestGap2VaultAcceptNoisyHelperBranches(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Run("put writing to stdout fails the round-trip", func(t *testing.T) {
		check := runVaultAcceptPutGet(ctx, gap2VaultHelper2(t, "put-noisy"), "secret/data/x", key, "http://127.0.0.1:9", "tok", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "unexpectedly wrote to stdout") {
			t.Fatalf("put-noisy check = %+v", check)
		}
	})
	t.Run("rotation get failure fails rotation", func(t *testing.T) {
		check := runVaultAcceptRotation(ctx, gap2VaultHelper(t, "get-fail"), "secret/data/x", "http://127.0.0.1:9", "tok", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "helper get after replacement failed") {
			t.Fatalf("rotation get-fail check = %+v", check)
		}
	})
	t.Run("rotation mismatch fails rotation", func(t *testing.T) {
		check := runVaultAcceptRotation(ctx, gap2VaultHelper(t, "get-mismatch"), "secret/data/x", "http://127.0.0.1:9", "tok", 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "not read back byte-identically") {
			t.Fatalf("rotation mismatch check = %+v", check)
		}
	})
	// NOTE: the wrong-token / outage / revoked stdout-on-failure
	// branches are deliberately not tested: runVaultAcceptHelper
	// returns nil output whenever the helper exits non-zero, so a
	// failing helper can never present stdout to those checks. The
	// branches are defensive-only; the failing-helper behavior itself
	// is covered by the put/get/rotation tests above.
	t.Run("invalid helper key material fails helper get", func(t *testing.T) {
		if _, err := runVaultAcceptHelperGet(ctx, gap2VaultHelper2(t, "get-garbage"), "secret/data/x", "http://127.0.0.1:9", "tok", 30); err == nil || !strings.Contains(err.Error(), "invalid key material") {
			t.Fatalf("helper get garbage error = %v, want invalid-material complaint", err)
		}
	})
}

// gap2RevokedVariations serves the token-create / read / revoke
// endpoints with scripted failures for the revoked-token branches.
type gap2RevokedVariations struct {
	createCode int
	createBody string
	revokeCode int
	revoked    bool
}

func (f *gap2RevokedVariations) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/auth/token/create":
		if f.createBody != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.createBody))
			return
		}
		w.WriteHeader(f.createCode)
		if f.createCode >= 200 && f.createCode < 300 {
			writeVaultAcceptJSON(w, map[string]any{"auth": map[string]any{"client_token": "child-var"}})
		}
		return
	case "/v1/auth/token/revoke-self":
		w.WriteHeader(f.revokeCode)
		if f.revokeCode >= 200 && f.revokeCode < 300 {
			f.revoked = true
		}
		return
	}
	token := r.Header.Get("X-Vault-Token")
	if token != "primary-token-7f31c9" && token != "child-var" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if token == "child-var" && f.revoked {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && r.Method == http.MethodGet {
		writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{
			"data": map[string]any{"key_b64": base64.StdEncoding.EncodeToString(make([]byte, 32))}, "metadata": map[string]any{"version": 1},
		}})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestGap2VaultAcceptRevokedTokenBranches(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	t.Run("create request error blocks the check", func(t *testing.T) {
		check := runVaultAcceptRevokedToken(ctx, client, vaultAcceptHelperPath(t), "secret/data/x", "http://127.0.0.1:9", "tok", 5*time.Second, 30)
		if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "cannot create a disposable child token") {
			t.Fatalf("revoked create-error check = %+v", check)
		}
	})
	t.Run("create HTTP 500 blocks the check", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc((&gap2RevokedVariations{createCode: 500, revokeCode: 204}).handler))
		defer srv.Close()
		check := runVaultAcceptRevokedToken(ctx, client, vaultAcceptHelperPath(t), "secret/data/x", srv.URL, "primary-token-7f31c9", 5*time.Second, 30)
		if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "HTTP 500") {
			t.Fatalf("revoked create-500 check = %+v", check)
		}
	})
	t.Run("create without a token fails the check", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc((&gap2RevokedVariations{createBody: `{}`, revokeCode: 204}).handler))
		defer srv.Close()
		check := runVaultAcceptRevokedToken(ctx, client, vaultAcceptHelperPath(t), "secret/data/x", srv.URL, "primary-token-7f31c9", 5*time.Second, 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "did not contain a client token") {
			t.Fatalf("revoked no-token check = %+v", check)
		}
	})
	t.Run("revoke failure fails the check", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc((&gap2RevokedVariations{createCode: 200, revokeCode: 500}).handler))
		defer srv.Close()
		check := runVaultAcceptRevokedToken(ctx, client, vaultAcceptHelperPath(t), "secret/data/x", srv.URL, "primary-token-7f31c9", 5*time.Second, 30)
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "could not revoke") {
			t.Fatalf("revoke-fail check = %+v", check)
		}
	})

}

func TestGap2VaultAcceptCleanupRequestError(t *testing.T) {
	check := runVaultAcceptCleanup(context.Background(), &http.Client{Timeout: 5 * time.Second}, "http://127.0.0.1:9", "tok", "secret/data/x")
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cleanup request failed") {
		t.Fatalf("cleanup request-error check = %+v", check)
	}
}

func TestGap2VaultAcceptHTTPReadAndOversize(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	t.Run("truncated response fails the read", func(t *testing.T) {
		check := runVaultAcceptHealth(ctx, client, gap2RawTCPServer(t), "tok")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot read Vault response") {
			t.Fatalf("health truncated check = %+v", check)
		}
	})
	t.Run("oversize response fails the size bound", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte("x"), vaultAcceptMaxBody+1))
		}))
		defer srv.Close()
		check := runVaultAcceptHealth(ctx, client, srv.URL, "tok")
		if check.Status != externalStatusFail || !strings.Contains(check.Reason, "exceeded the size bound") {
			t.Fatalf("health oversize check = %+v", check)
		}
	})
}

func TestGap2ConfigTLSFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nope.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nope.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.TLS.Enabled = true
	cfg.TLS.CertFile = "nope.crt"
	cfg.TLS.KeyFile = "nope.key"
	path := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runConfig([]string{"validate", "--config", path}, &stdout, &stderr)
	if code != configExitFailure || !strings.Contains(stderr.String(), "config invalid") {
		t.Fatalf("TLS-failure validate exit=%d stderr=%q, want config-invalid", code, stderr.String())
	}
}

func TestGap2ComfyUIAcceptReportWriteError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeTestConfig(t, config.Default())
	var stdout, stderr bytes.Buffer
	code := runComfyUIAccept([]string{
		"--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t),
		"--out", filepath.Join(blocker, "report.json"),
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "cannot write report") {
		t.Fatalf("report-write-error exit=%d stderr=%q", code, stderr.String())
	}
}

func TestGap2ComfyUIAcceptFFprobeUnavailable(t *testing.T) {
	cfg, configPath := gap2ComfyAcceptConfig(t, "http://127.0.0.1:9")
	report := comfyAcceptReport{}
	code := runComfyUIAcceptChecks(&report, cfg, configPath, t.TempDir(), filepath.Join(t.TempDir(), "no-such-ffprobe"), "a red cube", 1, 24, 0, 0, 0, time.Second, 10*time.Millisecond)
	if code != 3 {
		t.Fatalf("ffprobe-missing code=%d, want 3", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:preflight"); status != externalStatusBlocked || !strings.Contains(reason, "ffprobe executable is unavailable") {
		t.Fatalf("ffprobe-missing preflight status=%q reason=%q", status, reason)
	}
}

func TestGap2ComfyUIAcceptCustomDimensionsAndValidation(t *testing.T) {
	t.Run("custom dimensions take the custom branch", func(t *testing.T) {
		// Stats must pass first: the request build (custom branch)
		// runs after the system_stats check.
		srv := gap2ComfyServer(t, "processing")
		defer srv.Close()
		cfg, configPath := gap2ComfyAcceptConfig(t, srv.URL)
		report := comfyAcceptReport{}
		code := runComfyUIAcceptChecks(&report, cfg, configPath, t.TempDir(), comfyAcceptProbe(t), "a red cube", 1, 24, 640, 360, 0, time.Second, 10*time.Millisecond)
		if code != 1 {
			t.Fatalf("custom-dimensions code=%d, want 1 (poll timeout after submit)", code)
		}
		if status, _ := comfyAcceptCheckStatus(report, "comfyui:submit"); status != externalStatusPass {
			t.Fatalf("custom-dimensions submit status=%q, want pass (proves the custom request was built and accepted)", status)
		}
	})
	t.Run("overlong duration fails request validation", func(t *testing.T) {
		srv := gap2ComfyServer(t, "processing")
		defer srv.Close()
		cfg, configPath := gap2ComfyAcceptConfig(t, srv.URL)
		report := comfyAcceptReport{}
		code := runComfyUIAcceptChecks(&report, cfg, configPath, t.TempDir(), comfyAcceptProbe(t), "a red cube", 60, 24, 0, 0, 0, time.Second, 10*time.Millisecond)
		if code != 1 {
			t.Fatalf("overlong-duration code=%d, want 1", code)
		}
		if status, reason := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusFail || !strings.Contains(reason, "minimal acceptance request is invalid") {
			t.Fatalf("overlong-duration inference status=%q reason=%q", status, reason)
		}
	})
	t.Run("asset root under a regular file fails creation", func(t *testing.T) {
		srv := gap2ComfyServer(t, "processing")
		defer srv.Close()
		cfg, configPath := gap2ComfyAcceptConfig(t, srv.URL)
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		report := comfyAcceptReport{}
		code := runComfyUIAcceptChecks(&report, cfg, configPath, filepath.Join(blocker, "assets"), comfyAcceptProbe(t), "a red cube", 1, 24, 0, 0, 0, time.Second, 10*time.Millisecond)
		if code != 1 {
			t.Fatalf("asset-root code=%d, want 1", code)
		}
		if status, reason := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusFail || !strings.Contains(reason, "cannot create acceptance asset root") {
			t.Fatalf("asset-root inference status=%q reason=%q", status, reason)
		}
	})
}

func TestGap2ComfyUIAcceptPollFailedWithError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-1"}`))
		case r.URL.Path == "/history/prompt-1":
			_, _ = w.Write([]byte(`{"prompt-1":{"status":{"status_str":"error","completed":false},"outputs":{}}}`))
		case r.URL.Path == "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	code, report := gap2ComfyRunChecks(t, srv, comfyAcceptProbe(t), 5*time.Second, 10*time.Millisecond)
	if code != 1 {
		t.Fatalf("poll-error code=%d, want 1", code)
	}
	if status, reason := comfyAcceptCheckStatus(report, "comfyui:model_inference"); status != externalStatusFail || !strings.Contains(reason, "ComfyUI reported workflow failure") {
		t.Fatalf("poll-error inference status=%q reason=%q", status, reason)
	}
}

func TestGap2ResolveLiveAdminURLForms(t *testing.T) {
	got, err := resolveLiveAdminURL("", "https://admin.example.invalid:8080/")
	if err != nil || got != "https://admin.example.invalid:8080" {
		t.Fatalf("URL listen form = %q, err=%v, want trimmed URL", got, err)
	}
	if _, err := resolveLiveAdminURL("", ""); err == nil {
		t.Fatal("empty listen must error")
	}
	if _, err := resolveLiveAdminURL("", "/tmp/nexa.sock"); err == nil {
		t.Fatal("socket-path listen must error")
	}
}

func TestGap2ScanRestoreJournalsTempDirError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", blocker)
	interrupted, uninspectable := scanRestoreJournals()
	if len(interrupted) != 0 || len(uninspectable) != 0 {
		t.Fatalf("unreadable temp dir scan = (%v, %v), want empty", interrupted, uninspectable)
	}
	if got := findInterruptedRestoreJournals(); len(got) != 0 {
		t.Fatalf("interrupted journals = %v, want empty", got)
	}
}

func TestGap2ValidateJournalOwnership(t *testing.T) {
	mkJournal := func(t *testing.T, dirOwner, manifestOwner int) (string, *restoreJournalManifest) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "journal")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(dir, "manifest.json")
		if err := os.WriteFile(manifestPath, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if dirOwner >= 0 {
			if err := os.Chown(dir, dirOwner, -1); err != nil {
				t.Skipf("chown not permitted: %v", err)
			}
		}
		if manifestOwner >= 0 {
			if err := os.Chown(manifestPath, manifestOwner, -1); err != nil {
				t.Skipf("chown not permitted: %v", err)
			}
		}
		return dir, &restoreJournalManifest{Format: restoreJournalFormat, ID: "12345678", State: "prepared",
			Entries: []restoreJournalEntry{{Name: backupEntryConfig, Target: filepath.Join(t.TempDir(), "config.json")}}}
	}
	t.Run("foreign directory owner fails validation", func(t *testing.T) {
		dir, m := mkJournal(t, 1000, -1)
		if err := validateJournal(dir, m); err == nil || !strings.Contains(err.Error(), "not the current user") {
			t.Fatalf("foreign dir owner error = %v", err)
		}
	})
	t.Run("foreign manifest owner fails validation", func(t *testing.T) {
		dir, m := mkJournal(t, -1, 1000)
		if err := validateJournal(dir, m); err == nil || !strings.Contains(err.Error(), "not the current user") {
			t.Fatalf("foreign manifest owner error = %v", err)
		}
	})
}

func TestGap2ValidateBundledConfigErrors(t *testing.T) {
	t.Run("validate directory under a file fails mkdir", func(t *testing.T) {
		stage := t.TempDir()
		if err := os.WriteFile(filepath.Join(stage, "validate"), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := validateBundledConfig(stage, map[string][]byte{backupEntryConfig: []byte("{}")}); err == nil {
			t.Fatal("validate dir under a file must fail mkdir")
		}
	})
	t.Run("staged config as a directory fails the write", func(t *testing.T) {
		stage := t.TempDir()
		validateDir := filepath.Join(stage, "validate")
		if err := os.MkdirAll(validateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(validateDir, "config.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := validateBundledConfig(stage, map[string][]byte{backupEntryConfig: []byte("{}")}); err == nil {
			t.Fatal("staged config directory must fail the write")
		}
	})
	t.Run("staged key as a directory fails the write", func(t *testing.T) {
		stage := t.TempDir()
		validateDir := filepath.Join(stage, "validate")
		if err := os.MkdirAll(validateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(validateDir, "config.json.key"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := validateBundledConfig(stage, map[string][]byte{
			backupEntryConfig: []byte("{}"),
			backupEntryKey:    []byte("k"),
		}); err == nil {
			t.Fatal("staged key directory must fail the write")
		}
	})
}

func TestGap2KeylessRestoreJustifiedBranches(t *testing.T) {
	enveloped := []byte(`{"providers":[{"api_key":"nxs1:whatever"}]}`)
	if keylessRestoreJustified(map[string][]byte{backupEntryConfig: enveloped, backupEntryKey: []byte("k")}) {
		t.Fatal("bundled key must defeat keyless justification")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "dummy-env-key")
	if keylessRestoreJustified(map[string][]byte{backupEntryConfig: enveloped}) {
		t.Fatal("environment key material must defeat keyless justification")
	}
}

// NOTE: the truncated-entry branch in readAndVerifyBundle is
// deliberately not tested: Go's tar reader reports a truncated entry
// as an unexpected-EOF read error first, so the length-mismatch
// branch cannot be reached through a truncated stream. The read-error
// branch is covered by existing bundle tests.

func TestGap2DoctorRedisParseErrorAndBundleFreshness(t *testing.T) {
	t.Run("invalid redis URL fails the redis check", func(t *testing.T) {
		t.Setenv("GAP2_REDIS_URL_BAD", "://bad")
		cfg := config.Default()
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = "redis"
		cfg.ControlPlane.RedisURLEnv = "GAP2_REDIS_URL_BAD"
		out := gap2DoctorReports(t, func(d *doctor) {
			d.cfg = &cfg
			d.checkControlPlane(filepath.Join(t.TempDir(), "config.json"))
		})
		if !strings.Contains(out, "FAIL control_plane_redis") {
			t.Fatalf("doctor output = %q, want FAIL control_plane_redis", out)
		}
	})
	t.Run("stale bundle warns freshness and missing control-plane state", func(t *testing.T) {
		_, configPath := opsFixture(t)
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 0 {
			t.Fatalf("fixture backup exit=%d stderr=%q", code, stderr)
		}
		reports := gap2DoctorReports(t, func(d *doctor) {
			d.checkBackupBundle(out, configPath, time.Nanosecond)
		})
		if !strings.Contains(reports, "WARN backup_freshness") || !strings.Contains(reports, "WARN backup_migration_compat") {
			t.Fatalf("doctor bundle output = %q, want stale + compat warnings", reports)
		}
	})
	t.Run("garbage control-plane state fails migration compat", func(t *testing.T) {
		dir, configPath := opsFixture(t)
		state := filepath.Join(dir, "cp.json")
		if err := os.WriteFile(state, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteFixtureConfig(t, configPath, func(cfg *config.Config) {
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = "file"
			cfg.ControlPlane.StatePath = state
		})
		out := filepath.Join(t.TempDir(), "b.tar")
		if code, stderr := runBackupCode(t, configPath, out); code != 0 {
			t.Fatalf("cp-garbage backup exit=%d stderr=%q", code, stderr)
		}
		reports := gap2DoctorReports(t, func(d *doctor) {
			d.checkBackupBundle(out, configPath, 0)
		})
		if !strings.Contains(reports, "FAIL backup_migration_compat") {
			t.Fatalf("doctor bundle output = %q, want FAIL migration_compat", reports)
		}
	})
}

func TestGap2ExternalProviderUnexpectedStatus(t *testing.T) {
	p := config.ProviderConfig{
		ID: "bedrock-no-discovery", Type: "bedrock", BaseURL: "https://example.invalid",
		AuthMode: "none",
	}
	p.ApplyDefaults()
	check := runExternalProviderCheck(context.Background(), p, acceptOptions{
		discover: false, test: false, timeout: time.Second, maxModels: 1, maxCalls: 1, modelIDs: []string{"m"},
	}, livecanary.NewRedactor(nil))
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "unexpected status unsupported") {
		t.Fatalf("unexpected-status check = %+v", check)
	}
}

func TestGap2SanitizeProviderAcceptResultRedacts(t *testing.T) {
	result := &providerAcceptResult{Error: "token-abc-secret"}
	sanitizeProviderAcceptResult(result, livecanary.NewRedactor([]string{"token-abc-secret"}))
	if strings.Contains(result.Error, "token-abc-secret") {
		t.Fatalf("sanitized error = %q, must not contain the secret", result.Error)
	}
}
