package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestRunConfigCommandsAndStableExitContracts(t *testing.T) {
	if got := runConfig(nil, &bytes.Buffer{}, &bytes.Buffer{}); got != configExitUsage {
		t.Fatalf("empty config args exit=%d", got)
	}
	if got := runConfig([]string{"unknown"}, &bytes.Buffer{}, &bytes.Buffer{}); got != configExitUsage {
		t.Fatalf("unknown config command exit=%d", got)
	}
	if got := runConfig([]string{"validate", "unexpected"}, &bytes.Buffer{}, &bytes.Buffer{}); got != configExitUsage {
		t.Fatalf("positional config arg exit=%d", got)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if got := runConfig([]string{"validate", "--config", missing}, &bytes.Buffer{}, &bytes.Buffer{}); got != configExitFailure {
		t.Fatalf("missing config exit=%d", got)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Providers = nil
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if got := runConfig([]string{"validate", "--config", path}, &out, &bytes.Buffer{}); got != configExitOK || !strings.Contains(out.String(), "config valid:") {
		t.Fatalf("valid config exit=%d output=%q", got, out.String())
	}
	out.Reset()
	if got := runConfig([]string{"dry-run", "--config", path}, &out, &bytes.Buffer{}); got != configExitOK || !strings.Contains(out.String(), "providers_prepared=0") {
		t.Fatalf("dry-run exit=%d output=%q", got, out.String())
	}
	out.Reset()
	if got := runConfig([]string{"diff", "--config", path, "--against", path}, &out, &bytes.Buffer{}); got != configExitOK || !strings.Contains(out.String(), "no differences") {
		t.Fatalf("identical diff exit=%d output=%q", got, out.String())
	}
}

func TestRunSecretsRejectsUnsafeOrUnknownOperations(t *testing.T) {
	if err := runSecrets(nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("empty secrets args error=%v", err)
	}
	if err := runSecrets([]string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown secrets command") {
		t.Fatalf("unknown secrets command error=%v", err)
	}
	if err := runSecrets([]string{"decrypt", "--config", filepath.Join(t.TempDir(), "config.json"), "--to-stdout"}); err == nil || !strings.Contains(err.Error(), "requires both") {
		t.Fatalf("unsafe decrypt error=%v", err)
	}
	if err := runSecrets([]string{"status", "--config", filepath.Join(t.TempDir(), "missing.json")}); err == nil {
		t.Fatal("missing secrets config unexpectedly succeeded")
	}
}
