package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// ---------------------------------------------------------------------------
// vault_accept.go: helpers.
// ---------------------------------------------------------------------------

// covcliVaultDirect starts the fake Vault and returns an HTTP client and
// its address for direct calls to the runVaultAccept* check functions.
func covcliVaultDirect(t *testing.T, fake *fakeVaultAccept) (*http.Client, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	return newVaultAcceptClient(10 * time.Second), server.URL
}

// covcliVaultStatusServer starts a server that answers every request with
// the given status and body, for the unexpected-status and malformed-body
// branches of the Vault checks.
func covcliVaultStatusServer(t *testing.T, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// covcliVaultHelperScript writes an executable helper script and returns
// its path.
func covcliVaultHelperScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// covcliVaultKey is a fixed 32-byte acceptance key for direct-call tests.
func covcliVaultKey() []byte {
	return bytes.Repeat([]byte{0x07}, 32)
}

// ---------------------------------------------------------------------------
// runVaultAccept: helper-timeout resolution and key generation.
// ---------------------------------------------------------------------------

func TestCoverageAgentAVaultHelperTimeoutEnvError(t *testing.T) {
	fake := newFakeVaultAccept()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	t.Setenv("VAULT_ADDR", server.URL)
	t.Setenv("VAULT_TOKEN", fake.primaryToken)
	t.Setenv("VAULT_HELPER_TIMEOUT", "not-a-number")
	outPath := filepath.Join(t.TempDir(), "vault-acceptance.json")
	var stdout, stderr bytes.Buffer
	code := runVaultAccept([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", vaultAcceptHelperPath(t),
		"--confirm-live",
		"--out", outPath,
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "VAULT_HELPER_TIMEOUT must be a positive integer") {
		t.Fatalf("code=%d stderr=%q, want exit 2 with the helper-timeout error", code, stderr.String())
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("report exists at %s, want no report on a flag-validation failure", outPath)
	}
}

func TestCoverageAgentAVaultHelperTimeoutClamp(t *testing.T) {
	// A non-positive --timeout with timeoutSet=true can only reach
	// vaultAcceptHelperTimeout through a direct call: runVaultAccept
	// rejects non-positive timeouts before calling it. Ceil(0)=0, so
	// the clamp to 1 is the only way a sub-second timeout still hands
	// the helper a positive integer bound.
	got, err := vaultAcceptHelperTimeout(0, true)
	if err != nil || got != 1 {
		t.Fatalf("vaultAcceptHelperTimeout(0, true)=(%d, %v), want (1, nil)", got, err)
	}
	if got, err := vaultAcceptHelperTimeout(1500*time.Millisecond, true); err != nil || got != 2 {
		t.Fatalf("vaultAcceptHelperTimeout(1.5s, true)=(%d, %v), want (2, nil)", got, err)
	}
}

func TestCoverageAgentAVaultKeyGenRandError(t *testing.T) {
	fake := newFakeVaultAccept()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	t.Setenv("VAULT_ADDR", server.URL)
	t.Setenv("VAULT_TOKEN", fake.primaryToken)
	old := vaultAcceptRandRead
	vaultAcceptRandRead = func([]byte) (int, error) { return 0, errors.New("rand boom") }
	t.Cleanup(func() { vaultAcceptRandRead = old })
	outPath := filepath.Join(t.TempDir(), "vault-acceptance.json")
	var stdout, stderr bytes.Buffer
	code := runVaultAccept([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", vaultAcceptHelperPath(t),
		"--confirm-live",
		"--out", outPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d, want 1 (fail outcome)\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readVaultAcceptReport(t, outPath)
	if got := vaultAcceptStatus(report, "vault.kv_put_get"); got != externalStatusFail {
		t.Fatalf("kv_put_get=%q, want fail", got)
	}
	for _, check := range report.Checks {
		if check.ID == "vault.kv_put_get" && !strings.Contains(check.Reason, "cannot generate acceptance key") {
			t.Fatalf("kv_put_get reason=%q, want the key-generation error", check.Reason)
		}
	}
}

// ---------------------------------------------------------------------------
// runVaultAcceptDecrypt: envelope round-trip controls.
// ---------------------------------------------------------------------------

func TestCoverageAgentAVaultDecryptError(t *testing.T) {
	old := vaultAcceptTransformConfig
	vaultAcceptTransformConfig = func(raw, key []byte, encrypt bool) ([]byte, int, error) {
		if !encrypt {
			return nil, 0, errors.New("decrypt boom")
		}
		return secrets.TransformConfig(raw, key, encrypt)
	}
	t.Cleanup(func() { vaultAcceptTransformConfig = old })
	check := runVaultAcceptDecrypt(covcliVaultKey())
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot decrypt an acceptance config field") {
		t.Fatalf("check=%+v, want the decrypt failure", check)
	}
}

func TestCoverageAgentAVaultDecryptMismatch(t *testing.T) {
	old := vaultAcceptTransformConfig
	vaultAcceptTransformConfig = func(raw, key []byte, encrypt bool) ([]byte, int, error) {
		if !encrypt {
			return []byte(`{"providers":[{"id":"acceptance","type":"openai_compatible","api_key":"different"}]}`), 1, nil
		}
		return secrets.TransformConfig(raw, key, encrypt)
	}
	t.Cleanup(func() { vaultAcceptTransformConfig = old })
	check := runVaultAcceptDecrypt(covcliVaultKey())
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "did not match the original plaintext") {
		t.Fatalf("check=%+v, want the plaintext-mismatch failure", check)
	}
}

func TestCoverageAgentAVaultDecryptRandError(t *testing.T) {
	old := vaultAcceptRandRead
	vaultAcceptRandRead = func([]byte) (int, error) { return 0, errors.New("rand boom") }
	t.Cleanup(func() { vaultAcceptRandRead = old })
	check := runVaultAcceptDecrypt(covcliVaultKey())
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot generate a wrong-key control") {
		t.Fatalf("check=%+v, want the wrong-key RNG failure", check)
	}
}

func TestCoverageAgentAVaultDecryptWrongKeySucceeds(t *testing.T) {
	key := covcliVaultKey()
	old := vaultAcceptTransformConfig
	vaultAcceptTransformConfig = func(raw, k []byte, encrypt bool) ([]byte, int, error) {
		if !encrypt && !bytes.Equal(k, key) {
			// Wrong-key control: pretend the wrong key decrypted the
			// envelope, so the fail-closed check must fire.
			return []byte(`{"providers":[{"id":"acceptance","type":"openai_compatible","api_key":"acceptance-plaintext"}]}`), 1, nil
		}
		return secrets.TransformConfig(raw, k, encrypt)
	}
	t.Cleanup(func() { vaultAcceptTransformConfig = old })
	check := runVaultAcceptDecrypt(key)
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "wrong-key fail-closed is broken") {
		t.Fatalf("check=%+v, want the wrong-key fail-closed failure", check)
	}
}

// ---------------------------------------------------------------------------
// runVaultAcceptRotation: replacement-key generation.
// ---------------------------------------------------------------------------

func TestCoverageAgentAVaultRotationRandError(t *testing.T) {
	old := vaultAcceptRandRead
	vaultAcceptRandRead = func([]byte) (int, error) { return 0, errors.New("rand boom") }
	t.Cleanup(func() { vaultAcceptRandRead = old })
	check := runVaultAcceptRotation(context.Background(), "/nonexistent-helper", "secret/data/x", "http://127.0.0.1:1", "token", 30)
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot generate a replacement acceptance key") {
		t.Fatalf("check=%+v, want the replacement-key RNG failure", check)
	}
}

// ---------------------------------------------------------------------------
// runVaultAcceptKVVersion.
// ---------------------------------------------------------------------------

func TestCoverageAgentAVaultKVVersionNoDataRef(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	check := runVaultAcceptKVVersion(context.Background(), client, "http://127.0.0.1:1", "token", "secret/foo")
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "cannot derive the KV metadata path") {
		t.Fatalf("check=%+v, want the no-/data/ blocked result", check)
	}
}

func TestCoverageAgentAVaultKVVersionHTTPError(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	check := runVaultAcceptKVVersion(ctx, client, "http://127.0.0.1:1", "token", "secret/data/nexaroute/acceptance/master-key")
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "cannot read the KV metadata path") {
		t.Fatalf("check=%+v, want the metadata-read blocked result", check)
	}
}

func TestCoverageAgentAVaultKVVersionUnexpectedStatus(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	addr := covcliVaultStatusServer(t, http.StatusInternalServerError, `{"errors":["boom"]}`)
	check := runVaultAcceptKVVersion(context.Background(), client, addr, "token", "secret/data/nexaroute/acceptance/master-key")
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "KV metadata probe returned HTTP 500") {
		t.Fatalf("check=%+v, want the unexpected-status blocked result", check)
	}
}

// ---------------------------------------------------------------------------
// runVaultAcceptPolicyAssertion.
// ---------------------------------------------------------------------------

func TestCoverageAgentAVaultPolicyAssertionRoot(t *testing.T) {
	report := &vaultAcceptReport{Vault: vaultAcceptVault{Policies: []string{"root", "default"}}}
	check := runVaultAcceptPolicyAssertion([]string{"nexaroute-acceptance"}, report)
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "token carries the root policy") {
		t.Fatalf("check=%+v, want the root-policy failure", check)
	}
}

// ---------------------------------------------------------------------------
// runVaultAcceptMountVersion.
// ---------------------------------------------------------------------------

func TestCoverageAgentAVaultMountVersionNoDataRef(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	check := runVaultAcceptMountVersion(context.Background(), client, "http://127.0.0.1:1", "token", "secret/foo")
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot derive the Vault mount") {
		t.Fatalf("check=%+v, want the no-/data/ failure", check)
	}
}

func TestCoverageAgentAVaultMountVersionHTTPError(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	check := runVaultAcceptMountVersion(ctx, client, "http://127.0.0.1:1", "token", "secret/data/nexaroute/acceptance/master-key")
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "cannot read the Vault mount") {
		t.Fatalf("check=%+v, want the mount-read failure", check)
	}
}

func TestCoverageAgentAVaultMountVersionUnexpectedStatus(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	addr := covcliVaultStatusServer(t, http.StatusInternalServerError, `{"errors":["boom"]}`)
	check := runVaultAcceptMountVersion(context.Background(), client, addr, "token", "secret/data/nexaroute/acceptance/master-key")
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "mount lookup returned HTTP 500") {
		t.Fatalf("check=%+v, want the unexpected-status failure", check)
	}
}

func TestCoverageAgentAVaultMountVersionBadJSON(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	addr := covcliVaultStatusServer(t, http.StatusOK, `not json`)
	check := runVaultAcceptMountVersion(context.Background(), client, addr, "token", "secret/data/nexaroute/acceptance/master-key")
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "mount lookup response is not valid mount JSON") {
		t.Fatalf("check=%+v, want the malformed-mount-JSON failure", check)
	}
}

func TestCoverageAgentAVaultMountVersionTopLevelOptions(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	addr := covcliVaultStatusServer(t, http.StatusOK, `{"type":"kv","options":{"version":"2"}}`)
	check := runVaultAcceptMountVersion(context.Background(), client, addr, "token", "secret/data/nexaroute/acceptance/master-key")
	if check.Status != externalStatusPass || !strings.Contains(check.Detail, "mount=secret type=kv version=2") {
		t.Fatalf("check=%+v, want the top-level-options pass", check)
	}
}

// ---------------------------------------------------------------------------
// runVaultAcceptExpiredToken.
// ---------------------------------------------------------------------------

func TestCoverageAgentAVaultExpiredTokenCreateError(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	helper := covcliVaultHelperScript(t, "#!/bin/sh\nexit 0\n")
	check := runVaultAcceptExpiredToken(ctx, client, helper, "secret/data/nexaroute/acceptance/master-key", "http://127.0.0.1:1", "token", 30)
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "cannot create a disposable short-lived child token") {
		t.Fatalf("check=%+v, want the child-creation blocked result", check)
	}
}

func TestCoverageAgentAVaultExpiredTokenCreateStatus(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	addr := covcliVaultStatusServer(t, http.StatusInternalServerError, `{"errors":["boom"]}`)
	helper := covcliVaultHelperScript(t, "#!/bin/sh\nexit 0\n")
	check := runVaultAcceptExpiredToken(context.Background(), client, helper, "secret/data/nexaroute/acceptance/master-key", addr, "token", 30)
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "child token creation returned HTTP 500") {
		t.Fatalf("check=%+v, want the child-creation unexpected-status result", check)
	}
}

func TestCoverageAgentAVaultExpiredTokenNoToken(t *testing.T) {
	client := newVaultAcceptClient(10 * time.Second)
	addr := covcliVaultStatusServer(t, http.StatusOK, `{}`)
	helper := covcliVaultHelperScript(t, "#!/bin/sh\nexit 0\n")
	check := runVaultAcceptExpiredToken(context.Background(), client, helper, "secret/data/nexaroute/acceptance/master-key", addr, "token", 30)
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "did not contain a client token") {
		t.Fatalf("check=%+v, want the missing-client-token failure", check)
	}
}

func TestCoverageAgentAVaultExpiredTokenStillAccepted(t *testing.T) {
	fake := newFakeVaultAccept()
	// No child TTL: the child never expires, so the 10s poll deadline
	// is hit and the check must fail (best-effort revoke included).
	client, addr := covcliVaultDirect(t, fake)
	helper := covcliVaultHelperScript(t, "#!/bin/sh\nexit 0\n")
	start := time.Now()
	check := runVaultAcceptExpiredToken(context.Background(), client, helper, "secret/data/nexaroute/acceptance/master-key", addr, fake.primaryToken, 30)
	if elapsed := time.Since(start); elapsed < 9*time.Second {
		t.Fatalf("expired-token check took %v, want the ~10s poll deadline", elapsed)
	}
	if check.Status != externalStatusFail || !strings.Contains(check.Reason, "still accepted after the expiry deadline") {
		t.Fatalf("check=%+v, want the still-accepted failure", check)
	}
}

func TestCoverageAgentAVaultExpiredTokenCtxDone(t *testing.T) {
	fake := newFakeVaultAccept()
	client, addr := covcliVaultDirect(t, fake)
	helper := covcliVaultHelperScript(t, "#!/bin/sh\nexit 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	check := runVaultAcceptExpiredToken(ctx, client, helper, "secret/data/nexaroute/acceptance/master-key", addr, fake.primaryToken, 30)
	if check.Status != externalStatusBlocked || !strings.Contains(check.Reason, "expired-token check aborted by the overall run timeout") {
		t.Fatalf("check=%+v, want the aborted-by-timeout blocked result", check)
	}
}

// ---------------------------------------------------------------------------
// writeVaultAcceptReport.
// ---------------------------------------------------------------------------

func TestCoverageAgentAWriteVaultAcceptReportMarshalError(t *testing.T) {
	// vaultAcceptBounds carries float64 timeouts, so a NaN bound makes
	// MarshalIndent fail. In production the bounds come from validated
	// time.Durations (always finite); this pins the writer's
	// fail-closed behavior for a hand-built report.
	report := vaultAcceptReport{Schema: vaultAcceptSchema, Bounds: vaultAcceptBounds{CallTimeoutSeconds: math.NaN()}}
	path := filepath.Join(t.TempDir(), "vault-acceptance.json")
	if err := writeVaultAcceptReport(path, report); err == nil || !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("writeVaultAcceptReport err=%v, want the NaN marshal error", err)
	}
}

func TestCoverageAgentAWriteVaultAcceptReportChmodError(t *testing.T) {
	old := vaultAcceptReportTempChmod
	vaultAcceptReportTempChmod = func(*os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { vaultAcceptReportTempChmod = old })
	path := filepath.Join(t.TempDir(), "vault-acceptance.json")
	if err := writeVaultAcceptReport(path, vaultAcceptReport{Schema: vaultAcceptSchema}); err == nil || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("writeVaultAcceptReport err=%v, want the injected chmod error", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report exists at %s, want no report file on chmod failure", path)
	}
}

func TestCoverageAgentAWriteVaultAcceptReportWriteError(t *testing.T) {
	limitFileSize(t, 10)
	path := filepath.Join(t.TempDir(), "vault-acceptance.json")
	if err := writeVaultAcceptReport(path, vaultAcceptReport{Schema: vaultAcceptSchema}); err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("writeVaultAcceptReport err=%v, want the file-size write error", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report exists at %s, want no report file on write failure", path)
	}
}

func TestCoverageAgentAWriteVaultAcceptReportCloseError(t *testing.T) {
	old := vaultAcceptReportTempClose
	vaultAcceptReportTempClose = func(*os.File) error { return errors.New("close boom") }
	t.Cleanup(func() { vaultAcceptReportTempClose = old })
	path := filepath.Join(t.TempDir(), "vault-acceptance.json")
	if err := writeVaultAcceptReport(path, vaultAcceptReport{Schema: vaultAcceptSchema}); err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("writeVaultAcceptReport err=%v, want the injected close error", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report exists at %s, want no report file on close failure", path)
	}
}
