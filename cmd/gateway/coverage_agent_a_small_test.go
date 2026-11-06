// Coverage tests for the small cmd/gateway files (Agent A, 100% mission):
// main.go (default fatalLog, ensureConfig), init.go, config.go,
// audit.go, doctor.go, conformance.go, policy.go, nexaflow.go and
// controlplane_runtime.go. Where a branch is provably dead it was
// removed with a proof comment at the site; what remains here is
// tested through real behavior, including a scripted database/sql
// driver for the control-plane PostgreSQL wiring (CI's coverage job
// has no live PostgreSQL).

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// --- main.go ---

// TestCoverageAgentARealFatalLog proves the default fatalLog (the
// package-level func value) actually logs and exits 1. It runs in a
// child test process because log.Fatalf calls os.Exit: the parent
// re-execs the test binary with an env marker and asserts exit 1.
func TestCoverageAgentARealFatalLog(t *testing.T) {
	if os.Getenv("COVERAGE_AGENT_A_FATAL_CHILD") == "1" {
		fatalLog(log.New(io.Discard, "", 0), "boom %d", 42)
		t.Fatal("fatalLog returned instead of exiting")
		return
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoverageAgentARealFatalLog$", "-test.v")
	cmd.Env = append(os.Environ(), "COVERAGE_AGENT_A_FATAL_CHILD=1")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an exit error, got %v (stdout=%s stderr=%s)", err, stdout.String(), stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %d (stdout=%s stderr=%s)", exitErr.ExitCode(), stdout.String(), stderr.String())
	}
}

func TestCoverageAgentAEnsureConfigMkdirAllError(t *testing.T) {
	// Stat on /sys/<missing>/config.json reports IsNotExist, so
	// ensureConfig reaches MkdirAll — and sysfs is read-only, so the
	// mkdir fails (EROFS). A path under a regular file does NOT reach
	// this branch: its Stat fails ENOTDIR and returns earlier.
	err := ensureConfig("/sys/covcli-nonexistent-dir/config.json")
	if err == nil {
		t.Fatal("expected a MkdirAll error for a config path under a read-only filesystem")
	}
}

// --- init.go ---

func TestCoverageAgentARunInitFlagParseError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code != initExitUsage {
		t.Fatalf("expected usage exit %d, got %d (stderr=%s)", initExitUsage, code, stderr.String())
	}
}

func TestCoverageAgentARunInitFullyProvided(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "config.json")
	var stdout, stderr bytes.Buffer
	code := runInit([]string{
		"--out", out,
		"--listen", "127.0.0.1:8080",
		"--provider-id", "primary",
		"--type", "openai_compatible",
		"--base-url", "https://api.openai.com",
		"--api-key-env", "OPENAI_API_KEY",
		"--model", "gpt-4o-mini",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != initExitOK {
		t.Fatalf("expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Wrote "+out) {
		t.Fatalf("expected a Wrote line, got:\n%s", stdout.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected the config file to exist: %v", err)
	}
}

// --- controlplane_runtime.go: openControlPlaneRuntime ---

// NewFileStore is lazy (it only stores the path), so a state path
// under a regular file surfaces at the snapshot step, not at store
// construction: the file store's load fails ENOTDIR and
// ensureControlPlaneSnapshot reports it.
func TestCoverageAgentAOpenControlPlaneUnusableStatePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled:   true,
		Backend:   "file",
		StatePath: filepath.Join(blocker, "state.json"),
	}, filepath.Join(dir, "config.json"))
	if err == nil || !strings.Contains(err.Error(), "load control-plane snapshot") {
		t.Fatalf("expected a snapshot-load error for a state path under a regular file, got %v", err)
	}
}

func TestCoverageAgentAOpenControlPlaneUnsupportedBackend(t *testing.T) {
	_, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled: true,
		Backend: "bogus",
	}, filepath.Join(t.TempDir(), "config.json"))
	if err == nil || !strings.Contains(err.Error(), "unsupported control-plane backend") {
		t.Fatalf("expected an unsupported-backend error, got %v", err)
	}
}

func TestCoverageAgentAOpenControlPlaneSnapshotError(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "cp-state.json")
	if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled:   true,
		Backend:   "file",
		StatePath: statePath,
	}, filepath.Join(dir, "config.json"))
	if err == nil || !strings.Contains(err.Error(), "load control-plane snapshot") {
		t.Fatalf("expected a snapshot-load error for a corrupt state file, got %v", err)
	}
}

// --- controlplane_runtime.go: PostgreSQL via a scripted fake driver ---
//
// The fake implements just the database/sql surface the migrator uses
// (Ping, Conn, advisory-lock/table CREATE, the checksum lookup, a
// transaction applying one migration and recording it). It never
// touches a real server, so these tests run in CI's coverage job.

type fakeCPState struct {
	mu           sync.Mutex
	applied      map[uint64]string
	seedChecksum string // non-empty: the version-1 lookup returns this checksum
}

type fakeCPDriver struct{ state *fakeCPState }

func (d *fakeCPDriver) Open(string) (driver.Conn, error) {
	return &fakeCPConn{state: d.state}, nil
}

type fakeCPConn struct{ state *fakeCPState }

func (c *fakeCPConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeCPStmt{conn: c, query: query}, nil
}

func (c *fakeCPConn) Close() error { return nil }

func (c *fakeCPConn) Ping(ctx context.Context) error { return ctx.Err() }

func (c *fakeCPConn) CheckNamedValue(nv *driver.NamedValue) error {
	switch nv.Value.(type) {
	case uint64, uint, int, int32, int64, string, []byte, bool, nil, time.Time:
		return nil
	default:
		return fmt.Errorf("fake-cp: unsupported arg type %T", nv.Value)
	}
}

func (c *fakeCPConn) Begin() (driver.Tx, error) { return &fakeCPTx{state: c.state}, nil }

func (c *fakeCPConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &fakeCPTx{state: c.state}, nil
}

type fakeCPStmt struct {
	conn  *fakeCPConn
	query string
}

func (s *fakeCPStmt) Close() error  { return nil }
func (s *fakeCPStmt) NumInput() int { return -1 }

func (s *fakeCPStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.execQuery(s.query, args)
}

func (s *fakeCPStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.queryQuery(s.query, args)
}

func (s *fakeCPStmt) ExecContext(_ context.Context, args []driver.Value) (driver.Result, error) {
	return s.conn.execQuery(s.query, args)
}

func (s *fakeCPStmt) QueryContext(_ context.Context, args []driver.Value) (driver.Rows, error) {
	return s.conn.queryQuery(s.query, args)
}

func (c *fakeCPConn) execQuery(query string, args []driver.Value) (driver.Result, error) {
	if strings.Contains(query, "INSERT INTO nexaroute_control_migrations") {
		c.state.mu.Lock()
		defer c.state.mu.Unlock()
		var version uint64
		var checksum string
		if len(args) > 0 {
			switch v := args[0].(type) {
			case int64:
				version = uint64(v)
			case uint64:
				version = v
			}
		}
		if len(args) > 2 {
			checksum, _ = args[2].(string)
		}
		c.state.applied[version] = checksum
	}
	return fakeCPResult{}, nil
}

func (c *fakeCPConn) queryQuery(query string, _ []driver.Value) (driver.Rows, error) {
	if strings.Contains(query, "FROM nexaroute_control_migrations WHERE version") {
		c.state.mu.Lock()
		seed := c.state.seedChecksum
		c.state.mu.Unlock()
		if seed != "" {
			return &fakeCPRows{columns: []string{"checksum"}, rows: [][]driver.Value{{seed}}}, nil
		}
		return &fakeCPRows{columns: []string{"checksum"}}, nil
	}
	return &fakeCPRows{columns: []string{"x"}}, nil
}

type fakeCPResult struct{}

func (fakeCPResult) LastInsertId() (int64, error) { return 0, nil }

// RowsAffected reports 1: the control-plane SQL store treats a
// 0-row write as a compare-and-swap conflict (see
// sqlstore.go), so a fake that claims "nothing written" makes every
// store Put fail with a revision conflict even though the write
// logically succeeded.
func (fakeCPResult) RowsAffected() (int64, error) { return 1, nil }

type fakeCPRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
}

func (r *fakeCPRows) Columns() []string { return r.columns }
func (r *fakeCPRows) Close() error      { return nil }
func (r *fakeCPRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

type fakeCPTx struct{ state *fakeCPState }

func (tx *fakeCPTx) Commit() error   { return nil }
func (tx *fakeCPTx) Rollback() error { return nil }

// fakeCPDriverInstance is registered once; each test swaps in a fresh
// state (tests in this package run sequentially).
var fakeCPDriverInstance = &fakeCPDriver{state: &fakeCPState{applied: map[uint64]string{}}}

func init() {
	sql.Register("covcli-fake-pgx", fakeCPDriverInstance)
}

// useFakeCPDriver points openPostgresControlPlane at the fake driver
// and returns a config whose DSN env var holds a dummy value.
func useFakeCPDriver(t *testing.T, state *fakeCPState) config.ControlPlaneConfig {
	t.Helper()
	old := controlPlanePostgresDriver
	controlPlanePostgresDriver = "covcli-fake-pgx"
	fakeCPDriverInstance.state = state
	t.Cleanup(func() { controlPlanePostgresDriver = old })
	t.Setenv("NEXAROUTE_COV_CP_DSN", "fake-dsn")
	return config.ControlPlaneConfig{PostgresDSNEnv: "NEXAROUTE_COV_CP_DSN"}
}

func TestCoverageAgentAOpenPostgresSuccess(t *testing.T) {
	state := &fakeCPState{applied: map[uint64]string{}}
	cfg := useFakeCPDriver(t, state)
	store, db, err := openPostgresControlPlane(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil || db == nil {
		t.Fatal("expected a non-nil store and db")
	}
	defer db.Close()
	registry, err := controlplane.DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	migrations := registry.All()
	if len(migrations) != 1 {
		t.Fatalf("expected exactly one migration, got %d", len(migrations))
	}
	state.mu.Lock()
	checksum, ok := state.applied[migrations[0].Version]
	state.mu.Unlock()
	if !ok {
		t.Fatal("expected migration version 1 to be recorded as applied")
	}
	if checksum != migrations[0].Checksum() {
		t.Fatalf("applied checksum %q != migration checksum %q", checksum, migrations[0].Checksum())
	}
}

func TestCoverageAgentAOpenPostgresMigrationFailure(t *testing.T) {
	state := &fakeCPState{applied: map[uint64]string{}, seedChecksum: "bogus-checksum"}
	cfg := useFakeCPDriver(t, state)
	_, _, err := openPostgresControlPlane(cfg)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected a checksum-mismatch migration error, got %v", err)
	}
}

func TestCoverageAgentAOpenPostgresMigrationDryRun(t *testing.T) {
	state := &fakeCPState{applied: map[uint64]string{}}
	cfg := useFakeCPDriver(t, state)
	cfg.MigrationDryRun = true
	store, db, err := openPostgresControlPlane(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil || db == nil {
		t.Fatal("expected a non-nil store and db")
	}
	defer db.Close()
	state.mu.Lock()
	applied := len(state.applied)
	state.mu.Unlock()
	if applied != 0 {
		t.Fatalf("dry run must not apply migrations, but %d were recorded", applied)
	}
}

// --- doctor.go ---

func TestCoverageAgentADoctorSecurityStoreStatError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Admin.SecurityStorePath = filepath.Join(blocker, "security.db")
	path := writeCoverageConfig(t, dir, cfg)
	var w bytes.Buffer
	d := &doctor{w: &w}
	d.checkConfig(path)
	if d.cfg == nil {
		t.Fatalf("expected checkConfig to attach a config; output:\n%s", w.String())
	}
	w.Reset()
	d.checkSecurityStore(path)
	if d.fails == 0 || !strings.Contains(w.String(), "FAIL security_store: cannot inspect") {
		t.Fatalf("expected a FAIL security_store cannot-inspect line, got:\n%s", w.String())
	}
}

// --- audit.go ---

func TestCoverageAgentAAuditVerifyFailedChain(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	path := writeCoverageConfig(t, dir, cfg)
	storePath := filepath.Join(dir, "security", "nexaroute-security.db")
	store, err := securitystore.Open(storePath, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Actor:     "alice", Action: fmt.Sprintf("action-%d", i), Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Tamper with the second record (same technique as the
	// securitystore chain tests): rewrite it with a different actor
	// so its stored hash no longer matches its content.
	db, err := bolt.Open(storePath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("audit-v1"))
		if bucket == nil {
			return errors.New("audit-v1 bucket missing")
		}
		c := bucket.Cursor()
		idx := 0
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			if idx == 1 {
				var event securitystore.AuditEvent
				if err := json.Unmarshal(raw, &event); err != nil {
					return err
				}
				event.Actor = "mallory"
				encoded, err := json.Marshal(event)
				if err != nil {
					return err
				}
				keyCopy := make([]byte, len(k))
				copy(keyCopy, k)
				return bucket.Put(keyCopy, encoded)
			}
			idx++
		}
		return errors.New("second audit record not found")
	})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runAudit([]string{"verify", "--config", path}, &stdout, &stderr)
	if code != auditExitProblems {
		t.Fatalf("expected exit %d, got %d (stdout=%s stderr=%s)", auditExitProblems, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result: FAILED") {
		t.Fatalf("expected result: FAILED, got:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "problem: ") || !strings.Contains(stdout.String(), "modified") {
		t.Fatalf("expected a 'modified' problem line, got:\n%s", stdout.String())
	}
}

// --- conformance.go ---

func TestCoverageAgentACanaryReportMarshalError(t *testing.T) {
	// --max-cost-usd NaN passes normalizeLimits (NaN <= 0 is false)
	// and lands in Report.Limits.MaxCostUSD, which json.MarshalIndent
	// rejects. The provider is unreachable (127.0.0.1:1), so the
	// canary fails fast with no upstream call and no credential use.
	t.Setenv("NEXAROUTE_COV_CANARY_KEY", "test-key")
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p-canary", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1",
		Enabled: true, APIKeyEnv: "NEXAROUTE_COV_CANARY_KEY",
		Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}},
	}}
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	var stdout, stderr bytes.Buffer
	code := runConformanceCanary([]string{
		"--provider", "p-canary",
		"--config", path,
		"--acknowledge-cost",
		"--max-cost-usd", "NaN",
		"--scenarios", "core",
		"--call-timeout", "2s",
		"--overall-timeout", "30s",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot encode canary report") {
		t.Fatalf("expected a cannot-encode-canary-report error, got:\n%s", stderr.String())
	}
}

// limitFileSize caps the process's RLIMIT_FSIZE for the duration of
// the test (restored in cleanup). Tests in this package run
// sequentially (no t.Parallel), so the window cannot disturb another
// test's file writes; pipe writes (test output) are unaffected.
func limitFileSize(t *testing.T, maxBytes uint64) {
	t.Helper()
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}
	if orig.Max < maxBytes {
		t.Skip("hard file-size limit is below the test limit")
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: maxBytes, Max: orig.Max}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) })
}

func TestCoverageAgentAWriteCanaryReportChmodError(t *testing.T) {
	old := canaryTempChmod
	canaryTempChmod = func(*os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { canaryTempChmod = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	err := writeCanaryReport(path, []byte(`{"ok":true}`))
	if err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("expected the injected chmod error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no report file after a chmod failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteCanaryReportCloseError(t *testing.T) {
	old := canaryTempClose
	canaryTempClose = func(*os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { canaryTempClose = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	err := writeCanaryReport(path, []byte(`{"ok":true}`))
	if err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("expected the injected close error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no report file after a close failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteCanaryReportWriteError(t *testing.T) {
	limitFileSize(t, 10)
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	err := writeCanaryReport(path, []byte(`{"ok":true,"padding":"0123456789abcdef"}`))
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected a file-too-large write error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no report file after a write failure, stat=%v", statErr)
	}
}

// --- policy.go ---

func TestCoverageAgentAAppendPolicyAuditWriteError(t *testing.T) {
	err := appendPolicyAudit("/dev/full", policyAuditRecord{
		Timestamp: "2026-11-06T00:00:00Z", Action: "promote",
		FromPolicy: "a", ToPolicy: "b",
	})
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("expected a /dev/full write error, got %v", err)
	}
}

func policyTestConfig(t *testing.T, dir string) (string, string) {
	t.Helper()
	cfg := config.Default()
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		{ID: "policy-a", Weights: config.DecisionPolicyWeights{RouterBaseline: 1}, MinScoreDelta: 0.1},
		{ID: "policy-b", Weights: config.DecisionPolicyWeights{Latency: 1}, MinScoreDelta: 0.2},
	}
	cfg.Decision.Policy = "policy-a"
	path := writeCoverageConfig(t, dir, cfg)
	return path, filepath.Join(dir, "policy-audit.jsonl")
}

func TestCoverageAgentAApplyPolicyPointerHashErrors(t *testing.T) {
	t.Run("before hash fails", func(t *testing.T) {
		dir := t.TempDir()
		path, auditPath := policyTestConfig(t, dir)
		old := hashConfigFile
		hashConfigFile = func(string) (string, error) { return "", errors.New("hash boom") }
		t.Cleanup(func() { hashConfigFile = old })
		var stderr bytes.Buffer
		_, code := applyPolicyPointer(path, auditPath, "promote", "policy-b", &stderr)
		if code != policyExitError {
			t.Fatalf("expected exit %d, got %d", policyExitError, code)
		}
		if !strings.Contains(stderr.String(), "hash config") {
			t.Fatalf("expected a hash-config error, got:\n%s", stderr.String())
		}
	})
	t.Run("after hash fails", func(t *testing.T) {
		dir := t.TempDir()
		path, auditPath := policyTestConfig(t, dir)
		old := hashConfigFile
		calls := 0
		hashConfigFile = func(p string) (string, error) {
			calls++
			if calls == 1 {
				return fileSHA256(p)
			}
			return "", errors.New("hash boom")
		}
		t.Cleanup(func() { hashConfigFile = old })
		var stderr bytes.Buffer
		_, code := applyPolicyPointer(path, auditPath, "promote", "policy-b", &stderr)
		if code != policyExitError {
			t.Fatalf("expected exit %d, got %d", policyExitError, code)
		}
		if !strings.Contains(stderr.String(), "hash rewritten config") {
			t.Fatalf("expected a hash-rewritten-config error, got:\n%s", stderr.String())
		}
		// The config was saved before the second hash failed.
		saved, err := config.LoadBase(path)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Decision.Policy != "policy-b" {
			t.Fatalf("expected the saved policy to be policy-b, got %q", saved.Decision.Policy)
		}
	})
}

func TestCoverageAgentAApplyPolicyPointerSaveError(t *testing.T) {
	dir := t.TempDir()
	path, auditPath := policyTestConfig(t, dir)
	limitFileSize(t, 10)
	var stderr bytes.Buffer
	_, code := applyPolicyPointer(path, auditPath, "promote", "policy-b", &stderr)
	if code != policyExitError {
		t.Fatalf("expected exit %d, got %d", policyExitError, code)
	}
	if !strings.Contains(stderr.String(), "save config") {
		t.Fatalf("expected a save-config error, got:\n%s", stderr.String())
	}
}

// --- controlplane_runtime.go: postgres through the runtime ---

func TestCoverageAgentAOpenControlPlanePostgresRuntime(t *testing.T) {
	state := &fakeCPState{applied: map[uint64]string{}}
	cfg := useFakeCPDriver(t, state)
	cfg.Enabled = true
	cfg.Backend = "postgres"
	rt, err := openControlPlaneRuntime(cfg, filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt == nil || rt.manager == nil || rt.store == nil || rt.closer == nil {
		t.Fatal("expected a runtime with manager, store and a non-nil closer (the pg pool)")
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	state.mu.Lock()
	_, applied := state.applied[1]
	state.mu.Unlock()
	if !applied {
		t.Fatal("expected migration version 1 to be applied through the runtime path")
	}
}

// --- audit.go: verify error + JSON format ---

func TestCoverageAgentAAuditVerifyChainError(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	path := writeCoverageConfig(t, dir, cfg)
	storePath := filepath.Join(dir, "security", "nexaroute-security.db")
	store, err := securitystore.Open(storePath, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	old := auditVerifyContext
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	auditVerifyContext = func() context.Context { return cancelled }
	t.Cleanup(func() { auditVerifyContext = old })
	var stdout, stderr bytes.Buffer
	code := runAudit([]string{"verify", "--config", path}, &stdout, &stderr)
	if code != auditExitUsage {
		t.Fatalf("expected exit %d, got %d (stdout=%s stderr=%s)", auditExitUsage, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "audit verify:") {
		t.Fatalf("expected an audit-verify error on stderr, got:\n%s", stderr.String())
	}
}

func TestCoverageAgentAAuditVerifyJSONFormat(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	path := writeCoverageConfig(t, dir, cfg)
	storePath := filepath.Join(dir, "security", "nexaroute-security.db")
	store, err := securitystore.Open(storePath, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runAudit([]string{"verify", "--config", path, "--format", "json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	var report securitystore.ChainReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("expected a valid JSON chain report, got %q: %v", stdout.String(), err)
	}
	if !report.Verified {
		t.Fatalf("expected a verified empty chain, got %+v", report)
	}
}

// --- doctor.go: verify error ---

func TestCoverageAgentADoctorSecurityStoreVerifyError(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	path := writeCoverageConfig(t, dir, cfg)
	storePath := filepath.Join(dir, "security", "nexaroute-security.db")
	store, err := securitystore.Open(storePath, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	old := doctorAuditContext
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	doctorAuditContext = func() (context.Context, context.CancelFunc) { return cancelled, func() {} }
	t.Cleanup(func() { doctorAuditContext = old })
	var w bytes.Buffer
	d := &doctor{w: &w}
	d.checkConfig(path)
	if d.cfg == nil {
		t.Fatalf("expected checkConfig to attach a config; output:\n%s", w.String())
	}
	w.Reset()
	d.checkSecurityStore(path)
	if d.fails == 0 || !strings.Contains(w.String(), "FAIL audit_chain: verification errored") {
		t.Fatalf("expected a FAIL audit_chain verification-errored line, got:\n%s", w.String())
	}
}

// --- diagnostics.go ---

func TestCoverageAgentADiagnosticsBundleSafetyScan(t *testing.T) {
	// The provider Name is only length-checked by config validation,
	// so a name carrying a deny pattern reaches the bundle's provider
	// summary unfiltered — and the final safety scan must reject it.
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "bearer abcdefgh12345678", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}},
	}}
	path := writeCoverageConfig(t, dir, cfg)
	_, _, err := buildDiagnosticsBundle(path, "", 0, false)
	if err == nil || !strings.Contains(err.Error(), "secret pattern") {
		t.Fatalf("expected a secret-pattern safety-scan error, got %v", err)
	}
}

func TestCoverageAgentADiagnosticsGatewayInfoVCS(t *testing.T) {
	old := readBuildInfo
	t.Cleanup(func() { readBuildInfo = old })
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-11-06T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		}}, true
	}
	info := diagnosticsGatewayInfo()
	if info.Commit != "abc123" || info.CommitTime != "2026-11-06T00:00:00Z" || !info.Modified {
		t.Fatalf("unexpected gateway info: %+v", info)
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: ""}}}, true
	}
	if got := diagnosticsGatewayInfo(); got.Commit != "unavailable" {
		t.Fatalf("expected unavailable commit for an empty revision, got %q", got.Commit)
	}
}

func TestCoverageAgentADiagnosticsRecentErrorsSeekError(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "log.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	writerCh := make(chan *os.File, 1)
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		writerCh <- w
	}()
	cfg := config.Default()
	cfg.Logging.File = fifo
	san := newDiagnosticsSanitizer(&cfg)
	result := diagnosticsRecentErrorsFrom(cfg, filepath.Join(dir, "config.json"), san)
	// The writer's open only returns once a reader opened the fifo,
	// so receiving here proves the function opened it — and the empty
	// result then proves the Seek (ESPIPE) failed, not the open.
	var writer *os.File
	select {
	case writer = <-writerCh:
		defer writer.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("fifo writer never opened; the function may not have opened the log path")
	}
	if result.LogAvailable || len(result.Included) != 0 {
		t.Fatalf("expected no log data from an unseekable fifo, got %+v", result)
	}
}

func TestCoverageAgentADiagnosticsRecentErrorsReadError(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Logging.File = dir // a directory: open+seek succeed, read fails EISDIR
	san := newDiagnosticsSanitizer(&cfg)
	result := diagnosticsRecentErrorsFrom(cfg, filepath.Join(dir, "config.json"), san)
	if result.LogAvailable || len(result.Included) != 0 {
		t.Fatalf("expected no log data from a directory log path, got %+v", result)
	}
}

func TestCoverageAgentADiagnosticsRecentErrorsStatError(t *testing.T) {
	// fstat(2) on a just-opened descriptor fails only on a failing
	// device, so the Stat error branch is driven through the fileStat
	// seam; without the fail-closed check the nil FileInfo panics at
	// info.Size() below.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gateway.log")
	if err := os.WriteFile(logPath, []byte("error: boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := fileStat
	fileStat = func(*os.File) (os.FileInfo, error) { return nil, errors.New("stat boom") }
	t.Cleanup(func() { fileStat = old })
	cfg := config.Default()
	cfg.Logging.File = logPath
	san := newDiagnosticsSanitizer(&cfg)
	result := diagnosticsRecentErrorsFrom(cfg, filepath.Join(dir, "config.json"), san)
	if result.LogAvailable || len(result.Included) != 0 {
		t.Fatalf("expected no log data when Stat fails, got %+v", result)
	}
}

type covcliFileInfo struct{ mode os.FileMode }

func (f covcliFileInfo) Name() string       { return "bundle" }
func (f covcliFileInfo) Size() int64        { return 0 }
func (f covcliFileInfo) Mode() os.FileMode  { return f.mode }
func (f covcliFileInfo) ModTime() time.Time { return time.Time{} }
func (f covcliFileInfo) IsDir() bool        { return false }
func (f covcliFileInfo) Sys() any           { return nil }

func TestCoverageAgentAWriteDiagnosticsBundleAtomicChmodError(t *testing.T) {
	old := diagnosticsTempChmod
	diagnosticsTempChmod = func(*os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { diagnosticsTempChmod = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	err := writeDiagnosticsBundleAtomic(path, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("expected the injected chmod error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no bundle file after a chmod failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteDiagnosticsBundleAtomicSyncError(t *testing.T) {
	old := diagnosticsTempSync
	diagnosticsTempSync = func(*os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { diagnosticsTempSync = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	err := writeDiagnosticsBundleAtomic(path, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("expected the injected sync error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no bundle file after a sync failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteDiagnosticsBundleAtomicCloseError(t *testing.T) {
	old := diagnosticsTempClose
	diagnosticsTempClose = func(*os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { diagnosticsTempClose = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	err := writeDiagnosticsBundleAtomic(path, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("expected the injected close error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no bundle file after a close failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteDiagnosticsBundleAtomicWriteError(t *testing.T) {
	limitFileSize(t, 10)
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	err := writeDiagnosticsBundleAtomic(path, []byte(`{"ok":true,"padding":"0123456789abcdef"}`))
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected a file-too-large write error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no bundle file after a write failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteDiagnosticsBundleAtomicStatError(t *testing.T) {
	old := diagnosticsBundleStat
	diagnosticsBundleStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat boom") }
	t.Cleanup(func() { diagnosticsBundleStat = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	err := writeDiagnosticsBundleAtomic(path, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "stat boom") {
		t.Fatalf("expected the injected stat error, got %v", err)
	}
}

func TestCoverageAgentAWriteDiagnosticsBundleAtomicModeMismatch(t *testing.T) {
	old := diagnosticsBundleStat
	diagnosticsBundleStat = func(string) (os.FileInfo, error) { return covcliFileInfo{mode: 0o644}, nil }
	t.Cleanup(func() { diagnosticsBundleStat = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	err := writeDiagnosticsBundleAtomic(path, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "want 0600") {
		t.Fatalf("expected a mode-mismatch error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected the bundle file to be removed after a mode mismatch, stat=%v", statErr)
	}
}

// --- qualitygate.go ---

func TestCoverageAgentAWriteReportCreateTempError(t *testing.T) {
	// A symlink to /sys makes MkdirAll a no-op (the path already
	// exists as a directory) while CreateTemp fails — EROFS as root,
	// EACCES as an unprivileged user (GitHub runners run
	// unprivileged, so asserting "read-only" failed in CI). Either
	// errno proves the CreateTemp branch under test.
	dir := t.TempDir()
	link := filepath.Join(dir, "ro")
	if err := os.Symlink("/sys", link); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := writeReport(filepath.Join(link, "report.json"), []byte(`{}`), &stdout)
	if err == nil || (!errors.Is(err, fs.ErrPermission) && !errors.Is(err, syscall.EROFS)) {
		t.Fatalf("expected a permission-denied or read-only-filesystem CreateTemp error, got %v", err)
	}
}

func TestCoverageAgentAWriteReportWriteError(t *testing.T) {
	limitFileSize(t, 10)
	dir := t.TempDir()
	var stdout bytes.Buffer
	err := writeReport(filepath.Join(dir, "report.json"), []byte(`{"ok":true,"padding":"0123456789abcdef"}`), &stdout)
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected a file-too-large write error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "report.json")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no report file after a write failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteReportCloseError(t *testing.T) {
	old := qualityGateTempClose
	qualityGateTempClose = func(*os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { qualityGateTempClose = old })
	dir := t.TempDir()
	var stdout bytes.Buffer
	err := writeReport(filepath.Join(dir, "report.json"), []byte(`{}`), &stdout)
	if err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("expected the injected close error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "report.json")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no report file after a close failure, stat=%v", statErr)
	}
}

func TestCoverageAgentAWriteReportChmodError(t *testing.T) {
	old := qualityGateTempChmod
	qualityGateTempChmod = func(string) error { return errors.New("chmod boom") }
	t.Cleanup(func() { qualityGateTempChmod = old })
	dir := t.TempDir()
	var stdout bytes.Buffer
	err := writeReport(filepath.Join(dir, "report.json"), []byte(`{}`), &stdout)
	if err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("expected the injected chmod error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "report.json")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no report file after a chmod failure, stat=%v", statErr)
	}
}

// qualityGateSnapshot returns a minimal valid snapshot JSON for
// runQualityGateCompare. score is written verbatim, so 1e999 becomes
// +Inf after unmarshal — ParseSnapshot validates provenance and suite
// count only, so the non-finite score survives into Compare's report.
func qualityGateSnapshot(score string) string {
	return `{"format":"quality-gate-snapshot/v1","summary":{"provenance":{"gateway_commit":"abc123","model":"model-a","deployment_id":"dep-1","mode":"replay","generated_at":"2026-11-06T00:00:00Z"},"suites":[{"suite_id":"tool_calling","suite_version":"v1","scoreable":true,"score":` + score + `,"score_observed":true,"samples":10,"cases":10,"pass":10,"fail":0,"error":0,"missing":0}],"upstream_calls":0}}`
}

func TestCoverageAgentAQualityGateCompareRejectsNonFiniteScore(t *testing.T) {
	// A snapshot score of 1e999 overflows float64, and the JSON
	// decoder rejects out-of-range numbers — so a non-finite score
	// can never reach Compare's report. This pins the input
	// validation that the MarshalIndent proof above relies on.
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.json")
	candPath := filepath.Join(dir, "cand.json")
	if err := os.WriteFile(basePath, []byte(qualityGateSnapshot("1e999")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candPath, []byte(qualityGateSnapshot("0.9")), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runQualityGateCompare([]string{
		"--baseline", basePath,
		"--candidate", candPath,
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid quality gate snapshot") {
		t.Fatalf("expected an invalid-snapshot error, got:\n%s", stderr.String())
	}
}

func TestCoverageAgentAQualityGateCompareZeroBaselineLatency(t *testing.T) {
	// A zero baseline latency with a positive candidate latency is a
	// regression via compareMetric's zero-baseline branch (there is
	// no DeltaPct to compute), and the comparison reports it. All six
	// required suites are present on both sides so the verdict is a
	// regression, not insufficient evidence.
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.json")
	candPath := filepath.Join(dir, "cand.json")
	suite := func(id string, latency int) string {
		return `{"suite_id":"` + id + `","suite_version":"v1","scoreable":true,"score":0.9,"score_observed":true,"samples":10,"cases":10,"pass":10,"fail":0,"error":0,"missing":0,"latency_p95_ms":` + strconv.Itoa(latency) + `,"latency_observed":true}`
	}
	snapshot := func(commit string, toolLatency int) string {
		suites := []string{suite("tool_calling", toolLatency)}
		for _, id := range []string{"structured_output", "protocol_compat", "multi_turn", "streaming", "cancellation"} {
			suites = append(suites, suite(id, 50))
		}
		return `{"format":"quality-gate-snapshot/v1","summary":{"provenance":{"gateway_commit":"` + commit + `","model":"model-a","deployment_id":"dep-1","mode":"replay","generated_at":"2026-11-06T00:00:00Z"},"suites":[` + strings.Join(suites, ",") + `],"upstream_calls":0}}`
	}
	if err := os.WriteFile(basePath, []byte(snapshot("abc123", 0)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candPath, []byte(snapshot("def456", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runQualityGateCompare([]string{
		"--baseline", basePath,
		"--candidate", candPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 (regression), got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "regressions=1") {
		t.Fatalf("expected one regression in the report line, got:\n%s", stdout.String())
	}
}

// --- secrets.go ---

// secretsRotateConfig returns a config whose provider api_key is
// sealed under nxs1 with an auto-managed key file, ready for rotate.
func secretsRotateConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := config.Default()
	c.Providers = []config.ProviderConfig{{ID: "private", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "cli-canary", Enabled: false}}
	if err := config.SaveAtomic(p, c); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCoverageAgentARotateRandError(t *testing.T) {
	p := secretsRotateConfig(t)
	old := secretsRandRead
	secretsRandRead = func([]byte) (int, error) { return 0, errors.New("rand boom") }
	t.Cleanup(func() { secretsRandRead = old })
	err := runSecrets([]string{"rotate", "--config", p})
	if err == nil || !strings.Contains(err.Error(), "cannot generate replacement master key") {
		t.Fatalf("expected a cannot-generate-key error, got %v", err)
	}
}

func TestCoverageAgentARotateReencryptError(t *testing.T) {
	// Seal a plaintext that itself looks like an nxs2 envelope under
	// nxs1: the decrypt opens it fine, but the re-encrypt refuses the
	// nxs2 prefix (it needs the Vault Transit provider).
	dir := t.TempDir()
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath+".key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.LoadCipher(configPath)
	if err != nil {
		t.Fatal(err)
	}
	env, err := cipher.SealField(context.Background(), "private", "api_key", []byte("nxs2:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"providers":[{"id":"private","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"` + env + `"}]}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runSecrets([]string{"rotate", "--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "cannot re-encrypt secrets") {
		t.Fatalf("expected a cannot-re-encrypt-secrets error, got %v", err)
	}
}

func TestCoverageAgentARotateStageError(t *testing.T) {
	p := secretsRotateConfig(t)
	// Fail only the staged-key publish: the recovery copy
	// (<config>.key.previous) must still succeed first, so the
	// injected failure is scoped to the ".key.next" target.
	old := rotateFileRename
	rotateFileRename = func(src, dst string) error {
		if strings.HasSuffix(dst, ".key.next") {
			return errors.New("rename boom")
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { rotateFileRename = old })
	err := runSecrets([]string{"rotate", "--config", p})
	if err == nil || !strings.Contains(err.Error(), "cannot stage replacement master key") {
		t.Fatalf("expected a cannot-stage error, got %v", err)
	}
}

func TestCoverageAgentARotateWriteConfigError(t *testing.T) {
	p := secretsRotateConfig(t)
	// Both 32-byte key files fit under a 100-byte limit; the
	// re-encrypted config (hundreds of bytes) does not, so the
	// failure lands on the config write.
	limitFileSize(t, 100)
	err := runSecrets([]string{"rotate", "--config", p})
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected a file-too-large config-write error, got %v", err)
	}
}

func TestCoverageAgentARotateActivateError(t *testing.T) {
	p := secretsRotateConfig(t)
	old := rotateActivateRename
	rotateActivateRename = func(string, string) error { return errors.New("rename boom") }
	t.Cleanup(func() { rotateActivateRename = old })
	err := runSecrets([]string{"rotate", "--config", p})
	if err == nil || !strings.Contains(err.Error(), "key activation is pending") {
		t.Fatalf("expected a key-activation-pending error, got %v", err)
	}
}

func TestCoverageAgentAWriteRotateFileChmodError(t *testing.T) {
	old := rotateFileChmod
	rotateFileChmod = func(*os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { rotateFileChmod = old })
	err := writeRotateFile(filepath.Join(t.TempDir(), "k"), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("expected the injected chmod error, got %v", err)
	}
}

func TestCoverageAgentAWriteRotateFileSyncError(t *testing.T) {
	old := rotateFileSync
	rotateFileSync = func(*os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { rotateFileSync = old })
	err := writeRotateFile(filepath.Join(t.TempDir(), "k"), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("expected the injected sync error, got %v", err)
	}
}

func TestCoverageAgentAWriteRotateFileCloseError(t *testing.T) {
	old := rotateFileClose
	rotateFileClose = func(*os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { rotateFileClose = old })
	err := writeRotateFile(filepath.Join(t.TempDir(), "k"), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("expected the injected close error, got %v", err)
	}
}

func TestCoverageAgentAWriteRotateFileWriteError(t *testing.T) {
	limitFileSize(t, 10)
	err := writeRotateFile(filepath.Join(t.TempDir(), "k"), []byte("0123456789abcdef"))
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected a file-too-large write error, got %v", err)
	}
}
