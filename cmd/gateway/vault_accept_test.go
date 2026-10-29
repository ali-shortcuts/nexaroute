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
	"strings"
	"sync"
	"testing"
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
	requests     []vaultAcceptRequest
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
	return f.childToken != "" && token == f.childToken && !f.childRevoked
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
	f.mu.Unlock()

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
		writeVaultAcceptJSON(w, map[string]any{"data": map[string]any{
			"policies": []string{"nexaroute-acceptance", "default"}, "ttl": 3600, "renewable": false,
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
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	t.Setenv("VAULT_ADDR", server.URL)
	t.Setenv("VAULT_TOKEN", fake.primaryToken)
	outPath := filepath.Join(t.TempDir(), "vault-acceptance.json")
	args := append([]string{
		"--ref", "secret/data/nexaroute/acceptance/master-key",
		"--helper", vaultAcceptHelperPath(t),
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
		"vault.kv_put_get":         externalStatusPass,
		"vault.gateway_decrypt":    externalStatusPass,
		"vault.kv_rotation":        externalStatusPass,
		"vault.wrong_token":        externalStatusPass,
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
