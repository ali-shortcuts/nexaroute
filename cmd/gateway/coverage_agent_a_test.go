// Coverage tests for cmd/gateway main() startup/shutdown paths (Agent A,
// 100% coverage mission). Every test asserts observable behavior — a fatal
// message, a log line, a healthy shutdown — never mere line execution.
//
// The fatalLog/osExit/evalSymlinks/osHostname/netListen/initBreakerSync
// seams in main.go exist so these paths can be driven deterministically:
// several of them (hostname failure, listener accept errors, breaker-sync
// construction) have no filesystem or network trigger.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/breakersync"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/desktop"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// fatalPanic carries a message recorded by the fatalLog test stub.
type fatalPanic struct{ msg string }

// exitPanic carries the code recorded by the osExit test stub.
type exitPanic struct{ code int }

// installFatalStub replaces fatalLog with a stub that records the formatted
// message and panics with fatalPanic, so main() can be run in a goroutine
// and its fatal observed instead of exiting the test process.
func installFatalStub(t *testing.T) {
	t.Helper()
	old := fatalLog
	fatalLog = func(_ *log.Logger, format string, args ...any) {
		panic(fatalPanic{msg: fmt.Sprintf(format, args...)})
	}
	t.Cleanup(func() { fatalLog = old })
}

// installExitStub replaces osExit with a stub that records the code and
// panics with exitPanic.
func installExitStub(t *testing.T) *[]int {
	t.Helper()
	codes := &[]int{}
	old := osExit
	osExit = func(code int) {
		*codes = append(*codes, code)
		panic(exitPanic{code: code})
	}
	t.Cleanup(func() { osExit = old })
	return codes
}

func unsetEnvVars(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		unsetGatewayEnv(t, name)
	}
}

// clearGatewayEnv mirrors the lifecycle test's env hygiene, except it
// UNSETS NEXAROUTE_LOG_FILE: a set-but-empty value forces logging.file=off
// (config.ApplyEnvOverrides) and would defeat tests that need the log file.
func clearGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "false")
	t.Setenv("NEXAROUTE_LISTEN", "")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "")
	unsetGatewayEnv(t, "NEXAROUTE_ADMIN_BIND_LOCAL_ONLY")
	unsetGatewayEnv(t, "NEXAROUTE_LOG_FILE")
}

// baseTestConfig returns a minimal valid config listening on a free loopback
// port with probes and logging disabled, plus the address string.
func baseTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	cfg := config.Default()
	cfg.Listen = address
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Logging.File = "off"
	cfg.Logging.ConsoleMaxLinesPerMinute = 0
	return cfg, address
}

func writeCoverageConfig(t *testing.T, dir string, cfg config.Config) string {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

// mainRun is a main() invocation in a goroutine with captured stdout/stderr
// and a fatalLog stub that reports through r.fatal instead of exiting.
type mainRun struct {
	done       chan struct{}
	fatal      chan string
	cancelCh   chan context.CancelFunc
	stdoutFile *os.File
	stderrFile *os.File
}

// startMain runs main() in a goroutine with the given args. notify, when
// non-nil, replaces notifyGatewayContext for the run; when nil the default
// cancellable context is used and its cancel func is delivered on
// r.cancelCh. The caller must end the run with waitFatal, waitDone, or
// cancelAndWait before the test returns.
func startMain(t *testing.T, args []string, notify func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)) *mainRun {
	t.Helper()
	installFatalStub(t)
	dir := t.TempDir()
	stdoutFile, err := os.CreateTemp(dir, "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	stderrFile, err := os.CreateTemp(dir, "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	oldArgs, oldStdout, oldStderr, oldNotify := os.Args, os.Stdout, os.Stderr, notifyGatewayContext
	os.Args, os.Stdout, os.Stderr = args, stdoutFile, stderrFile
	r := &mainRun{
		done:       make(chan struct{}),
		fatal:      make(chan string, 1),
		cancelCh:   make(chan context.CancelFunc, 1),
		stdoutFile: stdoutFile,
		stderrFile: stderrFile,
	}
	if notify == nil {
		notify = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(parent)
			r.cancelCh <- cancel
			return ctx, cancel
		}
	}
	notifyGatewayContext = notify
	go func() {
		defer close(r.done)
		defer func() {
			if rec := recover(); rec != nil {
				if fp, ok := rec.(fatalPanic); ok {
					r.fatal <- fp.msg
					return
				}
				panic(rec)
			}
		}()
		main()
	}()
	t.Cleanup(func() {
		os.Args, os.Stdout, os.Stderr, notifyGatewayContext = oldArgs, oldStdout, oldStderr, oldNotify
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
	})
	return r
}

// waitFatal waits for main() to fatal and returns the recorded message.
func (r *mainRun) waitFatal(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case msg := <-r.fatal:
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Fatal("main fatalled but did not return")
		}
		return msg
	case <-r.done:
		select {
		case msg := <-r.fatal:
			return msg
		default:
			t.Fatal("main returned without a fatal")
			return ""
		}
	case <-time.After(timeout):
		t.Fatal("main did not fatal within the timeout")
		return ""
	}
}

// waitDone waits for main() to return normally, failing if it fatals.
func (r *mainRun) waitDone(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-r.done:
	case msg := <-r.fatal:
		t.Fatalf("main fatalled unexpectedly: %s", msg)
	case <-time.After(timeout):
		t.Fatal("main did not return within the timeout")
	}
}

// cancelAndWait cancels the gateway context and waits for a clean shutdown.
func (r *mainRun) cancelAndWait(t *testing.T) {
	t.Helper()
	var cancel context.CancelFunc
	select {
	case cancel = <-r.cancelCh:
	case <-time.After(10 * time.Second):
		t.Fatal("main never reached signal setup")
	}
	cancel()
	select {
	case <-r.done:
	case msg := <-r.fatal:
		t.Fatalf("main fatalled during shutdown: %s", msg)
	case <-time.After(30 * time.Second):
		t.Fatal("gateway did not shut down within 30s")
	}
}

func (r *mainRun) stdoutString(t *testing.T) string {
	t.Helper()
	if _, err := r.stdoutFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.stdoutFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (r *mainRun) stderrString(t *testing.T) string {
	t.Helper()
	if _, err := r.stderrFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.stderrFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// waitHealthy polls /healthz until it returns 200, failing if the gateway
// fatals first.
func waitHealthy(t *testing.T, r *mainRun, scheme, address string) {
	t.Helper()
	transport := &http.Transport{}
	if scheme == "https" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- test certificate is self-signed.
	}
	client := &http.Client{Timeout: 200 * time.Millisecond, Transport: transport}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case msg := <-r.fatal:
			t.Fatalf("gateway fatalled while waiting for health: %s", msg)
		default:
		}
		resp, err := client.Get(scheme + "://" + address + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("gateway did not become healthy at %s://%s within 15s", scheme, address)
}

// fakeBrowser installs an executable google-chrome stub in PATH and a
// graphical session so desktop.OpenBrowser succeeds.
func fakeBrowser(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "google-chrome")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
}

// failingListener is a listener whose Accept always fails, driving the
// server_error fatal path that no real listener can reach (the server
// tolerates handshake and transient accept errors).
type failingListener struct {
	net.Listener
	err error
}

func (l *failingListener) Accept() (net.Conn, error) { return nil, l.err }

// waitForOutput polls one of the captured streams until needle appears.
func (r *mainRun) waitForOutput(t *testing.T, stream string, needle string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var out string
		if stream == "stdout" {
			out = r.stdoutString(t)
		} else {
			out = r.stderrString(t)
		}
		if strings.Contains(out, needle) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q in %s; output so far:\n%s", needle, stream, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// --- Startup fatal paths ---

func TestCoverageAgentAMainNoConfigPath(t *testing.T) {
	clearGatewayEnv(t)
	unsetEnvVars(t, "HOME", "XDG_CONFIG_HOME", "NEXAROUTE_CONFIG")
	r := startMain(t, []string{"nexaroute", "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot locate user config directory") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainAbsError(t *testing.T) {
	clearGatewayEnv(t)
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	r := startMain(t, []string{"nexaroute", "-config", "relative.json", "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "no such file or directory") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainMkdirAllError(t *testing.T) {
	clearGatewayEnv(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := startMain(t, []string{"nexaroute", "-config", filepath.Join(blocker, "config.json"), "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "not a directory") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainEvalSymlinksError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, _ := baseTestConfig(t)
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	old := evalSymlinks
	evalSymlinks = func(string) (string, error) { return "", errors.New("symlink boom") }
	t.Cleanup(func() { evalSymlinks = old })
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "symlink boom") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainLockError(t *testing.T) {
	clearGatewayEnv(t)
	// /sys is read-only, so the lock file cannot be created there.
	r := startMain(t, []string{"nexaroute", "-config", "/sys/config.json", "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot acquire local gateway lock") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAEnsureConfigStatError(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop.json")
	if err := os.Symlink("loop.json", loop); err != nil {
		t.Fatal(err)
	}
	err := ensureConfig(loop)
	if err == nil {
		t.Fatal("expected a stat error for a symlink loop")
	}
	if !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoverageAgentAMainEnsureConfigError(t *testing.T) {
	clearGatewayEnv(t)
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop.json")
	if err := os.Symlink("loop.json", loop); err != nil {
		t.Fatal(err)
	}
	r := startMain(t, []string{"nexaroute", "-config", loop, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "too many levels of symbolic links") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainLoadError(t *testing.T) {
	clearGatewayEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "invalid character") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainControlPlaneError(t *testing.T) {
	clearGatewayEnv(t)
	unsetEnvVars(t, "NEXAROUTE_CP_DSN_MISSING")
	cfg, _ := baseTestConfig(t)
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.Backend = "postgres"
	cfg.ControlPlane.PostgresDSNEnv = "NEXAROUTE_CP_DSN_MISSING"
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot initialize control-plane runtime") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
	if !strings.Contains(msg, "NEXAROUTE_CP_DSN_MISSING") {
		t.Fatalf("expected the DSN env name in the fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainRegistryError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, _ := baseTestConfig(t)
	// A query string passes config validation (validateEndpointURL does
	// not reject it) but the egress transport refuses it at adapter
	// construction, so the failure surfaces at NewRegistry, not Load.
	cfg.Providers = []config.ProviderConfig{{
		ID: "p-query", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9/v1?x=1", Enabled: true,
	}}
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "malformed") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainSecurityStoreError(t *testing.T) {
	clearGatewayEnv(t)
	unsetEnvVars(t, "NEXAROUTE_SECSTORE_MISSING")
	cfg, _ := baseTestConfig(t)
	cfg.Admin.SecurityStoreBackend = "postgres"
	cfg.Admin.SecurityStoreURL = "env:NEXAROUTE_SECSTORE_MISSING"
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot initialize admin security store") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
	if !strings.Contains(msg, "NEXAROUTE_SECSTORE_MISSING") {
		t.Fatalf("expected the store URL env name in the fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainOTLPError(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("NEXAROUTE_OTLP_BAD", "ftp://collector:4318/v1/traces")
	cfg, _ := baseTestConfig(t)
	cfg.Telemetry.OTLPEndpointEnv = "NEXAROUTE_OTLP_BAD"
	cfg.Telemetry.ServiceName = "coverage-test"
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot initialize telemetry exporter") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
	if !strings.Contains(msg, "invalid OTLP endpoint") {
		t.Fatalf("expected the endpoint rejection in the fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainVideoInitError(t *testing.T) {
	clearGatewayEnv(t)
	unsetEnvVars(t, "NEXAROUTE_VIDEO_TOKEN_MISSING")
	cfg, _ := baseTestConfig(t)
	cfg.Video.Enabled = true
	cfg.Video.AuthTokenEnv = "NEXAROUTE_VIDEO_TOKEN_MISSING"
	cfg.Video.DevelopmentFakeProvider = true
	dir := t.TempDir()
	cfg.Video.StorePath = filepath.Join(dir, "video-jobs.json")
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot initialize video gateway") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
	if !strings.Contains(msg, "NEXAROUTE_VIDEO_TOKEN_MISSING") {
		t.Fatalf("expected the token env name in the fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainNexaflowError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, _ := baseTestConfig(t)
	cfg.Nexaflow.Enabled = true
	cfg.Nexaflow.StoreBackend = "file"
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Nexaflow.StorePath = filepath.Join(blocker, "store.json")
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot initialize nexaflow service") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
	if !strings.Contains(msg, "open store") {
		t.Fatalf("expected the store-open failure in the fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainVideoStartError(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("NEXAROUTE_COV_VIDEO_TOKEN", "test-token")
	cfg, _ := baseTestConfig(t)
	cfg.Video.Enabled = true
	cfg.Video.AuthTokenEnv = "NEXAROUTE_COV_VIDEO_TOKEN"
	cfg.Video.DevelopmentFakeProvider = true
	dir := t.TempDir()
	cfg.Video.StorePath = filepath.Join(dir, "video-jobs.json")
	path := writeCoverageConfig(t, dir, cfg)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	notify := func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
		return cancelled, func() {}
	}
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, notify)
	msg := r.waitFatal(t, 15*time.Second)
	if !strings.Contains(msg, "cannot recover video jobs") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
	if !strings.Contains(msg, "context canceled") {
		t.Fatalf("expected the cancelled-context cause in the fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainListenError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	occupant, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot listen on "+address) {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainTLSError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, _ := baseTestConfig(t)
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.TLS.Enabled = true
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "TLS configuration is invalid") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainBreakerSyncInitError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, _ := baseTestConfig(t)
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.Backend = "memory"
	cfg.ControlPlane.BreakerSync = true
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	old := initBreakerSync
	initBreakerSync = func(*controlPlaneRuntime, *health.Manager, string, time.Duration, func(string, ...any)) (*breakersync.Syncer, error) {
		return nil, errors.New("syncer boom")
	}
	t.Cleanup(func() { initBreakerSync = old })
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 10*time.Second)
	if !strings.Contains(msg, "cannot initialize breaker sync") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
	if !strings.Contains(msg, "syncer boom") {
		t.Fatalf("expected the syncer cause in the fatal message: %q", msg)
	}
}

func TestCoverageAgentAMainServerError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	realLn, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	oldListen := netListen
	netListen = func(string, string) (net.Listener, error) {
		return &failingListener{Listener: realLn, err: errors.New("accept boom")}, nil
	}
	t.Cleanup(func() { netListen = oldListen })
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	msg := r.waitFatal(t, 15*time.Second)
	if !strings.Contains(msg, "server_error=accept boom") {
		t.Fatalf("unexpected fatal message: %q", msg)
	}
}

// TestCoverageAgentAMainDispatchExitCode exercises main's subcommand
// dispatch directly: an unknown config subcommand exits 2, while a valid
// validate returns 0 without exiting.
func TestCoverageAgentAMainDispatchExitCode(t *testing.T) {
	clearGatewayEnv(t)
	codes := installExitStub(t)
	cfg, _ := baseTestConfig(t)
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	stdoutFile, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	oldArgs, oldStdout, oldStderr := os.Args, os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutFile, stderrFile
	t.Cleanup(func() {
		os.Args, os.Stdout, os.Stderr = oldArgs, oldStdout, oldStderr
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
	})
	runMain := func(args []string) {
		os.Args = args
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					if _, ok := rec.(exitPanic); ok {
						return
					}
					panic(rec)
				}
			}()
			main()
		}()
	}
	runMain([]string{"nexaroute", "config", "bogus"})
	runMain([]string{"nexaroute", "config", "validate", "--config", path})
	if len(*codes) != 1 || (*codes)[0] != 2 {
		t.Fatalf("expected exactly one osExit(2), got %v", *codes)
	}
}

// --- Unit tests for the small main.go helpers ---

func TestCoverageAgentALogCloseError(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	logCloseError(logger, "test_close_error", nil)
	if buf.Len() != 0 {
		t.Fatalf("nil error logged %q", buf.String())
	}
	logCloseError(logger, "test_close_error", errors.New("boom"))
	if !strings.Contains(buf.String(), "test_close_error=boom") {
		t.Fatalf("expected label and error in log, got %q", buf.String())
	}
}

func TestCoverageAgentAInitBreakerSync(t *testing.T) {
	hm := health.New(3, time.Second)
	if _, err := initBreakerSync(&controlPlaneRuntime{}, hm, "r1", time.Second, log.Printf); err == nil {
		t.Fatal("expected an error for a runtime with no store")
	}
	rt, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "memory"}, filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if rt == nil {
		t.Fatal("expected a runtime for the memory backend")
	}
	if _, err := initBreakerSync(rt, hm, "", time.Second, log.Printf); err == nil {
		t.Fatal("expected an error for an empty replica ID")
	}
	// A typed-nil *health.Manager would NOT trip breakersync.New's
	// manager check (a non-nil interface holding a nil pointer), so the
	// empty replica ID above is the construction failure reachable
	// through this seam; main always passes a live manager.
	syncer, err := initBreakerSync(rt, hm, "r1", time.Second, log.Printf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if syncer == nil {
		t.Fatal("expected a non-nil syncer")
	}
}

// --- Happy-path lifecycle variants ---

func TestCoverageAgentAMainLogFileAuto(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	cfg.Logging.File = "auto"
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "http", address)
	r.cancelAndWait(t)
	logPath := filepath.Join(dir, "nexaroute.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected the auto log file at %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), "bounded_log_file=") {
		t.Fatalf("expected bounded_log_file line in %s, got:\n%s", logPath, string(data))
	}
}

func TestCoverageAgentAMainVideoPlannerDefaults(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("NEXAROUTE_COV_VIDEO_TOKEN", "test-token")
	cfg, address := baseTestConfig(t)
	// Listen on all interfaces so the planner URL derivation takes the
	// 0.0.0.0 -> 127.0.0.1 branch.
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "0.0.0.0:" + port
	cfg.Routing.PublicModel = "test-public"
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "default", Name: "Default (all eligible)", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "default", Name: "Default", CandidatePool: "default"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "ve-public", RouteProfile: "default"}}
	cfg.Providers = []config.ProviderConfig{
		{
			ID: "p-enabled", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
			Enabled: true,
			Models: []config.ModelConfig{
				{ID: "m1", Model: "model-a", Enabled: true, Aliases: []string{"model-a-alias"}},
				{ID: "m2", Model: "model-b", Enabled: false},
			},
		},
		{
			ID: "p-enabled2", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
			Enabled: true,
			Models: []config.ModelConfig{
				{ID: "m1", Model: "model-a", Enabled: true},
			},
		},
		{
			ID: "p-disabled", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
			Enabled: false,
			Models: []config.ModelConfig{
				{ID: "m1", Model: "model-never", Enabled: true},
			},
		},
	}
	cfg.Video.Enabled = true
	cfg.Video.AuthTokenEnv = "NEXAROUTE_COV_VIDEO_TOKEN"
	cfg.Video.DevelopmentFakeProvider = true
	cfg.Video.CodeRenderEnabled = true
	cfg.Video.CodeRenderPlannerModel = "pinned-planner"
	dir := t.TempDir()
	cfg.Video.StorePath = filepath.Join(dir, "video-jobs.json")
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "http", "127.0.0.1:"+port)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainBreakerSyncHappyPath(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	dir := t.TempDir()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.Backend = "file"
	cfg.ControlPlane.StatePath = filepath.Join(dir, "cp-state.json")
	cfg.ControlPlane.BreakerSync = true
	cfg.ControlPlane.BreakerSyncReplicaID = ""
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "http", address)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainBreakerSyncUnknownHost(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	dir := t.TempDir()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.Backend = "file"
	cfg.ControlPlane.StatePath = filepath.Join(dir, "cp-state.json")
	cfg.ControlPlane.BreakerSync = true
	cfg.ControlPlane.BreakerSyncReplicaID = ""
	path := writeCoverageConfig(t, dir, cfg)
	old := osHostname
	osHostname = func() (string, error) { return "", errors.New("hostname boom") }
	t.Cleanup(func() { osHostname = old })
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "http", address)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainDrainStreamIdle(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	cfg.Providers = []config.ProviderConfig{{
		ID: "p-slow", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		Enabled: true, StreamIdleTimeoutSeconds: 120,
	}}
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "http", address)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainDrainMinimum(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	cfg.Routing.RequestTimeoutMS = 1000
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "http", address)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainShutdownError(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	// 1s request timeout -> 10s drain floor -> 15s shutdown budget, well
	// under the server's 60s read timeout, so a connection stuck mid-body
	// forces Shutdown to return a deadline error.
	cfg.Routing.RequestTimeoutMS = 1000
	cfg.Logging.File = "auto"
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "http", address)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	// Complete headers claiming a 100-byte JSON body, then a partial body
	// that never completes: the data-plane handler blocks in readJSON.
	req := "POST /v1/messages HTTP/1.1\r\nHost: " + address + "\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n" +
		`{"model": "test-public", "messages": [{"role": "user", "content": "hel`
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	r.cancelAndWait(t)
	// The handler is still blocked in readJSON on this connection: it
	// only finishes (and writes its status=400 access-log line) once
	// the client side closes. Close it now, then wait for that line
	// before the test ends — otherwise the late write lands during
	// TempDir's RemoveAll and re-creates nexaroute.log after it was
	// deleted, failing cleanup with "directory not empty" (flaked
	// ~1/3 of runs). Closing before cancelAndWait would be wrong: the
	// handler would finish early and Shutdown would never hit its
	// deadline, so there would be no shutdown_error to assert.
	conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "nexaroute.log"))
		if err == nil && strings.Contains(string(data), "status=400") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the stuck request's access-log line; log=%q err=%v", string(data), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(dir, "nexaroute.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "shutdown_error=") {
		t.Fatalf("expected shutdown_error in log, got:\n%s", string(data))
	}
}

func TestCoverageAgentAMainUIReadinessFailed(t *testing.T) {
	clearGatewayEnv(t)
	cfg, address := baseTestConfig(t)
	dir := t.TempDir()
	certFile, keyFile := writeTestTLSMaterial(t, dir)
	cfg.TLS.Enabled = true
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path, "-no-browser"}, nil)
	waitHealthy(t, r, "https", address)
	// The readiness probe retries for 10s (plain client, self-signed cert)
	// before giving up; wait past it for the bootstrap message.
	r.waitForOutput(t, "stderr", "UI readiness check failed", 15*time.Second)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainBrowserUnavailable(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	cfg, address := baseTestConfig(t)
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path}, nil)
	waitHealthy(t, r, "http", address)
	r.waitForOutput(t, "stderr", "browser unavailable", 10*time.Second)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainBrowserOpened(t *testing.T) {
	clearGatewayEnv(t)
	fakeBrowser(t)
	cfg, address := baseTestConfig(t)
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	r := startMain(t, []string{"nexaroute", "-config", path}, nil)
	waitHealthy(t, r, "http", address)
	r.waitForOutput(t, "stdout", "Opened NexaRoute dashboard with google-chrome", 10*time.Second)
	r.cancelAndWait(t)
}

func TestCoverageAgentAMainLockHeldOpensExistingUI(t *testing.T) {
	clearGatewayEnv(t)
	fakeBrowser(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	cfg, _ := baseTestConfig(t)
	cfg.Listen = upstream.Listener.Addr().String()
	dir := t.TempDir()
	path := writeCoverageConfig(t, dir, cfg)
	lock, acquired, err := desktop.Acquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected to acquire the config lock")
	}
	defer lock.Close()
	r := startMain(t, []string{"nexaroute", "-config", path}, nil)
	r.waitDone(t, 15*time.Second)
	out := r.stdoutString(t)
	if !strings.Contains(out, "NexaRoute is already running; opened dashboard with google-chrome:") {
		t.Fatalf("expected already-running browser line, got:\n%s", out)
	}
}
