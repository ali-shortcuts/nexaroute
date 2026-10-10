package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestMainStartsHealthyGatewayAndShutsDownOnContextCancellation(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "false")
	t.Setenv("NEXAROUTE_LISTEN", "")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "")
	unsetGatewayEnv(t, "NEXAROUTE_ADMIN_BIND_LOCAL_ONLY")
	t.Setenv("NEXAROUTE_LOG_FILE", "")

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	cfg := config.Default()
	cfg.Listen = address
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Logging.File = "off"
	cfg.Logging.ConsoleMaxLinesPerMinute = 0
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	oldArgs, oldStdout, oldNotify := os.Args, os.Stdout, notifyGatewayContext
	defer func() {
		os.Args, os.Stdout, notifyGatewayContext = oldArgs, oldStdout, oldNotify
		_ = stdout.Close()
	}()
	os.Args = []string{"nexaroute", "-config", configPath, "-no-browser"}
	os.Stdout = stdout
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

	client := &http.Client{Timeout: 150 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, getErr := client.Get("http://" + address + "/healthz")
		if getErr == nil {
			_, readErr := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	client.CloseIdleConnections()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not complete graceful shutdown")
	}
	if !ready {
		t.Fatal("gateway did not become healthy before shutdown")
	}
}

func unsetGatewayEnv(t *testing.T, name string) {
	t.Helper()
	old, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, old)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}
