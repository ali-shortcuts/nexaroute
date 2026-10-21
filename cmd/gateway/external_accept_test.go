package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
	jose "github.com/go-jose/go-jose/v4"
)

type externalOIDCFixture struct {
	server          *httptest.Server
	key             *rsa.PrivateKey
	issuerOverride  string
	discoveryStatus int
	jwksStatus      int
	jwksCalls       atomic.Int64
	groups          []string
}

func newExternalOIDCFixture(t *testing.T) *externalOIDCFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &externalOIDCFixture{key: key, groups: []string{"nexaroute-admins"}}
	mux := http.NewServeMux()
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if f.discoveryStatus != 0 {
			http.Error(w, "discovery unavailable", f.discoveryStatus)
			return
		}
		issuer := f.issuerOverride
		if issuer == "" {
			issuer = f.server.URL
		}
		writeExternalTestJSON(w, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                f.server.URL + "/authorize",
			"token_endpoint":                        f.server.URL + "/token",
			"jwks_uri":                              f.server.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		f.jwksCalls.Add(1)
		if f.jwksStatus != 0 {
			http.Error(w, "jwks unavailable", f.jwksStatus)
			return
		}
		writeExternalTestJSON(w, map[string]any{"keys": []any{externalTestJWK(&f.key.PublicKey, "test-key")}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "interactive token exchange is not used by this harness", http.StatusNotImplemented)
	})
	return f
}

func (f *externalOIDCFixture) idToken(t *testing.T, audience string, expiry time.Time, groups []string) string {
	t.Helper()
	claims := map[string]any{
		"iss": f.server.URL, "sub": "acceptance-user", "aud": audience,
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": expiry.Unix(), "groups": groups,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), "test-key")
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, options)
	if err != nil {
		t.Fatal(err)
	}
	object, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := object.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func externalTestJWK(key *rsa.PublicKey, kid string) map[string]string {
	return map[string]string{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func writeExternalTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func externalOIDCConfig(t *testing.T, issuer, secretEnv string) string {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.OIDC = config.OIDCConfig{
		Enabled: true, IssuerURL: issuer, ClientID: "staging-client",
		ClientSecretEnv: secretEnv, RedirectURL: "http://localhost/admin/auth/oidc/callback",
		Audience: "nexaroute-control-plane", RoleClaim: "groups",
		RoleMappings: map[string]string{"nexaroute-admins": "admin"},
	}
	return writeTestConfig(t, cfg)
}

func readExternalReport(t *testing.T, path string) externalAcceptReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report externalAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestExternalAcceptRequiresConfirmLiveAndWritesNothing(t *testing.T) {
	const secret = "external-accept-secret-7f31"
	t.Setenv("NEXAROUTE_EXT_ACCEPT_KEY", secret)
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "NEXAROUTE_EXT_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	if code := runExternalAccept([]string{"--config", configPath, "--provider", "tenant", "--out", reportPath}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit=%d want 2\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if discoveryCalls.Load() != 0 || probeCalls.Load() != 0 {
		t.Fatalf("unconfirmed run made upstream calls: discovery=%d probe=%d", discoveryCalls.Load(), probeCalls.Load())
	}
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed run wrote a report: %v", err)
	}
	if !strings.Contains(stderr.String(), "--confirm-live") {
		t.Fatalf("missing opt-in diagnostic: %s", stderr.String())
	}
}

func TestExternalAcceptProviderPassReportsPassAndRedacts(t *testing.T) {
	const secret = "external-accept-secret-5c19"
	t.Setenv("NEXAROUTE_EXT_ACCEPT_KEY", secret)
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, secret, http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "NEXAROUTE_EXT_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "nested", "report.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--provider", "tenant", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if discoveryCalls.Load() != 1 || probeCalls.Load() != 1 {
		t.Fatalf("calls: discovery=%d probe=%d", discoveryCalls.Load(), probeCalls.Load())
	}
	report := readExternalReport(t, reportPath)
	if report.Schema != externalAcceptSchema || report.Outcome != externalStatusPass || report.Mode != "live" {
		t.Fatalf("report header=%+v outcome=%s", report, report.Outcome)
	}
	if report.Totals.ProvidersTested != 1 || report.Totals.Passed != 1 {
		t.Fatalf("totals=%+v", report.Totals)
	}
	if len(report.Checks) != 2 {
		t.Fatalf("checks=%+v", report.Checks)
	}
	providerCheck := report.Checks[0]
	if providerCheck.ID != "provider:tenant" || providerCheck.Status != externalStatusPass || providerCheck.Provider == nil {
		t.Fatalf("provider check=%+v", providerCheck)
	}
	if providerCheck.Provider.Status != "passed" || len(providerCheck.Provider.Models) != 1 {
		t.Fatalf("provider evidence=%+v", providerCheck.Provider)
	}
	oidcCheck := report.Checks[1]
	if oidcCheck.ID != "oidc" || oidcCheck.Selected || oidcCheck.Status != externalStatusNotRun {
		t.Fatalf("unselected OIDC check=%+v", oidcCheck)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{string(raw), stdout.String(), stderr.String()} {
		if strings.Contains(out, secret) {
			t.Fatalf("acceptance output leaked a credential value:\n%s", out)
		}
	}
	if info, err := os.Stat(reportPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
}

func TestExternalAcceptMissingProviderCredentialIsBlockedWithoutCalls(t *testing.T) {
	up, discoveryCalls, probeCalls := fakeOpenAIUpstream(t, "unused", http.StatusOK, http.StatusOK)
	configPath := acceptanceConfig(t, config.ProviderConfig{
		ID: "tenant", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		APIKeyEnv: "NEXAROUTE_EXT_ACCEPT_UNSET", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	})
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--provider", "tenant", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit=%d want 3 BLOCKED\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if discoveryCalls.Load() != 0 || probeCalls.Load() != 0 {
		t.Fatalf("blocked run made upstream calls: discovery=%d probe=%d", discoveryCalls.Load(), probeCalls.Load())
	}
	report := readExternalReport(t, reportPath)
	if report.Outcome != externalStatusBlocked || report.Checks[0].Status != externalStatusBlocked {
		t.Fatalf("report=%+v", report)
	}
	if !strings.Contains(report.Checks[0].Reason, "NEXAROUTE_EXT_ACCEPT_UNSET") {
		t.Fatalf("blocked reason does not name the missing env var: %s", report.Checks[0].Reason)
	}
}

func TestExternalAcceptOIDCDiscoveryAndJWKSPass(t *testing.T) {
	const secret = "external-oidc-client-secret-91de"
	t.Setenv("NEXAROUTE_EXT_OIDC_SECRET", secret)
	fixture := newExternalOIDCFixture(t)
	configPath := externalOIDCConfig(t, fixture.server.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	reportPath := filepath.Join(t.TempDir(), "oidc.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--oidc", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	if report.Outcome != externalStatusPass || report.Totals.OIDCChecks != 1 {
		t.Fatalf("report outcome=%s totals=%+v", report.Outcome, report.Totals)
	}
	check := report.Checks[0]
	if check.ID != "oidc" || !check.Selected || check.Status != externalStatusPass || check.OIDC == nil {
		t.Fatalf("oidc check=%+v", check)
	}
	if check.OIDC.Discovery.Status != externalStatusPass || check.OIDC.JWKS.Status != externalStatusPass || check.OIDC.JWKS.KeyCount != 1 {
		t.Fatalf("oidc probes=%+v", check.OIDC)
	}
	if check.OIDC.IDToken.Status != externalStatusNotRun || check.OIDC.AuthorizationCode.Status != externalStatusNotRun {
		t.Fatalf("interactive steps must be NOT_RUN: %+v", check.OIDC)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("OIDC report leaked the client secret")
	}
}

func TestExternalAcceptOIDCIDTokenValidationPass(t *testing.T) {
	const secret = "external-oidc-client-secret-1b77"
	const tokenEnv = "NEXAROUTE_EXT_OIDC_ID_TOKEN"
	t.Setenv("NEXAROUTE_EXT_OIDC_SECRET", secret)
	fixture := newExternalOIDCFixture(t)
	t.Setenv(tokenEnv, fixture.idToken(t, "nexaroute-control-plane", time.Now().Add(5*time.Minute), []string{"nexaroute-admins"}))
	configPath := externalOIDCConfig(t, fixture.server.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	reportPath := filepath.Join(t.TempDir(), "oidc-token.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--oidc", "--oidc-id-token-env", tokenEnv,
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	check := report.Checks[0]
	if check.Status != externalStatusPass || check.OIDC.IDToken.Status != externalStatusPass {
		t.Fatalf("oidc token check=%+v", check)
	}
	if !check.OIDC.RoleMappingChecked || check.OIDC.IDToken.RoleCount != 1 {
		t.Fatalf("role mapping evidence=%+v", check.OIDC)
	}
	raw, _ := os.ReadFile(reportPath)
	if strings.Contains(string(raw), os.Getenv(tokenEnv)) {
		t.Fatal("report leaked the supplied ID token")
	}
}

func TestExternalAcceptOIDCIDTokenWrongAudienceFails(t *testing.T) {
	const secret = "external-oidc-client-secret-c4a2"
	const tokenEnv = "NEXAROUTE_EXT_OIDC_ID_TOKEN"
	t.Setenv("NEXAROUTE_EXT_OIDC_SECRET", secret)
	fixture := newExternalOIDCFixture(t)
	t.Setenv(tokenEnv, fixture.idToken(t, "another-audience", time.Now().Add(5*time.Minute), []string{"nexaroute-admins"}))
	configPath := externalOIDCConfig(t, fixture.server.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	reportPath := filepath.Join(t.TempDir(), "oidc-token.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--oidc", "--oidc-id-token-env", tokenEnv,
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1 FAIL\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	if report.Outcome != externalStatusFail || report.Checks[0].OIDC.IDToken.Status != externalStatusFail {
		t.Fatalf("report=%+v", report)
	}
}

func TestExternalAcceptOIDCDiscoveryFailureIsFail(t *testing.T) {
	fixture := newExternalOIDCFixture(t)
	fixture.discoveryStatus = http.StatusBadGateway
	configPath := externalOIDCConfig(t, fixture.server.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	reportPath := filepath.Join(t.TempDir(), "oidc-fail.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--oidc", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	if report.Outcome != externalStatusFail || report.Checks[0].OIDC.Discovery.Status != externalStatusFail {
		t.Fatalf("report=%+v", report)
	}
	if report.Checks[0].OIDC.JWKS.Status != externalStatusNotRun {
		t.Fatalf("JWKS should not run after failed discovery: %+v", report.Checks[0].OIDC)
	}
}

func TestExternalAcceptOIDCJWKSFailureIsFail(t *testing.T) {
	fixture := newExternalOIDCFixture(t)
	fixture.jwksStatus = http.StatusServiceUnavailable
	configPath := externalOIDCConfig(t, fixture.server.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	reportPath := filepath.Join(t.TempDir(), "oidc-jwks.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--oidc", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	if report.Checks[0].OIDC.JWKS.Status != externalStatusFail {
		t.Fatalf("oidc=%+v", report.Checks[0].OIDC)
	}
}

func TestExternalAcceptMissingOIDCIssuerEnvIsBlocked(t *testing.T) {
	const secret = "external-oidc-client-secret-3ab8"
	t.Setenv(secretEnvForTest, secret)
	t.Setenv("NEXAROUTE_EXT_MISSING_ISSUER", "")
	reportPath := filepath.Join(t.TempDir(), "oidc-blocked.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--oidc-issuer-env", "NEXAROUTE_EXT_MISSING_ISSUER",
		"--oidc-client-id", "staging-client", "--oidc-audience", "nexaroute-control-plane",
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit=%d want 3 BLOCKED\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	if report.Outcome != externalStatusBlocked || report.Checks[0].Status != externalStatusBlocked {
		t.Fatalf("report=%+v", report)
	}
	if !strings.Contains(report.Checks[0].Reason, "NEXAROUTE_EXT_MISSING_ISSUER") {
		t.Fatalf("blocked reason does not name the missing env var: %s", report.Checks[0].Reason)
	}
}

const secretEnvForTest = "NEXAROUTE_EXT_OIDC_SECRET"

func TestExternalAcceptOIDCEnvIssuerWithoutConfig(t *testing.T) {
	fixture := newExternalOIDCFixture(t)
	t.Setenv("NEXAROUTE_EXT_OIDC_ISSUER", fixture.server.URL)
	reportPath := filepath.Join(t.TempDir(), "oidc-env.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--oidc-issuer-env", "NEXAROUTE_EXT_OIDC_ISSUER",
		"--oidc-client-id", "staging-client", "--oidc-audience", "nexaroute-control-plane",
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	if report.Outcome != externalStatusPass || report.Checks[0].OIDC.Discovery.Status != externalStatusPass {
		t.Fatalf("report=%+v", report)
	}
}

func TestExternalAcceptOutcomePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"fail wins", []string{externalStatusPass, externalStatusBlocked, externalStatusFail}, externalStatusFail},
		{"blocked over pass", []string{externalStatusPass, externalStatusBlocked}, externalStatusBlocked},
		{"no selection", nil, externalStatusNotRun},
		{"only not run", []string{externalStatusNotRun}, externalStatusNotRun},
		{"pass", []string{externalStatusPass}, externalStatusPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := externalOutcome(tc.statuses); got != tc.want {
				t.Fatalf("externalOutcome(%v)=%s want %s", tc.statuses, got, tc.want)
			}
		})
	}
}

func TestExternalAcceptRedactorCoversSecrets(t *testing.T) {
	const secret = "external-accept-redaction-canary"
	redactor := livecanary.NewRedactor([]string{secret})
	if strings.Contains(redactor.Redact("Bearer "+secret), secret) {
		t.Fatal("redactor leaked a secret")
	}
	if got := redactor.Redact("Authorization: Bearer abcdefgh"); strings.Contains(got, "abcdefgh") {
		t.Fatalf("auth header not redacted: %q", got)
	}
}

func TestExternalAcceptSubcommandIsDispatched(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatchSubcommand([]string{"external-accept", "--confirm-live"}, nil, &stdout, &stderr)
	if !handled {
		t.Fatal("external-accept was not dispatched")
	}
	if code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "select at least one") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}
