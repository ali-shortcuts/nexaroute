package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type FileStore struct {
	mu   sync.RWMutex
	path string
	jobs map[string]video.VideoJob
	idem map[string]string
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
		var st fileState
		if err = json.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("decode job store: %w", err)
		}
		if st.Jobs != nil {
			s.jobs = st.Jobs
		}
		if st.Idempotency != nil {
			s.idem = st.Idempotency
		}
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
	dir := filepath.Dir(s.path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".video-jobs-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
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
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *FileStore) Create(_ context.Context, j video.VideoJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.JobID]; ok {
		return fmt.Errorf("job already exists")
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
	j, ok := s.jobs[id]
	if !ok {
		return video.VideoJob{}, ErrNotFound
	}
	return j, nil
}

func (s *FileStore) GetByIdempotency(_ context.Context, key string) (video.VideoJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	old, ok := s.jobs[j.JobID]
	if !ok {
		return ErrNotFound
	}
	if err := checkTerminalUpdate(old, j); err != nil {
		return err
	}
	s.jobs[j.JobID] = j
	if err := s.persist(); err != nil {
		s.jobs[j.JobID] = old
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
	out := make([]video.VideoJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out, nil
}
