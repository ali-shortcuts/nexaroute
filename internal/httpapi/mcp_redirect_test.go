package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Audit #371 finding F9: the MCP broker used a plain &http.Client{} with
// no redirect policy, so a cross-origin redirect was followed and the
// Authorization header attached from the operator's env secret
// (mcp.go: the auth_env value) was forwarded to the redirect target —
// Go's stdlib strips it only across hostnames, and a same-hostname
// different-port target keeps it. The broker client must refuse
// cross-origin redirects.
func TestMCPBrokerRefusesCrossOriginRedirect(t *testing.T) {
	var targetHits atomic.Int32
	var targetSawAuth atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			targetSawAuth.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	b := newMCPBroker()
	req, err := http.NewRequest(http.MethodPost, source.URL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if err != nil {
		t.Fatal(err)
	}
	// Attached exactly as the broker does from the operator's auth_env secret.
	req.Header.Set("Authorization", "Bearer mcp-operator-secret")
	_, err = b.client.Do(req)
	if err == nil {
		t.Fatal("MCP broker followed a cross-origin redirect: no error returned")
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target was contacted %d times", targetHits.Load())
	}
	if targetSawAuth.Load() {
		t.Fatal("operator Authorization secret was forwarded to the cross-origin redirect target")
	}
}
