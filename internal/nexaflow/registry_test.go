package nexaflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

func TestRegistryNodeCount(t *testing.T) {
	reg := DefaultRegistry()
	if got := len(reg.All()); got != 23 {
		t.Fatalf("node count = %d, want 23", got)
	}
	ids := map[string]bool{}
	for _, spec := range reg.All() {
		if ids[spec.ID] {
			t.Fatalf("duplicate node id %q", spec.ID)
		}
		ids[spec.ID] = true
		if spec.Version != "v1" {
			t.Fatalf("node %q version = %q, want v1", spec.ID, spec.Version)
		}
	}
}

func TestLookupRejectsUnknownVersion(t *testing.T) {
	reg := DefaultRegistry()
	if _, err := reg.Lookup("nexaroute.llm.chat", "v2"); err == nil {
		t.Fatal("v2 lookup accepted")
	}
	if _, err := reg.Lookup("nexaroute.nope", "v1"); err == nil {
		t.Fatal("unknown node accepted")
	}
}

func TestExecutePermissionAndAnonymousDenial(t *testing.T) {
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.cost.usage", "v1")
	if _, err := reg.Execute(context.Background(), spec, authz.Identity{}, nil, nil, newFakeBackend(), false); err == nil {
		t.Fatal("anonymous principal accepted")
	}
	// Viewer lacks video.manage, so the video node must deny it.
	videoSpec, _ := reg.Lookup("nexaroute.video.status", "v1")
	if _, err := reg.Execute(context.Background(), videoSpec, viewerPrincipal(), map[string]any{"job_id": "job-1"}, nil, newFakeBackend(), false); err == nil {
		t.Fatal("viewer without video.manage accepted")
	} else if !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("viewer error = %v, want permission_denied", err)
	}
}

func TestExecuteUnknownKeyRejected(t *testing.T) {
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.cost.usage", "v1")
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(), map[string]any{"surprise": 1}, nil, newFakeBackend(), false); err == nil {
		t.Fatal("unknown input key accepted")
	}
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(), nil, map[string]any{"surprise": 1}, newFakeBackend(), false); err == nil {
		t.Fatal("unknown config key accepted")
	}
}

func TestExecutePolicyRequired(t *testing.T) {
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.llm.chat", "v1")
	_, err := reg.Execute(context.Background(), spec, ownerPrincipal(),
		map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding"}, newFakeBackend(), false)
	if err == nil || !strings.Contains(err.Error(), "policy_required") {
		t.Fatalf("unapproved billable node error = %v, want policy_required", err)
	}
}

func TestExecuteTimeoutAndCancellation(t *testing.T) {
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.control.wait", "v1")
	// Timeout: node default is 5m, so use a caller context with a short deadline instead.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := reg.Execute(ctx, spec, ownerPrincipal(), map[string]any{"duration_ms": float64(60000)}, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "cancelled") && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timed-out wait error = %v", err)
	}
	// Cancellation: cancel mid-wait.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel2()
	}()
	_, err = reg.Execute(ctx2, spec, ownerPrincipal(), map[string]any{"duration_ms": float64(60000)}, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled wait error = %v, want cancelled", err)
	}
}

func TestExecuteOutputBoundAndSecretRedaction(t *testing.T) {
	// Output bound: a spec with a tiny output bound must fail, not leak.
	spec := &NodeSpec{
		ID: "test.tiny", Version: "v1", Permission: "",
		Retry:          RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: time.Second, MaxTimeout: time.Second,
		MaxInputBytes: 1024, MaxOutputBytes: 10,
		Run: func(ctx context.Context, rc *RunContext) (map[string]any, error) {
			return map[string]any{"big": strings.Repeat("x", 100)}, nil
		},
	}
	reg, err := NewRegistry([]*NodeSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(), nil, nil, nil, false); err == nil || !strings.Contains(err.Error(), "output_too_large") {
		t.Fatalf("oversize output error = %v", err)
	}
	// Redaction in errors and JSON.
	if got := RedactError(errors.New("failed with Bearer abc123XYZ and more")); !strings.Contains(got, "Bearer [REDACTED]") || strings.Contains(got, "abc123XYZ") {
		t.Fatalf("RedactError = %q", got)
	}
	redacted := RedactJSON(map[string]any{"api_key": "sekret", "nested": map[string]any{"password": "pw"}, "ok": 1})
	if strings.Contains(redacted, "sekret") || strings.Contains(redacted, "pw") {
		t.Fatalf("RedactJSON leaked: %s", redacted)
	}
	if got := RedactError(errors.New(strings.Repeat("x", 600))); len(got) != 512 {
		t.Fatalf("RedactError length = %d, want 512", len(got))
	}
	// Execute redacts secret-keyed output values.
	spec2 := &NodeSpec{
		ID: "test.leak", Version: "v1", Permission: "",
		Retry:          RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: time.Second, MaxTimeout: time.Second,
		MaxInputBytes: 1024, MaxOutputBytes: 4096,
		InputSchema: []Field{{Name: "token", Type: "string"}},
		Run: func(ctx context.Context, rc *RunContext) (map[string]any, error) {
			return map[string]any{"api_key": "should-not-escape"}, nil
		},
	}
	reg2, _ := NewRegistry([]*NodeSpec{spec2})
	out, err := reg2.Execute(context.Background(), spec2, ownerPrincipal(), nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if out["api_key"] != "[REDACTED]" {
		t.Fatalf("output api_key = %v, want [REDACTED]", out["api_key"])
	}
}
