package providers

import (
	"net/http"
	"strings"
	"testing"
)

// Audit #371 finding F8: the ComfyUI client used the default transport,
// so no egress policy (deny lists, IP blocklist, DNS pinning) applied to
// the operator-configured base URL — https://169.254.169.254/ as
// comfyui_base_url would be dialed. The client must route through the
// egress-guarded transport, and dials to blocked addresses must fail
// fast at the policy instead of reaching the network.
func TestComfyUIClientUsesEgressGuardedTransport(t *testing.T) {
	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:1"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := p.client.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("ComfyUI client transport = %T, want *http.Transport from the egress policy", p.client.Transport)
	}
}

// The audit's exact case: a link-local metadata address as the base URL
// must be refused by the egress policy — at construction time, before
// any dial is ever attempted.
func TestComfyUIRefusesBlockedBaseURL(t *testing.T) {
	_, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "https://169.254.169.254/"), 1024)
	if err == nil {
		t.Fatal("ComfyUI accepted blocked metadata address https://169.254.169.254/ as base URL")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("err = %q, want egress blocklist refusal", err.Error())
	}
}

// A legitimate loopback ComfyUI endpoint must keep working through the
// guarded transport.
func TestComfyUIAllowsLoopbackThroughGuardedTransport(t *testing.T) {
	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:1"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	// The dial fails (nothing listens), but it must fail with a
	// connection error — not an egress-policy refusal.
	_, err = p.client.Get("http://127.0.0.1:1/prompt")
	if err == nil {
		t.Fatal("expected connection failure to empty port")
	}
	if strings.Contains(err.Error(), "blocked") || strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "allow-list") {
		t.Fatalf("loopback dial wrongly refused by egress policy: %v", err)
	}
}
