package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeVaultAccept struct {
	mu           sync.Mutex
	primaryToken string
	childToken   string
	childRevoked bool
	keyB64       string
	writtenKeys  []string
	devMode      bool
	createDenied bool
	deleteDenied bool
	lookupDenied bool
	// policies, when non-nil, replaces the default lookup-self policy set.
	policies []string
	// metadataDenied makes GET on a KV metadata path return 403.
	metadataDenied bool
	// metadataMissing makes GET on a KV metadata path return 404 even when
	// a key exists, simulating a KV v1 mount (no metadata endpoint).
	metadataMissing bool
	// mountType/mountVersion, when non-empty, replace the default kv/2
	// answer for GET on sys/mounts/secret. mountDenied makes it 403 and
	// mountMissing makes it 404.
	mountType    string
	mountVersion string
	mountDenied  bool
	mountMissing bool
	// childTTL, when positive, makes a disposable child token expire
	// that long after creation, so the expired-token poll can observe
	// a real expiry window. Fixtures that run the full harness must
	// set it, or the expired-token check polls to its 10s deadline and
	// fails on a child token that never expires.
	childTTL       time.Duration
	childExpiresAt time.Time
	requests       []vaultAcceptRequest
}

type vaultAcceptRequest struct {
	method string
	path   string
}

func newFakeVaultAccept() *fakeVaultAccept {
	return &fakeVaultAccept{primaryToken: "primary-token-7f31c9"}
}

func (f *fakeVaultAccept) authorized(token string) bool {
	if token == f.primaryToken {
		return true
	}
	if f.childToken == "" || token != f.childToken || f.childRevoked {
		return false
	}
	if !f.childExpiresAt.IsZero() && time.Now().After(f.childExpiresAt) {
		return false
	}
	return true
}

func (f *fakeVaultAccept) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	token := r.Header.Get("X-Vault-Token")
	f.mu.Lock()
	f.requests = append(f.requests, vaultAcceptRequest{r.Method, r.URL.Path})
	devMode := f.devMode
	createDenied := f.createDenied
	deleteDenied := f.deleteDenied
	lookupDenied := f.lookupDenied
	metadataDenied := f.metadataDenied
	metadataMissing := f.metadataMissing
	mountType := f.mountType
	mountVersion := f.mountVersion
	mountDenied := f.mountDenied
	mountMissing := f.mountMissing
	f.mu.Unlock()

	if strings.HasPrefix(r.URL.Path, "/v1/sys/mounts/") {
		if !f.authorized(token) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mount := strings.TrimPrefix(r.URL.Path, "/v1/sys/mounts/")
		if mount != "secret" || mountMissing {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if mountDenied {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if mountType == "" {
			mountType = "kv"
		}
		if mountVersion == "" {
			mountVersion = "2"
		}
		writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{
			"type": mountType, "options": map[string]any{"version": mountVersion},
		}})
		return
	}

	switch r.URL.Path {
	case "/v1/sys/seal-status":
		storage := "raft"
		if devMode {
			storage = "inmem"
		}
		writeVaultAcceptJSON(w, map[string]any{
			"version": "1.21.0", "storage_type": storage, "sealed": false, "standby": false,
		})
		return
	case "/v1/sys/leader":
		writeVaultAcceptJSON(w, map[string]any{"ha_enabled": true})
		return
	case "/v1/auth/token/lookup-self":
		if lookupDenied {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !f.authorized(token) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		policies := f.policies
		if policies == nil {
			policies = []string{"nexaroute-acceptance", "default"}
		}
		writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{
			"policies": policies, "ttl": 3600, "renewable": false,
		}})
		return
	case "/v1/auth/token/create":
		if createDenied {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !f.authorized(token) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		f.childToken = "child-token-5c19ab"
		if f.childTTL > 0 {
			f.childExpiresAt = time.Now().Add(f.childTTL)
		} else {
			f.childExpiresAt = time.Time{}
		}
		child := f.childToken
		f.mu.Unlock()
		writeVaultAcceptJSON(w, map[string]any{"auth": map[string]any{"client_token": child}})
		return
	case "/v1/auth/token/revoke-self":
		f.mu.Lock()
		if token != "" && token == f.childToken {
			f.childRevoked = true
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	kv := strings.HasPrefix(r.URL.Path, "/v1/secret/data/") || strings.HasPrefix(r.URL.Path, "/v1/secret/metadata/")
	if !kv {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !f.authorized(token) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/secret/metadata/") {
		if r.Method == http.MethodGet {
			if metadataDenied {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.mu.Lock()
			key := f.keyB64
			f.mu.Unlock()
			if metadataMissing || key == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{
				"current_version": 1, "versions": map[string]any{"1": map[string]any{}},
			}})
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if deleteDenied {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		f.keyB64 = ""
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		key := f.keyB64
		f.mu.Unlock()
		if key == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{
			"data": map[string]any{"key_b64": key}, "metadata": map[string]any{"version": 1},
		}})
	case http.MethodPost:
		var payload struct {
			Data struct {
				KeyB64 string `json:"key_b64"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Data.KeyB64 == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.keyB64 = payload.Data.KeyB64
		f.writtenKeys = append(f.writtenKeys, payload.Data.KeyB64)
		f.mu.Unlock()
		writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{"version": 1}})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeVaultAcceptJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func vaultAcceptHelperPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "key-provider-vault-kv.sh")
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		t.Skipf("helper script not found at %s", abs)
	}
	return abs
}

func readVaultAcceptReport(t *testing.T, path string) vaultAcceptReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report vaultAcceptReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func vaultAcceptStatus(report vaultAcceptReport, id string) string {
	for _, check := range report.Checks {
		if check.ID == id {
			return check.Status
		}
	}
	return ""
}

func runVaultAcceptFixture(t *testing.T, fake *fakeVaultAccept, extra ...string) (int, vaultAcceptReport, string, string, string) {
	t.Helper()
	return runVaultAcceptFixtureWithHelper(t, fake, vaultAcceptHelperPath(t), extra...)
}

func runVaultAcceptFixtureWithHelper(t *testing.T, fake *fakeVaultAccept, helper string, extra ...string) (int, vaultAcceptReport, string, string, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	t.Setenv("VAULT_ADDR", server.URL)
	t.Setenv("VAULT_TOKEN", fake.primaryToken)
	outPath := filepath.Join(t.TempDir(), "vault-acceptance.json")
	args := append([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", helper,
		"--confirm-live",
		"--out", outPath,
	}, extra...)
	var stdout, stderr bytes.Buffer
	code := runVaultAccept(args, &stdout, &stderr)
	report := readVaultAcceptReport(t, outPath)
	return code, report, outPath, stdout.String(), stderr.String()
}

func TestVaultAcceptFullRunPassesAndRedacts(t *testing.T) {
	fake := newFakeVaultAccept()
	// The expired-token check polls a 2s-TTL child token; the fixture
	// child expires after 1s so the poll observes a real expiry window.
	fake.childTTL = time.Second
	code, report, outPath, stdout, stderr := runVaultAcceptFixture(t, fake)
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Schema != vaultAcceptSchema || report.Outcome != externalStatusPass {
		t.Fatalf("report header/outcome=%+v", report)
	}
	if report.EvidenceClass != "loopback" {
		t.Fatalf("evidence_class=%q want loopback", report.EvidenceClass)
	}
	if report.Vault.Version != "1.21.0" || report.Vault.StorageType != "raft" || !report.Vault.HAEnabled || !report.Vault.HAKnown {
		t.Fatalf("vault metadata=%+v", report.Vault)
	}
	if !report.Vault.TokenMetadataOK || len(report.Vault.Policies) != 2 {
		t.Fatalf("token metadata=%+v", report.Vault)
	}
	want := map[string]string{
		"vault.health":             externalStatusPass,
		"vault.token_metadata":     externalStatusPass,
		"vault.ref_preflight":      externalStatusPass,
		"vault.mount_version":      externalStatusPass,
		"vault.kv_put_get":         externalStatusPass,
		"vault.kv_version":         externalStatusPass,
		"vault.gateway_decrypt":    externalStatusPass,
		"vault.kv_rotation":        externalStatusPass,
		"vault.wrong_token":        externalStatusPass,
		"vault.expired_token":      externalStatusPass,
		"vault.revoked_token":      externalStatusPass,
		"vault.outage_fail_closed": externalStatusPass,
		"vault.cleanup":            externalStatusPass,
		"vault.ha_failover":        externalStatusNotRun,
		"vault.transit":            externalStatusNotRun,
	}
	for id, status := range want {
		if got := vaultAcceptStatus(report, id); got != status {
			t.Errorf("%s status=%q want %q", id, got, status)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.keyB64 != "" {
		t.Fatal("acceptance key was not deleted")
	}
	if !fake.childRevoked {
		t.Fatal("disposable child token was not revoked")
	}
	if len(fake.writtenKeys) == 0 {
		t.Fatal("fixture saw no writes")
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{string(raw), stdout, stderr}
	for _, key := range fake.writtenKeys {
		for _, out := range outputs {
			if strings.Contains(out, key) {
				t.Fatal("acceptance output leaked key bytes")
			}
		}
	}
	for _, token := range []string{fake.primaryToken, fake.childToken} {
		if token == "" {
			continue
		}
		for _, out := range outputs {
			if strings.Contains(out, token) {
				t.Fatal("acceptance output leaked a token")
			}
		}
	}
	if info, err := os.Stat(outPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
}

func TestVaultAcceptRejectsDevModeByDefault(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.devMode = true
	code, report, _, stdout, stderr := runVaultAcceptFixture(t, fake)
	if code != 3 {
		t.Fatalf("exit=%d want 3 BLOCKED\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Outcome != externalStatusBlocked {
		t.Fatalf("outcome=%s", report.Outcome)
	}
	if vaultAcceptStatus(report, "vault.health") != externalStatusPass {
		t.Fatalf("health=%+v", report.Checks)
	}
	if vaultAcceptStatus(report, "vault.dev_mode") != externalStatusBlocked {
		t.Fatalf("dev_mode check=%+v", report.Checks)
	}
	if vaultAcceptStatus(report, "vault.kv_put_get") != externalStatusBlocked {
		t.Fatalf("kv check should be blocked: %+v", report.Checks)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.keyB64 != "" || len(fake.writtenKeys) != 0 {
		t.Fatal("dev-mode rejection still wrote a key")
	}
}

func TestVaultAcceptAllowDevLabelsEvidence(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.devMode = true
	fake.childTTL = time.Second
	code, report, _, stdout, stderr := runVaultAcceptFixture(t, fake, "--allow-dev")
	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.EvidenceClass != "dev" || report.Outcome != externalStatusPass {
		t.Fatalf("evidence_class=%q outcome=%s", report.EvidenceClass, report.Outcome)
	}
	found := false
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "MUST NOT") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dev-mode warning missing: %+v", report.Warnings)
	}
}

func TestVaultAcceptRefusesOccupiedRefWithoutWriting(t *testing.T) {
	fake := newFakeVaultAccept()
	existing := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	fake.keyB64 = existing
	code, report, _, stdout, stderr := runVaultAcceptFixture(t, fake)
	if code != 1 {
		t.Fatalf("exit=%d want 1 FAIL\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Outcome != externalStatusFail || vaultAcceptStatus(report, "vault.ref_preflight") != externalStatusFail {
		t.Fatalf("report=%+v", report)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.keyB64 != existing {
		t.Fatal("occupied ref was overwritten")
	}
	for _, request := range fake.requests {
		if request.method == http.MethodPost {
			t.Fatalf("unexpected write request: %+v", request)
		}
	}
}

func TestVaultAcceptMissingConfigWritesBlockedReport(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "blocked.json")
	t.Setenv("NEXAROUTE_VAULT_ACCEPT_ADDR_UNSET", "")
	var stdout, stderr bytes.Buffer
	code := runVaultAccept([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", vaultAcceptHelperPath(t),
		"--addr-env", "NEXAROUTE_VAULT_ACCEPT_ADDR_UNSET",
		"--confirm-live", "--out", outPath,
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit=%d want 3 BLOCKED\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := readVaultAcceptReport(t, outPath)
	if report.Outcome != externalStatusBlocked || vaultAcceptStatus(report, "vault.config") != externalStatusBlocked {
		t.Fatalf("report=%+v", report)
	}
	if vaultAcceptStatus(report, "vault.ha_failover") != externalStatusNotRun {
		t.Fatalf("HA check=%+v", report.Checks)
	}
	if !strings.Contains(report.Checks[2].Reason, "NEXAROUTE_VAULT_ACCEPT_ADDR_UNSET") {
		t.Fatalf("blocked reason does not name the env var: %+v", report.Checks)
	}
}

func TestVaultAcceptRevokedTokenCreationDeniedIsBlocked(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.createDenied = true
	code, report, _, stdout, stderr := runVaultAcceptFixture(t, fake)
	if code != 3 {
		t.Fatalf("exit=%d want 3 BLOCKED\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if report.Outcome != externalStatusBlocked || vaultAcceptStatus(report, "vault.revoked_token") != externalStatusBlocked {
		t.Fatalf("report=%+v", report)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.keyB64 != "" {
		t.Fatal("cleanup did not run after a blocked revocation check")
	}
}

func TestVaultAcceptRequiresConfirmLiveAndWritesNothing(t *testing.T) {
	fake := newFakeVaultAccept()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	t.Setenv("VAULT_ADDR", server.URL)
	t.Setenv("VAULT_TOKEN", fake.primaryToken)
	outPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := runVaultAccept([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", vaultAcceptHelperPath(t),
		"--out", outPath,
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d want 2\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed run wrote a report: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Fatalf("unconfirmed run made requests: %+v", fake.requests)
	}
}

func TestVaultAcceptRejectsUnsafeRefAndHTTP(t *testing.T) {
	for _, ref := range []string{"", "/secret/data/x", "secret/data/../x", "secret/not-v2/x", "secret/data/x?y=1"} {
		if err := validateVaultAcceptRef(ref); err == nil {
			t.Errorf("validateVaultAcceptRef(%q) accepted an unsafe path", ref)
		}
	}
	if _, err := validateVaultAcceptAddr("http://vault.example:8200"); err == nil {
		t.Fatal("plain HTTP to a non-loopback host was accepted")
	}
	if _, err := validateVaultAcceptAddr("https://vault.example:8200"); err != nil {
		t.Fatalf("HTTPS staging address rejected: %v", err)
	}
	if _, err := validateVaultAcceptAddr("http://127.0.0.1:8200"); err != nil {
		t.Fatalf("loopback HTTP address rejected: %v", err)
	}
}

func TestVaultAcceptSubcommandIsDispatched(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatchSubcommand([]string{"vault-accept"}, nil, &stdout, &stderr)
	if !handled {
		t.Fatal("vault-accept was not dispatched")
	}
	if code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--ref is required") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestVaultAcceptRootTokenFailsBeforeWriting(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.policies = []string{"root", "default"}
	code, report, _, _, _ := runVaultAcceptFixture(t, fake)
	if code != 1 || report.Outcome != externalStatusFail {
		t.Fatalf("exit=%d outcome=%q want 1/fail", code, report.Outcome)
	}
	if got := vaultAcceptStatus(report, "vault.token_metadata"); got != externalStatusFail {
		t.Fatalf("token_metadata=%q want fail", got)
	}
	if got := vaultAcceptStatus(report, "vault.kv_put_get"); got != "" {
		t.Fatalf("kv_put_get=%q want absent (no write with a root token)", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.writtenKeys) != 0 {
		t.Fatalf("wrote %d keys with a root token", len(fake.writtenKeys))
	}
}

func TestVaultAcceptKVVersionFailsOnV1Mount(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.metadataMissing = true
	fake.childTTL = time.Second
	code, report, _, _, _ := runVaultAcceptFixture(t, fake)
	if code != 1 || report.Outcome != externalStatusFail {
		t.Fatalf("exit=%d outcome=%q want 1/fail", code, report.Outcome)
	}
	// The point of the check: the helper round-trip alone would have
	// passed on this mount, so kv_put_get PASS with kv_version FAIL is
	// the regression this test guards.
	if got := vaultAcceptStatus(report, "vault.kv_put_get"); got != externalStatusPass {
		t.Fatalf("kv_put_get=%q want pass", got)
	}
	if got := vaultAcceptStatus(report, "vault.kv_version"); got != externalStatusFail {
		t.Fatalf("kv_version=%q want fail", got)
	}
}

func TestVaultAcceptKVVersionBlockedWithoutMetadataRead(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.metadataDenied = true
	fake.childTTL = time.Second
	code, report, _, _, _ := runVaultAcceptFixture(t, fake)
	if code != 3 || report.Outcome != externalStatusBlocked {
		t.Fatalf("exit=%d outcome=%q want 3/blocked", code, report.Outcome)
	}
	if got := vaultAcceptStatus(report, "vault.kv_version"); got != externalStatusBlocked {
		t.Fatalf("kv_version=%q want blocked", got)
	}
}

func TestVaultAcceptHelperTimeoutResolution(t *testing.T) {
	t.Setenv("VAULT_HELPER_TIMEOUT", "")
	if got, err := vaultAcceptHelperTimeout(10*time.Second, true); err != nil || got != 10 {
		t.Fatalf("explicit timeout: got=%d err=%v want 10", got, err)
	}
	if got, err := vaultAcceptHelperTimeout(500*time.Millisecond, true); err != nil || got != 1 {
		t.Fatalf("sub-second timeout: got=%d err=%v want 1", got, err)
	}
	if got, err := vaultAcceptHelperTimeout(10*time.Second, false); err != nil || got != 30 {
		t.Fatalf("unset env: got=%d err=%v want 30", got, err)
	}
	t.Setenv("VAULT_HELPER_TIMEOUT", "42")
	if got, err := vaultAcceptHelperTimeout(10*time.Second, false); err != nil || got != 42 {
		t.Fatalf("operator env: got=%d err=%v want 42", got, err)
	}
	for _, bad := range []string{"abc", "0", "-5", "7.5"} {
		t.Setenv("VAULT_HELPER_TIMEOUT", bad)
		if _, err := vaultAcceptHelperTimeout(10*time.Second, false); err == nil {
			t.Fatalf("env %q was accepted, want an error", bad)
		}
	}
}

func TestVaultAcceptHelperTimeoutPropagation(t *testing.T) {
	// The stub records the timeout the harness hands the helper, then
	// execs the real helper so the full run still exercises real semantics.
	stub := func(t *testing.T, record string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "recording-helper.sh")
		script := "#!/bin/sh\n" +
			"echo \"$VAULT_HELPER_TIMEOUT\" >> \"$VAULT_ACCEPT_TIMEOUT_RECORD\"\n" +
			"exec '" + vaultAcceptHelperPath(t) + "' \"$@\"\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name  string
		env   string
		extra []string
		want  string
	}{
		{"explicit --timeout wins", "", []string{"--timeout", "7s"}, "7"},
		{"operator env honored", "42", nil, "42"},
		{"script default otherwise", "", nil, "30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "record")
			t.Setenv("VAULT_ACCEPT_TIMEOUT_RECORD", record)
			if tc.env != "" {
				t.Setenv("VAULT_HELPER_TIMEOUT", tc.env)
			} else {
				t.Setenv("VAULT_HELPER_TIMEOUT", "")
			}
			fake := newFakeVaultAccept()
			fake.childTTL = time.Second
			code, report, _, _, _ := runVaultAcceptFixtureWithHelper(t, fake, stub(t, record), tc.extra...)
			if code != 0 || report.Outcome != externalStatusPass {
				t.Fatalf("exit=%d outcome=%q want 0/pass", code, report.Outcome)
			}
			raw, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Fields(string(raw))
			if len(lines) == 0 {
				t.Fatal("helper was never invoked")
			}
			for _, line := range lines {
				if line != tc.want {
					t.Fatalf("recorded timeouts=%v want all %q", lines, tc.want)
				}
			}
			if got := report.Bounds.HelperTimeoutSeconds; got != int64(atoi(t, tc.want)) {
				t.Fatalf("bounds helper_timeout_seconds=%d want %s", got, tc.want)
			}
		})
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestVaultAcceptPolicyAssertion(t *testing.T) {
	t.Run("matching policy passes", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.childTTL = time.Second
		code, report, _, _, _ := runVaultAcceptFixture(t, fake, "--expect-policy", "nexaroute-acceptance")
		if code != 0 || report.Outcome != externalStatusPass {
			t.Fatalf("exit=%d outcome=%q want 0/pass", code, report.Outcome)
		}
		if got := vaultAcceptStatus(report, "vault.policy_assertion"); got != externalStatusPass {
			t.Fatalf("policy_assertion=%q want pass", got)
		}
	})
	t.Run("missing policy fails before writing", func(t *testing.T) {
		fake := newFakeVaultAccept()
		code, report, _, _, _ := runVaultAcceptFixture(t, fake, "--expect-policy", "nexaroute-acceptance,nexaroute-missing")
		if code != 1 || report.Outcome != externalStatusFail {
			t.Fatalf("exit=%d outcome=%q want 1/fail", code, report.Outcome)
		}
		if got := vaultAcceptStatus(report, "vault.policy_assertion"); got != externalStatusFail {
			t.Fatalf("policy_assertion=%q want fail", got)
		}
		if got := vaultAcceptStatus(report, "vault.kv_put_get"); got != "" {
			t.Fatalf("kv_put_get=%q want absent (no write with a rejected policy)", got)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.writtenKeys) != 0 {
			t.Fatalf("wrote %d keys with a rejected policy", len(fake.writtenKeys))
		}
	})
	t.Run("absent flag records manual-review limitation", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.childTTL = time.Second
		_, report, _, _, _ := runVaultAcceptFixture(t, fake)
		found := false
		for _, limitation := range report.Limitations {
			if strings.Contains(limitation, "manual policy review") {
				found = true
			}
		}
		if !found {
			t.Fatalf("manual-review limitation missing: %+v", report.Limitations)
		}
	})
}

func TestVaultAcceptTokenMetadataBlockedStopsBeforeWriting(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.lookupDenied = true
	code, report, _, _, _ := runVaultAcceptFixture(t, fake)
	if code != 3 || report.Outcome != externalStatusBlocked {
		t.Fatalf("exit=%d outcome=%q want 3/blocked", code, report.Outcome)
	}
	if got := vaultAcceptStatus(report, "vault.token_metadata"); got != externalStatusBlocked {
		t.Fatalf("token_metadata=%q want blocked", got)
	}
	if got := vaultAcceptStatus(report, "vault.kv_put_get"); got != "" {
		t.Fatalf("kv_put_get=%q want absent (no write with unrecorded token metadata)", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.writtenKeys) != 0 {
		t.Fatalf("wrote %d keys with BLOCKED token metadata", len(fake.writtenKeys))
	}
}

func TestVaultAcceptMountVersion(t *testing.T) {
	t.Run("v1 mount fails before writing", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.mountType = "kv"
		fake.mountVersion = "1"
		code, report, _, _, _ := runVaultAcceptFixture(t, fake)
		if code != 1 || report.Outcome != externalStatusFail {
			t.Fatalf("exit=%d outcome=%q want 1/fail", code, report.Outcome)
		}
		if got := vaultAcceptStatus(report, "vault.mount_version"); got != externalStatusFail {
			t.Fatalf("mount_version=%q want fail", got)
		}
		if got := vaultAcceptStatus(report, "vault.kv_put_get"); got != "" {
			t.Fatalf("kv_put_get=%q want absent (no write to a v1 mount)", got)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.writtenKeys) != 0 {
			t.Fatalf("wrote %d keys to a v1 mount", len(fake.writtenKeys))
		}
	})
	t.Run("unreadable mount blocks before writing", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.mountDenied = true
		code, report, _, _, _ := runVaultAcceptFixture(t, fake)
		if code != 3 || report.Outcome != externalStatusBlocked {
			t.Fatalf("exit=%d outcome=%q want 3/blocked", code, report.Outcome)
		}
		if got := vaultAcceptStatus(report, "vault.mount_version"); got != externalStatusBlocked {
			t.Fatalf("mount_version=%q want blocked", got)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.writtenKeys) != 0 {
			t.Fatalf("wrote %d keys with an unverified mount", len(fake.writtenKeys))
		}
	})
	t.Run("absent mount fails before writing", func(t *testing.T) {
		fake := newFakeVaultAccept()
		fake.mountMissing = true
		code, report, _, _, _ := runVaultAcceptFixture(t, fake)
		if code != 1 || report.Outcome != externalStatusFail {
			t.Fatalf("exit=%d outcome=%q want 1/fail", code, report.Outcome)
		}
		if got := vaultAcceptStatus(report, "vault.mount_version"); got != externalStatusFail {
			t.Fatalf("mount_version=%q want fail", got)
		}
	})
}

func TestVaultMountFromRef(t *testing.T) {
	cases := map[string]string{
		"secret/data/nexaroute/acceptance/master-key": "secret",
		"team/secret/data/nexaroute/key":              "team/secret",
		"secret/data/data/x":                          "secret",
		"secret/nexaroute/key":                        "",
	}
	for ref, want := range cases {
		if got := vaultMountFromRef(ref); got != want {
			t.Errorf("vaultMountFromRef(%q)=%q want %q", ref, got, want)
		}
	}
}

func TestVaultAcceptExpiredTokenPasses(t *testing.T) {
	fake := newFakeVaultAccept()
	fake.childTTL = time.Second
	code, report, _, _, _ := runVaultAcceptFixture(t, fake)
	if code != 0 || report.Outcome != externalStatusPass {
		t.Fatalf("exit=%d outcome=%q want 0/pass", code, report.Outcome)
	}
	if got := vaultAcceptStatus(report, "vault.expired_token"); got != externalStatusPass {
		t.Fatalf("expired_token=%q want pass", got)
	}
}
