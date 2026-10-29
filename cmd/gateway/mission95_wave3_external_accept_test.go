package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
)

// wave3SignIDToken signs an ID token with the OIDC fixture's RSA key
// and kid "test-key", mirroring the fixture signer but accepting
// arbitrary claims so edge cases (missing sub, wrong-type roles) can
// be exercised.
func wave3SignIDToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// wave3ExternalAccept runs runExternalAccept with captured output.
func wave3ExternalAccept(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runExternalAccept(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// wave3NoAuthUpstream answers discovery and probe requests with 200
// and no credential check, for auth-none provider tests.
func wave3NoAuthUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = w.Write([]byte(`{"id":"chatcmpl-wave3","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wave3OpenAIProvider builds an auth-none openai_compatible provider
// with one enabled manual model backed by srv.
func wave3OpenAIProvider(id string, srv *httptest.Server, models []config.ModelConfig) config.ProviderConfig {
	return config.ProviderConfig{
		ID: id, Name: id, Type: "openai_compatible",
		BaseURL: srv.URL + "/v1", AuthMode: "none", Enabled: true, Models: models,
	}
}

// wave3ProviderCheck finds one provider check in an external report.
func wave3ProviderCheck(t *testing.T, report externalAcceptReport, id string) externalAcceptCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Kind == "provider" && check.ID == "provider:"+id {
			return check
		}
	}
	t.Fatalf("report has no provider check %q (checks=%v)", id, report.Checks)
	return externalAcceptCheck{}
}

// wave3OIDCCheck finds the OIDC check in an external report.
func wave3OIDCCheck(t *testing.T, report externalAcceptReport) externalAcceptCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Kind == "oidc" {
			return check
		}
	}
	t.Fatalf("report has no oidc check (checks=%v)", report.Checks)
	return externalAcceptCheck{}
}

func TestWave3ExternalAcceptFlagValidation(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	common := []string{"--provider", "demo", "--confirm-live", "--config", cfgPath}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad flag", []string{"--bogus"}, "flag provided but not defined"},
		{"timeout zero", []string{"--timeout", "0"}, "--timeout must be between"},
		{"timeout over max", []string{"--timeout", "61s"}, "--timeout must be between"},
		{"overall below timeout", []string{"--timeout", "10s", "--overall-timeout", "5s"}, "--overall-timeout must be between"},
		{"overall over max", []string{"--overall-timeout", "6m"}, "--overall-timeout must be between"},
		{"max models zero", []string{"--max-models", "0"}, "--max-models must be between"},
		{"max models over max", []string{"--max-models", "101"}, "--max-models must be between"},
		{"max calls zero", []string{"--max-calls", "0"}, "--max-calls must be between"},
		{"max calls over max", []string{"--max-calls", "101"}, "--max-calls must be between"},
		{"provider plus all", []string{"--provider", "demo", "--all-providers"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), common...)
			// The "provider plus all" case already carries --provider;
			// the shared tail would duplicate it, so drop the tail's
			// --provider pair for that case.
			if tc.name == "provider plus all" {
				args = append(append([]string{}, tc.args...), "--confirm-live", "--config", cfgPath)
			}
			code, _, stderr := wave3ExternalAccept(t, args)
			if code != 2 {
				t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr=%q, want substring %q", stderr, tc.want)
			}
		})
	}
}

func TestWave3ExternalAcceptConfigLoadFailure(t *testing.T) {
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--provider", "tenant", "--config", filepath.Join(t.TempDir(), "missing.json"), "--confirm-live",
	})
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "cannot load config") {
		t.Fatalf("stderr=%q, want 'cannot load config'", stderr)
	}
}

func TestWave3ExternalAcceptUnknownProvider(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--provider", "ghost", "--config", cfgPath, "--confirm-live",
	})
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "not found") {
		t.Fatalf("stderr=%q, want 'not found'", stderr)
	}
}

func TestWave3ExternalAcceptAllProvidersNoneEnabled(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	p := wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	})
	p.Enabled = false
	cfgPath := acceptanceConfig(t, p)
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--all-providers", "--config", cfgPath, "--confirm-live",
	})
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "no matching enabled providers selected") {
		t.Fatalf("stderr=%q, want 'no matching enabled providers selected'", stderr)
	}
}

func TestWave3ExternalAcceptAllProvidersSelectsEnabled(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	enabled := wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	})
	disabled := wave3OpenAIProvider("other", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	})
	disabled.Enabled = false
	cfgPath := acceptanceConfig(t, enabled, disabled)
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--all-providers", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	report := readExternalReport(t, out)
	check := wave3ProviderCheck(t, report, "demo")
	if check.Status != externalStatusPass {
		t.Fatalf("demo check=%+v, want PASS", check)
	}
	for _, c := range report.Checks {
		if c.Kind == "provider" && c.ID != "provider:demo" {
			t.Fatalf("disabled provider leaked into report: %+v", c)
		}
	}
}

func TestWave3ExternalAcceptDefaultOutPath(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	workdir := t.TempDir()
	t.Chdir(workdir)
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--all-providers", "--config", cfgPath, "--confirm-live",
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
		if strings.HasPrefix(e.Name(), "external-acceptance-") && strings.HasSuffix(e.Name(), ".json") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no external-acceptance-*.json in %v", entries)
	}
}

func TestWave3ExternalAcceptReportWriteFailure(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--all-providers", "--config", cfgPath, "--confirm-live", "--out", filepath.Join(blocker, "report.json"),
	})
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "cannot write report") {
		t.Fatalf("stderr=%q, want 'cannot write report'", stderr)
	}
}

func TestWave3ExternalAcceptOIDCDisabledBlocked(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--oidc", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (stderr=%s)", code, stderr)
	}
	check := wave3OIDCCheck(t, readExternalReport(t, out))
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "admin.oidc is not enabled") {
		t.Fatalf("oidc check=%+v, want BLOCKED 'admin.oidc is not enabled'", check)
	}
	if check.OIDC == nil || check.OIDC.Discovery.Status != externalStatusBlocked || check.OIDC.JWKS.Status != externalStatusBlocked {
		t.Fatalf("oidc probes=%+v, want discovery+JWKS BLOCKED", check.OIDC)
	}
}

func TestWave3ExternalAcceptOIDCNonAbsoluteIssuerBlocked(t *testing.T) {
	t.Setenv("WAVE3_OIDC_ISSUER", "not-a-url")
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--oidc-issuer-env", "WAVE3_OIDC_ISSUER", "--confirm-live", "--out", out,
	})
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (stderr=%s)", code, stderr)
	}
	check := wave3OIDCCheck(t, readExternalReport(t, out))
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "not an absolute URL") {
		t.Fatalf("oidc check=%+v, want BLOCKED 'not an absolute URL'", check)
	}
	if check.OIDC == nil || check.OIDC.Discovery.Status != externalStatusBlocked || check.OIDC.JWKS.Status != externalStatusBlocked {
		t.Fatalf("oidc probes=%+v, want discovery+JWKS BLOCKED", check.OIDC)
	}
}

func TestWave3ExternalAcceptOIDCEndpointValidationFails(t *testing.T) {
	// go-oidc's NewProvider itself rejects a discovery document
	// whose issuer differs from the requested issuer, so an issuer
	// mismatch surfaces as "OIDC discovery failed", not as the
	// endpoint-validation branch. The endpoint-validation branch is
	// reached instead with a discovery document whose issuer matches
	// but whose authorization endpoint is a non-loopback HTTP URL
	// while the issuer is loopback.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `","authorization_endpoint":"http://example.com/authorize","token_endpoint":"` + srv.URL + `/token","jwks_uri":"` + srv.URL + `/jwks","response_types_supported":["code"],"subject_types_supported":["public"],"id_token_signing_alg_values_supported":["RS256"]}`))
	}))
	defer srv.Close()
	check := runExternalOIDCCheck(context.Background(), srv.URL,
		config.OIDCConfig{ClientID: "staging-client"}, "", "", 5*time.Second, livecanary.NewRedactor(nil))
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "failed endpoint validation") {
		t.Fatalf("oidc check=%+v, want FAIL 'failed endpoint validation'", check)
	}
	if check.OIDC == nil || check.OIDC.Discovery.Status != externalStatusFail || check.OIDC.JWKS.Status != externalStatusNotRun {
		t.Fatalf("oidc probes=%+v, want discovery FAIL, JWKS NOT_RUN", check.OIDC)
	}
}

func TestWave3ExternalAcceptOIDCEmptyIDTokenEnvBlocked(t *testing.T) {
	fixture := newExternalOIDCFixture(t)
	t.Setenv("NEXAROUTE_EXT_OIDC_SECRET", "wave3-oidc-secret")
	cfgPath := externalOIDCConfig(t, fixture.server.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--oidc", "--oidc-id-token-env", "WAVE3_OIDC_TOKEN", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (stderr=%s)", code, stderr)
	}
	check := wave3OIDCCheck(t, readExternalReport(t, out))
	if check.Status != externalStatusBlocked || check.OIDC == nil || check.OIDC.IDToken.Status != externalStatusBlocked || !strings.Contains(check.OIDC.IDToken.Detail, "not set") {
		t.Fatalf("oidc check=%+v, want IDToken BLOCKED 'not set'", check)
	}
}

func TestWave3ExternalAcceptProviderNoEnabledModelBlocked(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, nil))
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--provider", "demo", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (stderr=%s)", code, stderr)
	}
	check := wave3ProviderCheck(t, readExternalReport(t, out), "demo")
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "no enabled model") {
		t.Fatalf("demo check=%+v, want BLOCKED 'no enabled model'", check)
	}
}

func TestWave3ExternalAcceptProviderDegradedIsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-wave3","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	cfgPath := acceptanceConfig(t, wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	}))
	out := filepath.Join(t.TempDir(), "report.json")
	code, _, stderr := wave3ExternalAccept(t, []string{
		"--provider", "demo", "--config", cfgPath, "--confirm-live", "--out", out,
	})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	check := wave3ProviderCheck(t, readExternalReport(t, out), "demo")
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "did not pass every selected check") {
		t.Fatalf("demo check=%+v, want FAIL 'did not pass every selected check'", check)
	}
}

func TestWave3ExternalAcceptProviderCanceledContextFails(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	p := wave3OpenAIProvider("demo", srv, []config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := runExternalProviderCheck(ctx, p, acceptOptions{
		discover: true, test: true, timeout: time.Second, maxModels: 1, maxCalls: 1,
	}, livecanary.NewRedactor(nil))
	if result.Status != externalStatusFail {
		t.Fatalf("status=%q, want FAIL", result.Status)
	}
	if !strings.Contains(result.Reason, "cancellation") {
		t.Fatalf("reason=%q, want 'cancellation'", result.Reason)
	}
}

func TestWave3ResolveAcceptanceOIDC(t *testing.T) {
	fixture := newExternalOIDCFixture(t)

	t.Run("disabled with config", func(t *testing.T) {
		cfg := config.Default()
		oidc, issuer, reason := resolveAcceptanceOIDC(cfg, true, true, "", "", "")
		if reason != "admin.oidc is not enabled in the selected configuration" || issuer != "" {
			t.Fatalf("oidc=%+v issuer=%q reason=%q, want disabled reason", oidc, issuer, reason)
		}
	})
	t.Run("overrides win", func(t *testing.T) {
		cfg := config.Default()
		cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: fixture.server.URL, ClientID: "base-client", Audience: "base-audience"}
		oidc, issuer, reason := resolveAcceptanceOIDC(cfg, true, true, "", "override-client", "override-audience")
		if reason != "" || issuer != fixture.server.URL {
			t.Fatalf("issuer=%q reason=%q, want %q and empty reason", issuer, reason, fixture.server.URL)
		}
		if oidc.ClientID != "override-client" || oidc.Audience != "override-audience" {
			t.Fatalf("oidc=%+v, want client override-client audience override-audience", oidc)
		}
	})
	t.Run("env branch defaults audience to client id", func(t *testing.T) {
		t.Setenv("WAVE3_OIDC_ISSUER_ENV", fixture.server.URL)
		cfg := config.Default()
		cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: fixture.server.URL, ClientID: "env-client", Audience: ""}
		oidc, issuer, reason := resolveAcceptanceOIDC(cfg, true, false, "WAVE3_OIDC_ISSUER_ENV", "", "")
		if reason != "" || issuer != fixture.server.URL {
			t.Fatalf("issuer=%q reason=%q, want %q and empty reason", issuer, reason, fixture.server.URL)
		}
		if oidc.Audience != "env-client" {
			t.Fatalf("audience=%q, want env-client", oidc.Audience)
		}
	})
}

func TestWave3ExternalProviderCredentialReady(t *testing.T) {
	t.Setenv("WAVE3_BEDROCK_SECRET", "")
	t.Setenv("WAVE3_BEDROCK_KEY", "")

	none := config.ProviderConfig{ID: "n", Type: "openai_compatible", AuthMode: "none"}
	if ready, name := externalProviderCredentialReady(none); !ready || name != "" {
		t.Fatalf("none: ready=%v name=%q, want true ''", ready, name)
	}
	bedrockNoCreds := config.ProviderConfig{ID: "b", Type: "bedrock"}
	if ready, name := externalProviderCredentialReady(bedrockNoCreds); ready || name != "api_key_env" {
		t.Fatalf("bedrock no creds: ready=%v name=%q, want false 'api_key_env'", ready, name)
	}
	bedrockNoSecret := config.ProviderConfig{ID: "b", Type: "bedrock", APIKey: "bedrock-key", SecretKeyEnv: "WAVE3_BEDROCK_SECRET"}
	if ready, name := externalProviderCredentialReady(bedrockNoSecret); ready || name != "WAVE3_BEDROCK_SECRET" {
		t.Fatalf("bedrock no secret: ready=%v name=%q, want false 'WAVE3_BEDROCK_SECRET'", ready, name)
	}
	bedrockKeyEnvUnset := config.ProviderConfig{ID: "b", Type: "bedrock", APIKeyEnv: "WAVE3_BEDROCK_KEY", SecretKeyEnv: "WAVE3_BEDROCK_SECRET"}
	if ready, name := externalProviderCredentialReady(bedrockKeyEnvUnset); ready || name != "WAVE3_BEDROCK_KEY" {
		t.Fatalf("bedrock key env unset: ready=%v name=%q, want false 'WAVE3_BEDROCK_KEY'", ready, name)
	}
	t.Setenv("WAVE3_BEDROCK_SECRET", "wave3-bedrock-secret-value")
	if ready, name := externalProviderCredentialReady(bedrockNoSecret); !ready || name != "" {
		t.Fatalf("bedrock with secret: ready=%v name=%q, want true ''", ready, name)
	}
	openaiNoCreds := config.ProviderConfig{ID: "o", Type: "openai_compatible", AuthMode: "bearer"}
	if ready, name := externalProviderCredentialReady(openaiNoCreds); ready || name != "api_key_env" {
		t.Fatalf("openai no creds: ready=%v name=%q, want false 'api_key_env'", ready, name)
	}
}

func TestWave3FetchOIDCJWKS(t *testing.T) {
	fixture := newExternalOIDCFixture(t)

	t.Run("non loopback http rejected", func(t *testing.T) {
		_, err := fetchOIDCJWKS(context.Background(), "http://example.com/.well-known/jwks.json", time.Second)
		if err == nil || !strings.Contains(err.Error(), "not an absolute HTTPS URL") {
			t.Fatalf("err=%v, want 'not an absolute HTTPS URL'", err)
		}
	})
	t.Run("closed server fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL + "/jwks"
		srv.Close()
		_, err := fetchOIDCJWKS(context.Background(), url, time.Second)
		if err == nil || !strings.Contains(err.Error(), "JWKS request failed") {
			t.Fatalf("err=%v, want 'JWKS request failed'", err)
		}
	})
	t.Run("truncated body fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("short"))
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}))
		defer srv.Close()
		_, err := fetchOIDCJWKS(context.Background(), srv.URL+"/jwks", time.Second)
		if err == nil || !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("err=%v, want 'could not be read'", err)
		}
	})
	t.Run("oversize body fails", func(t *testing.T) {
		big := make([]byte, 1024*1024+1)
		for i := range big {
			big[i] = 'x'
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(big)
		}))
		defer srv.Close()
		_, err := fetchOIDCJWKS(context.Background(), srv.URL+"/jwks", time.Second)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("err=%v, want 'exceeds'", err)
		}
	})
	t.Run("invalid json fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		_, err := fetchOIDCJWKS(context.Background(), srv.URL+"/jwks", time.Second)
		if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Fatalf("err=%v, want 'not valid JSON'", err)
		}
	})
	t.Run("no usable key fails", func(t *testing.T) {
		for _, body := range []string{`{"keys":[]}`, `{"keys":[{"kty":"RSA"}]}`} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			_, err := fetchOIDCJWKS(context.Background(), srv.URL+"/jwks", time.Second)
			srv.Close()
			if err == nil || !strings.Contains(err.Error(), "no usable signing key") {
				t.Fatalf("body=%s err=%v, want 'no usable signing key'", body, err)
			}
		}
	})
	t.Run("valid loopback jwks succeeds", func(t *testing.T) {
		count, err := fetchOIDCJWKS(context.Background(), fixture.server.URL+"/jwks", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("count=%d, want 1", count)
		}
	})
}

func TestWave3VerifyAcceptanceIDToken(t *testing.T) {
	fixture := newExternalOIDCFixture(t)
	issuer := fixture.server.URL
	jwksURL := issuer + "/jwks"
	now := time.Now().Unix()

	base := func() map[string]any {
		return map[string]any{
			"iss": issuer, "aud": "staging-client", "sub": "user-1",
			"iat": now - 60, "exp": now + 3600, "groups": []string{"nexaroute-admins"},
		}
	}
	cfg := func() config.OIDCConfig {
		return config.OIDCConfig{
			IssuerURL: issuer, ClientID: "staging-client", Audience: "staging-client",
			RoleClaim: "groups", RoleMappings: map[string]string{"nexaroute-admins": "admin", "nexaroute-operators": "operator"},
		}
	}

	t.Run("audience falls back to client id", func(t *testing.T) {
		c := cfg()
		c.Audience = ""
		c.RoleClaim = ""
		c.RoleMappings = nil
		token := wave3SignIDToken(t, fixture.key, base())
		roleCount, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, c, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if roleCount != -1 {
			t.Fatalf("roleCount=%d, want -1", roleCount)
		}
	})
	t.Run("neither audience nor client id", func(t *testing.T) {
		c := cfg()
		c.Audience = ""
		c.ClientID = ""
		token := wave3SignIDToken(t, fixture.key, base())
		_, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, c, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "expected OIDC audience or client id") {
			t.Fatalf("err=%v, want 'expected OIDC audience or client id'", err)
		}
	})
	t.Run("missing subject rejected", func(t *testing.T) {
		claims := base()
		delete(claims, "sub")
		token := wave3SignIDToken(t, fixture.key, claims)
		_, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, cfg(), 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "issuer or subject is invalid") {
			t.Fatalf("err=%v, want 'issuer or subject is invalid'", err)
		}
	})
	t.Run("role claim wrong type rejected", func(t *testing.T) {
		claims := base()
		claims["groups"] = 123
		token := wave3SignIDToken(t, fixture.key, claims)
		_, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, cfg(), 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "invalid type") {
			t.Fatalf("err=%v, want 'invalid type'", err)
		}
	})
	t.Run("unmapped roles rejected", func(t *testing.T) {
		claims := base()
		claims["groups"] = []string{"nobody"}
		token := wave3SignIDToken(t, fixture.key, claims)
		_, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, cfg(), 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "no allowlisted role") {
			t.Fatalf("err=%v, want 'no allowlisted role'", err)
		}
	})
	t.Run("conflicting roles rejected", func(t *testing.T) {
		claims := base()
		claims["groups"] = []string{"nexaroute-admins", "nexaroute-operators"}
		token := wave3SignIDToken(t, fixture.key, claims)
		_, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, cfg(), 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err=%v, want 'conflicting'", err)
		}
	})
	t.Run("valid mapped role accepted", func(t *testing.T) {
		token := wave3SignIDToken(t, fixture.key, base())
		roleCount, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, cfg(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if roleCount != 1 {
			t.Fatalf("roleCount=%d, want 1", roleCount)
		}
	})
}

func TestWave3FinalizeExternalAcceptReport(t *testing.T) {
	pass := func(id, class string) externalAcceptCheck {
		return externalAcceptCheck{ID: id, Kind: "provider", Selected: true, Status: externalStatusPass, TargetClass: class}
	}

	t.Run("mixed evidence class", func(t *testing.T) {
		report := externalAcceptReport{Checks: []externalAcceptCheck{
			pass("provider:local", "loopback"), pass("provider:remote", "external"),
		}}
		finalizeExternalAcceptReport(&report)
		if report.EvidenceClass != "mixed" || report.Outcome != externalStatusPass {
			t.Fatalf("evidence=%q outcome=%q, want mixed/PASS", report.EvidenceClass, report.Outcome)
		}
		if report.Totals.Checks != 2 || report.Totals.Passed != 2 || report.Totals.ProvidersTested != 2 {
			t.Fatalf("totals=%+v, want checks=2 passed=2 providers=2", report.Totals)
		}
		found := false
		for _, l := range report.Limitations {
			if strings.Contains(l, "loopback") {
				found = true
			}
		}
		if !found {
			t.Fatalf("limitations=%v, want a loopback limitation", report.Limitations)
		}
	})
	t.Run("external only", func(t *testing.T) {
		report := externalAcceptReport{Checks: []externalAcceptCheck{pass("provider:remote", "external")}}
		finalizeExternalAcceptReport(&report)
		if report.EvidenceClass != "external" {
			t.Fatalf("evidence=%q, want external", report.EvidenceClass)
		}
		for _, l := range report.Limitations {
			if strings.Contains(l, "loopback") {
				t.Fatalf("unexpected loopback limitation: %v", report.Limitations)
			}
		}
	})
	t.Run("not run outcome limitation", func(t *testing.T) {
		report := externalAcceptReport{Checks: []externalAcceptCheck{
			{ID: "provider:remote", Kind: "provider", Selected: true, Status: externalStatusNotRun, TargetClass: "external"},
		}}
		finalizeExternalAcceptReport(&report)
		if report.Outcome != externalStatusNotRun {
			t.Fatalf("outcome=%q, want NOT_RUN", report.Outcome)
		}
		found := false
		for _, l := range report.Limitations {
			if strings.Contains(l, "not acceptance evidence") {
				found = true
			}
		}
		if !found {
			t.Fatalf("limitations=%v, want 'not acceptance evidence'", report.Limitations)
		}
	})
}

func TestWave3ExternalAcceptExitCode(t *testing.T) {
	cases := []struct {
		outcome string
		want    int
	}{
		{"PASS", 0},
		{"FAIL", 1},
		{"BLOCKED", 3},
		{"NOT_RUN", 4},
		{"bogus-outcome", 2},
	}
	for _, tc := range cases {
		if got := externalAcceptExitCode(tc.outcome); got != tc.want {
			t.Errorf("externalAcceptExitCode(%q)=%d, want %d", tc.outcome, got, tc.want)
		}
	}
}

func TestWave3WriteExternalAcceptReportErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeExternalAcceptReport(filepath.Join(blocker, "report.json"), externalAcceptReport{Outcome: "PASS"}); err == nil {
		t.Fatal("write under a regular file must fail")
	}
}
