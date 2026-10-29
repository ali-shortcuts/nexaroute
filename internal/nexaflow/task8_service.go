package nexaflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Task 8 service: the single HTTP-facing facade over the canonical
// engine and store. It owns the boundaries the engine does not:
//   - drafts are validated before they are saved, and re-validated
//     before publish, so a published revision is always valid;
//   - run access is ownership-checked: an admin/owner may see any run,
//     anyone else only their own runs (V1 ownership ceiling — this is
//     NOT multi-tenant isolation, which is explicitly out of V1);
//   - approvals require an authenticated approver different from the
//     run's requester (the engine enforces the same rule again);
//   - an optional BudgetGuard is consulted at admission: if it cannot
//     reserve, the run is never created (fail-closed). Reservation
//     release happens on terminal run state via HandleEngineEvent
//     (wire it as the engine Audit callback) or ReconcileBudget.
//     Admission reserve + release is NOT full atomic settle accounting;
//     the matrix labels it as such and full PG settle work stays open.
//   - an append-only audit log records service decisions (create,
//     publish, approve, cancel, budget reserve/release). Engine audit
//     events (node_started/finished, run_finished, recovered) flow
//     through HandleEngineEvent into the same log when wired.
// Route-level workflow.* permissions do not exist in internal/authz
// yet (in-flight task7 work owns them); this service therefore does
// NOT invent a permission matrix — it enforces authentication,
// separation of duties, and ownership, and the HTTP layer keeps the
// existing admin authentication in front of every route.

// BudgetGuard reserves cost for a run before admission. The returned
// release function must be idempotent.
type BudgetGuard interface {
	Reserve(ctx context.Context, key string, estimateUSD float64) (release func(), err error)
}

// AuditRecord is one service-level audit entry. It never carries
// secret values: run inputs/outputs are already redacted at persist
// time by the engine, and this log records decisions, not payloads.
type AuditRecord struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	RunID  string    `json:"run_id,omitempty"`
	Actor  string    `json:"actor,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Service is the Task 8 facade. It is safe for concurrent use.
type Service struct {
	Store    Store
	Engine   *Engine
	Registry *Registry
	Guard    BudgetGuard

	mu       sync.Mutex
	audit    []AuditRecord
	releases map[string]func()
}

func NewService(store Store, engine *Engine, registry *Registry, guard BudgetGuard) (*Service, error) {
	if store == nil {
		return nil, errors.New("nexaflow: service requires a store")
	}
	if engine == nil {
		return nil, errors.New("nexaflow: service requires an engine")
	}
	if registry == nil {
		return nil, errors.New("nexaflow: service requires a registry")
	}
	return &Service{Store: store, Engine: engine, Registry: registry, Guard: guard, releases: map[string]func(){}}, nil
}

func (s *Service) appendAudit(kind, runID, actor, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.audit) >= 10000 {
		s.audit = append([]AuditRecord{}, s.audit[1000:]...)
	}
	s.audit = append(s.audit, AuditRecord{At: time.Now(), Kind: kind, RunID: runID, Actor: actor, Detail: detail})
}

// Audit returns a copy of the service audit log.
func (s *Service) Audit() []AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditRecord, len(s.audit))
	copy(out, s.audit)
	return out
}

// HandleEngineEvent records engine audit events and releases budget
// reservations when a run reaches a terminal state. Wire it as the
// engine's Audit callback. Needs-manual-action is not terminal, so its
// reservation is held until an operator force-fails or cancels the run
// and ReconcileBudget runs.
func (s *Service) HandleEngineEvent(ev AuditEvent) {
	s.appendAudit("engine:"+ev.Kind, ev.RunID, "", ev.State)
	if ev.Kind != "run_finished" {
		return
	}
	if RunState(ev.State).Terminal() {
		s.releaseBudget(ev.RunID, "engine run_finished "+ev.State)
	}
}

func (s *Service) releaseBudget(runID, why string) {
	s.mu.Lock()
	release, ok := s.releases[runID]
	if ok {
		delete(s.releases, runID)
	}
	s.mu.Unlock()
	if ok && release != nil {
		release()
		s.appendAudit("budget_released", runID, "", why)
	}
}

// ReconcileBudget releases reservations for runs that are terminal in
// the store (covers cancel/force-fail paths that do not emit
// run_finished). It is idempotent.
func (s *Service) ReconcileBudget(ctx context.Context) error {
	runs, err := s.Store.ListRuns(ctx, RunFilter{})
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.State.Terminal() {
			s.releaseBudget(r.ID, "reconciled terminal")
		}
	}
	return nil
}

// Validate checks a definition against the registry.
func (s *Service) Validate(def *Definition) error {
	return ValidateDefinition(def, s.Registry)
}

// SaveDraft validates, then saves the draft.
func (s *Service) SaveDraft(ctx context.Context, def Definition) error {
	if err := ValidateDefinition(&def, s.Registry); err != nil {
		return err
	}
	if err := s.Store.SaveDraft(ctx, def); err != nil {
		return err
	}
	s.appendAudit("draft_saved", "", "", def.ID)
	return nil
}

// GetDraft returns a draft by id.
func (s *Service) GetDraft(ctx context.Context, id string) (Definition, error) {
	return s.Store.GetDraft(ctx, id)
}

// ListDrafts returns all drafts sorted by definition id.
func (s *Service) ListDrafts(ctx context.Context) ([]Definition, error) {
	return s.Store.ListDrafts(ctx)
}

// LatestPublished returns the currently published revision of a
// definition, or an error when none is published.
func (s *Service) LatestPublished(ctx context.Context, definitionID string) (PublishedDefinition, error) {
	revs, err := s.Store.ListRevisions(ctx, definitionID)
	if err != nil {
		return PublishedDefinition{}, err
	}
	for _, rev := range revs {
		if rev.Version.Status == DefinitionPublished {
			return rev, nil
		}
	}
	return PublishedDefinition{}, ErrNotFound
}

// Publish re-validates the draft, then publishes a new revision.
func (s *Service) Publish(ctx context.Context, definitionID string, by authz.Identity) (PublishedDefinition, error) {
	if !by.Authenticated() {
		return PublishedDefinition{}, errors.New("nexaflow: publish requires an authenticated principal")
	}
	draft, err := s.Store.GetDraft(ctx, definitionID)
	if err != nil {
		return PublishedDefinition{}, err
	}
	if err := ValidateDefinition(&draft, s.Registry); err != nil {
		return PublishedDefinition{}, err
	}
	pub, err := s.Store.Publish(ctx, definitionID, by.Subject)
	if err != nil {
		return PublishedDefinition{}, err
	}
	s.appendAudit("published", "", by.Subject, fmt.Sprintf("%s@%d", definitionID, pub.Version.Revision))
	return pub, nil
}

// ListRevisions returns published revisions for a definition.
func (s *Service) ListRevisions(ctx context.Context, definitionID string) ([]PublishedDefinition, error) {
	return s.Store.ListRevisions(ctx, definitionID)
}

// GetRevision returns one published revision.
func (s *Service) GetRevision(ctx context.Context, definitionID string, revision uint64) (PublishedDefinition, error) {
	return s.Store.GetRevision(ctx, definitionID, revision)
}

// StartRun admits a run: authenticated principal, optional budget
// reserve (fail-closed), then engine creation with idempotency. On
// creation failure the reservation is rolled back immediately.
func (s *Service) StartRun(ctx context.Context, pub PublishedDefinition, principal authz.Identity, input map[string]any, idempotencyKey string) (Run, error) {
	if !principal.Authenticated() {
		return Run{}, errors.New("nexaflow: start requires an authenticated principal")
	}
	var release func()
	if s.Guard != nil {
		var err error
		release, err = s.Guard.Reserve(ctx, idempotencyKey, 0)
		if err != nil {
			return Run{}, fmt.Errorf("nexaflow: budget reservation failed, run not created: %w", err)
		}
	}
	run, err := s.Engine.CreateRun(ctx, pub, principal, input, idempotencyKey)
	if err != nil {
		if release != nil {
			release()
		}
		return Run{}, err
	}
	if release != nil {
		s.mu.Lock()
		s.releases[run.ID] = release
		s.mu.Unlock()
	}
	s.appendAudit("run_started", run.ID, principal.Subject, pub.Version.DefinitionID)
	return run, nil
}

// ExecuteRun drives a run through the engine.
func (s *Service) ExecuteRun(ctx context.Context, runID string) (Run, error) {
	return s.Engine.Execute(ctx, runID)
}

// ResumeRun re-drives a run the caller owns (or any run for
// owner/admin) after a restart, a recovery re-queue, or an approval
// decision made out of band. It is the HTTP resume endpoint's
// backing call; unlike ExecuteRun it is ownership-checked, so a
// non-owner cannot resume another principal's run. Parked runs
// stay parked (ErrRunParked) until an operator cancels or
// force-fails them; resume never revives them.
func (s *Service) ResumeRun(ctx context.Context, runID string, principal authz.Identity) (Run, error) {
	if err := s.checkRunAccess(ctx, runID, principal); err != nil {
		return Run{}, err
	}
	run, err := s.Engine.Execute(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	s.appendAudit("resumed", runID, principal.Subject, "")
	return run, nil
}

// ApproveRun records an approval decision. The approver must be
// authenticated and different from the run's requester; the engine
// enforces the same separation again at the record.
func (s *Service) ApproveRun(ctx context.Context, runID, nodeID string, approver authz.Identity, decision string) (Approval, error) {
	if !approver.Authenticated() {
		return Approval{}, errors.New("nexaflow: approval requires an authenticated approver")
	}
	appr, err := s.Engine.DecideApproval(ctx, runID, nodeID, approver.Subject, decision)
	if err != nil {
		return Approval{}, err
	}
	s.appendAudit("approved", runID, approver.Subject, decision)
	return appr, nil
}

// CancelRun cancels a run the caller owns (or any run for owner/admin).
func (s *Service) CancelRun(ctx context.Context, runID string, principal authz.Identity) (Run, error) {
	if err := s.checkRunAccess(ctx, runID, principal); err != nil {
		return Run{}, err
	}
	run, err := s.Engine.CancelRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	s.appendAudit("cancelled", runID, principal.Subject, "")
	return run, nil
}

// GetRun returns a run plus its node executions, ownership-checked.
// Node outputs are already redacted at persist time by the engine.
func (s *Service) GetRun(ctx context.Context, runID string, principal authz.Identity) (Run, []NodeExecution, error) {
	if err := s.checkRunAccess(ctx, runID, principal); err != nil {
		return Run{}, nil, err
	}
	run, err := s.Store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, nil, err
	}
	execs, err := s.Store.GetNodeExecutions(ctx, runID)
	if err != nil {
		return Run{}, nil, err
	}
	return run, execs, nil
}

// ListRuns returns runs visible to the caller: all runs for
// owner/admin, only the caller's own runs otherwise.
func (s *Service) ListRuns(ctx context.Context, filter RunFilter, principal authz.Identity) ([]Run, error) {
	runs, err := s.Store.ListRuns(ctx, filter)
	if err != nil {
		return nil, err
	}
	if principal.HasRole(authz.RoleOwner) || principal.HasRole(authz.RoleAdmin) {
		return runs, nil
	}
	var out []Run
	for _, r := range runs {
		if r.Principal == principal.Subject {
			out = append(out, r)
		}
	}
	return out, nil
}

// Recover runs engine recovery (restart/crash reconciliation).
func (s *Service) Recover(ctx context.Context) error {
	return s.Engine.Recover(ctx)
}

func (s *Service) checkRunAccess(ctx context.Context, runID string, principal authz.Identity) error {
	if !principal.Authenticated() {
		return errors.New("nexaflow: run access requires an authenticated principal")
	}
	if principal.HasRole(authz.RoleOwner) || principal.HasRole(authz.RoleAdmin) {
		return nil
	}
	run, err := s.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Principal != principal.Subject {
		return fmt.Errorf("nexaflow: principal %q may not access run %q", principal.Subject, runID)
	}
	return nil
}
