package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// wave3ProviderAccept runs runProviderAccept with captured output.
func wave3ProviderAccept(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runProviderAccept(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// wave3ReadProviderReport parses a provider-acceptance report file.
func wave3ReadProviderReport(t *testing.T, path string) providerAcceptReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report providerAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

// wave3PerModelUpstream answers discovery with 200 and fails probes
// only for the model named "bad-model".
func wave3PerModelUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"good-model"},{"id":"bad-model"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "bad-model") {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"bad model probe denied"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"chatcmpl-wave3","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWave3ProviderAcceptFlagValidation(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	disabled := wave3OpenAIProvider("off", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	})
	disabled.Enabled = false
	disabledCfg := acceptanceConfig(t, disabled)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad flag", []string{"--bogus"}, "flag provided but not defined"},
		{"discover and test both false", []string{"--discover=false", "--test=false", "--confirm-live", "--config", cfgPath}, "cannot both be false"},
		{"timeout zero", []string{"--timeout", "0"}, "--timeout must be between"},
		{"timeout over max", []string{"--timeout", "61s"}, "--timeout must be between"},
		{"max models zero", []string{"--max-models", "0"}, "--max-models must be between"},
		{"max models over max", []string{"--max-models", "101"}, "--max-models must be between"},
		{"max calls zero", []string{"--max-calls", "0"}, "--max-calls must be between"},
		{"max calls over max", []string{"--max-calls", "101"}, "--max-calls must be between"},
		{"test without provider", []string{"--confirm-live", "--config", cfgPath}, "--provider is required when --test=true"},
		{"unknown provider", []string{"--provider", "ghost", "--confirm-live", "--config", cfgPath}, "not found"},
		{"no matching providers", []string{"--discover", "--test=false", "--confirm-live", "--config", disabledCfg}, "no matching providers selected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := wave3ProviderAccept(t, tc.args)
			if code != 2 {
				t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr=%q, want substring %q", stderr, tc.want)
			}
		})
	}
}

func TestWave3ProviderAcceptReportWriteFailure(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := wave3ProviderAccept(t, []string{
		"--provider", "demo", "--config", cfgPath, "--confirm-live", "--out", filepath.Join(blocker, "report.json"),
	})
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "cannot write report") {
		t.Fatalf("stderr=%q, want 'cannot write report'", stderr)
	}
}

func TestWave3ProviderAcceptDefaultOutPath(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	workdir := t.TempDir()
	t.Chdir(workdir)
	code, _, stderr := wave3ProviderAccept(t, []string{
		"--provider", "demo", "--config", cfgPath, "--confirm-live",
	})
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	entries, err := os.ReadDir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "provider-acceptance-") && strings.HasSuffix(e.Name(), ".json") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no provider-acceptance-*.json in %v", entries)
	}
}

func TestWave3ProviderAcceptTestFalseStatuses(t *testing.T) {
	const secret = "wave3-provider-accept-secret"
	t.Setenv("WAVE3_PROVIDER_ACCEPT_KEY", secret)

	t.Run("discovery ok is configured", func(t *testing.T) {
		up, _, _ := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
		cfgPath := acceptanceConfig(t, config.ProviderConfig{
			ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
			APIKeyEnv: "WAVE3_PROVIDER_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}},
		})
		out := filepath.Join(t.TempDir(), "report.json")
		code, _, stderr := wave3ProviderAccept(t, []string{
			"--provider", "tenant", "--test=false", "--config", cfgPath, "--confirm-live", "--out", out,
		})
		if code != 0 {
			t.Fatalf("exit=%d, want 0 (stderr=%s)", code, stderr)
		}
		report := wave3ReadProviderReport(t, out)
		if len(report.Providers) != 1 || report.Providers[0].Status != "configured" {
			t.Fatalf("providers=%v, want one configured", report.Providers)
		}
		if report.Totals.Configured != 1 {
			t.Fatalf("totals=%+v, want Configured=1", report.Totals)
		}
	})
	t.Run("unsupported type is unsupported", func(t *testing.T) {
		srv := wave3NoAuthUpstream(t)
		cfgPath := acceptanceConfig(t, config.ProviderConfig{
			ID: "az", Type: "azure_openai", BaseURL: srv.URL + "/v1",
			APIKeyEnv: "WAVE3_PROVIDER_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}},
		})
		out := filepath.Join(t.TempDir(), "report.json")
		code, _, stderr := wave3ProviderAccept(t, []string{
			"--provider", "az", "--test=false", "--config", cfgPath, "--confirm-live", "--out", out,
		})
		if code != 0 {
			t.Fatalf("exit=%d, want 0 (stderr=%s)", code, stderr)
		}
		report := wave3ReadProviderReport(t, out)
		if len(report.Providers) != 1 || report.Providers[0].Status != "unsupported" {
			t.Fatalf("providers=%v, want one unsupported", report.Providers)
		}
		if report.Totals.Unsupported != 1 {
			t.Fatalf("totals=%+v, want Unsupported=1", report.Totals)
		}
	})
	t.Run("discovery failed is failed", func(t *testing.T) {
		up, _, _ := fakeOpenAIUpstream(t, secret, http.StatusForbidden, http.StatusOK)
		cfgPath := acceptanceConfig(t, config.ProviderConfig{
			ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
			APIKeyEnv: "WAVE3_PROVIDER_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}},
		})
		out := filepath.Join(t.TempDir(), "report.json")
		code, _, stderr := wave3ProviderAccept(t, []string{
			"--provider", "tenant", "--test=false", "--config", cfgPath, "--confirm-live", "--out", out,
		})
		if code != 1 {
			t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
		}
		report := wave3ReadProviderReport(t, out)
		if len(report.Providers) != 1 || report.Providers[0].Status != "failed" {
			t.Fatalf("providers=%v, want one failed", report.Providers)
		}
	})
}

func TestWave3ProviderAcceptDegradedMixedModels(t *testing.T) {
	srv := wave3PerModelUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "good-model", Model: "good-model", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
		{ID: "bad-model", Model: "bad-model", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ProviderAccept(t, []string{
		"--provider", "demo", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	report := wave3ReadProviderReport(t, out)
	if len(report.Providers) != 1 || report.Providers[0].Status != "degraded" {
		t.Fatalf("providers=%v, want one degraded", report.Providers)
	}
	byModel := map[string]providerModelResult{}
	for _, m := range report.Providers[0].Models {
		byModel[m.Model] = m
	}
	if byModel["good-model"].Result != "passed" {
		t.Fatalf("good-model=%+v, want passed", byModel["good-model"])
	}
	if byModel["bad-model"].Result != "failed" || byModel["bad-model"].Error == "" {
		t.Fatalf("bad-model=%+v, want failed with an error", byModel["bad-model"])
	}
}

func TestWave3ProviderAcceptFailedPlusDiscoveryWarning(t *testing.T) {
	const secret = "wave3-provider-accept-secret-2"
	t.Setenv("WAVE3_PROVIDER_ACCEPT_KEY_2", secret)
	up, _, _ := fakeOpenAIUpstream(t, secret, http.StatusForbidden, http.StatusInternalServerError)
	cfgPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "WAVE3_PROVIDER_ACCEPT_KEY_2", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}},
	})
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ProviderAccept(t, []string{
		"--provider", "tenant", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	report := wave3ReadProviderReport(t, out)
	if len(report.Providers) != 1 || report.Providers[0].Status != "failed" {
		t.Fatalf("providers=%v, want one failed", report.Providers)
	}
	found := false
	for _, w := range report.Providers[0].Warnings {
		if strings.Contains(w, "generation probes failed and discovery was not observed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings=%v, want the discovery warning", report.Providers[0].Warnings)
	}
}

func TestWave3ProviderAcceptInsufficientCredentialTotal(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: srv.URL + "/v1",
		APIKeyEnv: "WAVE3_MISSING_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}},
	})
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ProviderAccept(t, []string{
		"--provider", "tenant", "--discover", "--test=false", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	report := wave3ReadProviderReport(t, out)
	if report.Totals.InsufficientCredential != 1 {
		t.Fatalf("totals=%+v, want InsufficientCredential=1", report.Totals)
	}
}

func TestWave3SelectAcceptanceProviders(t *testing.T) {
	enabled := wave3OpenAIProvider("demo", wave3NoAuthUpstream(t), nil)
	disabled := wave3OpenAIProvider("off", wave3NoAuthUpstream(t), nil)
	disabled.Enabled = false

	selected, err := selectAcceptanceProviders([]config.ProviderConfig{enabled, disabled}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].ID != "demo" {
		t.Fatalf("selected=%v, want only demo", selected)
	}
	if _, err := selectAcceptanceProviders([]config.ProviderConfig{enabled}, []string{"ghost"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err=%v, want 'not found'", err)
	}
}

func TestWave3SelectAcceptanceModels(t *testing.T) {
	models := []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
		{ID: "m2", Model: "m2", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
		{ID: "m3", Model: "m3", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}
	p := config.ProviderConfig{ID: "demo", Models: models}

	t.Run("truncation warns", func(t *testing.T) {
		result := &providerAcceptResult{}
		selected := selectAcceptanceModels(p, nil, 2, result)
		if len(selected) != 2 || selected[0].Model != "m1" || selected[1].Model != "m2" {
			t.Fatalf("selected=%v, want m1 m2", selected)
		}
		found := false
		for _, w := range result.Warnings {
			if strings.Contains(w, "1 enabled model(s) not tested") {
				found = true
			}
		}
		if !found {
			t.Fatalf("warnings=%v, want the truncation warning", result.Warnings)
		}
	})
	t.Run("empty and duplicate requested names skipped", func(t *testing.T) {
		result := &providerAcceptResult{}
		selected := selectAcceptanceModels(p, []string{"m1", "m1", ""}, 10, result)
		if len(selected) != 1 || selected[0].Model != "m1" {
			t.Fatalf("selected=%v, want only m1", selected)
		}
	})
	t.Run("requested beyond max warns and continues", func(t *testing.T) {
		result := &providerAcceptResult{}
		selected := selectAcceptanceModels(p, []string{"m1", "m2"}, 1, result)
		if len(selected) != 1 || selected[0].Model != "m1" {
			t.Fatalf("selected=%v, want only m1", selected)
		}
		found := false
		for _, w := range result.Warnings {
			if strings.Contains(w, `model "m2" not tested`) {
				found = true
			}
		}
		if !found {
			t.Fatalf("warnings=%v, want the m2 warning", result.Warnings)
		}
	})
	t.Run("requested model appended", func(t *testing.T) {
		result := &providerAcceptResult{}
		selected := selectAcceptanceModels(p, []string{"m3"}, 10, result)
		if len(selected) != 1 || selected[0].Model != "m3" {
			t.Fatalf("selected=%v, want only m3", selected)
		}
	})
}

func TestWave3AcceptProviderDirect(t *testing.T) {
	srv := wave3NoAuthUpstream(t)

	t.Run("bedrock credential configured false", func(t *testing.T) {
		t.Setenv("WAVE3_BEDROCK_SECRET", "")
		p := config.ProviderConfig{ID: "b", Type: "bedrock", SecretKeyEnv: "WAVE3_BEDROCK_SECRET"}
		calls := 0
		result := acceptProvider(context.Background(), p, acceptOptions{discover: false, test: false, timeout: time.Second, maxModels: 10, maxCalls: 20, calls: &calls})
		if result.CredentialConfigured {
			t.Fatalf("result=%+v, want CredentialConfigured=false", result)
		}
	})
	t.Run("bedrock credential configured true", func(t *testing.T) {
		t.Setenv("WAVE3_BEDROCK_SECRET", "wave3-bedrock-secret-value")
		p := config.ProviderConfig{ID: "b", Type: "bedrock", APIKey: "bedrock-key", SecretKeyEnv: "WAVE3_BEDROCK_SECRET"}
		calls := 0
		result := acceptProvider(context.Background(), p, acceptOptions{discover: false, test: false, timeout: time.Second, maxModels: 10, maxCalls: 20, calls: &calls})
		if !result.CredentialConfigured {
			t.Fatalf("result=%+v, want CredentialConfigured=true", result)
		}
	})
	t.Run("discovery budget exhausted", func(t *testing.T) {
		p := wave3OpenAIProvider("demo", srv, []config.ModelConfig{
			{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
		})
		calls := 20
		result := acceptProvider(context.Background(), p, acceptOptions{discover: true, test: false, timeout: time.Second, maxModels: 10, maxCalls: 20, calls: &calls})
		if !result.Discovery.Attempted || result.Discovery.Error != "upstream call budget exhausted" {
			t.Fatalf("discovery=%+v, want attempted budget-exhausted", result.Discovery)
		}
		if calls != 20 {
			t.Fatalf("calls=%d, want 20 (no upstream call made)", calls)
		}
	})
	t.Run("no enabled model fails", func(t *testing.T) {
		p := wave3OpenAIProvider("demo", srv, nil)
		calls := 0
		result := acceptProvider(context.Background(), p, acceptOptions{discover: false, test: true, timeout: time.Second, maxModels: 10, maxCalls: 20, calls: &calls})
		if len(result.Models) != 1 || result.Models[0].Error != "no enabled model configured; add a model manually before acceptance" {
			t.Fatalf("models=%v, want the no-enabled-model failure entry", result.Models)
		}
	})
	t.Run("adapter creation error fails per model", func(t *testing.T) {
		p := config.ProviderConfig{ID: "bogus", Type: "bogus-type", BaseURL: srv.URL + "/v1", AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}}}
		calls := 0
		result := acceptProvider(context.Background(), p, acceptOptions{discover: false, test: true, timeout: time.Second, maxModels: 10, maxCalls: 20, calls: &calls})
		if len(result.Models) != 1 || result.Models[0].Result != "failed" || !strings.Contains(result.Models[0].Error, "adapter:") {
			t.Fatalf("models=%v, want one failed entry with 'adapter:'", result.Models)
		}
	})
	t.Run("canceled context fails per model", func(t *testing.T) {
		p := wave3OpenAIProvider("demo", srv, []config.ModelConfig{
			{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		result := acceptProvider(ctx, p, acceptOptions{discover: false, test: true, timeout: time.Second, maxModels: 10, maxCalls: 20, calls: &calls})
		if len(result.Models) != 1 || result.Models[0].Error != "canceled" {
			t.Fatalf("models=%v, want one 'canceled' entry", result.Models)
		}
	})
}

func TestWave3WriteProviderAcceptReportErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeProviderAcceptReport(filepath.Join(blocker, "report.json"), providerAcceptReport{}); err == nil {
		t.Fatal("write under a regular file must fail")
	}
}
