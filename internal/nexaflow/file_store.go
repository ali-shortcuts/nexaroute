package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrFileStoreCommitUncertain means a rename succeeded but directory sync
// did not. The operation may have committed; do not blindly retry it. Close
// the store, reopen it, and reconcile against the recovered state.
var ErrFileStoreCommitUncertain = errors.New("nexaflow: file store commit uncertain; close and reopen")

type fileStoreTemp interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

type fileStoreDir interface {
	Sync() error
	Close() error
}

type fileStoreFS struct {
	createTemp  func(string, string) (fileStoreTemp, error)
	rename      func(string, string) error
	openDir     func(string) (fileStoreDir, error)
	releaseLock func(*os.File) error
}

func defaultFileStoreFS() fileStoreFS {
	return fileStoreFS{
		createTemp:  func(dir, pattern string) (fileStoreTemp, error) { return os.CreateTemp(dir, pattern) },
		rename:      os.Rename,
		openDir:     func(path string) (fileStoreDir, error) { return os.Open(path) },
		releaseLock: releaseFileLock,
	}
}

// FileStore is the durable single-instance Store implementation. It
// keeps the whole dataset in memory (behind the same transition logic
// as MemoryStore) and persists it to one JSON document after every
// mutation, using the control-plane file-store discipline: write a temp
// file in the same directory, fsync it, rename over the target, fsync
// the directory. Before rename, persist errors leave the old state intact.
// After rename, directory open or sync errors make the commit uncertain: the
// new state may be on disk but its durability is not established. The store
// rejects further reads and writes until closed and reopened, so callers must
// reconcile the operation against reloaded state before retrying. Reopening
// observes the file that exists at that time, not a guarantee against a later
// crash after an unsuccessful directory sync.
//
// On Linux and Darwin, OpenFileStore takes an exclusive non-blocking flock
// on <path>.lock for the store's lifetime. On other platforms no interprocess
// lock is implemented, so concurrent writers must not use the same path.
// Cross-process sharing beyond one writer is the PostgreSQL store's job.
// Close releases the lock and all later operations on this instance fail.

type FileStore struct {
	mu        sync.Mutex
	path      string
	lockFile  *os.File
	state     *storeState
	uncertain bool
	fs        fileStoreFS
	Clock     func() time.Time
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
	fs := &FileStore{path: path, lockFile: lockFile, state: newStoreState(), fs: defaultFileStoreFS()}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Join(fmt.Errorf("nexaflow: read file store: %w", err), releaseFileLock(lockFile))
		}
		return fs, nil
	}
	if len(raw) == 0 {
		return nil, errors.Join(fmt.Errorf("nexaflow: corrupt file store %q: empty file", path), releaseFileLock(lockFile))
	}
	var doc *fileStateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errors.Join(fmt.Errorf("nexaflow: corrupt file store %q: %w", path, err), releaseFileLock(lockFile))
	}
	if doc == nil {
		return nil, errors.Join(fmt.Errorf("nexaflow: corrupt file store %q: null document", path), releaseFileLock(lockFile))
	}
	// The writer emits all six maps, even when empty. For compatibility,
	// the historical empty-object representation remains a valid empty
	// store; partially missing/null sections are not valid snapshots.
	if doc.Drafts == nil || doc.Revisions == nil || doc.Runs == nil || doc.RunByIdem == nil || doc.Nodes == nil || doc.Approvals == nil {
		var sections map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sections); err != nil || len(sections) != 0 {
			return nil, errors.Join(fmt.Errorf("nexaflow: corrupt file store %q: missing or null section", path), releaseFileLock(lockFile))
		}
	}
	// Null records are not generated by the writer. Skipping them silently
	// loses data, while a null node can later cause a nil-pointer panic.
	for id, run := range doc.Runs {
		if run == nil {
			return nil, errors.Join(fmt.Errorf("nexaflow: corrupt file store %q: null run %q", path, id), releaseFileLock(lockFile))
		}
	}
	for id, nodes := range doc.Nodes {
		for i, node := range nodes {
			if node == nil {
				return nil, errors.Join(fmt.Errorf("nexaflow: corrupt file store %q: null node %q[%d]", path, id, i), releaseFileLock(lockFile))
			}
		}
	}
	for key, approval := range doc.Approvals {
		if approval == nil {
			return nil, errors.Join(fmt.Errorf("nexaflow: corrupt file store %q: null approval %q", path, key), releaseFileLock(lockFile))
		}
	}
	fs.state = docToState(doc)
	return fs, nil
}

// Close releases the path lock. It does not delete the data file.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFile == nil {
		return nil
	}
	err := s.fs.releaseLock(s.lockFile)
	s.lockFile = nil
	return err
}

func (s *FileStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// persist reports whether rename succeeded, even when the directory could not
// be synced. It must be called with s.mu held.
func (s *FileStore) persist(state *storeState) (renamed bool, err error) {
	raw, err := json.Marshal(state.toDoc())
	if err != nil {
		return false, fmt.Errorf("nexaflow: marshal file store: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := s.fs.createTemp(dir, ".nexaflow-store-*")
	if err != nil {
		return false, fmt.Errorf("nexaflow: create file store temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	n, err := tmp.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("nexaflow: write file store temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("nexaflow: fsync file store temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("nexaflow: close file store temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return false, fmt.Errorf("nexaflow: chmod file store temp: %w", err)
	}
	if err := s.fs.rename(tmpName, s.path); err != nil {
		return false, fmt.Errorf("nexaflow: rename file store: %w", err)
	}
	d, err := s.fs.openDir(dir)
	if err != nil {
		return true, fmt.Errorf("nexaflow: open file store dir: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil || closeErr != nil {
		return true, fmt.Errorf("nexaflow: sync/close file store dir: %w", errors.Join(syncErr, closeErr))
	}
	return true, nil
}

// usable must be called with s.mu held.
func (s *FileStore) usable() error {
	if s.lockFile == nil {
		return fmt.Errorf("nexaflow: file store: %w", os.ErrClosed)
	}
	if s.uncertain {
		return ErrFileStoreCommitUncertain
	}
	return nil
}

// mutate applies fn to a clone and persists before swapping it in. An error
// after rename poisons this instance until it is closed and reopened.
func (s *FileStore) mutate(fn func(st *storeState) error) error {
	if err := s.usable(); err != nil {
		return err
	}
	clone := s.state.clone()
	if err := fn(clone); err != nil {
		return err
	}
	renamed, err := s.persist(clone)
	if err != nil {
		if renamed {
			s.uncertain = true
			return fmt.Errorf("%w: %w", ErrFileStoreCommitUncertain, err)
		}
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
	if err := s.usable(); err != nil {
		return Definition{}, err
	}
	return s.state.getDraft(definitionID)
}

func (s *FileStore) ListDrafts(_ context.Context) ([]Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
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

func (s *FileStore) PublishIfUnchanged(_ context.Context, definitionID, publishedBy, expectedHash string) (PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return PublishedDefinition{}, err
	}
	if expectedHash == "" {
		return PublishedDefinition{}, ErrStateConflict
	}
	var out PublishedDefinition
	err := s.mutate(func(st *storeState) error {
		rev, err := st.publishIfUnchanged(definitionID, publishedBy, expectedHash, s.now())
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
	if err := s.usable(); err != nil {
		return PublishedDefinition{}, err
	}
	return s.state.getRevision(definitionID, revision)
}

func (s *FileStore) ListRevisions(_ context.Context, definitionID string) ([]PublishedDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
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
	if err := s.usable(); err != nil {
		return Run{}, err
	}
	return s.state.getRun(runID)
}

func (s *FileStore) GetRunByIdempotency(_ context.Context, key string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return Run{}, err
	}
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
	if err := s.usable(); err != nil {
		return nil, err
	}
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
	if err := s.usable(); err != nil {
		return nil, err
	}
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
	if err := s.usable(); err != nil {
		return Approval{}, err
	}
	return s.state.getApproval(runID, nodeID)
}
