package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileStore is the durable single-instance Store implementation. It
// keeps the whole dataset in memory (behind the same transition logic
// as MemoryStore) and persists it to one JSON document after every
// mutation, using the control-plane file-store discipline: write a temp
// file in the same directory, fsync it, rename over the target, fsync
// the directory. A mutation whose persist fails returns the persist
// error and leaves the previous state in place, in memory and on disk,
// so a failed write can never leave memory ahead of disk.
//
// Single-writer scope: OpenFileStore takes an exclusive, non-blocking
// flock on <path>.lock for the store's lifetime. A second live store
// on the same path fails to open. The lock releases when the process
// dies or Close is called, which is what makes restart recovery work:
// a new process opening the same path sees the last persisted state.
// Cross-process sharing beyond one writer is the PostgreSQL store's
// job, not this file's.
type FileStore struct {
	mu       sync.Mutex
	path     string
	lockFile *os.File
	state    *storeState
	Clock    func() time.Time
}

// fileStateDoc is the on-disk shape of storeState. All fields are
// exported for JSON; the in-memory maps use the same key shapes.
type fileStateDoc struct {
	Drafts    map[string]Definition            `json:"drafts"`
	Revisions map[string][]PublishedDefinition `json:"revisions"`
	Runs      map[string]*Run                  `json:"runs"`
	RunByIdem map[string]string                `json:"run_by_idem"`
	Nodes     map[string][]*NodeExecution      `json:"nodes"`
	Approvals map[string]*Approval             `json:"approvals"`
}

func (s *storeState) toDoc() *fileStateDoc {
	doc := &fileStateDoc{
		Drafts:    make(map[string]Definition, len(s.drafts)),
		Revisions: make(map[string][]PublishedDefinition, len(s.revisions)),
		Runs:      make(map[string]*Run, len(s.runs)),
		RunByIdem: make(map[string]string, len(s.runByIdem)),
		Nodes:     make(map[string][]*NodeExecution, len(s.nodes)),
		Approvals: make(map[string]*Approval, len(s.approvals)),
	}
	for k, v := range s.drafts {
		doc.Drafts[k] = cloneDefinition(v)
	}
	for k, list := range s.revisions {
		cp := make([]PublishedDefinition, len(list))
		for i, r := range list {
			cp[i] = r
			cp[i].Definition = cloneDefinition(r.Definition)
		}
		doc.Revisions[k] = cp
	}
	for k, v := range s.runs {
		cp := cloneRun(*v)
		doc.Runs[k] = &cp
	}
	for k, v := range s.runByIdem {
		doc.RunByIdem[k] = v
	}
	for k, list := range s.nodes {
		cp := make([]*NodeExecution, len(list))
		for i, e := range list {
			ec := cloneNodeExec(*e)
			cp[i] = &ec
		}
		doc.Nodes[k] = cp
	}
	for k, v := range s.approvals {
		cp := *v
		doc.Approvals[k] = &cp
	}
	return doc
}

func docToState(doc *fileStateDoc) *storeState {
	s := newStoreState()
	if doc == nil {
		return s
	}
	for k, v := range doc.Drafts {
		s.drafts[k] = cloneDefinition(v)
	}
	for k, list := range doc.Revisions {
		cp := make([]PublishedDefinition, len(list))
		for i, r := range list {
			cp[i] = r
			cp[i].Definition = cloneDefinition(r.Definition)
		}
		s.revisions[k] = cp
	}
	for k, v := range doc.Runs {
		if v == nil {
			continue
		}
		cp := cloneRun(*v)
		s.runs[k] = &cp
	}
	for k, v := range doc.RunByIdem {
		s.runByIdem[k] = v
	}
	for k, list := range doc.Nodes {
		cp := make([]*NodeExecution, len(list))
		for i, e := range list {
			if e == nil {
				continue
			}
			ec := cloneNodeExec(*e)
			cp[i] = &ec
		}
		s.nodes[k] = cp
	}
	for k, v := range doc.Approvals {
		if v == nil {
			continue
		}
		cp := *v
		s.approvals[k] = &cp
	}
	return s
}

// OpenFileStore opens (or creates) the durable store at path. A missing
// file starts empty; a corrupt file fails closed rather than starting
// from a half-written state.
func OpenFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("nexaflow: file store path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("nexaflow: create file store dir: %w", err)
	}
	lockFile, err := acquireFileLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	fs := &FileStore{path: path, lockFile: lockFile, state: newStoreState()}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			// Best-effort unlock on the error path: the read error is
			// the returned error, and the release attempt closes the
			// lock file either way, so no lock is leaked.
			_ = releaseFileLock(lockFile)
			return nil, fmt.Errorf("nexaflow: read file store: %w", err)
		}
		return fs, nil
	}
	if len(raw) == 0 {
		return fs, nil
	}
	var doc fileStateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// As above: the corruption error dominates; the release
		// attempt closes the lock file either way.
		_ = releaseFileLock(lockFile)
		return nil, fmt.Errorf("nexaflow: corrupt file store %q: %w", path, err)
	}
	fs.state = docToState(&doc)
	return fs, nil
}

// Close releases the path lock. It does not delete the data file.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFile == nil {
		return nil
	}
	err := releaseFileLock(s.lockFile)
	s.lockFile = nil
	return err
}

func (s *FileStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// persist writes state atomically. It must be called with s.mu held.
func (s *FileStore) persist(state *storeState) error {
	raw, err := json.Marshal(state.toDoc())
	if err != nil {
		return fmt.Errorf("nexaflow: marshal file store: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".nexaflow-store-*")
	if err != nil {
		return fmt.Errorf("nexaflow: create file store temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		// The write error is returned and the deferred Remove
		// deletes the temp file; the final Close below (on the
		// success path) remains the checked one.
		_ = tmp.Close()
		return fmt.Errorf("nexaflow: write file store temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		// As above: the fsync error dominates and the temp file
		// is removed by the deferred cleanup.
		_ = tmp.Close()
		return fmt.Errorf("nexaflow: fsync file store temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("nexaflow: close file store temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("nexaflow: chmod file store temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("nexaflow: rename file store: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("nexaflow: open file store dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("nexaflow: fsync file store dir: %w", err)
	}
	return nil
}

// mutate applies fn to a clone of the state and persists the clone
// before swapping it in. A persist failure leaves the previous state
// untouched, so memory never runs ahead of disk.
func (s *FileStore) mutate(fn func(st *storeState) error) error {
	clone := s.state.clone()
	if err := fn(clone); err != nil {
		return err
	}
	if err := s.persist(clone); err != nil {
		return err
	}
	s.state = clone
	return nil
}

func (s *FileStore) SaveDraft(_ context.Context, def Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(func(st *storeState) error { return st.saveDraft(def) })
}

func (s *FileStore) GetDraft(_ context.Context, definitionID string) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getDraft(definitionID)
}

func (s *FileStore) ListDrafts(_ context.Context) ([]Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.listDrafts(), nil
}

func (s *FileStore) Publish(_ context.Context, definitionID, publishedBy string) (PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out PublishedDefinition
	err := s.mutate(func(st *storeState) error {
		rev, err := st.publish(definitionID, publishedBy, s.now())
		if err != nil {
			return err
		}
		out = rev
		return nil
	})
	return out, err
}

func (s *FileStore) SupersedeRevision(_ context.Context, definitionID string, revision uint64) (PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out PublishedDefinition
	err := s.mutate(func(st *storeState) error {
		r, err := st.supersedeRevision(definitionID, revision)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

func (s *FileStore) GetRevision(_ context.Context, definitionID string, revision uint64) (PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getRevision(definitionID, revision)
}

func (s *FileStore) ListRevisions(_ context.Context, definitionID string) ([]PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.listRevisions(definitionID)
}

func (s *FileStore) CreateRun(_ context.Context, run Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(func(st *storeState) error { return st.createRun(run, s.now()) })
}

func (s *FileStore) GetRun(_ context.Context, runID string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getRun(runID)
}

func (s *FileStore) GetRunByIdempotency(_ context.Context, key string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getRunByIdempotency(key)
}

func (s *FileStore) UpdateRunState(_ context.Context, runID string, expected RunState, state RunState, errorCode, errMsg string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Run
	err := s.mutate(func(st *storeState) error {
		r, err := st.updateRunState(runID, expected, state, errorCode, errMsg, s.now())
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

func (s *FileStore) ListRuns(_ context.Context, filter RunFilter) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.listRuns(filter)
}

func (s *FileStore) Prune(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.mutate(func(st *storeState) error {
		c, err := st.prune(before)
		if err != nil {
			return err
		}
		n = c
		return nil
	})
	return n, err
}

func (s *FileStore) AppendNodeExecution(_ context.Context, exec NodeExecution) (NodeExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out NodeExecution
	err := s.mutate(func(st *storeState) error {
		e, err := st.appendNodeExecution(exec, s.now())
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	return out, err
}

func (s *FileStore) SaveNodeExecution(_ context.Context, exec NodeExecution, expected NodeState) (NodeExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out NodeExecution
	err := s.mutate(func(st *storeState) error {
		e, err := st.saveNodeExecution(exec, expected, s.now())
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	return out, err
}

func (s *FileStore) GetNodeExecutions(_ context.Context, runID string) ([]NodeExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getNodeExecutions(runID)
}

func (s *FileStore) TryClaim(_ context.Context, runID, nodeID, owner string, ttl time.Duration, now time.Time) (NodeExecution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out NodeExecution
	var claimed bool
	err := s.mutate(func(st *storeState) error {
		e, ok, err := st.tryClaim(runID, nodeID, owner, ttl, now)
		if err != nil {
			return err
		}
		out, claimed = e, ok
		return nil
	})
	return out, claimed, err
}

func (s *FileStore) ReleaseLease(_ context.Context, runID, nodeID, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(func(st *storeState) error { return st.releaseLease(runID, nodeID, owner) })
}

func (s *FileStore) RecordApproval(_ context.Context, appr Approval) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Approval
	err := s.mutate(func(st *storeState) error {
		a, err := st.recordApproval(appr, s.now())
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

func (s *FileStore) GetApproval(_ context.Context, runID, nodeID string) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.getApproval(runID, nodeID)
}
