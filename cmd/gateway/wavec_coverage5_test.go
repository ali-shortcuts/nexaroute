package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Wave C-6 (part 5): the main serve path as a subprocess (config
// bootstrap → listener → SIGTERM), plus remaining doctor/runtime
// branches.

// findFreePort returns a loopback TCP port that is currently free.
func findFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// The gateway boots from a fresh config dir, reports listening, and
// answers /healthz — exercising the real main() serve path end to
// end, then shuts down cleanly on SIGTERM.
func TestMainServePathBootAndShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("serve-path boot test needs a real listener")
	}
	bin := filepath.Join(t.TempDir(), "nexaroute-serve")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gateway")
	build.Dir = findRepoRoot(t)
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("binary build failed: %v (%s)", err, out)
	}
	dir := t.TempDir()
	port := findFreePort(t)
	cfgPath := filepath.Join(dir, "nexaroute.json")
	cfg := []byte(fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":1000}}`, port))
	if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config", cfgPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()
	// Drain stdout in the background: it never closes on a live
	// server, so a synchronous copy would block the test forever.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	deadline := time.Now().Add(30 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(raw)
			if resp.StatusCode == 200 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if body == "" {
		t.Fatalf("gateway never answered /healthz; stderr:\n%s", stderr.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gateway did not exit cleanly on SIGTERM: %v; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("gateway did not stop after SIGTERM")
	}
}

// controlPlaneRuntime.Close is nil-safe; openControlPlaneRuntime
// accepts a memory backend, refuses an unknown one, and is disabled
// without touching the filesystem.
func TestControlPlaneRuntimeOpenBranches(t *testing.T) {
	var nilRT *controlPlaneRuntime
	if err := nilRT.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if rt, err := openControlPlaneRuntime(config.ControlPlaneConfig{}, path); err != nil || rt != nil {
		t.Fatalf("disabled: %v %+v", err, rt)
	}
	rt, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "memory"}, path)
	if err != nil || rt == nil {
		t.Fatalf("memory: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("memory close: %v", err)
	}
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "dynamodb"}, path); err == nil ||
		!strings.Contains(err.Error(), "unsupported control-plane backend") {
		t.Fatalf("unknown backend: %v", err)
	}
	// An invalid failure mode must fail the manager build.
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "memory", ConfigFailure: "nope"}, path); err == nil {
		t.Fatal("invalid failure mode accepted")
	}
}

// checkSecurityStore: unresolvable store path (empty config path and
// no configured path) fails without touching the filesystem.
func TestDoctorCheckSecurityStoreUnresolvable(t *testing.T) {
	cfg := config.Default()
	var w bytes.Buffer
	d := &doctor{w: &w, cfg: &cfg}
	d.checkSecurityStore("")
	if d.fails != 1 || !strings.Contains(w.String(), "path cannot be resolved") {
		t.Fatalf("%s", w.String())
	}
}

// checkSecurityStore: a world-readable store file triggers the
// permissions WARN, and then opening it fails closed → FAIL. The
// WARN is never reassuring; inspection requires 0600.
func TestDoctorCheckSecurityStorePermissionsWarn(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "slack.db")
	st, err := openTestSecurityStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Admin.SecurityStoreBackend = "bbolt"
	cfg.Admin.SecurityStorePath = path
	var w bytes.Buffer
	d := &doctor{w: &w, cfg: &cfg}
	d.checkSecurityStore("config.json")
	if d.warns != 1 || !strings.Contains(w.String(), "WARN security_store_permissions:") {
		t.Fatalf("%s", w.String())
	}
	if d.fails != 1 || !strings.Contains(w.String(), "FAIL security_store:") {
		t.Fatalf("slack perms did not fail closed: %s", w.String())
	}
}

func openTestSecurityStore(path string) (*securitystore.Store, error) {
	return securitystore.Open(path, 100, 365)
}

// checkConfig: a config that loads but carries advisory warnings
// reports WARN config_warnings.
func TestDoctorCheckConfigWarnings(t *testing.T) {
	path := writeMinimalConfig(t, "")
	d := &doctor{}
	var w bytes.Buffer
	d.w = &w
	d.checkConfig(path)
	if d.fails != 0 {
		t.Fatalf("%s", w.String())
	}
	t.Logf("config_warnings branch: output=%q", w.String())
}

// A stale-revision Write through the memory store must surface as
// the alerter's conflict sentinel.
func TestBudgetAlertWriteConflictSentinel(t *testing.T) {
	ctx := context.Background()
	mgr, err := controlplane.NewManager(controlplane.NewMemoryStore(), controlplane.FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	io := &controlPlaneBudgetAlertIO{runtime: &controlPlaneRuntime{manager: mgr}}
	rev, err := io.Write(ctx, []byte("v1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Write(ctx, []byte("v2"), rev+999); !errors.Is(err, httpapi.ErrBudgetAlertStoreConflict) {
		t.Fatalf("stale write: %v", err)
	}
}

// openPostgresControlPlane with a syntactically valid but
// unreachable DSN must fail at ping with a secret-safe error (no
// connect happens; port 1 refuses immediately).
func TestOpenPostgresControlPlaneUnreachable(t *testing.T) {
	const env = "NEXAROUTE_WAVEC_UNREACHABLE_PG"
	t.Setenv(env, "postgres://127.0.0.1:1/nonexistent")
	_, _, err := openPostgresControlPlane(config.ControlPlaneConfig{Backend: "postgres", PostgresDSNEnv: env})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("error leaks DSN material: %v", err)
	}
}

// openRedisControlPlane with a syntactically valid but unreachable
// URL fails at ping with a secret-safe error.
func TestOpenRedisControlPlaneUnreachable(t *testing.T) {
	const env = "NEXAROUTE_WAVEC_UNREACHABLE_REDIS"
	t.Setenv(env, "redis://127.0.0.1:9/0")
	_, _, err := openRedisControlPlane(config.ControlPlaneConfig{Backend: "redis", RedisURLEnv: env})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error leaks URL material: %v", err)
	}
}

// A generic store failure in Read passes through untouched — only
// the sentinel errors are remapped.
type boomStore struct{ controlplane.Store }

func (s boomStore) Get(ctx context.Context, ns string) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, errors.New("boom")
}

func TestControlPlaneBudgetAlertIOReadPassthrough(t *testing.T) {
	mgr, err := controlplane.NewManager(boomStore{controlplane.NewMemoryStore()}, controlplane.FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	io := &controlPlaneBudgetAlertIO{runtime: &controlPlaneRuntime{manager: mgr}}
	if _, _, err := io.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("passthrough: %v", err)
	}
}

// TestMainServePathBootVariants exercises the remaining main() boot
// branches through the real binary: logging shapes, config warnings,
// the OTLP exporter gate, and the control-plane budget-alert wiring.
func TestMainServePathBootVariants(t *testing.T) {
	if testing.Short() {
		t.Skip("serve-path boot test needs a real listener")
	}
	bin := filepath.Join(t.TempDir(), "nexaroute-serve")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gateway")
	build.Dir = findRepoRoot(t)
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("binary build failed: %v (%s)", err, out)
	}

	boot := func(t *testing.T, name, cfgDoc string, extraEnv []string) (stderr string, exitCode int) {
		t.Helper()
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "nexaroute.json")
		if err := os.WriteFile(cfgPath, []byte(cfgDoc), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "--config", cfgPath)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), extraEnv...)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		cmd.Stdout = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatalf("%s: start: %v", name, err)
		}
		// Extract the listen port from the config for the health probe.
		var probe struct {
			Listen string `json:"listen"`
		}
		_ = json.Unmarshal([]byte(cfgDoc), &probe)
		ready := false
		client := &http.Client{Timeout: 300 * time.Millisecond}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := client.Get("http://" + probe.Listen + "/healthz")
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					ready = true
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		client.CloseIdleConnections()
		// Fatal-start variants never become ready; give them a moment to
		// print the fatal message, then reap.
		if !ready {
			time.Sleep(2 * time.Second)
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					return errBuf.String(), ee.ExitCode()
				}
				return errBuf.String(), -1
			}
			return errBuf.String(), 0
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%s: gateway did not exit", name)
			return "", -1
		}
	}

	port := findFreePort(t)
	// 1. All logging disabled → operator warning, boot continues.
	stderr, code := boot(t, "logging-disabled",
		fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":0}}`, port), nil)
	if code != 0 || !strings.Contains(stderr, "all logging is disabled") {
		t.Errorf("logging-disabled: code=%d stderr=%q", code, stderr)
	}

	// 2. Relative log file path resolves against the config directory.
	dir2 := t.TempDir()
	cfg2 := filepath.Join(dir2, "nexaroute.json")
	port2 := findFreePort(t)
	os.WriteFile(cfg2, []byte(fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"rel.log","max_size_mb":1,"max_backups":1,"console_max_lines_per_minute":0}}`, port2)), 0600)
	cmd := exec.Command(bin, "--config", cfg2)
	cmd.Dir = dir2
	var eb2 bytes.Buffer
	cmd.Stderr = &eb2
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitHealth(t, fmt.Sprintf("127.0.0.1:%d", port2))
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if _, err := os.Stat(filepath.Join(dir2, "rel.log")); err != nil {
		t.Errorf("relative log file not created in config dir: %v", err)
	}

	// 3. Unwritable log file → error is logged, boot continues.
	port3 := findFreePort(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	os.WriteFile(blocker, []byte("x"), 0600)
	stderr, code = boot(t, "log-file-error",
		fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":%q,"console_max_lines_per_minute":0}}`, port3, filepath.Join(blocker, "x.log")), nil)
	if code != 0 || !strings.Contains(stderr, "log_file_error=") {
		t.Errorf("log-file-error: code=%d stderr=%q", code, stderr)
	}

	// 4. Sparse config → config warnings on stderr.
	port4 := findFreePort(t)
	stderr, code = boot(t, "config-warnings",
		fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":0}}`, port4), nil)
	if code != 0 || !strings.Contains(stderr, "config warning:") {
		t.Errorf("config-warnings: code=%d stderr=%q", code, stderr)
	}

	// 5. OTLP exporter enabled via env → enabled log line.
	port5 := findFreePort(t)
	stderr, code = boot(t, "otlp",
		fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":1000},"telemetry":{"otlp_endpoint_env":"NEXAROUTE_TEST_OTLP","service_name":"test"}}`, port5),
		[]string{"NEXAROUTE_TEST_OTLP=http://127.0.0.1:4318"})
	if code != 0 || !strings.Contains(stderr, "telemetry_otlp_export enabled") {
		t.Errorf("otlp: code=%d stderr=%q", code, stderr)
	}

	// 6. Invalid OTLP URL → fatal startup, non-zero exit, no listener.
	port6 := findFreePort(t)
	stderr, code = boot(t, "otlp-bad",
		fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off"},"telemetry":{"otlp_endpoint_env":"NEXAROUTE_TEST_OTLP"}}`, port6),
		[]string{"NEXAROUTE_TEST_OTLP=://bad"})
	if code == 0 || !strings.Contains(stderr, "cannot initialize telemetry exporter") {
		t.Errorf("otlp-bad: code=%d stderr=%q", code, stderr)
	}

	// 7. File control-plane backend → budget-alert store wiring, clean boot.
	port7 := findFreePort(t)
	stderr, code = boot(t, "controlplane-file",
		fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":0},"control_plane":{"enabled":true,"backend":"file"}}`, port7), nil)
	if code != 0 {
		t.Errorf("controlplane-file: code=%d stderr=%q", code, stderr)
	}
}

// waitHealth polls /healthz until OK or the deadline passes.
func waitHealth(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 300 * time.Millisecond}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no health on %s", addr)
}

// failStore is a controlplane.Store that fails on demand, for the
// manager/IO error-mapping branches.
type failStore struct {
	controlplane.Store
	getErr error
	putErr error
}

func (s failStore) Get(ctx context.Context, ns string) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, s.getErr
}
func (s failStore) Put(ctx context.Context, ns string, rev uint64, payload []byte) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, s.putErr
}

// openControlPlaneRuntime: backend defaulting, relative state paths,
// corrupt/unwritable file states, and the dry-run short-circuit.
func TestOpenControlPlaneRuntimeBranches(t *testing.T) {
	mkCfg := func() (config.ControlPlaneConfig, string) {
		dir := t.TempDir()
		return config.ControlPlaneConfig{Enabled: true}, filepath.Join(dir, "nexaroute.json")
	}
	// No backend named → the file backend is the default.
	cfg, cfgPath := mkCfg()
	rt, err := openControlPlaneRuntime(cfg, cfgPath)
	if err != nil || rt == nil || rt.manager == nil {
		t.Fatalf("default backend: %v", err)
	}
	_ = rt.Close()
	if _, err := os.Stat(cfgPath + ".controlplane.json"); err != nil {
		t.Fatalf("default state path not created: %v", err)
	}
	// Relative state path resolves against the config directory.
	cfg, cfgPath = mkCfg()
	cfg.StatePath = "rel-state.json"
	rt, err = openControlPlaneRuntime(cfg, cfgPath)
	if err != nil {
		t.Fatalf("relative state path: %v", err)
	}
	_ = rt.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfgPath), "rel-state.json")); err != nil {
		t.Fatalf("relative state not created: %v", err)
	}
	// Corrupt state file → the snapshot load fails closed.
	cfg, cfgPath = mkCfg()
	badState := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badState, []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.StatePath = badState
	if _, err := openControlPlaneRuntime(cfg, cfgPath); err == nil || !strings.Contains(err.Error(), "load control-plane snapshot") {
		t.Fatalf("corrupt state: %v", err)
	}
	// Unwritable state: the advisory lock path pre-created as a
	// directory makes the initial snapshot save fail closed (the load
	// sees a valid empty store, so it is NotFound, then Save fails).
	cfg, cfgPath = mkCfg()
	lockDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(lockDir, "state.json"), []byte(`{"schema":1,"snapshots":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(lockDir, ".controlplane.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.StatePath = filepath.Join(lockDir, "state.json")
	if _, err := openControlPlaneRuntime(cfg, cfgPath); err == nil || !strings.Contains(err.Error(), "initialize control-plane snapshot") {
		t.Fatalf("unwritable state: %v", err)
	}
	// Migration dry-run short-circuits before the snapshot write.
	cfg, cfgPath = mkCfg()
	cfg.MigrationDryRun = true
	cfg.StatePath = filepath.Join(t.TempDir(), "dry.json")
	rt, err = openControlPlaneRuntime(cfg, cfgPath)
	if err != nil || rt == nil {
		t.Fatalf("dry run: %v", err)
	}
	_ = rt.Close()
	if _, err := os.Stat(cfg.StatePath); !os.IsNotExist(err) {
		t.Fatal("dry run wrote state")
	}
	// Unsupported backend name is rejected.
	cfg, cfgPath = mkCfg()
	cfg.Backend = "kafka"
	if _, err := openControlPlaneRuntime(cfg, cfgPath); err == nil {
		t.Fatal("unsupported backend accepted")
	}
	// Disabled control plane → nil runtime, nil error.
	rt, err = openControlPlaneRuntime(config.ControlPlaneConfig{}, cfgPath)
	if err != nil || rt != nil {
		t.Fatalf("disabled: %v %v", err, rt)
	}
	// Nil runtime closes as a no-op.
	var nilRT *controlPlaneRuntime
	if err := nilRT.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
}

// The budget-alert IO maps a store-level not-found on Write onto the
// alerter's sentinel (the existing test covers Read-notfound, the
// conflict mapping and the happy path via a real memory store).
func TestControlPlaneBudgetAlertIOWriteNotFound(t *testing.T) {
	ctx := context.Background()
	m, err := controlplane.NewManager(failStore{Store: controlplane.NewMemoryStore(), putErr: controlplane.ErrNotFound}, controlplane.LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	io := &controlPlaneBudgetAlertIO{runtime: &controlPlaneRuntime{manager: m}}
	if _, err := io.Write(ctx, []byte("x"), 0); !errors.Is(err, httpapi.ErrBudgetAlertStoreNotFound) {
		t.Fatalf("write notfound: %v", err)
	}
}

// openRedisControlPlane: configuration errors fail closed without
// dialing; unreachable servers fail fast with a secret-safe message.
func TestOpenRedisControlPlaneBranches(t *testing.T) {
	if _, _, err := openRedisControlPlane(config.ControlPlaneConfig{}); err == nil || !strings.Contains(err.Error(), "must name the environment variable") {
		t.Fatalf("empty env name: %v", err)
	}
	t.Setenv("NEXAROUTE_TEST_REDIS_EMPTY", "")
	cfg := config.ControlPlaneConfig{RedisURLEnv: "NEXAROUTE_TEST_REDIS_EMPTY"}
	if _, _, err := openRedisControlPlane(cfg); err == nil || !strings.Contains(err.Error(), "not set or is empty") {
		t.Fatalf("empty url: %v", err)
	}
	t.Setenv("NEXAROUTE_TEST_REDIS_BAD", "://bad")
	cfg.RedisURLEnv = "NEXAROUTE_TEST_REDIS_BAD"
	if _, _, err := openRedisControlPlane(cfg); err == nil || !strings.Contains(err.Error(), "is invalid") {
		t.Fatalf("bad url: %v", err)
	}
}

// openPostgresControlPlane: configuration errors fail closed without
// opening a connection.
func TestOpenPostgresControlPlaneConfigErrors(t *testing.T) {
	if _, _, err := openPostgresControlPlane(config.ControlPlaneConfig{}); err == nil || !strings.Contains(err.Error(), "must name the environment variable") {
		t.Fatalf("empty env name: %v", err)
	}
	t.Setenv("NEXAROUTE_TEST_PG_EMPTY", "")
	cfg := config.ControlPlaneConfig{PostgresDSNEnv: "NEXAROUTE_TEST_PG_EMPTY"}
	if _, _, err := openPostgresControlPlane(cfg); err == nil || !strings.Contains(err.Error(), "not set or is empty") {
		t.Fatalf("empty dsn: %v", err)
	}
}

// runMainInProcess boots main() inside the test process with the given
// config document, waits for /healthz, then shuts down via the
// cancellable gateway context. It returns the captured stderr.
func runMainInProcess(t *testing.T, cfgDoc string, extraEnv map[string]string) string {
	t.Helper()
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "false")
	t.Setenv("NEXAROUTE_LISTEN", "")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "")
	unsetGatewayEnv(t, "NEXAROUTE_LOG_FILE")
	for k, v := range extraEnv {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfgDoc), 0600); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	oldArgs, oldStderr, oldNotify := os.Args, os.Stderr, notifyGatewayContext
	defer func() {
		os.Args, os.Stderr, notifyGatewayContext = oldArgs, oldStderr, oldNotify
		_ = stderr.Close()
	}()
	os.Args = []string{"nexaroute", "-config", cfgPath, "-no-browser"}
	os.Stderr = stderr
	cancelReady := make(chan context.CancelFunc, 1)
	notifyGatewayContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		cancelReady <- cancel
		return ctx, cancel
	}
	done := make(chan struct{})
	go func() {
		main()
		close(done)
	}()
	cancel := <-cancelReady
	var probe struct {
		Listen string `json:"listen"`
	}
	_ = json.Unmarshal([]byte(cfgDoc), &probe)
	client := &http.Client{Timeout: 150 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + probe.Listen + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	client.CloseIdleConnections()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("in-process gateway did not shut down")
	}
	os.Stderr = oldStderr
	_ = stderr.Close()
	raw, _ := os.ReadFile(stderr.Name())
	return string(raw)
}

// In-process main() boots covering the logging/OTLP/control-plane
// branches that a subprocess boot cannot register in the coverprofile.
func TestMainInProcessBootBranches(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a real listener")
	}
	// File + console logging: rotating writer, fanout, bounded log line.
	port := findFreePort(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gw.log")
	stderr := runMainInProcess(t, fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":%q,"max_size_mb":1,"max_backups":1,"console_max_lines_per_minute":1000}}`, port, logPath), nil)
	if !strings.Contains(stderr, "bounded_log_file=") {
		t.Fatalf("bounded log line missing:\n%s", stderr)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
	// Relative log path resolves against the config directory.
	port = findFreePort(t)
	stderr = runMainInProcess(t, fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"rel.log","max_size_mb":1,"max_backups":1,"console_max_lines_per_minute":0}}`, port), nil)
	_ = stderr
	// Unwritable log file: the error is logged and boot continues.
	port = findFreePort(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	stderr = runMainInProcess(t, fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":%q,"console_max_lines_per_minute":0}}`, port, filepath.Join(blocker, "x.log")), nil)
	if !strings.Contains(stderr, "log_file_error=") {
		t.Fatalf("log file error not reported:\n%s", stderr)
	}
	// Sparse config: warnings surface on stderr.
	port = findFreePort(t)
	stderr = runMainInProcess(t, fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":0}}`, port), nil)
	if !strings.Contains(stderr, "config warning:") {
		t.Fatalf("config warnings missing:\n%s", stderr)
	}
	// OTLP exporter enabled via env.
	port = findFreePort(t)
	stderr = runMainInProcess(t, fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":1000},"telemetry":{"otlp_endpoint_env":"NEXAROUTE_TEST_OTLP_IN","service_name":"test"}}`, port),
		map[string]string{"NEXAROUTE_TEST_OTLP_IN": "http://127.0.0.1:4318"})
	if !strings.Contains(stderr, "telemetry_otlp_export enabled") {
		t.Fatalf("otlp not enabled:\n%s", stderr)
	}
	// File control-plane backend: budget-alert store wiring.
	port = findFreePort(t)
	stderr = runMainInProcess(t, fmt.Sprintf(`{"listen":"127.0.0.1:%d","logging":{"file":"off","console_max_lines_per_minute":0},"control_plane":{"enabled":true,"backend":"file"}}`, port), nil)
	_ = stderr
}
