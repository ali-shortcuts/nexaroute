// Coverage tests for the acceptance commands (Agent A, 100% mission):
// provider_accept.go, external_accept.go and restore_verify.go.
// Error branches with no deterministic filesystem trigger are
// exercised through the seams declared at the top of each command
// file; cancellation and malformed-input paths are driven through
// the real commands.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
	jose "github.com/go-jose/go-jose/v4"
)

// --- provider_accept.go ---

func TestCoverageAgentAWriteProviderAcceptReportChmodError(t *testing.T) {
	old := providerAcceptTempChmod
	providerAcceptTempChmod = func(f *os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { providerAcceptTempChmod = old })
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeProviderAcceptReport(path, providerAcceptReport{}); err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("expected a chmod error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report file must not exist after a chmod failure, stat err=%v", err)
	}
}

func TestCoverageAgentAWriteProviderAcceptReportSyncError(t *testing.T) {
	old := providerAcceptTempSync
	providerAcceptTempSync = func(f *os.File) error { return errors.New("sync boom") }
	t.Cleanup(func() { providerAcceptTempSync = old })
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeProviderAcceptReport(path, providerAcceptReport{}); err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("expected a sync error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report file must not exist after a sync failure, stat err=%v", err)
	}
}

func TestCoverageAgentAWriteProviderAcceptReportCloseError(t *testing.T) {
	old := providerAcceptTempClose
	providerAcceptTempClose = func(f *os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { providerAcceptTempClose = old })
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeProviderAcceptReport(path, providerAcceptReport{}); err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("expected a close error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report file must not exist after a close failure, stat err=%v", err)
	}
}

func TestCoverageAgentAWriteProviderAcceptReportWriteError(t *testing.T) {
	limitFileSize(t, 10)
	path := filepath.Join(t.TempDir(), "report.json")
	err := writeProviderAcceptReport(path, providerAcceptReport{Schema: providerAcceptSchema, Mode: "test"})
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected a file-too-large write error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report file must not exist after a write failure, stat err=%v", err)
	}
}

func TestCoverageAgentAPrintProviderAcceptSummaryLatency(t *testing.T) {
	report := providerAcceptReport{
		Mode: "test",
		Totals: providerAcceptTotals{
			Providers: 1,
		},
		Providers: []providerAcceptResult{{
			Provider: "p1", Protocol: "openai_compatible", AuthMode: "bearer",
			CredentialSource: "env", Status: "passed",
			Discovery: providerDiscovery{Supported: true, Attempted: true, OK: true, StatusCode: 200, ModelCount: 2},
			Models: []providerModelResult{
				{Model: "fast", Source: config.ModelSourceManual, Result: "passed", StatusCode: 200, LatencyMS: 42},
				{Model: "slow", Source: config.ModelSourceManual, Result: "failed", Error: "boom"},
			},
		}},
	}
	var out bytes.Buffer
	printProviderAcceptSummary(&out, report, "report.json")
	if !strings.Contains(out.String(), "model fast [manual, ] -> passed HTTP 200 42ms") {
		t.Fatalf("expected the latency suffix on the fast model line, got:\n%s", out.String())
	}
	// The failed model has no latency and no status code, so its line
	// carries neither suffix.
	if !strings.Contains(out.String(), "model slow [manual, ] -> failed error=boom") {
		t.Fatalf("expected the plain failed model line, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "model slow [manual, ] -> failed HTTP") {
		t.Fatalf("a model without a status code must not print one:\n%s", out.String())
	}
}

func TestCoverageAgentAProviderAcceptCanceled(t *testing.T) {
	// The upstream blocks the first probe until the request context
	// is cancelled; the handler itself sends SIGTERM to this process
	// shortly after the first request arrives, while
	// runProviderAccept's signal.NotifyContext is registered. The
	// cancelled context makes the loop warn and stop before the
	// second provider is processed.
	const secret = "accept-secret-canary-covcli"
	t.Setenv("NEXAROUTE_TEST_ACCEPT_KEY", secret)
	var sent atomic.Bool
	// The handler blocks until the test releases it. A handler
	// blocked on r.Context().Done() alone never unblocks: when the
	// client cancels, client.Do returns promptly, but the server
	// does not reliably cancel the request context while the
	// handler is still running, and httptest.Server.Close would
	// then block forever in cleanup. The test releases the handler
	// after runProviderAccept returns.
	// Release the blocked handler when the test returns (deferred
	// closes run before t.Cleanup's up.Close); otherwise
	// httptest.Server.Close would wait on it.
	handlerRelease := make(chan struct{})
	defer close(handlerRelease)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sent.CompareAndSwap(false, true) {
			go func() {
				time.Sleep(100 * time.Millisecond)
				_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
			}()
		}
		select {
		case <-handlerRelease:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	providers := []config.ProviderConfig{
		{ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: up.URL + "/v1", APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}}},
		{ID: "p2", Name: "P2", Type: "openai_compatible", BaseURL: up.URL + "/v1", APIKeyEnv: "NEXAROUTE_TEST_ACCEPT_KEY", AuthMode: "bearer", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Source: config.ModelSourceManual}}},
	}
	configPath := acceptanceConfig(t, providers...)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runProviderAccept([]string{
		"--config", configPath,
		"--provider", "p1", "--provider", "p2",
		"--confirm-live", "--discover=false", "--test=true",
		"--timeout", "30s", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 (report not ok), got %d (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report providerAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Providers) != 1 {
		t.Fatalf("expected exactly 1 provider processed before the cancel, got %d", len(report.Providers))
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "canceled before all providers were processed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the cancellation warning, got warnings=%v", report.Warnings)
	}
}

// --- external_accept.go ---

func TestCoverageAgentAWriteExternalAcceptReportChmodError(t *testing.T) {
	old := externalAcceptTempChmod
	externalAcceptTempChmod = func(f *os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { externalAcceptTempChmod = old })
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeExternalAcceptReport(path, externalAcceptReport{}); err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("expected a chmod error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report file must not exist after a chmod failure, stat err=%v", err)
	}
}

func TestCoverageAgentAWriteExternalAcceptReportCloseError(t *testing.T) {
	old := externalAcceptTempClose
	externalAcceptTempClose = func(f *os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { externalAcceptTempClose = old })
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeExternalAcceptReport(path, externalAcceptReport{}); err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("expected a close error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report file must not exist after a close failure, stat err=%v", err)
	}
}

func TestCoverageAgentAWriteExternalAcceptReportWriteError(t *testing.T) {
	limitFileSize(t, 10)
	path := filepath.Join(t.TempDir(), "report.json")
	err := writeExternalAcceptReport(path, externalAcceptReport{Schema: externalAcceptSchema, Mode: "oidc"})
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected a file-too-large write error, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report file must not exist after a write failure, stat err=%v", err)
	}
}

func TestCoverageAgentAExternalOIDCJWKSRequestError(t *testing.T) {
	// The discovery document's jwks_uri carries a leading space and
	// an https scheme: validAcceptanceOIDCEndpoint trims before
	// parsing, and the https branch returns true without ever
	// parsing the untrimmed URL, so the endpoint check passes. But
	// fetchOIDCJWKS hands the untrimmed URL to
	// http.NewRequestWithContext, which rejects it ("first path
	// segment in URL cannot contain colon"). The JWKS check must
	// fail with that construction error, not a network error.
	// (An http jwks_uri would not work: the loopback check parses
	// the untrimmed URL and would reject the padding first.)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeExternalTestJSON(w, map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/authorize",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              " https://127.0.0.1:1/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	configPath := externalOIDCConfig(t, srv.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	reportPath := filepath.Join(t.TempDir(), "oidc-jwks-req.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--oidc", "--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1 FAIL\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	if report.Checks[0].OIDC.JWKS.Status != externalStatusFail {
		t.Fatalf("JWKS check=%+v, want fail", report.Checks[0].OIDC.JWKS)
	}
	if !strings.Contains(report.Checks[0].OIDC.JWKS.Detail, "jwks request could not be created") {
		t.Fatalf("JWKS detail=%q, want the request-construction error", report.Checks[0].OIDC.JWKS.Detail)
	}
}

// externalMintIDToken signs an ID token with the fixture's key, but
// with a caller-chosen subject, so the subject-validation branch can
// be exercised (the fixture's idToken always uses a fixed subject).
func externalMintIDToken(t *testing.T, f *externalOIDCFixture, subject string) string {
	t.Helper()
	claims := map[string]any{
		"iss": f.server.URL, "sub": subject, "aud": "nexaroute-control-plane",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
		"groups": []string{"nexaroute-admins"},
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

func TestCoverageAgentAExternalOIDCEmptySubjectFails(t *testing.T) {
	// go-oidc's Verify checks signature, issuer, audience and
	// expiry, but NOT that the subject is non-empty — so a token
	// with an empty "sub" passes Verify and must be rejected by
	// verifyAcceptanceIDToken's own issuer/subject check.
	const secret = "external-oidc-client-secret-covcli"
	const tokenEnv = "NEXAROUTE_EXT_OIDC_ID_TOKEN"
	t.Setenv("NEXAROUTE_EXT_OIDC_SECRET", secret)
	fixture := newExternalOIDCFixture(t)
	t.Setenv(tokenEnv, externalMintIDToken(t, fixture, ""))
	configPath := externalOIDCConfig(t, fixture.server.URL, "NEXAROUTE_EXT_OIDC_SECRET")
	reportPath := filepath.Join(t.TempDir(), "oidc-empty-sub.json")
	var stdout, stderr bytes.Buffer
	code := runExternalAccept([]string{
		"--config", configPath, "--oidc", "--oidc-id-token-env", tokenEnv,
		"--confirm-live", "--out", reportPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1 FAIL\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readExternalReport(t, reportPath)
	idToken := report.Checks[0].OIDC.IDToken
	if idToken.Status != externalStatusFail {
		t.Fatalf("ID token check=%+v, want fail", idToken)
	}
	if !strings.Contains(idToken.Detail, "oidc ID token issuer or subject is invalid") {
		t.Fatalf("ID token detail=%q, want the issuer/subject error", idToken.Detail)
	}
}

// --- restore_verify.go ---

func TestCoverageAgentASha256FileDirectory(t *testing.T) {
	// os.Open succeeds on a directory, Stat succeeds, but the
	// streaming read fails EISDIR — the only deterministic trigger
	// for the io.Copy error in sha256File.
	_, _, err := sha256File(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("expected an is-a-directory read error, got %v", err)
	}
}

func TestCoverageAgentACheckBundledAuditChainVerifyError(t *testing.T) {
	// A pre-cancelled context makes VerifyAuditChain fail on a
	// store that opens fine, proving the verification-error branch
	// (the 30s timeout cannot fire deterministically on a
	// just-opened store).
	storeDir := t.TempDir()
	if err := os.Chmod(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(storeDir, "security.db")
	store, err := securitystore.Open(storePath, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	old := bundledAuditContext
	bundledAuditContext = func() (context.Context, context.CancelFunc) {
		c, cn := context.WithCancel(context.Background())
		cn()
		return c, cn
	}
	t.Cleanup(func() { bundledAuditContext = old })
	check := checkBundledAuditChain(map[string][]byte{backupEntrySecurity: payload})
	if check.Status != statusFail {
		t.Fatalf("check=%+v, want FAIL", check)
	}
	if !strings.Contains(check.Detail, "audit-chain verification errored") {
		t.Fatalf("detail=%q, want the verification-error detail", check.Detail)
	}
}

func TestCoverageAgentAPrepareDisposableDirReadDirError(t *testing.T) {
	old := disposableReadDir
	disposableReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("readdir boom") }
	t.Cleanup(func() { disposableReadDir = old })
	dir := filepath.Join(t.TempDir(), "into")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := prepareDisposableDir(dir)
	if err == nil || !strings.Contains(err.Error(), "inspect --into directory") || !strings.Contains(err.Error(), "readdir boom") {
		t.Fatalf("expected an inspect---into error wrapping the readdir failure, got %v", err)
	}
}

func TestCoverageAgentAMaterializeBundleReadBackError(t *testing.T) {
	old := materializeReadFile
	materializeReadFile = func(string) ([]byte, error) { return nil, errors.New("readback boom") }
	t.Cleanup(func() { materializeReadFile = old })
	dir := t.TempDir()
	err := materializeBundle(dir, map[string][]byte{backupEntryConfig: []byte("{}")})
	if err == nil || !strings.Contains(err.Error(), "materialized config.json failed hash verification") {
		t.Fatalf("expected a hash-verification error, got %v", err)
	}
}

func TestCoverageAgentARestoreVerifyFileSizeLimit(t *testing.T) {
	// With a 10-byte file-size limit, every staging write in the
	// verify flow fails: the audit-chain store cannot be staged
	// (336-339) and the bundle cannot be materialized (158-159).
	// The flow still runs to the end and reports both as FAIL.
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		t.Setenv(env, "")
	}
	_, configPath := fullDrillFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr)
	}
	into := filepath.Join(t.TempDir(), "restored")
	limitFileSize(t, 10)
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{
		"--bundle", bundle, "--config", configPath, "--verify", "--into", into,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("verify exit=%d want 1 FAIL\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "FAIL audit_chain: cannot stage the bundled security store for verification") {
		t.Fatalf("expected the audit-chain staging failure, got:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "FAIL isolated_materialization") || !strings.Contains(stdout.String(), "materialize config.json") {
		t.Fatalf("expected the materialization failure, got:\n%s", stdout.String())
	}
}
