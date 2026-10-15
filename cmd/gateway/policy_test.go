package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M6-2 CLI: promote -> rollback round trip over a real config
// file, with the audit log tracking digests; an out-of-band edit
// makes rollback refuse without --force.

func policyCLIConfig(t *testing.T) string {
	t.Helper()
	cfg := config.Default()
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "baseline-first"
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		{ID: "baseline-first", Weights: config.DecisionPolicyWeights{RouterBaseline: 1}},
		{ID: "latency-first", Weights: config.DecisionPolicyWeights{Latency: 1}},
	}
	path := filepath.Join(t.TempDir(), "nexaroute.json")
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadDecisionPolicy(t *testing.T, path string) string {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Decision.Policy
}

func TestPolicyPromoteRollbackRoundTrip(t *testing.T) {
	path := policyCLIConfig(t)
	var out, errBuf bytes.Buffer
	if code := runPolicy([]string{"promote", "--config", path, "--policy", "latency-first"}, &out, &errBuf); code != 0 {
		t.Fatalf("promote exit %d: %s", code, errBuf.String())
	}
	if got := loadDecisionPolicy(t, path); got != "latency-first" {
		t.Fatalf("after promote: decision.policy = %q", got)
	}
	records, err := readPolicyAudit(defaultAuditLogPath(path))
	if err != nil || len(records) != 1 || records[0].Action != "promote" || records[0].FromPolicy != "baseline-first" || records[0].ToPolicy != "latency-first" {
		t.Fatalf("audit records: %v %+v", err, records)
	}
	out.Reset()
	errBuf.Reset()
	if code := runPolicy([]string{"rollback", "--config", path}, &out, &errBuf); code != 0 {
		t.Fatalf("rollback exit %d: %s", code, errBuf.String())
	}
	if got := loadDecisionPolicy(t, path); got != "baseline-first" {
		t.Fatalf("after rollback: decision.policy = %q", got)
	}
	records, err = readPolicyAudit(defaultAuditLogPath(path))
	if err != nil || len(records) != 2 || records[1].Action != "rollback" {
		t.Fatalf("audit after rollback: %v %+v", err, records)
	}
	// Promoting an undefined policy fails and changes nothing.
	if code := runPolicy([]string{"promote", "--config", path, "--policy", "ghost"}, &out, &errBuf); code != 1 {
		t.Fatalf("ghost promote exit = %d, want 1", code)
	}
	if got := loadDecisionPolicy(t, path); got != "baseline-first" {
		t.Fatalf("ghost promote changed the config: %q", got)
	}
}

func TestPolicyRollbackRefusesOutOfBandChange(t *testing.T) {
	path := policyCLIConfig(t)
	var out, errBuf bytes.Buffer
	if code := runPolicy([]string{"promote", "--config", path, "--policy", "latency-first"}, &out, &errBuf); code != 0 {
		t.Fatalf("promote exit %d: %s", code, errBuf.String())
	}
	// Out-of-band edit: append a byte-level change via a full
	// re-save with a different (still valid) field.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "127.0.0.1:9999"
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	if code := runPolicy([]string{"rollback", "--config", path}, &out, &errBuf); code != 1 {
		t.Fatalf("rollback after out-of-band edit exit = %d, want 1 (%s)", code, out.String())
	}
	if !strings.Contains(errBuf.String(), "out-of-band") {
		t.Fatalf("expected out-of-band refusal, got: %s", errBuf.String())
	}
	if code := runPolicy([]string{"rollback", "--config", path, "--force"}, &out, &errBuf); code != 0 {
		t.Fatalf("forced rollback exit %d: %s", code, errBuf.String())
	}
	if got := loadDecisionPolicy(t, path); got != "baseline-first" {
		t.Fatalf("after forced rollback: %q", got)
	}
	// The audit log file is owner-only.
	info, err := os.Stat(defaultAuditLogPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit log perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestPolicySimulateCLI(t *testing.T) {
	path := policyCLIConfig(t)
	fixtures := filepath.Join(t.TempDir(), "fixtures.json")
	content := `{"cases":[{"name":"c1","request":{"task_profile":{"task_type":"coding"},"candidates":[
		{"id":"a","provider_id":"p","router_score":100,"ewma_latency_ms":50},
		{"id":"b","provider_id":"p","router_score":90,"ewma_latency_ms":10}]}}]}`
	if err := os.WriteFile(fixtures, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runPolicy([]string{"simulate", "--config", path, "--policy", "latency-first", "--fixtures", fixtures, "--format", "json"}, &out, &errBuf); code != 0 {
		t.Fatalf("simulate exit %d: %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"winner": "b"`) || !strings.Contains(out.String(), `"digest"`) {
		t.Fatalf("simulate output: %s", out.String())
	}
	if code := runPolicy([]string{"bogus"}, &out, &errBuf); code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", code)
	}
}
