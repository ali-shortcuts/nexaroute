package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Production NexaFlow wiring tests (issue #765). These exercise the
// exact production decision path in cmd/gateway: flag gating, store
// selection, startup recovery, and shutdown cleanup — against a real
// httpapi Server object, not a fake. HTTP route/authz behaviour of
// the attached service is covered in internal/httpapi.

func wiringTestServer(t *testing.T) (*httpapi.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "wiring-test-key"
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	dir := t.TempDir()
	srv := httpapi.New(cfg, filepath.Join(dir, "config.json"), reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
	if err := srv.SecurityStoreError(); err != nil {
		t.Fatalf("security store: %v", err)
	}
	t.Cleanup(func() { _ = srv.CloseSecurityStore() })
	return srv, cfg.Admin.APIKey
}

func wiringDo(t *testing.T, srv *httpapi.Server, key, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://gateway"+path, rdr)
	if key == "" {
		req.RemoteAddr = "127.0.0.2:1234"
	} else {
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("x-admin-key", key)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNexaflowWiringDisabledMountsNothing(t *testing.T) {
	srv, key := wiringTestServer(t)
	closer, err := maybeAttachNexaflow(srv, config.NexaflowConfig{Enabled: false}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("disabled wiring: %v", err)
	}
	if closer != nil {
		t.Fatalf("disabled wiring returned a closer")
	}
	// Routes fall through to the static server as 404, exactly as
	// before this wiring existed.
	if rec := wiringDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/nodes", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled nodes = %d, want 404", rec.Code)
	}
	if rec := wiringDo(t, srv, key, http.MethodGet, "/studio.html", ""); rec.Code != http.StatusOK {
		t.Fatalf("disabled studio page = %d, want 200 (static page is always served)", rec.Code)
	}
}

func TestNexaflowWiringConfigValidation(t *testing.T) {
	cfg := config.Default()
	cfg.Nexaflow = config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: ""}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("enabled file backend without path: want validation error")
	}
	cfg.Nexaflow = config.NexaflowConfig{Enabled: true, StoreBackend: "postgres"}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("unknown backend: want validation error")
	}
	cfg.Nexaflow = config.NexaflowConfig{Enabled: false, StoreBackend: "bogus"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled config must validate regardless of backend: %v", err)
	}
	cfg.Nexaflow = config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: "/tmp/x.json"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled file backend with path: %v", err)
	}
	cfg.Nexaflow = config.NexaflowConfig{Enabled: true, StoreBackend: "memory"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled memory backend: %v", err)
	}
}

func TestNexaflowWiringFileStoreFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := newNexaflowService(config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: ""}); err == nil {
		t.Fatalf("empty file path: want error")
	}
	if _, _, err := newNexaflowService(config.NexaflowConfig{Enabled: true, StoreBackend: "bogus"}); err == nil {
		t.Fatalf("unknown backend: want error")
	}
	// Corrupt store file fails at open, never silently replaced.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newNexaflowService(config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: bad}); err == nil {
		t.Fatalf("corrupt store: want error")
	}
	// Second process on the same path fails on the file lock.
	path := filepath.Join(dir, "store.json")
	svc, closer, err := newNexaflowService(config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: path})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if svc == nil {
		t.Fatalf("first open returned nil service")
	}
	if _, _, err := newNexaflowService(config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: path}); err == nil {
		t.Fatalf("second open on locked path: want lock error")
	}
	// Shutdown cleanup: after Close, the same path reopens cleanly.
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	svc2, closer2, err := newNexaflowService(config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: path})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if svc2 == nil {
		t.Fatalf("reopen returned nil service")
	}
	_ = closer2.Close()
}

// approvalDef is a manual-trigger -> approval -> json-transform graph:
// Execute parks at the approval node, a different approver releases it,
// and the run then succeeds. Pure nodes only, no backend needed.
func approvalDef() nexaflow.Definition {
	return nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion, ID: "wf-approval", Name: "approval",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "gate", NodeID: "nexaroute.control.approval", Version: nexaflow.SupportedVersion},
			{ID: "done", NodeID: "nexaroute.control.json_transform", Version: nexaflow.SupportedVersion,
				Input: map[string]any{"value": map[string]any{}, "operations": []any{map[string]any{"op": "set_const", "field": "done", "value": true}}}},
		},
		Edges: []nexaflow.DefinitionEdge{
			{ID: "e1", From: "start", To: "gate"},
			{ID: "e2", From: "gate", To: "done"},
		},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual, Config: map[string]any{}}},
	}
}

func TestNexaflowWiringRestartRecoversApprovalRun(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.json")
	cfg := config.NexaflowConfig{Enabled: true, StoreBackend: "file", StorePath: path}
	svc, closer, err := newNexaflowService(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	def := approvalDef()
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	requester := authz.Identity{Subject: "alice"}
	pub, err := svc.Publish(ctx, def.ID, requester)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	run, err := svc.StartRun(ctx, pub, requester, map[string]any{}, "restart-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, err = svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun (park): %v", err)
	}
	if run.State != nexaflow.RunWaitingForApproval {
		t.Fatalf("parked state = %q, want waiting_for_approval", run.State)
	}
	// Simulate a gateway restart: close the store, reopen through the
	// same production constructor, run startup recovery.
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	svc2, closer2, err := newNexaflowService(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer closer2.Close()
	if err := svc2.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// The parked run survives the restart and is still waiting.
	after, _, err := svc2.GetRun(ctx, run.ID, requester)
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if after.State != nexaflow.RunWaitingForApproval {
		t.Fatalf("state after restart = %q, want waiting_for_approval", after.State)
	}
	// A different approver releases it; the requester cannot approve
	// their own run (separation of duties, enforced on the restarted
	// service too).
	approver := authz.Identity{Subject: "bob"}
	if _, err := svc2.ApproveRun(ctx, run.ID, "gate", requester, "approved"); err == nil {
		t.Fatalf("self-approval: want error")
	}
	if _, err := svc2.ApproveRun(ctx, run.ID, "gate", approver, "approved"); err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
	// Canonical lease rule: the parked attempt still carries the old
	// process's live 30s lease, and the restarted process (new random
	// worker id, as production constructs it) must NOT steal it. The
	// first resume stalls: run stays running, node stays pending,
	// nothing is corrupted. This is designed behaviour, not a gap.
	stalled, err := svc2.ResumeRun(ctx, run.ID, requester)
	if err != nil {
		t.Fatalf("ResumeRun stalled: %v", err)
	}
	if stalled.State != nexaflow.RunRunning {
		t.Fatalf("stalled resume state = %q, want running (live lease must block, not fail)", stalled.State)
	}
	execs, err := svc2.Store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	stalledGate := false
	for _, e := range execs {
		if e.NodeID == "gate" && e.State == nexaflow.NodePending {
			stalledGate = true
		}
	}
	if !stalledGate {
		t.Fatalf("stalled approval node not pending: %+v", execs)
	}
	// Lease now stale (clock advanced past it here, so the test never
	// sleeps for the real 30s TTL): the same store resumes to
	// completion. Only the clock is test-controlled; the store, the
	// approval record, and the run are the production ones.
	store := svc2.Store
	engine3, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: nexaflow.DefaultRegistry(), Backend: nil, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }})
	if err != nil {
		t.Fatalf("NewEngine #3: %v", err)
	}
	svc3, err := nexaflow.NewService(store, engine3, nexaflow.DefaultRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService #3: %v", err)
	}
	final, err := svc3.ResumeRun(ctx, run.ID, requester)
	if err != nil {
		t.Fatalf("ResumeRun after lease expiry: %v", err)
	}
	if final.State != nexaflow.RunSucceeded {
		t.Fatalf("final state = %q, want succeeded", final.State)
	}
	// Cancel after terminal state is a no-op, not an error.
	if _, err := svc3.CancelRun(ctx, run.ID, requester); err != nil {
		t.Fatalf("CancelRun terminal: %v", err)
	}
}

func TestNexaflowWiringPrincipalFilteringIsOwnershipNotTenancy(t *testing.T) {
	ctx := context.Background()
	svc, closer, err := newNexaflowService(config.NexaflowConfig{Enabled: true, StoreBackend: "memory"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if closer != nil {
			_ = closer.Close()
		}
	}()
	def := approvalDef()
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	alice := authz.Identity{Subject: "alice"}
	bob := authz.Identity{Subject: "bob"}
	owner := authz.Identity{Subject: "owner", Roles: []authz.Role{authz.RoleOwner}}
	pub, err := svc.Publish(ctx, def.ID, alice)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	run, err := svc.StartRun(ctx, pub, alice, map[string]any{}, "filter-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// Bob sees none of Alice's runs and cannot read her run; the
	// owner sees all. This is the ownership ceiling, not tenant
	// isolation: there is no tenant dimension in the store.
	if runs, err := svc.ListRuns(ctx, nexaflow.RunFilter{}, bob); err != nil || len(runs) != 0 {
		t.Fatalf("bob ListRuns = %d, err %v; want 0", len(runs), err)
	}
	if _, _, err := svc.GetRun(ctx, run.ID, bob); err == nil {
		t.Fatalf("bob GetRun: want access error")
	}
	if runs, err := svc.ListRuns(ctx, nexaflow.RunFilter{}, owner); err != nil || len(runs) != 1 {
		t.Fatalf("owner ListRuns = %d, err %v; want 1", len(runs), err)
	}
	if runs, err := svc.ListRuns(ctx, nexaflow.RunFilter{}, alice); err != nil || len(runs) != 1 {
		t.Fatalf("alice ListRuns = %d, err %v; want 1", len(runs), err)
	}
}
