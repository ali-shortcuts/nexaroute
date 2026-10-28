package nexaflow

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is the in-process Store implementation. It is the store
// for tests, single-process deployments, and as the write-through
// cache shape that FileStore persists. All state lives behind one
// mutex, so every transition is atomic within the process.
type MemoryStore struct {
	mu    sync.Mutex
	state *storeState
	// Clock is the store's clock, defaulting to time.Now. Tests set it
	// to control lease expiry without sleeping.
	Clock func() time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{state: newStoreState()}
}

func (s *MemoryStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *MemoryStore) SaveDraft(_ context.Context, def Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.saveDraft(def)
}

func (s *MemoryStore) GetDraft(_ context.Context, definitionID string) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getDraft(definitionID)
}

func (s *MemoryStore) ListDrafts(_ context.Context) ([]Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.listDrafts(), nil
}

func (s *MemoryStore) Publish(_ context.Context, definitionID, publishedBy string) (PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.publish(definitionID, publishedBy, s.now())
}

func (s *MemoryStore) SupersedeRevision(_ context.Context, definitionID string, revision uint64) (PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.supersedeRevision(definitionID, revision)
}

func (s *MemoryStore) GetRevision(_ context.Context, definitionID string, revision uint64) (PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getRevision(definitionID, revision)
}

func (s *MemoryStore) ListRevisions(_ context.Context, definitionID string) ([]PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.listRevisions(definitionID)
}

func (s *MemoryStore) CreateRun(_ context.Context, run Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.createRun(run, s.now())
}

func (s *MemoryStore) GetRun(_ context.Context, runID string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getRun(runID)
}

func (s *MemoryStore) GetRunByIdempotency(_ context.Context, key string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getRunByIdempotency(key)
}

func (s *MemoryStore) UpdateRunState(_ context.Context, runID string, expected RunState, state RunState, errorCode, errMsg string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.updateRunState(runID, expected, state, errorCode, errMsg, s.now())
}

func (s *MemoryStore) ListRuns(_ context.Context, filter RunFilter) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.listRuns(filter)
}

func (s *MemoryStore) Prune(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.prune(before)
}

func (s *MemoryStore) AppendNodeExecution(_ context.Context, exec NodeExecution) (NodeExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.appendNodeExecution(exec, s.now())
}

func (s *MemoryStore) SaveNodeExecution(_ context.Context, exec NodeExecution, expected NodeState) (NodeExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.saveNodeExecution(exec, expected, s.now())
}

func (s *MemoryStore) GetNodeExecutions(_ context.Context, runID string) ([]NodeExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getNodeExecutions(runID)
}

func (s *MemoryStore) TryClaim(_ context.Context, runID, nodeID, owner string, ttl time.Duration, now time.Time) (NodeExecution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.tryClaim(runID, nodeID, owner, ttl, now)
}

func (s *MemoryStore) ReleaseLease(_ context.Context, runID, nodeID, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.releaseLease(runID, nodeID, owner)
}

func (s *MemoryStore) RecordApproval(_ context.Context, appr Approval) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.recordApproval(appr, s.now())
}

func (s *MemoryStore) GetApproval(_ context.Context, runID, nodeID string) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getApproval(runID, nodeID)
}
