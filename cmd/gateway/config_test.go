package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func writeTestConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunConfigValidateAndDryRun(t *testing.T) {
	path := writeTestConfig(t, config.Default())
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"validate", "dry-run"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runConfig([]string{command, "--config", path}, &stdout, &stderr)
			if code != configExitOK {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), map[string]string{"validate": "config valid", "dry-run": "dry-run passed"}[command]) {
				t.Fatalf("unexpected output: %s", stdout.String())
			}
		})
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("read-only config commands modified the source file")
	}
}

func TestRunConfigExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := runConfig(nil, &stdout, &stderr); got != configExitUsage {
		t.Fatalf("missing command exit=%d", got)
	}
	stdout.Reset()
	stderr.Reset()
	if got := runConfig([]string{"typo", "--config", "ignored"}, &stdout, &stderr); got != configExitUsage {
		t.Fatalf("unknown command exit=%d", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if got := runConfig([]string{"validate", "--config", bad}, &stdout, &stderr); got != configExitFailure {
		t.Fatalf("invalid config exit=%d", got)
	}
}

func TestConfigDiffNeverPrintsSecretValues(t *testing.T) {
	left := config.Default()
	right := config.Default()
	left.Providers = []config.ProviderConfig{{ID: "p", APIKey: "do-not-print-left", ProxyURL: "https://user:secret@example.invalid", Headers: map[string]string{"X-Custom": "private-header"}}}
	right.Providers = []config.ProviderConfig{{ID: "p", APIKey: "do-not-print-right", ProxyURL: "https://user:other@example.invalid", Headers: map[string]string{"X-Custom": "changed-header"}}}
	changes, err := configDiff(left, right)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(changes, "\n")
	for _, secret := range []string{"do-not-print-left", "do-not-print-right", "private-header", "changed-header", "user:secret", "user:other"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("secret leaked in diff output (%q): %s", secret, joined)
		}
	}
	if !strings.Contains(joined, "api_key: changed (value redacted)") || !strings.Contains(joined, "headers.X-Custom: changed (value redacted)") {
		t.Fatalf("expected redacted change markers, got: %s", joined)
	}
}
