package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTransitServer implements the Vault Transit contract of ADR 0007
// §8.1 for tests only. Ciphertexts are opaque: the body is a random id
// mapped to the DEK server-side (like real Vault, the ciphertext does not
// encode the plaintext). It never sees associated data or a context
// parameter: the new design sends neither (§5.3).
type fakeTransitRequest struct {
	op        string
	path      string
	token     string
	namespace string
	bodyKeys  []string
}

type fakeTransitServer struct {
	mu             sync.Mutex
	entries        map[string]fakeTransitEntry // ciphertext body id -> entry
	currentVersion int
	revoked        map[int]bool
	requests       []fakeTransitRequest
	failOp         map[string]int
	slow           time.Duration
	oversized      bool
	malformed      bool
	versionLie     bool
	missingVersion bool
}

type fakeTransitEntry struct {
	dek     []byte
	version int
}

func newFakeTransitServer() *fakeTransitServer {
	return &fakeTransitServer{
		entries:        map[string]fakeTransitEntry{},
		currentVersion: 1,
		revoked:        map[int]bool{},
		failOp:         map[string]int{},
	}
}

func (f *fakeTransitServer) rotate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.currentVersion++
}

func (f *fakeTransitServer) revoke(version int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[version] = true
}

func (f *fakeTransitServer) counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, r := range f.requests {
		out[r.op]++
	}
	return out
}

func (f *fakeTransitServer) handler(w http.ResponseWriter, r *http.Request) {
	if f.slow > 0 {
		time.Sleep(f.slow)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	keys := make([]string, 0, len(req))
	for k := range req {
		keys = append(keys, k)
	}
	op := ""
	rest := strings.TrimPrefix(r.URL.Path, "/v1/")
	parts := strings.Split(rest, "/")
	if len(parts) >= 2 {
		op = parts[len(parts)-2]
	}
	f.requests = append(f.requests, fakeTransitRequest{
		op: op, path: r.URL.Path,
		token: r.Header.Get("X-Vault-Token"), namespace: r.Header.Get("X-Vault-Namespace"),
		bodyKeys: keys,
	})
	if status, ok := f.failOp[op]; ok {
		http.Error(w, "injected failure", status)
		return
	}
	if f.oversized {
		_, _ = w.Write(make([]byte, maxTransitResponse+10))
		return
	}
	if f.malformed {
		_, _ = w.Write([]byte(`{"data":`))
		return
	}
	switch op {
	case "encrypt":
		plain, err := base64.StdEncoding.DecodeString(req["plaintext"].(string))
		if err != nil || len(plain) != 32 {
			http.Error(w, "bad plaintext", http.StatusBadRequest)
			return
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("entry-%d", len(f.entries)+1)))
		ct := fmt.Sprintf("vault:v%d:%s", f.currentVersion, id)
		f.entries[ct] = fakeTransitEntry{dek: plain, version: f.currentVersion}
		kv := f.currentVersion
		if f.versionLie {
			kv = f.currentVersion + 1
		}
		data := map[string]any{"ciphertext": ct}
		if !f.missingVersion {
			data["key_version"] = kv
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case "decrypt":
		ct := req["ciphertext"].(string)
		ent, ok := f.entries[ct]
		if !ok {
			http.Error(w, "unknown ciphertext", http.StatusBadRequest)
			return
		}
		if f.revoked[ent.version] {
			http.Error(w, "version revoked", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(ent.dek)}})
	case "rewrap":
		ct := req["ciphertext"].(string)
		ent, ok := f.entries[ct]
		if !ok {
			http.Error(w, "unknown ciphertext", http.StatusBadRequest)
			return
		}
		if f.revoked[ent.version] {
			http.Error(w, "version revoked", http.StatusBadRequest)
			return
		}
		if ent.version == f.currentVersion {
			// Already at latest: return the identical ciphertext
			// (ADR 0007 §7.3 no-op check). If real Vault always
			// re-randomizes, the gateway still behaves correctly;
			// it just rewrites instead of skipping.
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.currentVersion}})
			return
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("entry-%d", len(f.entries)+1)))
		next := fmt.Sprintf("vault:v%d:%s", f.currentVersion, id)
		f.entries[next] = fakeTransitEntry{dek: ent.dek, version: f.currentVersion}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": next, "key_version": f.currentVersion}})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func newTransitTestProvider(t *testing.T, srv *httptest.Server) *VaultTransitProvider {
	t.Helper()
	p, err := NewVaultTransitProvider(srv.URL, "test-token", "", "transit", "nexaroute", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVaultTransitContractAndRoundTrip(t *testing.T) {
	fake := newFakeTransitServer()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	p := newTransitTestProvider(t, srv)
	dek := []byte("01234567890123456789012345678901")
	wrapped, version, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 || !strings.HasPrefix(wrapped, "vault:v1:") {
		t.Fatalf("unexpected wrapped/version: %q %d", wrapped, version)
	}
	last := fake.requests[len(fake.requests)-1]
	if last.path != "/v1/transit/encrypt/nexaroute" || last.token != "test-token" {
		t.Fatalf("unexpected request path/token: %q %q", last.path, last.token)
	}
	if len(last.bodyKeys) != 1 || last.bodyKeys[0] != "plaintext" {
		t.Fatalf("encrypt request must carry exactly {plaintext}, got %v", last.bodyKeys)
	}
	got, v, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil || string(got) != string(dek) || v != 1 {
		t.Fatalf("unwrap: %q %d %v", got, v, err)
	}
	fake.rotate()
	rewrapped, v, err := p.RewrapDEK(context.Background(), wrapped)
	if err != nil || rewrapped == wrapped || v != 2 {
		t.Fatalf("rewrap: %q %d %v", rewrapped, v, err)
	}
	got, v, err = p.UnwrapDEK(context.Background(), rewrapped)
	if err != nil || string(got) != string(dek) || v != 2 {
		t.Fatalf("unwrap after rewrap: %q %d %v", got, v, err)
	}
	// Rewrap of an already-latest ciphertext is the identical value.
	same, _, err := p.RewrapDEK(context.Background(), rewrapped)
	if err != nil || same != rewrapped {
		t.Fatalf("already-latest rewrap must be identical: %q %v", same, err)
	}
	// Revoked version fails closed on decrypt and rewrap.
	fake.revoke(1)
	if _, _, err := p.UnwrapDEK(context.Background(), wrapped); err == nil {
		t.Fatal("revoked version unwrapped")
	}
	if _, _, err := p.RewrapDEK(context.Background(), wrapped); err == nil {
		t.Fatal("revoked version rewrapped")
	}
}

func TestVaultTransitMultiSegmentMountAndNamespace(t *testing.T) {
	fake := newFakeTransitServer()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	p, err := NewVaultTransitProvider(srv.URL, "tok", "team-a/prod", "transit/sub", "nexaroute", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dek := []byte("01234567890123456789012345678901")
	if _, _, err := p.WrapDEK(context.Background(), dek); err != nil {
		t.Fatal(err)
	}
	last := fake.requests[len(fake.requests)-1]
	if last.path != "/v1/transit/sub/encrypt/nexaroute" {
		t.Fatalf("multi-segment mount path wrong: %q", last.path)
	}
	if last.namespace != "team-a/prod" {
		t.Fatalf("namespace header wrong: %q", last.namespace)
	}
	want := TransitKeyRefHash("team-a/prod", "transit/sub", "nexaroute")
	if p.KeyRefHash() != want {
		t.Fatalf("key ref mismatch: %q != %q", p.KeyRefHash(), want)
	}
}

func TestVaultTransitKeyRefHashIsCanonical(t *testing.T) {
	canonical := "vault-transit|namespace=|mount=transit|key=nexaroute"
	sum := sha256.Sum256([]byte(canonical))
	want := hex.EncodeToString(sum[:])
	if got := TransitKeyRefHash("", "transit", "nexaroute"); got != want {
		t.Fatalf("canonical key ref mismatch: %q != %q", got, want)
	}
	if got := TransitKeyRefHash("ns", "transit", "nexaroute"); got == want {
		t.Fatal("namespace must change the key reference")
	}
	if got := TransitKeyRefHash("", "transit", "other"); got == want {
		t.Fatal("key name must change the key reference")
	}
}

func TestVaultTransitErrorClasses(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	cases := []struct {
		status int
		class  string
		op     string
	}{
		{http.StatusForbidden, ClassTransitForbidden, "encrypt"},
		{http.StatusUnauthorized, ClassTransitForbidden, "encrypt"},
		{http.StatusNotFound, ClassTransitKeyNotFound, "decrypt"},
		{http.StatusBadRequest, ClassTransitInvalidCiphertext, "decrypt"},
		{http.StatusInternalServerError, ClassTransitUnavailable, "encrypt"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "secret body that must be discarded", tc.status)
			}))
			defer srv.Close()
			p, _ := NewVaultTransitProvider(srv.URL, "TOKEN-CANARY", "", "transit", "nexaroute", time.Second)
			var err error
			if tc.op == "encrypt" {
				_, _, err = p.WrapDEK(context.Background(), dek)
			} else {
				_, _, err = p.UnwrapDEK(context.Background(), "vault:v1:AAAA")
			}
			var terr *TransitError
			if !errors.As(err, &terr) || terr.Class != tc.class {
				t.Fatalf("expected class %s, got %v", tc.class, err)
			}
			if strings.Contains(err.Error(), "secret body") || strings.Contains(err.Error(), "TOKEN-CANARY") {
				t.Fatalf("error leaked sensitive material: %v", err)
			}
		})
	}
}

func TestVaultTransitRetriesOnly429And503(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	t.Run("429 retries then succeeds", func(t *testing.T) {
		fake := newFakeTransitServer()
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if attempts.Add(1) <= 2 {
				http.Error(w, "slow down", http.StatusTooManyRequests)
				return
			}
			fake.handler(w, r)
		}))
		defer srv.Close()
		p := newTransitTestProvider(t, srv)
		if _, _, err := p.WrapDEK(context.Background(), dek); err != nil {
			t.Fatal(err)
		}
		if attempts.Load() != 3 {
			t.Fatalf("expected 3 attempts, got %d", attempts.Load())
		}
	})
	t.Run("503 retries exhausted", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			http.Error(w, "down", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		p := newTransitTestProvider(t, srv)
		_, _, err := p.WrapDEK(context.Background(), dek)
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitUnavailable {
			t.Fatalf("expected transit_unavailable, got %v", err)
		}
		if attempts.Load() != 3 {
			t.Fatalf("expected 3 attempts (initial + 2 retries), got %d", attempts.Load())
		}
	})
	t.Run("403 is not retried", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			http.Error(w, "denied", http.StatusForbidden)
		}))
		defer srv.Close()
		p := newTransitTestProvider(t, srv)
		if _, _, err := p.WrapDEK(context.Background(), dek); err == nil {
			t.Fatal("403 accepted")
		}
		if attempts.Load() != 1 {
			t.Fatalf("403 must not be retried, got %d attempts", attempts.Load())
		}
	})
}

func TestVaultTransitFailuresAreRedactedAndBounded(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "token", "", "transit", "nexaroute", 20*time.Millisecond)
		start := time.Now()
		_, _, err := p.WrapDEK(context.Background(), dek)
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
		}
	})
	t.Run("body cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(make([]byte, maxTransitResponse+10))
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "token", "", "transit", "nexaroute", time.Second)
		_, _, err := p.WrapDEK(context.Background(), dek)
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitBadResponse {
			t.Fatalf("oversized response must be transit_bad_response, got %v", err)
		}
	})
	t.Run("redirect rejected and not followed", func(t *testing.T) {
		var followed atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
		defer target.Close()
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer redirector.Close()
		p, _ := NewVaultTransitProvider(redirector.URL, "token", "", "transit", "nexaroute", time.Second)
		_, _, err := p.WrapDEK(context.Background(), dek)
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitBadResponse {
			t.Fatalf("redirect must be transit_bad_response, got %v", err)
		}
		if followed.Load() != 0 {
			t.Fatal("redirect was followed")
		}
	})
	t.Run("malformed response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":`))
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "token", "", "transit", "nexaroute", time.Second)
		if _, _, err := p.WrapDEK(context.Background(), dek); err == nil {
			t.Fatal("malformed response accepted")
		}
	})
	t.Run("version lie and missing version", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			lie, missing bool
		}{
			{"version lie", true, false},
			{"missing version", false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fake := newFakeTransitServer()
				fake.versionLie = tc.lie
				fake.missingVersion = tc.missing
				srv := httptest.NewServer(http.HandlerFunc(fake.handler))
				defer srv.Close()
				p := newTransitTestProvider(t, srv)
				if _, _, err := p.WrapDEK(context.Background(), dek); err == nil {
					t.Fatal("inconsistent encrypt response accepted")
				}
			})
		}
	})
}

func TestVaultTransitConfigurationValidation(t *testing.T) {
	cases := []struct {
		name, addr, token, ns, mount, key string
		timeout                           time.Duration
	}{
		{"missing addr", "", "token", "", "transit", "nexaroute", time.Second},
		{"missing token", "https://vault.example", "", "", "transit", "nexaroute", time.Second},
		{"bad scheme", "ftp://vault.example", "token", "", "transit", "nexaroute", time.Second},
		{"cleartext remote", "http://vault.example", "token", "", "transit", "nexaroute", time.Second},
		{"userinfo", "https://user@vault.example", "token", "", "transit", "nexaroute", time.Second},
		{"query", "https://vault.example?x=1", "token", "", "transit", "nexaroute", time.Second},
		{"slash key", "https://vault.example", "token", "", "transit", "a/b", time.Second},
		{"dot mount segment", "https://vault.example", "token", "", "transit/..", "nexaroute", time.Second},
		{"empty mount segment", "https://vault.example", "token", "", "transit//x", "nexaroute", time.Second},
		{"bad namespace", "https://vault.example", "token", "a b", "transit", "nexaroute", time.Second},
		{"excessive timeout", "https://vault.example", "token", "", "transit", "nexaroute", maxTransitTimeout + time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewVaultTransitProvider(tc.addr, tc.token, tc.ns, tc.mount, tc.key, tc.timeout); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if _, err := NewVaultTransitProvider("http://localhost:8200", "token", "", "transit", "nexaroute", 0); err != nil {
		t.Fatalf("loopback with default timeout rejected: %v", err)
	}
}

func TestConfiguredEnvelopeSelection(t *testing.T) {
	// Gate, selection, conflicts, and token-file rules (ADR 0007 §10).
	set := func(t *testing.T, kv map[string]string) {
		t.Helper()
		for _, name := range []string{
			"NEXAROUTE_KEY_PROVIDER", "NEXAROUTE_TRANSIT_ENABLED", "VAULT_ADDR",
			"VAULT_TOKEN", "VAULT_TOKEN_FILE", "NEXAROUTE_TRANSIT_MOUNT",
			"NEXAROUTE_TRANSIT_KEY", "VAULT_NAMESPACE",
			"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE",
			"NEXAROUTE_KEY_PROVIDER_ENV", "NEXAROUTE_KEY_PROVIDER_COMMAND",
			"NEXAROUTE_KEY_PROVIDER_REF",
		} {
			t.Setenv(name, "")
		}
		for k, v := range kv {
			t.Setenv(k, v)
		}
	}
	base := map[string]string{
		"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true",
		"VAULT_ADDR": "http://127.0.0.1:8200", "VAULT_TOKEN": "tok",
		"NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "nexaroute",
	}
	t.Run("valid selection", func(t *testing.T) {
		set(t, base)
		p, enabled, err := ConfiguredEnvelope()
		if err != nil || !enabled || p == nil {
			t.Fatalf("valid selection rejected: enabled=%v err=%v", enabled, err)
		}
	})
	t.Run("gate off is not enabled", func(t *testing.T) {
		kv := map[string]string{}
		for k, v := range base {
			kv[k] = v
		}
		kv["NEXAROUTE_TRANSIT_ENABLED"] = "false"
		set(t, kv)
		_, enabled, err := ConfiguredEnvelope()
		var terr *TransitError
		if err == nil || !errors.As(err, &terr) || terr.Class != ClassTransitNotEnabled || enabled {
			t.Fatalf("gate-off must be transit_not_enabled, got enabled=%v err=%v", enabled, err)
		}
	})
	t.Run("gate garbage is config error", func(t *testing.T) {
		kv := map[string]string{}
		for k, v := range base {
			kv[k] = v
		}
		kv["NEXAROUTE_TRANSIT_ENABLED"] = "yes"
		set(t, kv)
		_, _, err := ConfiguredEnvelope()
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitConfigInvalid {
			t.Fatalf("garbage gate must be config_invalid, got %v", err)
		}
	})
	t.Run("lone gate flag errors", func(t *testing.T) {
		set(t, map[string]string{"NEXAROUTE_TRANSIT_ENABLED": "true"})
		if _, _, err := ConfiguredEnvelope(); err == nil {
			t.Fatal("lone gate flag accepted")
		}
	})
	t.Run("conflicts error", func(t *testing.T) {
		for _, conflict := range []string{
			"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE",
			"NEXAROUTE_KEY_PROVIDER_ENV", "NEXAROUTE_KEY_PROVIDER_COMMAND",
			"NEXAROUTE_KEY_PROVIDER_REF",
		} {
			kv := map[string]string{}
			for k, v := range base {
				kv[k] = v
			}
			kv[conflict] = "x"
			set(t, kv)
			_, _, err := ConfiguredEnvelope()
			var terr *TransitError
			if !errors.As(err, &terr) || terr.Class != ClassTransitConfigInvalid {
				t.Fatalf("conflict %s must be config_invalid, got %v", conflict, err)
			}
		}
	})
	t.Run("both token sources error", func(t *testing.T) {
		kv := map[string]string{}
		for k, v := range base {
			kv[k] = v
		}
		kv["VAULT_TOKEN_FILE"] = "/tmp/x"
		set(t, kv)
		if _, _, err := ConfiguredEnvelope(); err == nil {
			t.Fatal("both token sources accepted")
		}
	})
	t.Run("missing mount and key error", func(t *testing.T) {
		kv := map[string]string{}
		for k, v := range base {
			kv[k] = v
		}
		delete(kv, "NEXAROUTE_TRANSIT_MOUNT")
		delete(kv, "NEXAROUTE_TRANSIT_KEY")
		set(t, kv)
		if _, _, err := ConfiguredEnvelope(); err == nil {
			t.Fatal("missing mount/key accepted (no defaults in the revised design)")
		}
	})
	t.Run("token file mode and whitespace", func(t *testing.T) {
		dir := t.TempDir()
		good := dir + "/tok"
		if err := os.WriteFile(good, []byte("file-token\n"), 0600); err != nil {
			t.Fatal(err)
		}
		kv := map[string]string{}
		for k, v := range base {
			kv[k] = v
		}
		kv["VAULT_TOKEN"] = ""
		kv["VAULT_TOKEN_FILE"] = good
		set(t, kv)
		if _, enabled, err := ConfiguredEnvelope(); err != nil || !enabled {
			t.Fatalf("valid token file rejected: %v", err)
		}
		loose := dir + "/loose"
		if err := os.WriteFile(loose, []byte("file-token\n"), 0644); err != nil {
			t.Fatal(err)
		}
		kv["VAULT_TOKEN_FILE"] = loose
		set(t, kv)
		if _, _, err := ConfiguredEnvelope(); err == nil {
			t.Fatal("token file with mode 0644 accepted")
		}
		spaced := dir + "/spaced"
		if err := os.WriteFile(spaced, []byte(" file-token\n"), 0600); err != nil {
			t.Fatal(err)
		}
		kv["VAULT_TOKEN_FILE"] = spaced
		set(t, kv)
		if _, _, err := ConfiguredEnvelope(); err == nil {
			t.Fatal("token file with leading whitespace accepted")
		}
	})
}

func TestVaultTransitKeyVersionErrorsAreClassified(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	versionBodies := []string{
		`{"errors":["ciphertext or signature version is disallowed by policy (too old)"]}`,
		`{"errors":["invalid ciphertext: version is too new"]}`,
		`{"errors":["no such key version"]}`,
		`{"errors":["encryption key version 2 is not available for decryption"]}`,
		`{"errors":["key version 2 has been deleted"]}`,
		`{"errors":["key has been deleted"]}`,
		`{"errors":["key is disabled"]}`,
	}
	for _, body := range versionBodies {
		for _, op := range []string{"decrypt", "rewrap"} {
			t.Run(op+"/"+body, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(body))
				}))
				defer srv.Close()
				p, err := NewVaultTransitProvider(srv.URL, "TOKEN-CANARY", "", "transit", "nexaroute", time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if op == "decrypt" {
					_, _, err = p.UnwrapDEK(context.Background(), "vault:v1:AAAA")
				} else {
					_, _, err = p.RewrapDEK(context.Background(), "vault:v1:AAAA")
				}
				var terr *TransitError
				if !errors.As(err, &terr) || terr.Class != ClassTransitKeyVersionUnavailable {
					t.Fatalf("version failure must be transit_key_version_unavailable, got %v", err)
				}
				if strings.Contains(err.Error(), "disallowed") || strings.Contains(err.Error(), "too new") ||
					strings.Contains(err.Error(), "has been deleted") || strings.Contains(err.Error(), "is disabled") ||
					strings.Contains(err.Error(), "TOKEN-CANARY") {
					t.Fatalf("error leaked the Vault body or token: %v", err)
				}
			})
		}
	}
	// A tampered/invalid ciphertext is not a key-version case.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":["ciphertext or signature is not valid"]}`))
	}))
	defer srv.Close()
	p, err := NewVaultTransitProvider(srv.URL, "tok", "", "transit", "nexaroute", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.UnwrapDEK(context.Background(), "vault:v1:AAAA"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	} else {
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitInvalidCiphertext {
			t.Fatalf("tampered ciphertext must stay transit_invalid_ciphertext, got %v", err)
		}
	}
	// Encrypt 400s are never key-version decrypt cases.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":["key version is too new"]}`))
	}))
	defer srv2.Close()
	p2, err := NewVaultTransitProvider(srv2.URL, "tok", "", "transit", "nexaroute", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p2.WrapDEK(context.Background(), dek); err == nil {
		t.Fatal("encrypt 400 accepted")
	} else {
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitInvalidCiphertext {
			t.Fatalf("encrypt 400 must stay transit_invalid_ciphertext, got %v", err)
		}
	}
}

func TestVaultTransitRejectsOversizedWrappedInputBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := newTransitTestProvider(t, srv)
	oversized := "vault:v1:" + strings.Repeat("A", 4097)
	if _, _, err := p.UnwrapDEK(context.Background(), oversized); err == nil {
		t.Fatal("oversized wrapped DEK accepted for decrypt")
	}
	if _, _, err := p.RewrapDEK(context.Background(), oversized); err == nil {
		t.Fatal("oversized wrapped DEK accepted for rewrap")
	}
	if calls.Load() != 0 {
		t.Fatalf("oversized wrapped input reached Vault (%d calls)", calls.Load())
	}
}

func TestVaultTransitOperationContextBudgetAndRetries(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	t.Run("overall context timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
		}))
		defer srv.Close()
		p, err := NewVaultTransitProvider(srv.URL, "tok", "", "transit", "nexaroute", 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, err = p.WrapDEK(ctx, dek)
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitUnavailable {
			t.Fatalf("context timeout must be transit_unavailable, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("overall context budget not enforced: %s", elapsed)
		}
	})
	t.Run("retry wait respects context", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "retry later", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		p, err := NewVaultTransitProvider(srv.URL, "tok", "", "transit", "nexaroute", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if _, _, err := p.WrapDEK(ctx, dek); err == nil {
			t.Fatal("exhausted budget accepted")
		}
		if calls.Load() != 1 {
			t.Fatalf("retry ran after the overall budget expired: %d calls", calls.Load())
		}
	})
}

// TestVaultTransitPerRequestTimeoutAndCancel pins ADR 0007 §8.3 for the
// Transit provider itself: a Vault that never answers must fail closed
// within the per-request timeout instead of hanging the gateway, and a
// caller cancel must abort a hanging request. No earlier test exercised
// the Transit provider against a hanging server: the fake's slow field
// was never set, and the only slow-server test covers the KV helper, a
// different path. Timeouts are not retried (doOnce marks client errors
// non-retryable), so exactly one attempt is expected.
func TestVaultTransitPerRequestTimeoutAndCancel(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	hang := func(attempts *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if attempts != nil {
				attempts.Add(1)
			}
			// Hang well beyond every client-side bound below
			// (200 ms timeout, 100 ms cancel): the client side must
			// fail closed long before this returns. Teardown waits
			// for the handler because a POST handler that never
			// reads or writes never observes the disconnect, so the
			// hang is kept short (2 s) to bound total test time.
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
	}
	checkUnavailable := func(t *testing.T, err error) {
		t.Helper()
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitUnavailable {
			t.Fatalf("hanging Vault must be transit_unavailable, got %v", err)
		}
		if strings.Contains(err.Error(), "test-token") {
			t.Fatalf("error leaked the token: %v", err)
		}
	}

	t.Run("per-request timeout fails closed without retry", func(t *testing.T) {
		var attempts atomic.Int32
		srv := hang(&attempts)
		defer srv.Close()
		p, err := NewVaultTransitProvider(srv.URL, "test-token", "", "transit", "nexaroute", 200*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, _, err = p.WrapDEK(context.Background(), dek)
		elapsed := time.Since(start)
		checkUnavailable(t, err)
		if attempts.Load() != 1 {
			t.Fatalf("timeout must not be retried, got %d attempts", attempts.Load())
		}
		if elapsed > 5*time.Second {
			t.Fatalf("per-request timeout took too long: %v", elapsed)
		}
	})

	t.Run("caller cancel aborts hanging request", func(t *testing.T) {
		srv := hang(nil)
		defer srv.Close()
		p, err := NewVaultTransitProvider(srv.URL, "test-token", "", "transit", "nexaroute", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		_, _, err = p.WrapDEK(ctx, dek)
		elapsed := time.Since(start)
		checkUnavailable(t, err)
		if elapsed > 5*time.Second {
			t.Fatalf("cancelled request took too long: %v", elapsed)
		}
	})
}
