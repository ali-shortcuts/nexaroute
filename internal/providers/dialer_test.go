package providers

import (
	"net/http"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestProviderDialerIsBounded(t *testing.T) {
	d := providerDialer()
	if d.Timeout != 10*time.Second {
		t.Fatalf("dial timeout must be 10s, got %v", d.Timeout)
	}
	if d.KeepAlive != 30*time.Second {
		t.Fatalf("dial keep-alive must be 30s, got %v", d.KeepAlive)
	}
	if !d.DualStack {
		t.Fatal("dialer must race A/AAAA resolution (DualStack)")
	}
}

func TestProviderTransportDoesNotCapConnectionsAtSemaphore(t *testing.T) {
	a, err := newHTTPAdapter(config.ProviderConfig{
		ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1",
		MaxConcurrency: 8,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := a.c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", a.c.Transport)
	}
	if tr.MaxConnsPerHost != 0 {
		t.Fatalf("MaxConnsPerHost must be unlimited (semaphore is the gate), got %d", tr.MaxConnsPerHost)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 must be attempted so many sub-agent streams can multiplex")
	}
}
