package httpapi

import (
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Deployment coordination reporting (issue #434). The gateway must
// never imply global state it does not have: this report is computed
// from the actual startup configuration and states, per subsystem,
// whether runtime state is shared across replicas, shared only on
// one host, or strictly per-instance. It is exposed on /api/hello so
// operators and the UI read the same truth the process runs with.

// Coordination scopes for one subsystem.
const (
	ScopeShared      = "shared"       // coherent across replicas via a shared store
	ScopeSharedHost  = "shared-host"  // coherent across processes on one host only
	ScopePerInstance = "per-instance" // each process has its own state
)

// DeploymentReport describes the deployment topology and the
// coordination scope of every runtime-state subsystem.
type DeploymentReport struct {
	StoreTopology        string   `json:"store_topology"` // local | shared-host | shared | test-only
	ControlPlaneEnabled  bool     `json:"control_plane_enabled"`
	ControlPlaneBackend  string   `json:"control_plane_backend,omitempty"`
	SecurityStoreBackend string   `json:"security_store_backend"`
	ConfigSnapshot       string   `json:"config_snapshot"`
	AuditSecurityStore   string   `json:"audit_security_store"`
	BreakerCooldown      string   `json:"breaker_cooldown"`
	QuotaReservations    string   `json:"quota_reservations"`
	SessionAffinity      string   `json:"session_affinity"`
	VideoJobsLedger      string   `json:"video_jobs_ledger"`
	RequestIdempotency   string   `json:"request_idempotency"`
	Warnings             []string `json:"warnings,omitempty"`
}

// deploymentReportForConfig derives the report from startup config.
// The defaults are the conservative truth of a bare gateway: every
// runtime subsystem is per-instance unless a shared store is
// actually configured for it.
func deploymentReportForConfig(cfg config.Config) DeploymentReport {
	r := DeploymentReport{
		StoreTopology:        "local",
		SecurityStoreBackend: "bbolt",
		ConfigSnapshot:       ScopePerInstance, // local config file, never control-plane backed
		AuditSecurityStore:   ScopeSharedHost,  // bbolt file on one host
		BreakerCooldown:      ScopePerInstance,
		QuotaReservations:    ScopePerInstance,
		SessionAffinity:      ScopePerInstance,
		VideoJobsLedger:      ScopePerInstance,
		RequestIdempotency:   ScopePerInstance,
	}
	if b := strings.ToLower(strings.TrimSpace(cfg.Admin.SecurityStoreBackend)); b != "" {
		r.SecurityStoreBackend = b
	}
	switch r.SecurityStoreBackend {
	case "postgres", "redis":
		r.AuditSecurityStore = ScopeShared
	case "memory":
		r.AuditSecurityStore = ScopePerInstance
	}

	r.ControlPlaneEnabled = cfg.ControlPlane.Enabled
	if cfg.ControlPlane.Enabled {
		r.ControlPlaneBackend = strings.ToLower(strings.TrimSpace(cfg.ControlPlane.Backend))
		switch r.ControlPlaneBackend {
		case "postgres", "redis":
			r.StoreTopology = "shared"
		case "file":
			r.StoreTopology = "shared-host"
		case "memory":
			r.StoreTopology = "test-only"
		}
	}

	// Breaker sync (ADR 0003) is opt-in: only when the operator enabled
	// it does the breaker scope follow the control-plane topology, and
	// the warning below is replaced with the sync-accurate one.
	breakerSyncShared := false
	if cfg.ControlPlane.BreakerSync {
		switch r.StoreTopology {
		case "shared":
			r.BreakerCooldown = ScopeShared
			breakerSyncShared = true
		case "shared-host":
			r.BreakerCooldown = ScopeSharedHost
			breakerSyncShared = true
		}
	}

	// Config snapshots are always local files. The control plane carries only
	// runtime metadata and budget-alert marks; it never stores or reconciles
	// config, so enabling it must not change this scope (#599).
	// Observable partial support: warnings are part of the contract,
	// not log noise. Anything not shared must say so where operators
	// look, so nobody scales replicas expecting global budgets,
	// cooldowns or video ledgers this build does not provide.
	if !cfg.ControlPlane.Enabled {
		r.Warnings = append(r.Warnings, "control plane disabled: config snapshots are per-instance files; replicas do not reconcile")
	} else {
		r.Warnings = append(r.Warnings, "config snapshots are per-instance local files: control-plane sharing does not propagate config edits across replicas")
	}
	if r.StoreTopology == "test-only" {
		r.Warnings = append(r.Warnings, "control plane backend 'memory' is test-only and loses all state on restart")
	}
	if breakerSyncShared {
		r.Warnings = append(r.Warnings,
			"breaker/cooldown state synchronizes across replicas via the opt-in breaker sync (control_plane.breaker_sync); remote retired markers apply only as bounded cooldowns",
		)
	} else {
		r.Warnings = append(r.Warnings,
			"breaker/cooldown state is per-instance: cross-replica breaker synchronization is not enabled",
		)
		if cfg.ControlPlane.BreakerSync {
			r.Warnings = append(r.Warnings,
				"control_plane.breaker_sync is enabled with a memory control-plane backend, which cannot share state across processes: breaker state remains per-instance",
			)
		}
	}
	r.Warnings = append(r.Warnings,
		"quota/RPM/TPM reservations are per-instance in-memory state and must never be read as global limits",
	)
	// Video durability follows the job-store backend: with PostgreSQL,
	// jobs and the cost ledger are durable and shared (assets are not —
	// they stay on per-instance local storage).
	if strings.EqualFold(strings.TrimSpace(cfg.Video.JobStoreBackend), "postgres") {
		r.VideoJobsLedger = ScopeShared
		r.Warnings = append(r.Warnings,
			"video jobs and the cost ledger are durable and shared via the PostgreSQL job-store backend; video assets remain on per-instance local storage and are not replicated",
		)
	} else {
		r.Warnings = append(r.Warnings,
			"video jobs and the cost ledger are per-instance and not durable or distributed",
		)
	}
	return r
}
