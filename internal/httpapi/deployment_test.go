package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// A shared control-plane backend shares its own store, but config snapshots
// stay in the local config file. Breaker, quota and video state also stay
// per-instance because this build does not synchronize them.
func TestDeploymentReportSharedControlPlane(t *testing.T) {
	for _, backend := range []string{"postgres", "redis"} {
		cfg := config.Config{}
		cfg.ControlPlane.Enabled = true
		cfg.ControlPlane.Backend = backend
		cfg.ApplyDefaults()
		r := deploymentReportForConfig(cfg)
		if r.StoreTopology != "shared" || r.ConfigSnapshot != ScopePerInstance {
			t.Fatalf("backend %s: topology=%q config=%q, want shared/per-instance", backend, r.StoreTopology, r.ConfigSnapshot)
		}
		if r.BreakerCooldown != ScopePerInstance || r.QuotaReservations != ScopePerInstance {
			t.Fatalf("backend %s: breaker/quota must stay per-instance: %+v", backend, r)
		}
		if !strings.Contains(strings.Join(r.Warnings, " "), "config snapshots are per-instance") {
			t.Fatalf("backend %s: missing local-config warning: %v", backend, r.Warnings)
		}
	}
}

func TestDeploymentReportFileAndMemoryBackends(t *testing.T) {
	cfg := config.Config{}
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.Backend = "file"
	cfg.ApplyDefaults()
	r := deploymentReportForConfig(cfg)
	if r.StoreTopology != "shared-host" || r.ConfigSnapshot != ScopePerInstance {
		t.Fatalf("file backend: topology=%q config=%q, want shared-host/per-instance", r.StoreTopology, r.ConfigSnapshot)
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

// Regression for #599: config snapshots are loaded from and written to the
// local config file, never the control-plane store. The report must therefore
// say per-instance for every control-plane backend, and the test proves the
// store actually in use by observing an admin save land in that local file and
// stay invisible to a second replica's file.
func TestConfigSnapshotReportMatchesLocalStore(t *testing.T) {
	for _, backend := range []string{"file", "postgres", "redis", "memory"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "nexaroute.json")
			cfg := config.Config{}
			cfg.ControlPlane.Enabled = true
			cfg.ControlPlane.Backend = backend
			cfg.ControlPlane.PostgresDSNEnv = "NEXAROUTE_TEST_CONTROL_PLANE_DSN"
			cfg.ControlPlane.RedisURLEnv = "NEXAROUTE_TEST_CONTROL_PLANE_REDIS_URL"
			cfg.ApplyDefaults()
			if err := config.SaveAtomic(configPath, cfg); err != nil {
				t.Fatalf("seed local config: %v", err)
			}

			s := newBenchServerAt(t, cfg, configPath)
			if got := s.deployment.ConfigSnapshot; got != ScopePerInstance {
				t.Fatalf("reported config snapshot scope = %q, want %q: config is backed by the local file, not the %s control-plane backend", got, ScopePerInstance, backend)
			}

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
			if body.Deployment.ConfigSnapshot != ScopePerInstance {
				t.Fatalf("hello config snapshot scope = %q, want %q", body.Deployment.ConfigSnapshot, ScopePerInstance)
			}

			const editedURL = "https://replica-a.invalid"
			if _, err := s.mutateConfig(func(c *config.Config) error {
				c.ClientBaseURL = editedURL
				return nil
			}); err != nil {
				t.Fatalf("admin config save: %v", err)
			}
			loaded, err := config.LoadBase(configPath)
			if err != nil {
				t.Fatalf("reload local config: %v", err)
			}
			if loaded.ClientBaseURL != editedURL {
				t.Fatalf("admin save did not persist to the local config file: got %q", loaded.ClientBaseURL)
			}

			// A second replica has its own local file; the first replica's edit
			// must not appear there, because config snapshots are not shared.
			otherPath := filepath.Join(dir, "other-replica.json")
			if err := config.SaveAtomic(otherPath, cfg); err != nil {
				t.Fatalf("seed second replica config: %v", err)
			}
			other, err := config.LoadBase(otherPath)
			if err != nil {
				t.Fatalf("reload second replica config: %v", err)
			}
			if other.ClientBaseURL == editedURL {
				t.Fatal("config edit propagated across replicas; report claims per-instance but storage is shared")
			}
		})
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
