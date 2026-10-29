package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// wave3CraftTar writes a tar file from header/body pairs. A body
// shorter than its declared size stays short on read: Go's tar
// writer does not pad a short final entry, so the reader reports
// unexpected EOF for it (see the truncated-entry test below).
func wave3CraftTar(t *testing.T, entries []struct {
	name string
	size int64
	body []byte
}) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crafted.tar")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(fh)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: e.size}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	// A short final entry makes Close report "missed writing N
	// bytes", but the bytes on disk are still a readable tar whose
	// short entry fails on read with unexpected EOF (verified by
	// probe); the Close error is therefore not fatal for fixtures.
	_ = tw.Close()
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWave3CreateBackupMissingOutputDir(t *testing.T) {
	_, configPath := opsFixture(t)
	out := filepath.Join(t.TempDir(), "missing-dir", "b.tar")
	code, stderr := runBackupCode(t, configPath, out)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "backup failed") || !strings.Contains(stderr, "no such file") {
		t.Fatalf("stderr=%q, want 'backup failed' + 'no such file' (the write stage, after output-collision checks pass)", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written into a missing directory")
	}
}

func TestWave3RunRestoreVerifyUnreadableBundle(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--verify", "--bundle", filepath.Join(t.TempDir(), "missing.tar")}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "FAIL bundle_readable") {
		t.Fatalf("stdout=%q, want 'FAIL bundle_readable'", stdout.String())
	}
}

func TestWave3RunRestoreVerifyReportWriteFailure(t *testing.T) {
	_, configPath := opsFixture(t)
	bundle := filepath.Join(t.TempDir(), "b.tar")
	if code, stderr := runBackupCode(t, configPath, bundle); code != 0 {
		t.Fatalf("backup exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--verify", "--bundle", bundle, "--report", filepath.Join(blocker, "report.json")}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (a report-write failure must not change the verification outcome) (stdout=%s stderr=%s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot write report") {
		t.Fatalf("stderr=%q, want 'cannot write report'", stderr.String())
	}
}

func TestWave3AvailableMasterKeyBytes(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x66
	}
	t.Run("bundled", func(t *testing.T) {
		got, source := availableMasterKeyBytes(map[string][]byte{backupEntryKey: key}, filepath.Join(t.TempDir(), "config.json"))
		if !bytes.Equal(got, key) || source != "bundled" {
			t.Fatalf("got=%x source=%q, want key + bundled", got, source)
		}
	})
	t.Run("environment variable", func(t *testing.T) {
		t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
		t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
		got, source := availableMasterKeyBytes(nil, filepath.Join(t.TempDir(), "config.json"))
		if !bytes.Equal(got, key) || source != "environment:NEXAROUTE_MASTER_KEY" {
			t.Fatalf("got=%x source=%q, want key + environment:NEXAROUTE_MASTER_KEY", got, source)
		}
	})
	t.Run("environment file", func(t *testing.T) {
		t.Setenv("NEXAROUTE_MASTER_KEY", "")
		keyFile := filepath.Join(t.TempDir(), "key.bin")
		if err := os.WriteFile(keyFile, key, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("NEXAROUTE_MASTER_KEY_FILE", keyFile)
		got, source := availableMasterKeyBytes(nil, filepath.Join(t.TempDir(), "config.json"))
		if !bytes.Equal(got, key) || source != "environment:NEXAROUTE_MASTER_KEY_FILE" {
			t.Fatalf("got=%x source=%q, want key + environment:NEXAROUTE_MASTER_KEY_FILE", got, source)
		}
	})
	t.Run("host file", func(t *testing.T) {
		t.Setenv("NEXAROUTE_MASTER_KEY", "")
		t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath+".key", key, 0o600); err != nil {
			t.Fatal(err)
		}
		got, source := availableMasterKeyBytes(nil, cfgPath)
		if !bytes.Equal(got, key) || source != "host:config.json.key" {
			t.Fatalf("got=%x source=%q, want key + host:config.json.key", got, source)
		}
	})
	t.Run("none available", func(t *testing.T) {
		t.Setenv("NEXAROUTE_MASTER_KEY", "")
		t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
		got, source := availableMasterKeyBytes(nil, filepath.Join(t.TempDir(), "config.json"))
		if len(got) != 0 || source != "" {
			t.Fatalf("got=%x source=%q, want empty", got, source)
		}
	})
}

func TestWave3ReadFileBoundedLimits(t *testing.T) {
	t.Run("oversize at stat", func(t *testing.T) {
		wave3SetLimits(t, 10, 1<<30)
		path := filepath.Join(t.TempDir(), "big.bin")
		if err := os.WriteFile(path, make([]byte, 20), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readFileBounded(path, "test file")
		if err == nil || !strings.Contains(err.Error(), "exceeding the 10-byte bundle entry limit") {
			t.Fatalf("err=%v, want the entry-limit error", err)
		}
	})
	t.Run("growth while reading", func(t *testing.T) {
		// /proc/version stats at size 0 but yields content on read,
		// so a tiny limit deterministically trips the re-check.
		wave3SetLimits(t, 10, 1<<30)
		_, err := readFileBounded("/proc/version", "test file")
		if err == nil || !strings.Contains(err.Error(), "grew past the 10-byte bundle entry limit") {
			t.Fatalf("err=%v, want the grew-past-limit error", err)
		}
	})
}

func TestWave3FetchOIDCJWKSLoopbackForms(t *testing.T) {
	// All three URLs pass endpoint validation (https, or loopback
	// HTTP) and then fail at the network step, proving validation
	// passed: a validation failure would say "not an absolute HTTPS
	// URL" instead.
	t.Run("https passes validation", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		_, err := fetchOIDCJWKS(context.Background(), srv.URL+"/jwks", time.Second)
		if err == nil || !strings.Contains(err.Error(), "JWKS request failed") {
			t.Fatalf("err=%v, want 'JWKS request failed'", err)
		}
	})
	t.Run("localhost passes validation", func(t *testing.T) {
		_, err := fetchOIDCJWKS(context.Background(), "http://localhost:1/jwks", time.Second)
		if err == nil || !strings.Contains(err.Error(), "JWKS request failed") {
			t.Fatalf("err=%v, want 'JWKS request failed'", err)
		}
	})
	t.Run("ipv6 loopback passes validation", func(t *testing.T) {
		_, err := fetchOIDCJWKS(context.Background(), "http://[::1]:1/jwks", time.Second)
		if err == nil || !strings.Contains(err.Error(), "JWKS request failed") {
			t.Fatalf("err=%v, want 'JWKS request failed'", err)
		}
	})
}

func TestWave3VerifyAcceptanceIDTokenClaimForms(t *testing.T) {
	fixture := newExternalOIDCFixture(t)
	issuer := fixture.server.URL
	jwksURL := issuer + "/jwks"
	now := time.Now().Unix()
	cfg := config.OIDCConfig{
		IssuerURL: issuer, ClientID: "staging-client", Audience: "staging-client",
		RoleClaim: "groups", RoleMappings: map[string]string{"nexaroute-admins": "admin"},
	}
	t.Run("single string role claim", func(t *testing.T) {
		token := wave3SignIDToken(t, fixture.key, map[string]any{
			"iss": issuer, "aud": "staging-client", "sub": "user-1",
			"iat": now - 60, "exp": now + 3600, "groups": "nexaroute-admins",
		})
		roleCount, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, cfg, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if roleCount != 1 {
			t.Fatalf("roleCount=%d, want 1", roleCount)
		}
	})
	t.Run("absent role claim", func(t *testing.T) {
		token := wave3SignIDToken(t, fixture.key, map[string]any{
			"iss": issuer, "aud": "staging-client", "sub": "user-1",
			"iat": now - 60, "exp": now + 3600,
		})
		_, err := verifyAcceptanceIDToken(context.Background(), token, issuer, jwksURL, cfg, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "no allowlisted role") {
			t.Fatalf("err=%v, want 'no allowlisted role'", err)
		}
	})
}

func TestWave3AcceptProviderModelMetadata(t *testing.T) {
	srv := wave3NoAuthUpstream(t)
	p := wave3OpenAIProvider("demo", srv, []config.ModelConfig{{
		ID: "m1", Model: "m1", Enabled: true, Weight: 1,
		Source: config.ModelSourceDiscovered, TestState: config.ModelTestPassed,
	}})
	calls := 0
	result := acceptProvider(context.Background(), p, acceptOptions{discover: false, test: true, timeout: time.Second, maxModels: 10, maxCalls: 20, calls: &calls})
	if len(result.Models) != 1 {
		t.Fatalf("models=%v, want one entry", result.Models)
	}
	if result.Models[0].Source != config.ModelSourceDiscovered {
		t.Fatalf("source=%q, want discovered", result.Models[0].Source)
	}
	if result.Models[0].PreviousTestState != config.ModelTestPassed {
		t.Fatalf("previousTestState=%q, want passed", result.Models[0].PreviousTestState)
	}
	if result.Models[0].Result != "passed" {
		t.Fatalf("result=%+v, want passed", result.Models[0])
	}
}

func TestWave3SanitizeAcceptanceText(t *testing.T) {
	// The adapter caps upstream error text below the redaction
	// limit, so the truncation branch is tested directly against
	// the helper rather than through a probe.
	long := strings.Repeat("x", 2000)
	got := sanitizeAcceptanceText(long, nil)
	runes := []rune(got)
	if len(runes) != providerAcceptMaxErrorRune+1 || runes[len(runes)-1] != '…' {
		t.Fatalf("truncated length=%d runes, want %d + ellipsis", len(runes), providerAcceptMaxErrorRune)
	}
	redacted := sanitizeAcceptanceText("key wave3-secret-value here", []string{"wave3-secret-value"})
	if strings.Contains(redacted, "wave3-secret-value") || !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("redacted=%q, want the secret replaced", redacted)
	}
	flat := sanitizeAcceptanceText("line one\nline two", nil)
	if flat != "line one line two" {
		t.Fatalf("flat=%q, want newlines flattened to spaces", flat)
	}
}

func TestWave3AcceptanceSecrets(t *testing.T) {
	t.Setenv("WAVE3_SECRET_KEY_ENV", "wave3-secret-env-value")
	t.Setenv("WAVE3_SESSION_ENV", "wave3-session-value")
	p := config.ProviderConfig{
		ID: "demo", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1",
		APIKey: "wave3-api-key", AuthMode: "bearer",
		Headers:         map[string]string{"X-Custom": "wave3-header-value"},
		SecretKeyEnv:    "WAVE3_SECRET_KEY_ENV",
		SessionTokenEnv: "WAVE3_SESSION_ENV",
		ProxyURL:        "http://wave3-proxy-user:wave3-proxy-pass@127.0.0.1:9",
	}
	secrets := acceptanceSecrets(p)
	for _, want := range []string{"wave3-api-key", "wave3-header-value", "wave3-secret-env-value", "wave3-session-value", "wave3-proxy-user", "wave3-proxy-pass"} {
		found := false
		for _, got := range secrets {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("secrets=%v, want %q", secrets, want)
		}
	}
}

func TestWave3CanonicalPath(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		if _, err := canonicalPath(""); err == nil || err.Error() != "empty path" {
			t.Fatalf("err=%v, want 'empty path'", err)
		}
	})
	t.Run("absolute symlink target", func(t *testing.T) {
		realPath := filepath.Join(t.TempDir(), "real.json")
		if err := os.WriteFile(realPath, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link.json")
		if err := os.Symlink(realPath, link); err != nil {
			t.Fatal(err)
		}
		got, err := canonicalPath(link)
		if err != nil {
			t.Fatal(err)
		}
		if got != realPath {
			t.Fatalf("got=%q, want %q", got, realPath)
		}
	})
	t.Run("symlink chain over the cap", func(t *testing.T) {
		dir := t.TempDir()
		realPath := filepath.Join(dir, "real")
		if err := os.WriteFile(realPath, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		prev := "real"
		for i := 40; i >= 0; i-- {
			link := filepath.Join(dir, "link"+strconv.Itoa(i))
			if err := os.Symlink(prev, link); err != nil {
				t.Fatal(err)
			}
			prev = "link" + strconv.Itoa(i)
		}
		_, err := canonicalPath(filepath.Join(dir, "link0"))
		if err == nil || !strings.Contains(err.Error(), "too many symlink resolutions") {
			t.Fatalf("err=%v, want 'too many symlink resolutions'", err)
		}
	})
	t.Run("relative path with deleted cwd", func(t *testing.T) {
		savedWd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		oldPWD, pwdSet := os.LookupEnv("PWD")
		defer func() {
			_ = os.Chdir(savedWd)
			if pwdSet {
				_ = os.Setenv("PWD", oldPWD)
			} else {
				_ = os.Unsetenv("PWD")
			}
		}()
		_ = os.Unsetenv("PWD")
		cwd, err := os.MkdirTemp("", "wave3-deleted-cwd-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(cwd); err != nil {
			t.Skipf("cannot remove the current working directory on this platform: %v", err)
		}
		if _, err := canonicalPath("relative/path.json"); err == nil {
			t.Fatal("canonicalizing a relative path with no working directory must fail")
		}
	})
}

func TestWave3SyncDestDirRootTarget(t *testing.T) {
	// Parent and grandparent coincide at the filesystem root, so
	// no second directory sync is attempted.
	if err := syncDestDir("/wave3-root-target"); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

func TestWave3ScanRestoreJournalsUninspectable(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "nexaroute-restore-journal-wave3uninspectable")
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, uninspectable := scanRestoreJournals()
	found := false
	for _, d := range uninspectable {
		if d == dir {
			found = true
		}
	}
	if !found {
		t.Fatalf("uninspectable=%v, want %q", uninspectable, dir)
	}
}

func TestWave3ReadAndVerifyBundle(t *testing.T) {
	t.Run("missing bundle file", func(t *testing.T) {
		_, _, _, err := readAndVerifyBundle(filepath.Join(t.TempDir(), "missing.tar"))
		if err == nil || !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("err=%v, want 'no such file'", err)
		}
	})
	t.Run("stage temp failure", func(t *testing.T) {
		bundle := writeTestBundle(t, map[string][]byte{backupEntryConfig: []byte("{}")})
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", blocker)
		_, _, _, err := readAndVerifyBundle(bundle)
		if err == nil {
			t.Fatal("stage creation under a regular file must fail")
		}
	})
	t.Run("unexpected entry refused", func(t *testing.T) {
		bundle := wave3CraftTar(t, []struct {
			name string
			size int64
			body []byte
		}{
			{"evil.txt", 4, []byte("evil")},
			{backupEntryManifest, 2, []byte("{}")},
		})
		_, _, _, err := readAndVerifyBundle(bundle)
		if err == nil || !strings.Contains(err.Error(), "unexpected entry") {
			t.Fatalf("err=%v, want 'unexpected entry'", err)
		}
	})
	t.Run("aggregate limit from declared sizes", func(t *testing.T) {
		wave3SetLimits(t, 1<<30, 100)
		bundle := wave3CraftTar(t, []struct {
			name string
			size int64
			body []byte
		}{
			{backupEntryManifest, 2, []byte("{}")},
			{backupEntryConfig, 200, make([]byte, 200)},
		})
		_, _, _, err := readAndVerifyBundle(bundle)
		if err == nil || !strings.Contains(err.Error(), "aggregate limit") {
			t.Fatalf("err=%v, want 'aggregate limit'", err)
		}
	})
	t.Run("short entry read error", func(t *testing.T) {
		// A body shorter than its declared size makes the tar
		// reader fail with unexpected EOF, which surfaces as a
		// read error (not the length check: a short read without
		// an error cannot occur because the declared size is
		// bounded by the entry limit before reading).
		bundle := wave3CraftTar(t, []struct {
			name string
			size int64
			body []byte
		}{
			{backupEntryManifest, 2, []byte("{}")},
			{backupEntryConfig, 100, make([]byte, 50)},
		})
		_, _, _, err := readAndVerifyBundle(bundle)
		if err == nil || !strings.Contains(err.Error(), "read bundle entry") {
			t.Fatalf("err=%v, want 'read bundle entry'", err)
		}
	})
}
