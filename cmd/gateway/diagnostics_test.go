package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// All secrets in this file are fake, test-only values. They exist so
// the bundle's redaction can be proven against realistic shapes: an
// env-resolved provider key, a custom header value, a proxy password,
// a bearer token, a cookie, a URL query value, and a JSON api_key.
const (
	diagFakeEnvKey      = "sk-fake-env-key-7f3a9c1e4b2d"
	diagFakeHeaderToken = "tok-fake-header-9d2c8a4f6e1b"
	diagFakeProxyPass   = "proxy-fake-pass-3a7e5d9c2b"
	diagFakeBearer      = "Bearer fake-bearer-token-8c4e2a6d"
	diagFakeCookie      = "session-fake-cookie-5b9d3e7a1c"
	diagFakeQueryValue  = "query-fake-value-6e1a8f4d2c"
	diagFakeJSONKey     = "json-fake-api-key-2d8b6e9a3f"
	diagFakePrompt      = "this prompt text must never leave the host"
)

func diagnosticsAdversarialConfig(t *testing.T, dir string) (string, string) {
	t.Helper()
	t.Setenv("NEXAROUTE_DIAG_FAKE_KEY", diagFakeEnvKey)
	cfg := config.Default()
	cfg.Logging.File = "diagnostics.log"
	cfg.Providers = []config.ProviderConfig{{
		ID:        "adv",
		Name:      "Adversarial Provider",
		Type:      "openai_compatible",
		Enabled:   true,
		BaseURL:   "https://api.example.test/v1",
		AuthMode:  "bearer",
		APIKeyEnv: "NEXAROUTE_DIAG_FAKE_KEY",
		Headers: map[string]string{
			"X-Custom-Token": diagFakeHeaderToken,
		},
		ProxyURL: "http://proxyuser:" + diagFakeProxyPass + "@proxy.example.test:8080",
		Models: []config.ModelConfig{
			{ID: "m1", Model: "model-a", Enabled: true, Priority: 1},
			{ID: "m2", Model: "model-b", Enabled: false, Priority: 2},
		},
	}}
	configPath := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	// Log lines an incident bundle must survive: every secret shape
	// the requirements name, plus a payload-shaped line that must be
	// dropped wholesale instead of redacted.
	logLines := []string{
		"nexaroute upstream error: provider adv failed with key " + diagFakeEnvKey,
		"nexaroute upstream error: rejected, authorization: " + diagFakeBearer,
		"nexaroute upstream error: redirect to https://operator:" + diagFakeProxyPass + "@api.example.test/v1?token=" + diagFakeQueryValue + "#frag",
		"nexaroute upstream error: cookie header Cookie: " + diagFakeCookie + " rejected",
		`nexaroute upstream error: body {"api_key": "` + diagFakeJSONKey + `"}`,
		"nexaroute upstream error: custom header X-Custom-Token: " + diagFakeHeaderToken,
		`nexaroute upstream error: response {"choices": [{"message": {"content": "` + diagFakePrompt + `"}}]}`,
		"nexaroute request ok: health check passed",
	}
	if err := os.WriteFile(filepath.Join(dir, "diagnostics.log"), []byte(strings.Join(logLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, configPath
}

func diagnosticsRunBundle(t *testing.T, configPath, output string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runDiagnostics([]string{"bundle", "--config", configPath, "--output", output}, &stdout, &stderr)
	if stderr.Len() > 0 {
		t.Logf("diagnostics stderr: %s", stderr.String())
	}
	return code, stdout.String()
}

func diagnosticsReadBundle(t *testing.T, path string) ([]byte, diagnosticsBundle) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var bundle diagnosticsBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatalf("bundle is not valid JSON: %v\n%s", err, data)
	}
	return data, bundle
}

func TestDiagnosticsBundleAdversarialConfig(t *testing.T) {
	dir := t.TempDir()
	_, configPath := diagnosticsAdversarialConfig(t, dir)
	output := filepath.Join(dir, "bundle.json")
	code, stdout := diagnosticsRunBundle(t, configPath, output)
	if code != 0 {
		t.Fatalf("diagnostics exit=%d, want 0", code)
	}
	// Requirement 7: the operator is told what happened, what was
	// excluded, and that nothing was sent anywhere.
	for _, want := range []string{"diagnostics bundle written", "mode 0600", "not sent anywhere", "before sharing", "retention"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode=%04o, want 0600", info.Mode().Perm())
	}
	data, bundle := diagnosticsReadBundle(t, output)
	forbidden := []string{
		diagFakeEnvKey, diagFakeHeaderToken, diagFakeProxyPass,
		diagFakeBearer, diagFakeCookie, diagFakeQueryValue,
		diagFakeJSONKey, diagFakePrompt, "proxyuser",
		// The bundle must not leak the operator's own directory layout
		// through doctor detail paths either.
		dir,
	}
	for _, secret := range forbidden {
		if strings.Contains(string(data), secret) {
			t.Fatalf("bundle leaks %q:\n%s", secret, data)
		}
	}
	if strings.Contains(string(data), "Bearer") {
		t.Fatalf("bundle contains a bearer token shape:\n%s", data)
	}
	if bundle.Format != diagnosticsFormat {
		t.Fatalf("bundle format=%q, want %q", bundle.Format, diagnosticsFormat)
	}
	if !bundle.Config.Valid {
		t.Fatalf("bundle config valid=false, want true (error=%q)", bundle.Config.Error)
	}
	if bundle.Config.ProvidersTotal != 1 || bundle.Config.ProvidersEnabled != 1 {
		t.Fatalf("provider counts=%d/%d, want 1/1", bundle.Config.ProvidersTotal, bundle.Config.ProvidersEnabled)
	}
	if len(bundle.Providers) != 1 {
		t.Fatalf("provider summaries=%d, want 1", len(bundle.Providers))
	}
	p := bundle.Providers[0]
	if p.BaseURLHost != "https://api.example.test" {
		t.Fatalf("provider base_url_host=%q, want scheme://host only", p.BaseURLHost)
	}
	if !p.CredentialConfigured || p.CredentialSource != "env:NEXAROUTE_DIAG_FAKE_KEY" {
		t.Fatalf("provider credential summary=%t/%q, want true/env:NAME only", p.CredentialConfigured, p.CredentialSource)
	}
	if len(p.HeaderNames) != 1 || p.HeaderNames[0] != "X-Custom-Token" {
		t.Fatalf("provider header_names=%v, want names only, never values", p.HeaderNames)
	}
	if p.ModelsTotal != 2 || p.ModelsEnabled != 1 {
		t.Fatalf("provider model counts=%d/%d, want 2/1", p.ModelsTotal, p.ModelsEnabled)
	}
	// The payload-shaped line was dropped, the operational error
	// lines survived in sanitized form, and the healthy line was
	// never an "error" line at all.
	if !bundle.RecentErrors.LogAvailable {
		t.Fatal("recent errors log_available=false, want true")
	}
	if len(bundle.RecentErrors.Included) == 0 {
		t.Fatal("recent errors included none, want sanitized error lines")
	}
	for _, line := range bundle.RecentErrors.Included {
		if strings.Contains(line, diagFakePrompt) {
			t.Fatalf("recent error includes prompt text: %q", line)
		}
	}
	if bundle.RecentErrors.Dropped < 1 {
		t.Fatalf("recent errors dropped=%d, want >=1 (payload line)", bundle.RecentErrors.Dropped)
	}
	if bundle.Gateway.GoVersion == "" || bundle.Gateway.OS == "" {
		t.Fatalf("gateway runtime detail missing: %+v", bundle.Gateway)
	}
	// No temp files left beside the bundle.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nexaroute-diagnostics-") {
			t.Fatalf("atomic-write temp file left behind: %s", e.Name())
		}
	}
}

func TestDiagnosticsBundleMalformedConfig(t *testing.T) {
	// Valid JSON, invalid validation: a base URL with embedded
	// credentials plus an inline fake key. The bundle must still be
	// produced, must report valid=false, and must contain neither
	// the secret nor the URL user info/query it was fed.
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID:       "bad",
		Type:     "openai_compatible",
		BaseURL:  "https://baduser:badpass-9f2e7c@example.test/v1?token=badquery-4d8a1e",
		AuthMode: "bearer",
		APIKey:   "sk-fake-malformed-1a3d7e9b",
	}}
	configPath := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "bundle.json")
	code, _ := diagnosticsRunBundle(t, configPath, output)
	if code != 0 {
		t.Fatalf("diagnostics exit=%d, want 0 (invalid config is bundle data, not a command failure)", code)
	}
	raw, bundle := diagnosticsReadBundle(t, output)
	if bundle.Config.Valid {
		t.Fatal("bundle config valid=true, want false for malformed config")
	}
	for _, forbidden := range []string{"sk-fake-malformed-1a3d7e9b", "baduser", "badpass-9f2e7c", "badquery-4d8a1e"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("malformed-config bundle leaks %q:\n%s", forbidden, raw)
		}
	}

	// Not JSON at all: still a bundle, still valid=false, no panic.
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	brokenOut := filepath.Join(dir, "broken-bundle.json")
	code, _ = diagnosticsRunBundle(t, broken, brokenOut)
	if code != 0 {
		t.Fatalf("broken-config diagnostics exit=%d, want 0", code)
	}
	_, brokenBundle := diagnosticsReadBundle(t, brokenOut)
	if brokenBundle.Config.Valid {
		t.Fatal("broken-config bundle valid=true, want false")
	}
}

func TestDiagnosticsSanitizerAdversarial(t *testing.T) {
	san := newDiagnosticsSanitizer(nil)
	cases := []struct {
		name string
		in   string
		ban  []string
		keep string
	}{
		{
			name: "bearer header",
			in:   "upstream error: Authorization: Bearer abcdefgh12345678 rejected",
			ban:  []string{"abcdefgh12345678", "Bearer"},
		},
		{
			name: "api key assignment",
			in:   "failed with api_key=sk-fake-assign-12345678 for provider",
			ban:  []string{"sk-fake-assign-12345678"},
		},
		{
			name: "json secret field",
			in:   `provider response {"api_key": "json-secret-value-1234", "status": "ok"}`,
			ban:  []string{"json-secret-value-1234"},
			keep: `"status": "ok"`,
		},
		{
			name: "url userinfo query fragment",
			in:   "redirect https://user:pass123456@api.example.test/v1?token=querysecret1#frag failed",
			ban:  []string{"pass123456", "querysecret1", "user@"},
			keep: "https://api.example.test",
		},
		{
			name: "cookie header",
			in:   "Cookie: session=cookievalue12345678; path=/ rejected",
			ban:  []string{"cookievalue12345678"},
		},
		{
			name: "encrypted envelope",
			in:   "stored value nxs1:QUJDREVGR0hJSktMTU5PUA== leaked into error",
			ban:  []string{"nxs1:QUJDREVGR0hJSktMTU5PUA=="},
		},
		{
			name: "absolute path",
			in:   "cannot read /home/operator/secret-dir/config.json: permission denied",
			ban:  []string{"/home/operator/secret-dir"},
			keep: "config.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := san.sanitizeText(tc.in)
			for _, banned := range tc.ban {
				if strings.Contains(out, banned) {
					t.Fatalf("sanitized text still contains %q:\n%q -> %q", banned, tc.in, out)
				}
			}
			if tc.keep != "" && !strings.Contains(out, tc.keep) {
				t.Fatalf("sanitized text lost safe content %q:\n%q -> %q", tc.keep, tc.in, out)
			}
			if diagnosticsDenyScan(out, nil) {
				t.Fatalf("second scan still flags sanitized text:\n%q -> %q", tc.in, out)
			}
		})
	}

	// Positive controls: the deny scan must catch raw forms.
	rawCases := []string{
		"Authorization: Bearer abcdefgh12345678",
		"https://user:pass123456@api.example.test/v1",
		"https://api.example.test/v1?token=querysecret1",
		"nxs1:QUJDREVGR0hJSktMTU5PUA==",
		`{"api_key": "json-secret-value-1234"}`,
		"api_key=sk-fake-assign-12345678",
	}
	for _, raw := range rawCases {
		if !diagnosticsDenyScan(raw, nil) {
			t.Fatalf("deny scan missed raw secret shape: %q", raw)
		}
	}
	// Known-value scan: an exact configured secret must fail even
	// when it appears without any secret-shaped syntax around it.
	if !diagnosticsDenyScan("the key is "+diagFakeEnvKey+" here", []string{diagFakeEnvKey}) {
		t.Fatal("deny scan missed an exact known secret value")
	}
	if diagnosticsDenyScan("ordinary operational text", []string{diagFakeEnvKey}) {
		t.Fatal("deny scan flagged ordinary text")
	}
}

func TestDiagnosticsOutputRules(t *testing.T) {
	_, configPath := opsFixture(t)
	dir := t.TempDir()
	output := filepath.Join(dir, "bundle.json")

	if code := runDiagnostics([]string{"bundle", "--config", configPath}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("missing --output exit=%d, want 2", code)
	}
	if code := runDiagnostics([]string{"bogus"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("unknown diagnostics subcommand exit=%d, want 2", code)
	}
	if code, _ := diagnosticsRunBundle(t, configPath, output); code != 0 {
		t.Fatalf("first bundle exit=%d, want 0", code)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	// Refuse to clobber an existing bundle without --overwrite.
	var stderr bytes.Buffer
	if code := runDiagnostics([]string{"bundle", "--config", configPath, "--output", output}, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("existing-output exit=%d, want 1", code)
	}
	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused bundle write modified the existing file")
	}
	// --overwrite replaces it, still at mode 0600.
	if code := runDiagnostics([]string{"bundle", "--config", configPath, "--output", output, "--overwrite"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("overwrite bundle exit=%d, want 0", code)
	}
	if info, err := os.Stat(output); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("overwritten bundle mode/stat: %v %v", info, err)
	}
}

func TestDiagnosticsSecondScanAndAtomicWrite(t *testing.T) {
	if err := verifyBundleSafe([]byte(`{"detail": "Authorization: Bearer abcdefgh12345678"}`), nil); err == nil {
		t.Fatal("verifyBundleSafe accepted a bundle containing an auth header")
	}
	if err := verifyBundleSafe([]byte(`{"detail": "https://user:pass123456@api.example.test/v1?token=x"}`), nil); err == nil {
		t.Fatal("verifyBundleSafe accepted a bundle containing URL credentials")
	}
	if err := verifyBundleSafe([]byte(`{"format": "nexaroute-diagnostics/v1", "detail": "all clear"}`), nil); err != nil {
		t.Fatalf("verifyBundleSafe rejected a clean bundle: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	if err := writeDiagnosticsBundleAtomic(path, []byte("{}\n")); err != nil {
		t.Fatalf("atomic write failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("atomic bundle mode=%04o, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the bundle file, got %v", entries)
	}
	if err := writeDiagnosticsBundleAtomic(filepath.Join(dir, "missing-dir", "bundle.json"), []byte("{}\n")); err == nil {
		t.Fatal("atomic write into a missing directory unexpectedly succeeded")
	}
}

func TestDiagnosticsBaseURLHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.example.test/v1", "https://api.example.test"},
		{"https://user:pass@example.test/v1?token=x#frag", "https://example.test"},
		{"http://127.0.0.1:8080/chat", "http://127.0.0.1:8080"},
		{"", ""},
		{"not a url ::", ""},
	}
	for _, tc := range cases {
		if got := diagnosticsBaseURLHost(tc.in); got != tc.want {
			t.Fatalf("diagnosticsBaseURLHost(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}
