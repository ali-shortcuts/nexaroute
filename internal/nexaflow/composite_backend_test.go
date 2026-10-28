package nexaflow

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// ResolveSelector validates against the gateway's own /v1/models list:
// listed selectors resolve, unlisted ones fail closed before any chat.
func TestGatewayClientResolveSelector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"team/coding"},{"id":"prov/model"}]}`)
	}))
	defer srv.Close()
	client := &GatewayClient{BaseURL: srv.URL}
	info, err := client.ResolveSelector(context.Background(), "team/coding")
	if err != nil || info.Kind != "route" || info.ResolvedModel != "team/coding" {
		t.Fatalf("selector = %+v, err = %v", info, err)
	}
	if _, err := client.ResolveSelector(context.Background(), "no/such"); err == nil {
		t.Fatal("unlisted selector resolved, want selector_not_found")
	}
}

// ResolveSecretRef fails closed with no resolver, and resolves through
// the configured resolver otherwise. The value is returned to the
// caller (the backend), never logged or stored by this package.
func TestGatewayClientResolveSecretRef(t *testing.T) {
	bare := &GatewayClient{BaseURL: "http://127.0.0.1:9"}
	if _, err := bare.ResolveSecretRef("x"); err == nil {
		t.Fatal("secret resolved with no resolver wired")
	}
	wired := &GatewayClient{BaseURL: "http://127.0.0.1:9", SecretResolver: func(ref string) (string, error) {
		if ref == "api-token" {
			return "tok-123", nil
		}
		return "", fmt.Errorf("unknown ref")
	}}
	if got, err := wired.ResolveSecretRef("api-token"); err != nil || got != "tok-123" {
		t.Fatalf("secret = %q, err = %v", got, err)
	}
}

// CompositeBackend delegates each port to the subsystem that owns it
// and fails closed on any unwired port instead of faking a result.
func TestCompositeBackendDelegation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"model":"vendor/model","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"team/coding"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	empty := &CompositeBackend{}
	ctx := context.Background()
	if _, err := empty.Chat(ctx, ChatRequest{}); err == nil {
		t.Fatal("chat succeeded with no gateway wired")
	}
	if _, err := empty.ProviderHealth(ctx, "x"); err == nil {
		t.Fatal("health succeeded with no health backend wired")
	}
	if _, err := empty.UsageSnapshot(ctx); err == nil {
		t.Fatal("usage succeeded with no cost backend wired")
	}
	if _, err := empty.RunEvalReplay(ctx, "coding", "p/m", nil); err == nil {
		t.Fatal("eval succeeded with no eval backend wired")
	}

	full := &CompositeBackend{
		Gateway:      &GatewayClient{BaseURL: srv.URL},
		HealthCompat: &DirectHealthCompatBackend{Health: health.New(3, time.Minute), CompatStore: compat.NewStore()},
		Cost:         &DirectCostBackend{Tracker: usage.New(), Ledger: usage.NewBudgetLedger()},
		Eval:         &ReplayEvalBackend{},
	}
	if _, err := full.ResolveSelector(ctx, "team/coding"); err != nil {
		t.Fatalf("composite selector: %v", err)
	}
	chat, err := full.Chat(ctx, ChatRequest{Selector: "team/coding", Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil || chat.Text != "hello" {
		t.Fatalf("composite chat = %+v, err = %v", chat, err)
	}
	h, err := full.ProviderHealth(ctx, "prov/model")
	if err != nil || h.Status != "unknown" {
		t.Fatalf("composite health = %+v, err = %v", h, err)
	}
	usageSnap, err := full.UsageSnapshot(ctx)
	if err != nil {
		t.Fatalf("composite usage: %v", err)
	}
	if usageSnap.TotalRequests != 0 {
		t.Fatalf("fresh tracker requests = %d, want 0", usageSnap.TotalRequests)
	}
	budget, err := full.BudgetSnapshot(ctx, "team")
	if err != nil || budget.Committed != 0 {
		t.Fatalf("composite budget = %+v, err = %v", budget, err)
	}
}
