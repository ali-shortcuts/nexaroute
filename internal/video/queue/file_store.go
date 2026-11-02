package queue

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

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// ErrFileStoreCommitUncertain means rename succeeded but the directory could
// not be synced. Close and reopen the store, then reconcile before retrying.
var ErrFileStoreCommitUncertain = errors.New("video job file store commit uncertain; close and reopen")

type jobStoreTemp interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Chmod(os.FileMode) error
	Name() string
}
type jobStoreDir interface {
	Sync() error
	Close() error
}
type jobStoreFS struct {
	mkdirAll   func(string, os.FileMode) error
	createTemp func(string, string) (jobStoreTemp, error)
	rename     func(string, string) error
	openDir    func(string) (jobStoreDir, error)
}

func defaultJobStoreFS() jobStoreFS {
	return jobStoreFS{mkdirAll: os.MkdirAll, createTemp: func(dir, pattern string) (jobStoreTemp, error) { return os.CreateTemp(dir, pattern) }, rename: os.Rename, openDir: func(path string) (jobStoreDir, error) { return os.Open(path) }}
}

// FileStore only serializes operations within this instance. Multiple
// instances or processes must not share a path; use PostgreSQL instead.
type FileStore struct {
	mu        sync.RWMutex
	path      string
	jobs      map[string]video.VideoJob
	idem      map[string]string
	fs        jobStoreFS
	closed    bool
	uncertain bool
}

// Close invalidates this instance; reopen and reconcile after uncertainty.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
func (s *FileStore) usable() error {
	if s.closed {
		return os.ErrClosed
	}
	if s.uncertain {
		return ErrFileStoreCommitUncertain
	}
	return nil
}

type fileState struct {
	Jobs        map[string]video.VideoJob `json:"jobs"`
	Idempotency map[string]string         `json:"idempotency"`
}

func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, fmt.Errorf("store path is required")
	}
	s := &FileStore{path: path, jobs: map[string]video.VideoJob{}, idem: map[string]string{}}
	if data, err := os.ReadFile(path); err == nil {
		var st *fileState
		if err = json.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("decode job store: %w", err)
		}
		if st == nil {
			return nil, fmt.Errorf("decode job store: null document")
		}
		if st.Jobs != nil {
			s.jobs = st.Jobs
		}
		// Jobs are authoritative: old stores may contain stale index entries
		// from an interrupted or historical key update. Rebuild the index
		// without allowing dangling or mismatched mappings to select a job.
		for id, job := range s.jobs {
			if job.IdempotencyKey == "" {
				continue
			}
			if existing := s.idem[job.IdempotencyKey]; existing == "" {
				s.idem[job.IdempotencyKey] = id
			} else if existing != id {
				return nil, fmt.Errorf("duplicate persisted idempotency key")
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *FileStore) persist() error {
	st := fileState{Jobs: s.jobs, Idempotency: s.idem}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Zero-value hooks support older internal callers constructing a store
	// directly; normal stores and tests can inject all filesystem stages.
	if s.fs.createTemp == nil {
		s.fs = defaultJobStoreFS()
	}
	dir := filepath.Dir(s.path)
	if err = s.fs.mkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := s.fs.createTemp(dir, ".video-jobs-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err = tmp.Chmod(0600); err == nil {
		var n int
		n, err = tmp.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = s.fs.rename(name, s.path); err != nil {
		return err
	}
	d, err := s.fs.openDir(dir)
	if err == nil {
		err = d.Sync()
		if closeErr := d.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		s.uncertain = true
		return fmt.Errorf("%w: %w", ErrFileStoreCommitUncertain, err)
	}
	return nil
}

func (s *FileStore) Create(_ context.Context, j video.VideoJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if _, ok := s.jobs[j.JobID]; ok {
		return ErrJobAlreadyExists
	}
	if j.IdempotencyKey != "" {
		if _, ok := s.idem[j.IdempotencyKey]; ok {
			return ErrIdempotencyConflict
		}
		s.idem[j.IdempotencyKey] = j.JobID
	}
	s.jobs[j.JobID] = j
	if err := s.persist(); err != nil {
		delete(s.jobs, j.JobID)
		if j.IdempotencyKey != "" {
			delete(s.idem, j.IdempotencyKey)
		}
		return err
	}
	return nil
}

func (s *FileStore) Get(_ context.Context, id string) (video.VideoJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usable(); err != nil {
		return video.VideoJob{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return video.VideoJob{}, ErrNotFound
	}
	return j, nil
}

func (s *FileStore) GetByIdempotency(_ context.Context, key string) (video.VideoJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usable(); err != nil {
		return video.VideoJob{}, err
	}
	id := s.idem[key]
	j, ok := s.jobs[id]
	if !ok {
		return video.VideoJob{}, ErrNotFound
	}
	return j, nil
}

func (s *FileStore) Update(_ context.Context, j video.VideoJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	old, ok := s.jobs[j.JobID]
	if !ok {
		return ErrNotFound
	}
	if err := checkTerminalUpdate(old, j); err != nil {
		return err
	}
	changedKey := old.IdempotencyKey != j.IdempotencyKey
	if changedKey {
		if j.IdempotencyKey != "" {
			if id, exists := s.idem[j.IdempotencyKey]; exists && id != j.JobID {
				return ErrIdempotencyConflict
			}
		}
		if old.IdempotencyKey != "" {
			delete(s.idem, old.IdempotencyKey)
		}
		if j.IdempotencyKey != "" {
			s.idem[j.IdempotencyKey] = j.JobID
		}
	}
	s.jobs[j.JobID] = j
	if err := s.persist(); err != nil {
		s.jobs[j.JobID] = old
		if changedKey {
			if j.IdempotencyKey != "" {
				delete(s.idem, j.IdempotencyKey)
			}
			if old.IdempotencyKey != "" {
				s.idem[old.IdempotencyKey] = j.JobID
			}
		}
		return err
	}
	return nil
}

// Delete removes a job and its idempotency mapping and
// persists the removal, rolling back in memory when the write
// fails. Deleting a missing job is a no-op.
func (s *FileStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	j, ok := s.jobs[id]
	if !ok {
		return nil
	}
	delete(s.jobs, id)
	if j.IdempotencyKey != "" {
		delete(s.idem, j.IdempotencyKey)
	}
	if err := s.persist(); err != nil {
		s.jobs[id] = j
		if j.IdempotencyKey != "" {
			s.idem[j.IdempotencyKey] = id
		}
		return err
	}
	return nil
}

// TryClaim leases the job and persists the lease, rolling
// back in memory when the write fails. Atomicity is
// in-process: two processes sharing one store FILE are not
// made safe by this (each keeps its own in-memory copy and
// persists by whole-file overwrite — a pre-existing,
// documented limit of the file store).
func (s *FileStore) TryClaim(_ context.Context, id, owner string, ttl time.Duration, now time.Time) (video.VideoJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return video.VideoJob{}, false, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return video.VideoJob{}, false, ErrNotFound
	}
	if !claimable(j, owner, now) {
		return video.VideoJob{}, false, nil
	}
	prev := j
	j.LeaseOwner = owner
	j.LeaseUntil = now.Add(ttl)
	s.jobs[id] = j
	if err := s.persist(); err != nil {
		s.jobs[id] = prev
		return video.VideoJob{}, false, err
	}
	return j, true, nil
}

func (s *FileStore) List(_ context.Context) ([]video.VideoJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	out := make([]video.VideoJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out, nil
}
