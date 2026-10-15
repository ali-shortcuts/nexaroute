package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Binary-level regression test for the missing `config` dispatch defect
// (found by the 2026-10-15 audit, proven against a binary built from
// main f06cf12): runConfig existed and was unit-tested through direct
// calls, but main() never dispatched the subcommand, so the real binary
// ignored `nexaroute config ...` and started the gateway instead.
//
// The function-level routing is pinned by TestDispatchSubcommandRouting;
// this test pins the compiled artifact itself: it builds the gateway
// binary once, then runs `config validate`, `config diff` and
// `config dry-run` as subprocesses. Each invocation must exit promptly
// with runConfig's own output and exit code — a binary that falls
// through to gateway startup would keep running (and log its listener)
// until the per-invocation timeout kills it, failing the test. On the
// pre-fix baseline all three subcommands fail exactly that way.
func TestBinaryConfigSubcommandsReachRunConfig(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not in PATH; cannot build the gateway binary")
	}
	bin := filepath.Join(t.TempDir(), "nexaroute")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, goTool, "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	configPath := writeTestConfig(t, config.Default())

	run := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		out, err := cmd.CombinedOutput()
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("binary did not exit within 15s for %v; it likely ignored the subcommand and started the gateway. output so far:\n%s", args, out)
		}
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return string(out), exitErr.ExitCode()
			}
			t.Fatalf("running binary %v: %v\n%s", args, err, out)
		}
		return string(out), 0
	}

	t.Run("validate", func(t *testing.T) {
		out, code := run(t, "config", "validate", "--config", configPath)
		if code != 0 || !strings.Contains(out, "config valid:") {
			t.Fatalf("code=%d output=%q", code, out)
		}
		if strings.Contains(out, "listening=") {
			t.Fatalf("gateway listener started for a config subcommand:\n%s", out)
		}
	})

	t.Run("diff", func(t *testing.T) {
		out, code := run(t, "config", "diff", "--config", configPath, "--against", configPath)
		if code != 0 || !strings.Contains(out, "no differences") {
			t.Fatalf("code=%d output=%q", code, out)
		}
		if strings.Contains(out, "listening=") {
			t.Fatalf("gateway listener started for a config subcommand:\n%s", out)
		}
	})

	t.Run("dry-run", func(t *testing.T) {
		out, code := run(t, "config", "dry-run", "--config", configPath)
		if code != 0 || !strings.Contains(out, "dry-run passed:") || !strings.Contains(out, "no listener opened") {
			t.Fatalf("code=%d output=%q", code, out)
		}
		if strings.Contains(out, "listening=") {
			t.Fatalf("gateway listener started for a config subcommand:\n%s", out)
		}
	})

	t.Run("validate invalid config fails through runConfig", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, code := run(t, "config", "validate", "--config", bad)
		if code != 1 || !strings.Contains(out, "config invalid:") {
			t.Fatalf("code=%d output=%q", code, out)
		}
	})
}
