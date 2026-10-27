package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTransitServer struct {
	mu      sync.Mutex
	keys    map[string][]byte // ciphertext -> aad
	version int
	path    string
	token   string
	encReq  map[string]any
	decReq  map[string]any
}

func newFakeTransitServer() *fakeTransitServer {
	return &fakeTransitServer{keys: map[string][]byte{}, version: 1}
}

func (f *fakeTransitServer) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.path = r.URL.Path
	f.token = r.Header.Get("X-Vault-Token")
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	op := strings.TrimPrefix(r.URL.Path, "/v1/transit/")
	switch {
	case strings.HasPrefix(op, "encrypt/"):
		f.encReq = req
		plain, err := base64.StdEncoding.DecodeString(req["plaintext"].(string))
		if err != nil {
			http.Error(w, "bad plaintext", http.StatusBadRequest)
			return
		}
		f.version++
		ct := fmt.Sprintf("vault:v%d:%s", f.version, base64.RawURLEncoding.EncodeToString(plain))
		f.keys[ct] = []byte(req["associated_data"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.version}})
	case strings.HasPrefix(op, "decrypt/"):
		f.decReq = req
		ct := req["ciphertext"].(string)
		aad, ok := f.keys[ct]
		if !ok || string(aad) != req["associated_data"].(string) {
			http.Error(w, "denied", http.StatusBadRequest)
			return
		}
		plain, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ct, fmt.Sprintf("vault:v%d:", f.version)))
		if err != nil {
			http.Error(w, "bad ciphertext", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(plain)}})
	case strings.HasPrefix(op, "rewrap/"):
		ct := req["ciphertext"].(string)
		aad, ok := f.keys[ct]
		if !ok || string(aad) != req["associated_data"].(string) {
			http.Error(w, "denied", http.StatusBadRequest)
			return
		}
		plain, err := base64.RawURLEncoding.DecodeString(strings.SplitN(strings.SplitN(ct, ":", 3)[2], ":", 2)[0])
		if err != nil {
			http.Error(w, "bad ciphertext", http.StatusBadRequest)
			return
		}
		f.version++
		next := fmt.Sprintf("vault:v%d:%s", f.version, base64.RawURLEncoding.EncodeToString(plain))
		f.keys[next] = aad
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": next, "key_version": f.version}})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func newTransitTestProvider(t *testing.T, srv *httptest.Server) *VaultTransitProvider {
	t.Helper()
	p, err := NewVaultTransitProvider(srv.URL, "test-token", "transit", "nexaroute", 2*time.Second)
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
	aad := AAD("provider-a", "api_key")
	dek := []byte("01234567890123456789012345678901")
	wrapped, err := p.WrapDEK(context.Background(), dek, aad)
	if err != nil {
		t.Fatal(err)
	}
	if fake.path != "/v1/transit/encrypt/nexaroute" || fake.token != "test-token" {
		t.Fatalf("unexpected request path/token: %q %q", fake.path, fake.token)
	}
	if fake.encReq["associated_data"] != base64.StdEncoding.EncodeToString(aad) {
		t.Fatal("AAD was not sent to Transit encrypt")
	}
	if !strings.HasPrefix(wrapped, "vault:v") {
		t.Fatalf("unexpected wrapped ciphertext: %q", wrapped)
	}
	got, err := p.UnwrapDEK(context.Background(), wrapped, aad)
	if err != nil || string(got) != string(dek) {
		t.Fatalf("unwrap: %q %v", got, err)
	}
	if fake.path != "/v1/transit/decrypt/nexaroute" {
		t.Fatalf("unexpected decrypt path: %q", fake.path)
	}
	if _, err = p.UnwrapDEK(context.Background(), wrapped, AAD("provider-b", "api_key")); err == nil {
		t.Fatal("wrong AAD unwrapped")
	}
	rewrapped, err := p.RewrapDEK(context.Background(), wrapped, aad)
	if err != nil || rewrapped == wrapped {
		t.Fatalf("rewrap: %q %v", rewrapped, err)
	}
	got, err = p.UnwrapDEK(context.Background(), rewrapped, aad)
	if err != nil || string(got) != string(dek) {
		t.Fatalf("unwrap after rewrap: %q %v", got, err)
	}
}

func TestVaultTransitKeyIDIsStableAndFullHash(t *testing.T) {
	a := VaultTransitKeyID("transit", "nexaroute")
	b := VaultTransitKeyID("transit", "nexaroute")
	c := VaultTransitKeyID("transit", "other")
	if a != b || len(a) != 64 || a == c {
		t.Fatalf("unexpected key ids: %q %q %q", a, b, c)
	}
}

func TestVaultTransitFailuresAreRedactedAndBounded(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	aad := AAD("p", "api_key")
	canary := "TOKEN-CANARY-DO-NOT-LEAK"

	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "secret body "+base64.StdEncoding.EncodeToString(dek), http.StatusForbidden)
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, canary, "transit", "nexaroute", time.Second)
		_, err := p.WrapDEK(context.Background(), dek, aad)
		if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("expected HTTP 403 error, got %v", err)
		}
		if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(dek)) {
			t.Fatalf("error leaked sensitive material: %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "token", "transit", "nexaroute", 20*time.Millisecond)
		start := time.Now()
		_, err := p.WrapDEK(context.Background(), dek, aad)
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
		}
	})

	t.Run("body cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(make([]byte, maxTransitResponse+10))
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "token", "transit", "nexaroute", time.Second)
		if _, err := p.WrapDEK(context.Background(), dek, aad); err == nil || !strings.Contains(err.Error(), "safe size limit") {
			t.Fatalf("oversized response not rejected: %v", err)
		}
	})

	t.Run("redirect rejected", func(t *testing.T) {
		var followed atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
		defer target.Close()
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer redirector.Close()
		p, _ := NewVaultTransitProvider(redirector.URL, "token", "transit", "nexaroute", time.Second)
		if _, err := p.WrapDEK(context.Background(), dek, aad); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
			t.Fatalf("redirect not rejected: %v", err)
		}
		if followed.Load() != 0 {
			t.Fatal("redirect was followed")
		}
	})

	t.Run("malformed and invalid responses", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":`))
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "token", "transit", "nexaroute", time.Second)
		if _, err := p.WrapDEK(context.Background(), dek, aad); err == nil {
			t.Fatal("malformed response accepted")
		}
	})
}

func TestVaultTransitConfigurationValidation(t *testing.T) {
	cases := []struct {
		name, addr, token, mount, key string
		timeout                       time.Duration
	}{
		{"missing addr", "", "token", "transit", "nexaroute", time.Second},
		{"missing token", "https://vault.example", "", "transit", "nexaroute", time.Second},
		{"bad scheme", "ftp://vault.example", "token", "transit", "nexaroute", time.Second},
		{"cleartext remote", "http://vault.example", "token", "transit", "nexaroute", time.Second},
		{"userinfo", "https://user@vault.example", "token", "transit", "nexaroute", time.Second},
		{"query", "https://vault.example?x=1", "token", "transit", "nexaroute", time.Second},
		{"slash mount", "https://vault.example", "token", "transit/other", "nexaroute", time.Second},
		{"slash key", "https://vault.example", "token", "transit", "a/b", time.Second},
		{"excessive timeout", "https://vault.example", "token", "transit", "nexaroute", maxTransitTimeout + time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewVaultTransitProvider(tc.addr, tc.token, tc.mount, tc.key, tc.timeout); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if _, err := NewVaultTransitProvider("http://localhost:8200", "token", "", "", 0); err != nil {
		t.Fatalf("loopback defaults rejected: %v", err)
	}
}

func TestConfiguredEnvelopeTimeoutValidation(t *testing.T) {
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "vault-transit")
	t.Setenv("NEXAROUTE_TRANSIT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("NEXAROUTE_TRANSIT_TOKEN", "token")
	t.Setenv("NEXAROUTE_TRANSIT_TIMEOUT_MS", "0")
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("zero timeout accepted")
	}
	t.Setenv("NEXAROUTE_TRANSIT_TIMEOUT_MS", "not-a-number")
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("malformed timeout accepted")
	}
	t.Setenv("NEXAROUTE_TRANSIT_TIMEOUT_MS", "1000")
	if _, enabled, err := ConfiguredEnvelope(); err != nil || !enabled {
		t.Fatalf("valid timeout rejected: %v", err)
	}
}
