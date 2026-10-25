//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

// gatewayAcceptanceProcess tracks one actual NexaRoute gateway binary started
// by the integration test. Logs go to a file so startup failures are preserved
// without introducing concurrent writes to a bytes.Buffer.
type gatewayAcceptanceProcess struct {
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	waitErr error
}

func (p *gatewayAcceptanceProcess) logs() string {
	data, _ := os.ReadFile(p.logPath)
	return string(data)
}

func (p *gatewayAcceptanceProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
	}
}

// TestConcurrentPostgresGatewayProcessesBootstrap starts two separately
// configured gateway binaries at the same time after clearing the reserved
// startup marker from a real PostgreSQL control-plane store. Both processes
// must become healthy and the database must contain one canonical revision-1
// marker. This exercises the complete process startup path, not just Manager.
func TestConcurrentPostgresGatewayProcessesBootstrap(t *testing.T) {
	const dsnEnv = "NEXAROUTE_TEST_POSTGRES_DSN"
	dsn := strings.TrimSpace(os.Getenv(dsnEnv))
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN is not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	binary := filepath.Join(root, "nexaroute")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build gateway binary: %v\n%s", err, buildOutput)
	}

	cpCfg := config.ControlPlaneConfig{
		Enabled: true, Backend: "postgres", PostgresDSNEnv: dsnEnv,
		ConfigFailure: "fail_closed",
	}
	store, db, err := openPostgresControlPlane(cpCfg)
	if err != nil {
		t.Fatalf("open PostgreSQL for process acceptance: %v", err)
	}
	defer db.Close()

	// The service container may retain tables created by migration, but the
	// gateway/runtime row itself must be absent to reproduce first bootstrap.
	if current, getErr := store.Get(ctx, controlPlaneNamespace); getErr == nil {
		if err := store.Delete(ctx, controlPlaneNamespace, current.Revision); err != nil {
			t.Fatalf("clear previous bootstrap marker: %v", err)
		}
	} else if !errors.Is(getErr, controlplane.ErrNotFound) {
		t.Fatalf("inspect bootstrap marker before test: %v", getErr)
	}

	type launchSpec struct {
		configPath string
		home       string
		logPath    string
		port       int
	}
	specs := make([]launchSpec, 2)
	for i := range specs {
		dir := filepath.Join(root, fmt.Sprintf("replica-%d", i+1))
		home := filepath.Join(dir, "home")
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		addr, port, err := reserveGatewayAcceptanceAddress(t)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.Listen = addr
		cfg.ControlPlane = cpCfg
		cfg.Probe.Enabled = false
		configPath := filepath.Join(dir, "config.json")
		if err := config.SaveAtomic(configPath, cfg); err != nil {
			t.Fatalf("write replica %d config: %v", i+1, err)
		}
		specs[i] = launchSpec{
			configPath: configPath,
			home: home,
			logPath: filepath.Join(dir, "gateway.log"),
			port: port,
		}
	}

	processes := make([]*gatewayAcceptanceProcess, 2)
	t.Cleanup(func() {
		for _, p := range processes {
			p.stop()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if current, getErr := store.Get(cleanupCtx, controlPlaneNamespace); getErr == nil {
			_ = store.Delete(cleanupCtx, controlPlaneNamespace, current.Revision)
		}
	})

	type startResult struct {
		index int
		proc  *gatewayAcceptanceProcess
		err   error
	}
	start := make(chan struct{})
	started := make(chan startResult, len(specs))
	for i, spec := range specs {
		go func(index int, spec launchSpec) {
			<-start
			logFile, err := os.Create(spec.logPath)
			if err != nil {
				started <- startResult{index: index, err: err}
				return
			}
			cmd := exec.Command(binary, "--config", spec.configPath, "--no-browser")
			cmd.Env = replaceAcceptanceEnv(os.Environ(),
				dsnEnv, dsn,
				"HOME", spec.home,
				"XDG_CONFIG_HOME", filepath.Join(spec.home, ".config"))
			cmd.Stdout = logFile
			cmd.Stderr = logFile
			if err := cmd.Start(); err != nil {
				_ = logFile.Close()
				started <- startResult{index: index, err: fmt.Errorf("start gateway: %w", err)}
				return
			}
			_ = logFile.Close()
			proc := &gatewayAcceptanceProcess{cmd: cmd, logPath: spec.logPath, done: make(chan struct{})}
			go func() {
				proc.waitErr = cmd.Wait()
				close(proc.done)
			}()
			started <- startResult{index: index, proc: proc}
		}(i, spec)
	}
	close(start)
	var launchFailures []string
	for range specs {
		result := <-started
		if result.err != nil {
			launchFailures = append(launchFailures, fmt.Sprintf("replica %d launch failed: %v", result.index+1, result.err))
			continue
		}
		processes[result.index] = result.proc
	}
	if len(launchFailures) > 0 {
		t.Fatalf("%s", strings.Join(launchFailures, "; "))
	}

	for i, proc := range processes {
		if err := waitForGatewayAcceptanceHealth(ctx, proc, specs[i].port); err != nil {
			t.Fatalf("replica %d did not become healthy: %v\nlogs:\n%s", i+1, err, proc.logs())
		}
	}

	current, err := store.Get(ctx, controlPlaneNamespace)
	if err != nil {
		t.Fatalf("read winning gateway bootstrap marker: %v", err)
	}
	const wantPayload = `{"schema":1,"backend":"postgres"}`
	if string(current.Payload) != wantPayload || current.Revision != 1 {
		t.Fatalf("unexpected bootstrap state after two process starts: revision=%d payload=%q; want revision=1 payload=%q",
			current.Revision, current.Payload, wantPayload)
	}
}

func reserveGatewayAcceptanceAddress(t *testing.T) (string, int, error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, err
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", 0, err
	}
	_, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return "", 0, err
	}
	return addr, port, nil
}

func replaceAcceptanceEnv(base []string, pairs ...string) []string {
	out := append([]string(nil), base...)
	for i := 0; i+1 < len(pairs); i += 2 {
		prefix := pairs[i] + "="
		filtered := out[:0]
		for _, item := range out {
			if !strings.HasPrefix(item, prefix) {
				filtered = append(filtered, item)
			}
		}
		out = append(filtered, pairs[i]+"="+pairs[i+1])
	}
	return out
}

func waitForGatewayAcceptanceHealth(ctx context.Context, proc *gatewayAcceptanceProcess, port int) error {
	client := &http.Client{Timeout: 750 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-proc.done:
			return fmt.Errorf("gateway exited before health check completed: %v", proc.waitErr)
		default:
		}
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		req, _ := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s: %w", url, ctx.Err())
		case <-proc.done:
			return fmt.Errorf("gateway exited before health check completed: %v", proc.waitErr)
		case <-ticker.C:
		}
	}
}
