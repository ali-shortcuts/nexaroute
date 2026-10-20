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

func minimalCanaryConfig(baseURL string) config.Config {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "demo", Name: "Demo", Type: "openai_compatible", BaseURL: baseURL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 1,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	return cfg
}

func openAICanaryServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
}

func TestConformanceCanaryRequiresCostAcknowledgement(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runConformance([]string{"canary", "--preset", "custom", "--model", "m"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--acknowledge-cost") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestConformanceCanaryMissingCredentialFailsBeforeNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := openAICanaryServer(t, &hits)
	defer srv.Close()
	t.Setenv("NEXAROUTE_CANARY_API_KEY", "")

	var stdout, stderr bytes.Buffer
	code := runConformance([]string{
		"canary", "--acknowledge-cost", "--preset", "custom", "--model", "m",
		"--base-url", srv.URL, "--scenarios", "text", "--max-calls", "1",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("missing credential made %d upstream calls", hits.Load())
	}
	if !strings.Contains(stderr.String(), "NEXAROUTE_CANARY_API_KEY") {
		t.Fatalf("stderr does not name the missing env var: %q", stderr.String())
	}
}

func TestConformanceCanaryWritesEvidence(t *testing.T) {
	var hits atomic.Int64
	srv := openAICanaryServer(t, &hits)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "nested", "canary.json")
	var stdout, stderr bytes.Buffer
	code := runConformance([]string{
		"canary", "--acknowledge-cost", "--preset", "custom", "--model", "m",
		"--base-url", srv.URL, "--auth-mode", "none", "--scenarios", "text",
		"--max-calls", "1", "--max-output-tokens", "32", "--out", out,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", hits.Load())
	}
	if strings.TrimSpace(stdout.String()) != out {
		t.Fatalf("stdout=%q, want report path %q", stdout.String(), out)
	}
	if !strings.Contains(stderr.String(), "outcome=PASS") {
		t.Fatalf("summary=%q", stderr.String())
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode=%o, want 600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Outcome string `json:"outcome"`
		Limit   struct {
			CallsUsed int `json:"calls_used"`
		} `json:"limits"`
		Verified []string `json:"verified_dimensions"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "PASS" || report.Limit.CallsUsed != 1 || len(report.Verified) == 0 {
		t.Fatalf("report=%+v", report)
	}
}

func TestConformanceCanaryProviderMode(t *testing.T) {
	var hits atomic.Int64
	srv := openAICanaryServer(t, &hits)
	defer srv.Close()

	cfg := writeTestConfig(t, minimalCanaryConfig(srv.URL))
	out := filepath.Join(t.TempDir(), "canary.json")
	var stdout, stderr bytes.Buffer
	code := runConformance([]string{
		"canary", "--acknowledge-cost", "--config", cfg, "--provider", "demo",
		"--scenarios", "text", "--max-calls", "1", "--out", out,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", hits.Load())
	}
}

func TestConformanceDispatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatchSubcommand([]string{"conformance"}, strings.NewReader(""), &stdout, &stderr)
	if !handled {
		t.Fatal("conformance subcommand is not dispatched; the binary would start the gateway")
	}
	if code != 2 {
		t.Fatalf("code=%d, want usage error 2", code)
	}
}
