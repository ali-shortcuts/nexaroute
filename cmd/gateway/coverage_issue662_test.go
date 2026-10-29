package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
)

func issue662ProviderConfig(t *testing.T, models ...config.ModelConfig) string {
	t.Helper()
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "demo", Name: "Demo", Type: "openai_compatible",
		BaseURL: "https://demo.example.test", AuthMode: "bearer",
		APIKeyEnv: "ISSUE662_DEMO_KEY", Enabled: true, MaxConcurrency: 1,
		Models: models,
	}}
	return writeTestConfig(t, cfg)
}

func issue662Model(id string, enabled bool) config.ModelConfig {
	return config.ModelConfig{ID: id, Model: id, Enabled: enabled, Weight: 1}
}

func TestIssue662ResolveCanaryTargetProviderModeErrors(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing.json")
	if _, _, err := resolveCanaryTarget(canaryFlags{configPath: missingPath, provider: "demo"}); err == nil || !strings.Contains(err.Error(), "cannot load config") {
		t.Fatalf("missing config error = %v", err)
	}

	path := issue662ProviderConfig(t, issue662Model("m", true))
	if _, _, err := resolveCanaryTarget(canaryFlags{configPath: path, provider: "absent"}); err == nil || !strings.Contains(err.Error(), `provider "absent" not found`) {
		t.Fatalf("unknown provider error = %v", err)
	}
	if _, _, err := resolveCanaryTarget(canaryFlags{configPath: path, provider: "demo", model: "absent"}); err == nil || !strings.Contains(err.Error(), `model "absent" not found`) {
		t.Fatalf("unknown model error = %v", err)
	}

	disabledPath := issue662ProviderConfig(t, issue662Model("m", false))
	if _, _, err := resolveCanaryTarget(canaryFlags{configPath: disabledPath, provider: "demo"}); err == nil || !strings.Contains(err.Error(), "has 0 enabled models") {
		t.Fatalf("zero enabled models error = %v", err)
	}
	if _, _, err := resolveCanaryTarget(canaryFlags{configPath: disabledPath, provider: "demo", model: "m"}); err == nil || !strings.Contains(err.Error(), `model "m" is disabled`) {
		t.Fatalf("disabled model error = %v", err)
	}

	ambiguousPath := issue662ProviderConfig(t, issue662Model("a", true), issue662Model("b", true))
	if _, _, err := resolveCanaryTarget(canaryFlags{configPath: ambiguousPath, provider: "demo"}); err == nil || !strings.Contains(err.Error(), "has 2 enabled models") {
		t.Fatalf("ambiguous enabled models error = %v", err)
	}
}

func TestIssue662ResolveCanaryTargetProviderModeSelectionAndOverrides(t *testing.T) {
	path := issue662ProviderConfig(t, issue662Model("only", true), issue662Model("disabled", false))

	provider, model, err := resolveCanaryTarget(canaryFlags{configPath: path, provider: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != "only" || provider.ID != "demo" {
		t.Fatalf("implicit model selection = provider %q model %q", provider.ID, model.ID)
	}

	provider, model, err = resolveCanaryTarget(canaryFlags{
		configPath: path, provider: "demo", model: "only",
		modelName: "upstream-name", baseURL: "https://override.example.test/",
		authMode: "none", apiKeyEnv: "ISSUE662_OVERRIDE_KEY",
		secretKeyEnv: "ISSUE662_OVERRIDE_SECRET", sessionEnv: "ISSUE662_OVERRIDE_SESSION",
		awsRegion: "eu-west-1", inputCost: 1.5, outputCost: 2.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.Model != "upstream-name" || model.InputCostPerMTok != 1.5 || model.OutputCostPerMTok != 2.5 {
		t.Fatalf("model overrides not applied: %+v", model)
	}
	if provider.BaseURL != "https://override.example.test" || provider.AuthMode != "none" ||
		provider.APIKeyEnv != "ISSUE662_OVERRIDE_KEY" || provider.SecretKeyEnv != "ISSUE662_OVERRIDE_SECRET" ||
		provider.SessionTokenEnv != "ISSUE662_OVERRIDE_SESSION" || provider.AWSRegion != "eu-west-1" {
		t.Fatalf("provider overrides not applied: %+v", provider)
	}
}

func TestIssue662WriteCanaryReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "report.json")
	payload := []byte(`{"outcome":"pass"}`)
	if err := writeCanaryReport(path, payload); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("report content = %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode = %o, want 600", info.Mode().Perm())
	}

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCanaryReport(filepath.Join(blocked, "report.json"), payload); err == nil {
		t.Fatal("report under a regular file unexpectedly succeeded")
	}
	if err := writeCanaryReport(t.TempDir(), payload); err == nil {
		t.Fatal("report targeting an existing directory unexpectedly succeeded")
	}
}

func TestIssue662ExternalProviderCredentialReady(t *testing.T) {
	t.Setenv("ISSUE662_BEDROCK_KEY", "")
	t.Setenv("ISSUE662_BEDROCK_SECRET", "")

	cases := []struct {
		name     string
		provider config.ProviderConfig
		env      map[string]string
		want     bool
		wantEnv  string
	}{
		{"auth none", config.ProviderConfig{AuthMode: "none"}, nil, true, ""},
		{"missing api key name falls back", config.ProviderConfig{AuthMode: "bearer"}, nil, false, "api_key_env"},
		{"missing api key value", config.ProviderConfig{AuthMode: "bearer", APIKeyEnv: "ISSUE662_BEDROCK_KEY"}, nil, false, "ISSUE662_BEDROCK_KEY"},
		{"present api key", config.ProviderConfig{AuthMode: "bearer", APIKeyEnv: "ISSUE662_BEDROCK_KEY"}, map[string]string{"ISSUE662_BEDROCK_KEY": "key-value"}, true, ""},
		{"bedrock missing access key", config.ProviderConfig{Type: "bedrock", AuthMode: "sigv4", APIKeyEnv: "ISSUE662_BEDROCK_KEY", SecretKeyEnv: "ISSUE662_BEDROCK_SECRET"}, nil, false, "ISSUE662_BEDROCK_KEY"},
		{"bedrock missing secret name", config.ProviderConfig{Type: "bedrock", AuthMode: "sigv4", APIKeyEnv: "ISSUE662_BEDROCK_KEY"}, map[string]string{"ISSUE662_BEDROCK_KEY": "access"}, false, "secret_key_env"},
		{"bedrock missing secret value", config.ProviderConfig{Type: "bedrock", AuthMode: "sigv4", APIKeyEnv: "ISSUE662_BEDROCK_KEY", SecretKeyEnv: "ISSUE662_BEDROCK_SECRET"}, map[string]string{"ISSUE662_BEDROCK_KEY": "access"}, false, "ISSUE662_BEDROCK_SECRET"},
		{"bedrock complete", config.ProviderConfig{Type: "bedrock", AuthMode: "sigv4", APIKeyEnv: "ISSUE662_BEDROCK_KEY", SecretKeyEnv: "ISSUE662_BEDROCK_SECRET"}, map[string]string{"ISSUE662_BEDROCK_KEY": "access", "ISSUE662_BEDROCK_SECRET": "secret"}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			ready, envName := externalProviderCredentialReady(tc.provider)
			if ready != tc.want || envName != tc.wantEnv {
				t.Fatalf("ready=%v env=%q, want %v %q", ready, envName, tc.want, tc.wantEnv)
			}
		})
	}
}

func TestIssue662RunExternalProviderCheckBlockedBeforeNetwork(t *testing.T) {
	ctx := context.Background()
	redactor := livecanary.NewRedactor(nil)

	t.Setenv("ISSUE662_ACCEPT_KEY", "")
	missingCredential := config.ProviderConfig{
		ID: "blocked", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "bearer", APIKeyEnv: "ISSUE662_ACCEPT_KEY", Enabled: true,
		Models: []config.ModelConfig{issue662Model("m", true)},
	}
	check := runExternalProviderCheck(ctx, missingCredential, acceptOptions{}, redactor)
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "ISSUE662_ACCEPT_KEY") || check.Provider != nil {
		t.Fatalf("missing credential check = %+v", check)
	}

	noModel := config.ProviderConfig{
		ID: "no-model", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{issue662Model("m", false)},
	}
	check = runExternalProviderCheck(ctx, noModel, acceptOptions{}, redactor)
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "no enabled model") || check.Provider != nil {
		t.Fatalf("no enabled model check = %+v", check)
	}
}

func TestIssue662ExternalAcceptExitCode(t *testing.T) {
	cases := map[string]int{
		externalStatusPass:    0,
		externalStatusFail:    1,
		externalStatusBlocked: 3,
		externalStatusNotRun:  4,
		"unexpected":          2,
	}
	for outcome, want := range cases {
		if got := externalAcceptExitCode(outcome); got != want {
			t.Fatalf("externalAcceptExitCode(%q) = %d, want %d", outcome, got, want)
		}
	}
}

func TestIssue662WriteExternalAcceptReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "external.json")
	report := externalAcceptReport{Schema: "issue662", Outcome: externalStatusPass}
	if err := writeExternalAcceptReport(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got externalAcceptReport
	if err := json.Unmarshal(data, &got); err != nil || got.Schema != "issue662" || got.Outcome != externalStatusPass {
		t.Fatalf("report = %+v, err=%v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode = %v, err=%v", info.Mode().Perm(), err)
	}

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeExternalAcceptReport(filepath.Join(blocked, "external.json"), report); err == nil {
		t.Fatal("report under a regular file unexpectedly succeeded")
	}
	if err := writeExternalAcceptReport(t.TempDir(), report); err == nil {
		t.Fatal("report targeting an existing directory unexpectedly succeeded")
	}
}

func TestIssue662WriteJournalManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := &restoreJournalManifest{Format: restoreJournalFormat, ID: "issue662", State: "prepared"}
	if err := writeJournalManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	read, err := readJournalManifest(dir)
	if err != nil || read.ID != manifest.ID || read.State != manifest.State {
		t.Fatalf("manifest = %+v, err=%v", read, err)
	}
	info, err := os.Stat(filepath.Join(dir, "manifest.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %v, err=%v", info.Mode().Perm(), err)
	}

	if err := writeJournalManifest(filepath.Join(t.TempDir(), "missing"), manifest); err == nil {
		t.Fatal("manifest write to a missing directory unexpectedly succeeded")
	}
	blocked := filepath.Join(t.TempDir(), "journal-file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJournalManifest(blocked, manifest); err == nil {
		t.Fatal("manifest write into a regular file unexpectedly succeeded")
	}
}

func TestIssue662CheckJournalOwnership(t *testing.T) {
	current := &issue662FileInfo{sys: &syscall.Stat_t{Uid: uint32(os.Geteuid())}}
	if err := checkJournalOwnership(current); err != nil {
		t.Fatalf("current owner rejected: %v", err)
	}
	foreign := &issue662FileInfo{name: "manifest.json", sys: &syscall.Stat_t{Uid: uint32(os.Geteuid()) + 1}}
	if err := checkJournalOwnership(foreign); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("foreign owner error = %v", err)
	}
	if err := checkJournalOwnership(&issue662FileInfo{sys: nil}); err != nil {
		t.Fatalf("unsupported Stat_t rejected: %v", err)
	}
}

type issue662FileInfo struct {
	name string
	sys  any
}

func (f *issue662FileInfo) Name() string       { return f.name }
func (f *issue662FileInfo) Size() int64        { return 0 }
func (f *issue662FileInfo) Mode() os.FileMode  { return 0o600 }
func (f *issue662FileInfo) ModTime() time.Time { return time.Time{} }
func (f *issue662FileInfo) IsDir() bool        { return false }
func (f *issue662FileInfo) Sys() any           { return f.sys }
