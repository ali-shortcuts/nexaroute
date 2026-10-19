package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// A bare gateway must report the conservative truth: local topology,
// per-instance runtime state, and explicit warnings — never implied
// global budgets or cooldowns (#434).
func TestDeploymentReportDefaultIsPerInstance(t *testing.T) {
	cfg := config.Config{}
	cfg.ApplyDefaults()
	r := deploymentReportForConfig(cfg)
	if r.StoreTopology != "local" {
		t.Fatalf("topology = %q, want local", r.StoreTopology)
	}
	if r.ConfigSnapshot != ScopePerInstance || r.BreakerCooldown != ScopePerInstance ||
		r.QuotaReservations != ScopePerInstance || r.VideoJobsLedger != ScopePerInstance {
		t.Fatalf("default scopes must be per-instance: %+v", r)
	}
	if r.AuditSecurityStore != ScopeSharedHost {
		t.Fatalf("default bbolt audit store scope = %q, want shared-host", r.AuditSecurityStore)
	}
	joined := strings.Join(r.Warnings, " ")
	for _, want := range []string{"control plane disabled", "breaker/cooldown", "per-instance", "video jobs"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings missing %q: %v", want, r.Warnings)
		}
	}
}

// A shared control-plane backend must be reported as shared for the
// config snapshot only — breaker, quota and video state stay
// per-instance because this build does not synchronize them.
func TestDeploymentReportSharedControlPlane(t *testing.T) {
	for _, backend := range []string{"postgres", "redis"} {
		cfg := config.Config{}
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = backend
		cfg.ApplyDefaults()
		r := deploymentReportForConfig(cfg)
		if r.StoreTopology != "shared" || r.ConfigSnapshot != ScopeShared {
			t.Fatalf("backend %s: topology=%q config=%q, want shared/shared", backend, r.StoreTopology, r.ConfigSnapshot)
		}
		if r.BreakerCooldown != ScopePerInstance || r.QuotaReservations != ScopePerInstance {
			t.Fatalf("backend %s: breaker/quota must stay per-instance: %+v", backend, r)
		}
	}
}

func TestDeploymentReportFileAndMemoryBackends(t *testing.T) {
	cfg := config.Config{}
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.Backend = "file"
	cfg.ApplyDefaults()
	r := deploymentReportForConfig(cfg)
	if r.StoreTopology != "shared-host" || r.ConfigSnapshot != ScopeSharedHost {
		t.Fatalf("file backend: topology=%q config=%q, want shared-host/shared-host", r.StoreTopology, r.ConfigSnapshot)
	}

	cfg.ControlPlane.Backend = "memory"
	r = deploymentReportForConfig(cfg)
	if r.StoreTopology != "test-only" {
		t.Fatalf("memory backend: topology=%q, want test-only", r.StoreTopology)
	}
	if !strings.Contains(strings.Join(r.Warnings, " "), "test-only") {
		t.Fatalf("memory backend must warn it is test-only: %v", r.Warnings)
	}

	cfg2 := config.Config{}
	cfg2.Admin.SecurityStoreBackend = "redis"
	cfg2.ApplyDefaults()
	r2 := deploymentReportForConfig(cfg2)
	if r2.AuditSecurityStore != ScopeShared {
		t.Fatalf("redis security store: scope=%q, want shared", r2.AuditSecurityStore)
	}
}

// The report must reach operators through the hello endpoint.
func TestHelloExposesDeploymentReport(t *testing.T) {
	cfg := config.Config{}
	cfg.ApplyDefaults()
	s := newBenchServer(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/hello", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hello status = %d, want 200", rec.Code)
	}
	var body struct {
		Deployment DeploymentReport `json:"deployment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("hello body is not JSON: %v", err)
	}
	if body.Deployment.StoreTopology != "local" || body.Deployment.BreakerCooldown != ScopePerInstance {
		t.Fatalf("hello deployment report wrong: %+v", body.Deployment)
	}
}
