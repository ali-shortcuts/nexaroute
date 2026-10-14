package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Per-provider key isolation at the gateway level (M3-4B). Two
// providers with distinct synthetic canary credentials serve requests
// through the real HTTP handlers; assertions scan every header each
// upstream actually received and the client-facing response bodies.

type keyIsoUpstream struct {
	mu      sync.Mutex
	headers []http.Header
	fail    atomic.Bool
	leakKey string // when set, the 500 body deliberately contains this key
}

func (u *keyIsoUpstream) handler(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.headers = append(u.headers, r.Header.Clone())
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if u.fail.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		if u.leakKey != "" {
			io.WriteString(w, `{"error":"upstream failure mentioning `+u.leakKey+`"}`)
			return
		}
		io.WriteString(w, `{"error":"upstream failure"}`)
		return
	}
	io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`)
}

func (u *keyIsoUpstream) all() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header(nil), u.headers...)
}

func anyCapturedHeaderContains(hs []http.Header, needle string) bool {
	for _, h := range hs {
		for _, vals := range h {
			for _, v := range vals {
				if strings.Contains(v, needle) {
					return true
				}
			}
		}
	}
	return false
}

const (
	keyIsoAlpha = "canary-gw-alpha"
	keyIsoBravo = "canary-gw-bravo"
)

// keyIsoConfig builds a gateway config: provider A (bearer, key alpha)
// and provider B (x-api-key auth mode, key bravo), each with a private
// model alias and a shared alias where A has routing priority.
func keyIsoConfig(urlA, urlB string, strategy string) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	if strategy != "" {
		cfg.Routing.Strategy = strategy
	}
	mk := func(id, typ, authMode, key, url, upstream string, aliases []string, prio int) config.ProviderConfig {
		return config.ProviderConfig{
			ID: id, Name: id, Type: typ, BaseURL: url, AuthMode: authMode, APIKey: key, Enabled: true,
			ForwardHeaders: []string{"Authorization", "X-API-Key", "X-Goog-API-Key", "X-Custom-Trace"},
			Models:         []config.ModelConfig{{ID: "m", Model: upstream, Aliases: aliases, Enabled: true, Priority: prio}},
		}
	}
	cfg.Providers = []config.ProviderConfig{
		mk("prov-a", "openai_compatible", "bearer", keyIsoAlpha, urlA, "up-a", []string{"model-a", "shared"}, 1),
		mk("prov-b", "openai_compatible", "x-api-key", keyIsoBravo, urlB, "up-b", []string{"model-b", "shared"}, 2),
	}
	return cfg
}

func keyIsoChat(t *testing.T, s *Server, model string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func keyIsoGateways(t *testing.T, strategy string) (*Server, *keyIsoUpstream, *keyIsoUpstream) {
	t.Helper()
	upA := &keyIsoUpstream{}
	srvA := httptest.NewServer(http.HandlerFunc(upA.handler))
	t.Cleanup(srvA.Close)
	upB := &keyIsoUpstream{}
	srvB := httptest.NewServer(http.HandlerFunc(upB.handler))
	t.Cleanup(srvB.Close)
	return testGateway(t, keyIsoConfig(srvA.URL+"/v1", srvB.URL+"/v1", strategy)), upA, upB
}

// Each provider receives only its own credential for its own model.
func TestGatewayRoutesEachProviderOnlyItsOwnKey(t *testing.T) {
	s, upA, upB := keyIsoGateways(t, "")

	rr := keyIsoChat(t, s, "model-a", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("model-a status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = keyIsoChat(t, s, "model-b", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("model-b status=%d body=%s", rr.Code, rr.Body.String())
	}
	seenA, seenB := upA.all(), upB.all()
	if len(seenA) != 1 || len(seenB) != 1 {
		t.Fatalf("expected one request per upstream, A=%d B=%d", len(seenA), len(seenB))
	}
	if got := seenA[0].Get("Authorization"); got != "Bearer "+keyIsoAlpha {
		t.Fatalf("A upstream auth = %q", got)
	}
	if anyCapturedHeaderContains(seenA, keyIsoBravo) {
		t.Fatalf("B key leaked into A request: %v", seenA[0])
	}
	if got := seenB[0].Get("X-Api-Key"); got != keyIsoBravo {
		t.Fatalf("B upstream auth = %q", got)
	}
	if anyCapturedHeaderContains(seenB, keyIsoAlpha) {
		t.Fatalf("A key leaked into B request: %v", seenB[0])
	}
}

// Failover: provider A (priority 1) fails; the retried request to
// provider B must carry B's credential only — A's key must not ride
// along into B's upstream on any header.
func TestGatewayFailoverDoesNotCrossProviderKeys(t *testing.T) {
	s, upA, upB := keyIsoGateways(t, "priority")
	upA.fail.Store(true)

	rr := keyIsoChat(t, s, "shared", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("failover status=%d body=%s", rr.Code, rr.Body.String())
	}
	seenA, seenB := upA.all(), upB.all()
	if len(seenA) == 0 || len(seenB) == 0 {
		t.Fatalf("expected attempts on both upstreams, A=%d B=%d", len(seenA), len(seenB))
	}
	if anyCapturedHeaderContains(seenA, keyIsoBravo) {
		t.Fatalf("B key leaked into A attempt: %v", seenA)
	}
	if anyCapturedHeaderContains(seenB, keyIsoAlpha) {
		t.Fatalf("A key crossed into failover request to B: %v", seenB)
	}
	last := seenB[len(seenB)-1]
	if got := last.Get("X-Api-Key"); got != keyIsoBravo {
		t.Fatalf("failover request to B carried auth %q, want B's own key", got)
	}
	if strings.Contains(rr.Body.String(), keyIsoAlpha) || strings.Contains(rr.Body.String(), keyIsoBravo) {
		t.Fatalf("client response contains a provider key: %s", rr.Body.String())
	}
}

// Client-supplied credential headers cannot contaminate either
// provider, even with the names present in ForwardHeaders; a benign
// listed header still forwards.
func TestGatewayClientCredentialHeadersCannotContaminateProviders(t *testing.T) {
	s, upA, upB := keyIsoGateways(t, "")
	clientHeaders := map[string]string{
		"Authorization":  "Bearer client-canary-auth",
		"X-Api-Key":      "client-canary-apikey",
		"X-Goog-Api-Key": "client-canary-goog",
		"X-Custom-Trace": "trace-1",
	}
	rr := keyIsoChat(t, s, "model-a", clientHeaders)
	if rr.Code != http.StatusOK {
		t.Fatalf("model-a status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = keyIsoChat(t, s, "model-b", clientHeaders)
	if rr.Code != http.StatusOK {
		t.Fatalf("model-b status=%d body=%s", rr.Code, rr.Body.String())
	}
	for name, seen := range map[string][]http.Header{"A": upA.all(), "B": upB.all()} {
		if len(seen) != 1 {
			t.Fatalf("upstream %s saw %d requests", name, len(seen))
		}
		for _, canary := range []string{"client-canary-auth", "client-canary-apikey", "client-canary-goog"} {
			if anyCapturedHeaderContains(seen, canary) {
				t.Fatalf("client %s reached upstream %s: %v", canary, name, seen[0])
			}
		}
		if got := seen[0].Get("X-Custom-Trace"); got != "trace-1" {
			t.Fatalf("benign listed header did not forward to %s: %q", name, got)
		}
	}
	if got := upA.all()[0].Get("Authorization"); got != "Bearer "+keyIsoAlpha {
		t.Fatalf("A auth after client contamination attempt = %q", got)
	}
	if got := upB.all()[0].Get("X-Api-Key"); got != keyIsoBravo {
		t.Fatalf("B auth after client contamination attempt = %q", got)
	}
}

// Error path: when every provider fails, the client-facing error must
// contain neither provider's key — including when an upstream error
// body itself mentions its own provider's key.
func TestGatewayErrorResponseContainsNoProviderKeys(t *testing.T) {
	s, upA, upB := keyIsoGateways(t, "priority")
	upA.fail.Store(true)
	upB.fail.Store(true)
	upA.leakKey = keyIsoAlpha // leaky upstream error body

	rr := keyIsoChat(t, s, "shared", nil)
	if rr.Code < 400 {
		t.Fatalf("expected an error status, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, keyIsoAlpha) {
		t.Fatalf("client error exposes provider A key: %s", body)
	}
	if strings.Contains(body, keyIsoBravo) {
		t.Fatalf("client error exposes provider B key: %s", body)
	}
}
