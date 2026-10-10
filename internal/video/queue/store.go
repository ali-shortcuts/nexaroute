package queue

import (
	"context"
	"errors"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var ErrNotFound = errors.New("video job not found")

type JobStore interface {
	Create(context.Context, video.VideoJob) error
	Get(context.Context, string) (video.VideoJob, error)
	GetByIdempotency(context.Context, string) (video.VideoJob, error)
	Update(context.Context, video.VideoJob) error
	List(context.Context) ([]video.VideoJob, error)
}

type MemoryStore struct {
	mu   sync.RWMutex
	jobs map[string]video.VideoJob
	idem map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: map[string]video.VideoJob{}, idem: map[string]string{}}
}
func (s *MemoryStore) Create(_ context.Context, j video.VideoJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.JobID]; ok {
		return errors.New("job already exists")
	}
	if j.IdempotencyKey != "" {
		if id := s.idem[j.IdempotencyKey]; id != "" {
			return errors.New("idempotency key already exists")
		}
		s.idem[j.IdempotencyKey] = j.JobID
	}
	s.jobs[j.JobID] = j
	return nil
}
func (s *MemoryStore) Get(_ context.Context, id string) (video.VideoJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return video.VideoJob{}, ErrNotFound
	}
	return j, nil
}
func (s *MemoryStore) GetByIdempotency(_ context.Context, key string) (video.VideoJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.idem[key]
	if id == "" {
		return video.VideoJob{}, ErrNotFound
	}
	return s.jobs[id], nil
}
func (s *MemoryStore) Update(_ context.Context, j video.VideoJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.JobID]; !ok {
		return ErrNotFound
	}
	s.jobs[j.JobID] = j
	return nil
}
func (s *MemoryStore) List(_ context.Context) ([]video.VideoJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]video.VideoJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out, nil
}
