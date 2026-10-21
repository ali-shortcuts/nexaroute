package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestProviderAcceptReportDirectoryIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "reports")
	path := filepath.Join(dir, "report.json")
	if err := writeProviderAcceptReport(path, providerAcceptReport{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("report directory mode=%o, want no group/other access (0700)", info.Mode().Perm())
	}
}

func fakeOpenAIUpstream(t *testing.T, secret string, discoveryStatus int, probeStatus int) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var discoveryCalls, probeCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret && r.Header.Get("api-key") != secret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			discoveryCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if discoveryStatus != 0 && discoveryStatus != http.StatusOK {
				w.WriteHeader(discoveryStatus)
				_, _ = w.Write([]byte(`{"error":"inventory denied for ` + secret + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a"},{"id":"model-b"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			probeCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if probeStatus != 0 && probeStatus != http.StatusOK {
				w.WriteHeader(probeStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"probe denied for ` + secret + `"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	return up, &discoveryCalls, &probeCalls
}

func acceptanceConfig(t *testing.T, providers ...config.ProviderConfig) string {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = providers
	return writeTestConfig(t, cfg)
}

func TestProviderAcceptLivePassWritesRedactedReport(t *testing.T) {
	const secret = "accept-secret-canary-4c1f"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Name: "Tenant", Type: "openai_compatible",
		BaseURL: up.URL + "/v1", APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY",
		AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "manual-model", Model: "manual-model", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
			{ID: "discovered-model", Model: "discovered-model", Enabled: true, Weight: 1, Source: config.ModelSourceDiscovered},
		},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(reportPath, []byte("stale report"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runProviderAccept([]string{
		"--config", configPath, "--provider", "tenant", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if discoveryCalls.Load() != 1 || probeCalls.Load() != 2 {
		t.Fatalf("calls: discovery=%d probe=%d", discoveryCalls.Load(), probeCalls.Load())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatal("command output leaked a credential value")
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("report leaked a credential value")
	}
	if info, err := os.Stat(reportPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
	var report providerAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Mode != "discover+test" {
		t.Fatalf("report ok=%v mode=%q", report.OK, report.Mode)
	}
	if report.Bounds.MaxOutputTokensPerCall != 1 || report.Bounds.UpstreamCalls != 3 {
		t.Fatalf("bounds=%+v", report.Bounds)
	}
	p := report.Providers[0]
	if p.Status != "passed" || !p.Discovery.OK || p.Discovery.ModelCount != 2 {
		t.Fatalf("provider result=%+v discovery=%+v", p, p.Discovery)
	}
	if p.CredentialSource != "env" || !p.CredentialConfigured {
		t.Fatalf("credential state=%+v", p)
	}
	if len(p.Models) != 2 || p.Models[0].Result != "passed" || p.Models[1].Result != "passed" {
		t.Fatalf("model results=%+v", p.Models)
	}
	if p.Models[0].Source != config.ModelSourceManual || p.Models[1].Source != config.ModelSourceDiscovered {
		t.Fatalf("provenance not preserved: %+v", p.Models)
	}
}

func TestProviderAcceptDiscoveryFailureStillTestsManualModelsAndRedacts(t *testing.T) {
	const secret = "accept-secret-canary-8d2b"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	up, _, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusForbidden, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Name: "Tenant", Type: "openai_compatible",
		BaseURL: up.URL + "/v1", APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY",
		AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "manual", Model: "manual", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runProviderAccept([]string{
		"--config", configPath, "--provider", "tenant", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	// Discovery failure is reported, but the manually configured model is
	// still tested. A discovery observation gap makes the run degraded.
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (degraded)\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("manual model probe calls=%d want 1", probeCalls.Load())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{string(raw), stdout.String(), stderr.String()} {
		if strings.Contains(out, secret) {
			t.Fatal("acceptance output leaked a credential value")
		}
	}
	var report providerAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	p := report.Providers[0]
	if p.Status != "degraded" || p.Discovery.OK {
		t.Fatalf("provider status=%q discovery=%+v", p.Status, p.Discovery)
	}
	if len(p.Models) != 1 || p.Models[0].Result != "passed" {
		t.Fatalf("manual model was not tested independently: %+v", p.Models)
	}
}

func TestProviderAcceptRequiresConfirmLiveAndMakesNoCalls(t *testing.T) {
	const secret = "accept-secret-canary-11aa"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	if code := runProviderAccept([]string{"--config", configPath, "--provider", "tenant", "--out", reportPath}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if discoveryCalls.Load() != 0 || probeCalls.Load() != 0 {
		t.Fatalf("unconfirmed run made upstream calls: discovery=%d probe=%d", discoveryCalls.Load(), probeCalls.Load())
	}
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed run wrote a report: %v", err)
	}
	if !strings.Contains(stderr.String(), "--confirm-live") {
		t.Fatalf("missing confirmation diagnostic: %s", stderr.String())
	}
}

func TestProviderAcceptUnsupportedDiscoveryDoesNotCallModelsEndpoint(t *testing.T) {
	const secret = "accept-secret-canary-73c0"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "azure", Type: "azure_openai", BaseURL: up.URL,
		APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY", AuthMode: "api-key", Enabled: true,
		Models: []config.ModelConfig{{ID: "deployment", Model: "deployment", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runProviderAccept([]string{
		"--config", configPath, "--provider", "azure", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if discoveryCalls.Load() != 0 {
		t.Fatalf("unsupported discovery called the models endpoint %d time(s)", discoveryCalls.Load())
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls=%d want 1", probeCalls.Load())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report providerAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	p := report.Providers[0]
	if p.Status != "passed" || p.Discovery.Supported || p.Discovery.Attempted {
		t.Fatalf("azure result=%+v discovery=%+v", p, p.Discovery)
	}
}

func TestProviderAcceptModelFilterRejectsUnconfiguredModel(t *testing.T) {
	const secret = "accept-secret-canary-b6d1"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	up, _, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runProviderAccept([]string{
		"--config", configPath, "--provider", "tenant", "--model", "not-configured",
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
	if probeCalls.Load() != 0 {
		t.Fatalf("unconfigured model was probed %d time(s)", probeCalls.Load())
	}
	raw, _ := os.ReadFile(reportPath)
	if !strings.Contains(string(raw), "not configured for this provider") {
		t.Fatalf("missing manual-model diagnostic: %s", raw)
	}
}

func TestProviderAcceptDiscoveryDisabledStillCountsModelTotals(t *testing.T) {
	const secret = "accept-secret-canary-95e4"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	if code := runProviderAccept([]string{
		"--config", configPath, "--provider", "tenant", "--discover=false",
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if discoveryCalls.Load() != 0 || probeCalls.Load() != 1 {
		t.Fatalf("calls: discovery=%d probe=%d", discoveryCalls.Load(), probeCalls.Load())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report providerAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Totals.ModelsTested != 1 || report.Totals.ModelsPassed != 1 {
		t.Fatalf("totals=%+v", report.Totals)
	}
	if report.Providers[0].Discovery.Attempted {
		t.Fatal("discovery was attempted despite --discover=false")
	}
}

func TestProviderAcceptStopsAtUpstreamCallBudget(t *testing.T) {
	const secret = "accept-secret-canary-a03d"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runProviderAccept([]string{
		"--config", configPath, "--provider", "tenant", "--max-calls", "1",
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
	if discoveryCalls.Load() != 1 || probeCalls.Load() != 0 {
		t.Fatalf("budget not enforced: discovery=%d probe=%d", discoveryCalls.Load(), probeCalls.Load())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "upstream call budget exhausted") {
		t.Fatalf("missing budget diagnostic: %s", raw)
	}
}

func TestProviderAcceptSubcommandIsDispatched(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatchSubcommand([]string{
		"provider-accept", "--confirm-live", "--provider", "tenant", "--config", filepath.Join(t.TempDir(), "missing.json"),
	}, nil, &stdout, &stderr)
	if !handled {
		t.Fatal("provider-accept was not dispatched; main would start the gateway")
	}
	if code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "cannot load config") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestProviderAcceptReportReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dir, "report.json")
	if err := os.Symlink(target, reportPath); err != nil {
		t.Fatal(err)
	}
	if err := writeProviderAcceptReport(reportPath, providerAcceptReport{Mode: "test"}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "keep" {
		t.Fatalf("symlink target was modified: %q err=%v", raw, err)
	}
	info, err := os.Lstat(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode=%v, want a regular 0600 file", info.Mode())
	}
}
