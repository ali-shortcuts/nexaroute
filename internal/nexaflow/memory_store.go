package nexaflow

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store for tests and embedding without
// durability. It implements the same claim, idempotency, and append-only
// rules as FileStore, but loses everything on process exit.
type MemoryStore struct {
	mu       sync.Mutex
	drafts   map[string]*Definition
	versions map[string]map[uint64]*versionRecord // definitionID -> revision -> record
	runs     map[string]*Run
	claims   map[string]bool
	delivery map[string]bool
	sched    map[string]*ScheduleState
	audit    []AuditEvent
}

// versionRecord pairs immutable published content with its version record.
type versionRecord struct {
	Content *Definition
	Version *DefinitionVersion
}

func cloneDefinition(d *Definition) *Definition {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Nodes = append([]DefinitionNode(nil), d.Nodes...)
	cp.Edges = append([]DefinitionEdge(nil), d.Edges...)
	cp.Triggers = append([]DefinitionTrigger(nil), d.Triggers...)
	if d.OnError != nil {
		oe := *d.OnError
		cp.OnError = &oe
	}
	return &cp
}

func cloneVersion(v *DefinitionVersion) *DefinitionVersion {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

func cloneRun(r *Run) *Run {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Input = cloneMap(r.Input)
	cp.Steps = append([]Step(nil), r.Steps...)
	cp.Outputs = cloneMap(r.Outputs)
	if r.Decisions != nil {
		cp.Decisions = make(map[string]ApprovalDecision, len(r.Decisions))
		for k, v := range r.Decisions {
			cp.Decisions[k] = v
		}
	}
	if r.HandlerUses != nil {
		cp.HandlerUses = make(map[string]int, len(r.HandlerUses))
		for k, v := range r.HandlerUses {
			cp.HandlerUses[k] = v
		}
	}
	return &cp
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		drafts:   make(map[string]*Definition),
		versions: make(map[string]map[uint64]*versionRecord),
		runs:     make(map[string]*Run),
		claims:   make(map[string]bool),
		delivery: make(map[string]bool),
		sched:    make(map[string]*ScheduleState),
	}
}

// SaveDraft stores the mutable working copy.
func (s *MemoryStore) SaveDraft(_ context.Context, d *Definition) error {
	if d == nil || d.ID == "" {
		return fmt.Errorf("nexaflow: cannot save draft without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drafts[d.ID] = cloneDefinition(d)
	return nil
}

// GetDraft returns a copy of a draft.
func (s *MemoryStore) GetDraft(_ context.Context, id string) (*Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[id]
	if !ok {
		return nil, &ErrNotFound{Kind: "draft", ID: id}
	}
	return cloneDefinition(d), nil
}

// ListDrafts returns drafts in the tenant scope, sorted by id.
func (s *MemoryStore) ListDrafts(_ context.Context, tenantScope string) ([]*Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Definition
	for _, d := range s.drafts {
		if tenantScope == "" || d.TenantScope == tenantScope {
			out = append(out, cloneDefinition(d))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Publish snapshots a draft as a new published revision, superseding any
// previously published revision.
func (s *MemoryStore) Publish(_ context.Context, draft *Definition, _ string) (*DefinitionVersion, *Definition, error) {
	if draft == nil || draft.ID == "" {
		return nil, nil, fmt.Errorf("nexaflow: cannot publish draft without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, err := ContentHash(draft)
	if err != nil {
		return nil, nil, err
	}
	versions := s.versions[draft.ID]
	var next uint64 = 1
	for rev, rec := range versions {
		if rec.Version.Status == DefinitionPublished {
			rec.Version.Status = DefinitionSuperseded
		}
		if rev >= next {
			next = rev + 1
		}
	}
	ver := &DefinitionVersion{DefinitionID: draft.ID, Revision: next, ContentHash: hash, Status: DefinitionPublished}
	content := cloneDefinition(draft)
	if s.versions[draft.ID] == nil {
		s.versions[draft.ID] = make(map[uint64]*versionRecord)
	}
	s.versions[draft.ID][next] = &versionRecord{Content: content, Version: ver}
	return cloneVersion(ver), cloneDefinition(content), nil
}

// GetVersion loads one published revision's content and record.
func (s *MemoryStore) GetVersion(_ context.Context, definitionID string, revision uint64) (*Definition, *DefinitionVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.versions[definitionID][revision]
	if !ok {
		return nil, nil, &ErrNotFound{Kind: "version", ID: fmt.Sprintf("%s@%d", definitionID, revision)}
	}
	return cloneDefinition(rec.Content), cloneVersion(rec.Version), nil
}

// ListVersions returns all version records, ascending by revision.
func (s *MemoryStore) ListVersions(_ context.Context, definitionID string) ([]*DefinitionVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*DefinitionVersion
	for _, rec := range s.versions[definitionID] {
		out = append(out, cloneVersion(rec.Version))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out, nil
}

// LatestPublished returns the currently published revision, if any.
func (s *MemoryStore) LatestPublished(_ context.Context, definitionID string) (*Definition, *DefinitionVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.versions[definitionID] {
		if rec.Version.Status == DefinitionPublished {
			return cloneDefinition(rec.Content), cloneVersion(rec.Version), nil
		}
	}
	return nil, nil, &ErrNotFound{Kind: "published version", ID: definitionID}
}

// ListPublished returns the latest published content of every definition.
func (s *MemoryStore) ListPublished(_ context.Context) ([]*Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Definition
	for id, versions := range s.versions {
		for _, rec := range versions {
			if rec.Version.Status == DefinitionPublished {
				out = append(out, cloneDefinition(rec.Content))
				_ = id
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CreateRun stores a new run record.
func (s *MemoryStore) CreateRun(_ context.Context, r *Run) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("nexaflow: cannot create run without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.runs[r.ID]; dup {
		return fmt.Errorf("nexaflow: run %q already exists", r.ID)
	}
	s.runs[r.ID] = cloneRun(r)
	return nil
}

// GetRun returns a copy of a run record.
func (s *MemoryStore) GetRun(_ context.Context, id string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, &ErrNotFound{Kind: "run", ID: id}
	}
	return cloneRun(r), nil
}

// ListRuns returns runs matching the filter, newest first.
func (s *MemoryStore) ListRuns(_ context.Context, filter RunFilter) ([]*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Run
	for _, r := range s.runs {
		if filter.DefinitionID != "" && r.DefinitionID != filter.DefinitionID {
			continue
		}
		if filter.TenantScope != "" && r.TenantScope != filter.TenantScope {
			continue
		}
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		out = append(out, cloneRun(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// UpdateRun persists a run, rejecting a shrinking step history.
func (s *MemoryStore) UpdateRun(_ context.Context, r *Run) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("nexaflow: cannot update run without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.runs[r.ID]
	if !ok {
		return &ErrNotFound{Kind: "run", ID: r.ID}
	}
	if len(r.Steps) < len(prev.Steps) {
		return fmt.Errorf("nexaflow: run %q step history must be append-only", r.ID)
	}
	cp := cloneRun(r)
	cp.Version = prev.Version + 1
	s.runs[r.ID] = cp
	r.Version = cp.Version
	return nil
}

// FindRunByIdempotency returns the run created with the given key, if any.
func (s *MemoryStore) FindRunByIdempotency(_ context.Context, definitionID, key string) (*Run, error) {
	if key == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.DefinitionID == definitionID && r.IdempotencyKey == key {
			return cloneRun(r), nil
		}
	}
	return nil, nil
}

// ClaimSchedule returns true only for the first claim of a fire slot.
func (s *MemoryStore) ClaimSchedule(_ context.Context, scheduleKey string, fireTime time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := scheduleKey + "\x00" + fireTime.UTC().Format(time.RFC3339)
	if s.claims[key] {
		return false, nil
	}
	s.claims[key] = true
	return true, nil
}

// RecordDelivery returns true only the first time a hash is seen.
func (s *MemoryStore) RecordDelivery(_ context.Context, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivery[hash] {
		return false, nil
	}
	s.delivery[hash] = true
	return true, nil
}

// GetScheduleState returns the persisted fire bookkeeping, if any.
func (s *MemoryStore) GetScheduleState(_ context.Context, scheduleKey string) (*ScheduleState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sched[scheduleKey]
	if !ok {
		return nil, nil
	}
	cp := *st
	return &cp, nil
}

// SaveScheduleState persists fire bookkeeping.
func (s *MemoryStore) SaveScheduleState(_ context.Context, st *ScheduleState) error {
	if st == nil || st.ScheduleKey == "" {
		return fmt.Errorf("nexaflow: cannot save schedule state without key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *st
	s.sched[st.ScheduleKey] = &cp
	return nil
}

// AppendAudit records an audit event.
func (s *MemoryStore) AppendAudit(_ context.Context, event AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, event)
	return nil
}

// ListAudit returns the most recent events first, up to limit.
func (s *MemoryStore) ListAudit(_ context.Context, limit int) ([]AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEvent, len(s.audit))
	copy(out, s.audit)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneRuns deletes terminal runs finished before t.
func (s *MemoryStore) PruneRuns(_ context.Context, t time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.runs {
		if r.Status.IsTerminal() && r.FinishedAt != nil && r.FinishedAt.Before(t) {
			delete(s.runs, id)
			n++
		}
	}
	return n, nil
}

// PruneRunsFor deletes terminal runs of one definition finished before t.
func (s *MemoryStore) PruneRunsFor(_ context.Context, definitionID string, t time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.runs {
		if r.DefinitionID != definitionID {
			continue
		}
		if r.Status.IsTerminal() && r.FinishedAt != nil && r.FinishedAt.Before(t) {
			delete(s.runs, id)
			n++
		}
	}
	return n, nil
}

// FormatVersion is always current for the memory store.
func (s *MemoryStore) FormatVersion() int { return CurrentFormatVersion }

// Migrate is a no-op for the memory store.
func (s *MemoryStore) Migrate(_ context.Context) error { return nil }

// Record implements AuditSink by appending to the in-memory audit log.
func (s *MemoryStore) Record(event AuditEvent) {
	_ = s.AppendAudit(context.Background(), event)
}
