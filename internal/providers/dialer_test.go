package providers

import (
	"testing"
	"time"
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
