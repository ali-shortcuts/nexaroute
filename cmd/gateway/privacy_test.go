package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const privacyCanary = "sk-CANARY-DO-NOT-PRINT-6789"

func writeAuditConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cleanAuditConfig = `{
  "providers": [{
    "id": "p1", "name": "P1", "type": "openai_compatible",
    "base_url": "https://api.example.com/v1", "enabled": true,
    "api_key_env": "P1_KEY",
    "data_handling": {"trains_on_data": "no", "retention": "none"},
    "models": [{"id": "m1", "model": "model-1", "enabled": true}]
  }],
  "egress": {"deny_hosts": ["blocked.example.com"]}
}`

func TestPrivacyAuditCleanConfigExitZero(t *testing.T) {
	path := writeAuditConfig(t, cleanAuditConfig)
	var out, errBuf bytes.Buffer
	code := runPrivacy([]string{"audit", "--config", path}, &out, &errBuf)
	if code != privacyExitOK {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "0 error(s)") {
		t.Fatalf("expected summary line, got: %s", out.String())
	}
}

func TestPrivacyAuditFindingsExitOneAndRedaction(t *testing.T) {
	cfg := strings.Replace(cleanAuditConfig, `"api_key_env": "P1_KEY"`,
		`"api_key": "`+privacyCanary+`"`, 1)
	path := writeAuditConfig(t, cfg)
	var out, errBuf bytes.Buffer
	code := runPrivacy([]string{"audit", "--config", path}, &out, &errBuf)
	if code != privacyExitFindings {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(out.String(), "PLAINTEXT_SECRET") {
		t.Fatalf("expected PLAINTEXT_SECRET finding, got: %s", out.String())
	}
	if strings.Contains(out.String()+errBuf.String(), privacyCanary) {
		t.Fatal("CLI output must never contain the secret value")
	}
}

func TestPrivacyAuditJSONFormat(t *testing.T) {
	path := writeAuditConfig(t, cleanAuditConfig)
	var out, errBuf bytes.Buffer
	code := runPrivacy([]string{"audit", "--config", path, "--format", "json"}, &out, &errBuf)
	if code != privacyExitOK {
		t.Fatalf("expected exit 0, got %d", code)
	}
	var rep struct {
		Findings []map[string]any `json:"findings"`
		Errors   int              `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v", err)
	}
	if rep.Errors != 0 {
		t.Fatalf("clean config must report 0 errors, got %d", rep.Errors)
	}
}

func TestPrivacyAuditMissingConfigExitTwo(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runPrivacy([]string{"audit", "--config", filepath.Join(t.TempDir(), "absent.json")}, &out, &errBuf)
	if code != privacyExitUsage {
		t.Fatalf("expected exit 2 for missing config, got %d", code)
	}
}

func TestPrivacyAuditMalformedConfigExitTwo(t *testing.T) {
	path := writeAuditConfig(t, "{ this is not json")
	var out, errBuf bytes.Buffer
	code := runPrivacy([]string{"audit", "--config", path}, &out, &errBuf)
	if code != privacyExitUsage {
		t.Fatalf("expected exit 2 for malformed config, got %d", code)
	}
}

func TestPrivacyAuditUsageErrors(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runPrivacy(nil, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("no args: expected exit 2, got %d", code)
	}
	if code := runPrivacy([]string{"bogus"}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("unknown subcommand: expected exit 2, got %d", code)
	}
	if code := runPrivacy([]string{"audit", "--format", "yaml"}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("bad format: expected exit 2, got %d", code)
	}
}

// TestPrivacyAuditIsReadOnly proves the audit never rewrites the config
// file, even for a legacy plaintext config that config.Load would migrate.
func TestPrivacyAuditIsReadOnly(t *testing.T) {
	cfg := strings.Replace(cleanAuditConfig, `"api_key_env": "P1_KEY"`,
		`"api_key": "`+privacyCanary+`"`, 1)
	path := writeAuditConfig(t, cfg)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	_ = runPrivacy([]string{"audit", "--config", path}, &out, &errBuf)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("privacy audit modified the config file; it must be read-only")
	}
	// No migration backup or sidecar files may appear either.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("audit created sidecar files: %v", names)
	}
}
