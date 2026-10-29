package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// wave3Conformance runs the conformance dispatcher with captured output.
func wave3Conformance(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runConformance(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestWave3ConformanceCanary(t *testing.T) {
	t.Run("bad flag", func(t *testing.T) {
		code, _, stderr := wave3Conformance(t, []string{"canary", "--bogus"})
		if code != 2 {
			t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
		}
	})
	t.Run("unexpected positional argument", func(t *testing.T) {
		code, _, stderr := wave3Conformance(t, []string{"canary", "--acknowledge-cost", "--preset", "custom", "--model", "m", "extra"})
		if code != 2 || !strings.Contains(stderr, "unexpected argument") {
			t.Fatalf("exit=%d, want 2 'unexpected argument' (stderr=%s)", code, stderr)
		}
	})
	t.Run("neither preset nor provider", func(t *testing.T) {
		code, _, stderr := wave3Conformance(t, []string{"canary", "--acknowledge-cost"})
		if code != 2 || !strings.Contains(stderr, "exactly one") {
			t.Fatalf("exit=%d, want 2 'exactly one' (stderr=%s)", code, stderr)
		}
	})
	t.Run("both preset and provider", func(t *testing.T) {
		code, _, stderr := wave3Conformance(t, []string{"canary", "--acknowledge-cost", "--preset", "custom", "--provider", "demo", "--model", "m"})
		if code != 2 || !strings.Contains(stderr, "exactly one") {
			t.Fatalf("exit=%d, want 2 'exactly one' (stderr=%s)", code, stderr)
		}
	})
	t.Run("unknown provider", func(t *testing.T) {
		var hits atomic.Int64
		srv := openAICanaryServer(t, &hits)
		defer srv.Close()
		cfg := minimalCanaryConfig(srv.URL)
		cfgPath := writeTestConfig(t, cfg)
		code, _, stderr := wave3Conformance(t, []string{"canary", "--acknowledge-cost", "--provider", "ghost", "--config", cfgPath})
		if code != 2 || !strings.Contains(stderr, "not found") {
			t.Fatalf("exit=%d, want 2 'not found' (stderr=%s)", code, stderr)
		}
		if hits.Load() != 0 {
			t.Fatalf("a resolution failure made %d upstream calls", hits.Load())
		}
	})
	t.Run("unknown scenario", func(t *testing.T) {
		var hits atomic.Int64
		srv := openAICanaryServer(t, &hits)
		defer srv.Close()
		cfg := minimalCanaryConfig(srv.URL)
		cfgPath := writeTestConfig(t, cfg)
		code, _, stderr := wave3Conformance(t, []string{
			"canary", "--acknowledge-cost", "--provider", "demo", "--config", cfgPath,
			"--scenarios", "bogus-scenario", "--max-calls", "1",
		})
		if code != 2 {
			t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
		}
		if hits.Load() != 0 {
			t.Fatalf("a scenario failure made %d upstream calls", hits.Load())
		}
	})
	t.Run("report write failure", func(t *testing.T) {
		var hits atomic.Int64
		srv := openAICanaryServer(t, &hits)
		defer srv.Close()
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := wave3Conformance(t, []string{
			"canary", "--acknowledge-cost", "--preset", "custom", "--model", "m",
			"--base-url", srv.URL, "--auth-mode", "none", "--scenarios", "text",
			"--max-calls", "1", "--out", filepath.Join(blocker, "canary.json"),
		})
		if code != 2 || !strings.Contains(stderr, "cannot write canary report") {
			t.Fatalf("exit=%d, want 2 'cannot write canary report' (stderr=%s)", code, stderr)
		}
		if hits.Load() != 1 {
			t.Fatalf("upstream calls=%d, want 1 (the run itself must succeed before the write fails)", hits.Load())
		}
	})
	t.Run("stdout report without out", func(t *testing.T) {
		var hits atomic.Int64
		srv := openAICanaryServer(t, &hits)
		defer srv.Close()
		code, stdout, stderr := wave3Conformance(t, []string{
			"canary", "--acknowledge-cost", "--preset", "custom", "--model", "m",
			"--base-url", srv.URL, "--auth-mode", "none", "--scenarios", "text",
			"--max-calls", "1",
		})
		if code != 0 {
			t.Fatalf("exit=%d, want 0 (stdout=%s stderr=%s)", code, stdout, stderr)
		}
		if !strings.Contains(stdout, `"outcome": "PASS"`) {
			t.Fatalf("stdout=%q, want a PASS report on stdout", stdout)
		}
		if hits.Load() != 1 {
			t.Fatalf("upstream calls=%d, want 1", hits.Load())
		}
	})
}

func TestWave3ResolveCanaryTarget(t *testing.T) {
	var hits atomic.Int64
	srv := openAICanaryServer(t, &hits)
	defer srv.Close()

	t.Run("config load error", func(t *testing.T) {
		_, _, err := resolveCanaryTarget(canaryFlags{provider: "demo", configPath: filepath.Join(t.TempDir(), "missing.json")})
		if err == nil || !strings.Contains(err.Error(), "cannot load config") {
			t.Fatalf("err=%v, want 'cannot load config'", err)
		}
	})
	t.Run("provider not found", func(t *testing.T) {
		cfgPath := writeTestConfig(t, minimalCanaryConfig(srv.URL))
		_, _, err := resolveCanaryTarget(canaryFlags{provider: "ghost", configPath: cfgPath})
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err=%v, want 'not found'", err)
		}
	})
	t.Run("zero enabled models", func(t *testing.T) {
		cfg := minimalCanaryConfig(srv.URL)
		cfg.Providers[0].Models = nil
		cfgPath := writeTestConfig(t, cfg)
		_, _, err := resolveCanaryTarget(canaryFlags{provider: "demo", configPath: cfgPath})
		if err == nil || !strings.Contains(err.Error(), "has 0 enabled models") {
			t.Fatalf("err=%v, want 'has 0 enabled models'", err)
		}
	})
	t.Run("two enabled models", func(t *testing.T) {
		cfg := minimalCanaryConfig(srv.URL)
		cfg.Providers[0].Models = append(cfg.Providers[0].Models,
			config.ModelConfig{ID: "m2", Model: "m2", Enabled: true, Weight: 1})
		cfgPath := writeTestConfig(t, cfg)
		_, _, err := resolveCanaryTarget(canaryFlags{provider: "demo", configPath: cfgPath})
		if err == nil || !strings.Contains(err.Error(), "has 2 enabled models") {
			t.Fatalf("err=%v, want 'has 2 enabled models'", err)
		}
	})
	t.Run("explicit model found", func(t *testing.T) {
		cfgPath := writeTestConfig(t, minimalCanaryConfig(srv.URL))
		_, model, err := resolveCanaryTarget(canaryFlags{provider: "demo", model: "m", configPath: cfgPath})
		if err != nil {
			t.Fatal(err)
		}
		if model.ID != "m" || !model.Enabled {
			t.Fatalf("model=%+v, want enabled m", model)
		}
	})
	t.Run("model not found", func(t *testing.T) {
		cfgPath := writeTestConfig(t, minimalCanaryConfig(srv.URL))
		_, _, err := resolveCanaryTarget(canaryFlags{provider: "demo", model: "nope", configPath: cfgPath})
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err=%v, want 'not found'", err)
		}
	})
	t.Run("disabled model", func(t *testing.T) {
		cfg := minimalCanaryConfig(srv.URL)
		cfg.Providers[0].Models = append(cfg.Providers[0].Models,
			config.ModelConfig{ID: "off", Model: "off", Enabled: false, Weight: 1})
		cfgPath := writeTestConfig(t, cfg)
		_, _, err := resolveCanaryTarget(canaryFlags{provider: "demo", model: "off", configPath: cfgPath})
		if err == nil || !strings.Contains(err.Error(), "is disabled") {
			t.Fatalf("err=%v, want 'is disabled'", err)
		}
	})
	t.Run("all overrides applied", func(t *testing.T) {
		cfgPath := writeTestConfig(t, minimalCanaryConfig(srv.URL))
		provider, model, err := resolveCanaryTarget(canaryFlags{
			provider: "demo", model: "m", configPath: cfgPath,
			modelName: "upstream-name", baseURL: "http://127.0.0.1:9/v1",
			authMode: "bearer", apiKeyEnv: "WAVE3_CANARY_KEY",
			secretKeyEnv: "WAVE3_CANARY_SECRET", sessionEnv: "WAVE3_CANARY_SESSION",
			awsRegion: "eu-west-1", inputCost: 1.5, outputCost: 2.5,
		})
		if err != nil {
			t.Fatal(err)
		}
		if model.Model != "upstream-name" || model.InputCostPerMTok != 1.5 || model.OutputCostPerMTok != 2.5 {
			t.Fatalf("model=%+v, want overrides applied", model)
		}
		if provider.BaseURL != "http://127.0.0.1:9/v1" || provider.AuthMode != "bearer" ||
			provider.APIKeyEnv != "WAVE3_CANARY_KEY" || provider.SecretKeyEnv != "WAVE3_CANARY_SECRET" ||
			provider.SessionTokenEnv != "WAVE3_CANARY_SESSION" || provider.AWSRegion != "eu-west-1" {
			t.Fatalf("provider=%+v, want overrides applied", provider)
		}
	})
}

func TestWave3ResolvePresetTarget(t *testing.T) {
	t.Run("unknown preset", func(t *testing.T) {
		_, _, err := resolvePresetTarget(canaryFlags{preset: "ghost", model: "m"})
		if err == nil || !strings.Contains(err.Error(), "unknown preset") {
			t.Fatalf("err=%v, want 'unknown preset'", err)
		}
	})
	t.Run("model required", func(t *testing.T) {
		_, _, err := resolvePresetTarget(canaryFlags{preset: "custom"})
		if err == nil || !strings.Contains(err.Error(), "--model is required") {
			t.Fatalf("err=%v, want '--model is required'", err)
		}
	})
	t.Run("custom without base url", func(t *testing.T) {
		_, _, err := resolvePresetTarget(canaryFlags{preset: "custom", model: "m"})
		if err == nil || !strings.Contains(err.Error(), "has no default base URL") {
			t.Fatalf("err=%v, want 'has no default base URL'", err)
		}
	})
	t.Run("openai default base url", func(t *testing.T) {
		provider, model, err := resolvePresetTarget(canaryFlags{preset: "openai", model: "gpt-x"})
		if err != nil {
			t.Fatal(err)
		}
		if provider.BaseURL != "https://api.openai.com/v1" {
			t.Fatalf("baseURL=%q, want https://api.openai.com/v1", provider.BaseURL)
		}
		if provider.AuthMode != "bearer" || provider.APIKeyEnv != "NEXAROUTE_CANARY_API_KEY" {
			t.Fatalf("provider=%+v, want bearer + NEXAROUTE_CANARY_API_KEY", provider)
		}
		if provider.ID != "canary-openai" || model.Model != "gpt-x" {
			t.Fatalf("provider=%+v model=%+v, want canary-openai/gpt-x", provider, model)
		}
	})
	t.Run("bedrock credential env defaults", func(t *testing.T) {
		provider, _, err := resolvePresetTarget(canaryFlags{preset: "amazon-bedrock", model: "anthropic.claude-x"})
		if err != nil {
			t.Fatal(err)
		}
		if provider.Type != "bedrock" || provider.SecretKeyEnv != "NEXAROUTE_CANARY_SECRET_KEY" ||
			provider.SessionTokenEnv != "NEXAROUTE_CANARY_SESSION_TOKEN" || provider.AWSRegion != "us-east-1" {
			t.Fatalf("provider=%+v, want bedrock credential defaults", provider)
		}
		if provider.BaseURL == "" {
			t.Fatal("bedrock preset must carry a default base URL")
		}
		if provider.APIKeyEnv != "" {
			t.Fatalf("apiKeyEnv=%q, want empty for the empty-authmode bedrock preset", provider.APIKeyEnv)
		}
	})
}
