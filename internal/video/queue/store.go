package queue

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var ErrNotFound = errors.New("video job not found")
var ErrTerminalJob = errors.New("video job is already terminal and cannot be overwritten")
var ErrIdempotencyConflict = errors.New("video job idempotency key already exists")

func checkTerminalUpdate(current, next video.VideoJob) error {
	if !current.IsTerminal() {
		return nil
	}
	if reflect.DeepEqual(current, next) {
		return nil
	}
	return ErrTerminalJob
}

type JobStore interface {
	Create(context.Context, video.VideoJob) error
	Get(context.Context, string) (video.VideoJob, error)
	GetByIdempotency(context.Context, string) (video.VideoJob, error)
	Update(context.Context, video.VideoJob) error
	List(context.Context) ([]video.VideoJob, error)
	Delete(context.Context, string) error
	// TryClaim atomically leases the job to owner until
	// now+ttl (#434 item 6). It succeeds when the job exists
	// and carries no live lease held by a DIFFERENT owner; a
	// same-owner call renews. The returned job is the claimed
	// record. Implementations must make the check-and-set
	// atomic within their consistency domain.
	TryClaim(ctx context.Context, id, owner string, ttl time.Duration, now time.Time) (video.VideoJob, bool, error)
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
			return ErrIdempotencyConflict
		}
		s.idem[j.IdempotencyKey] = j.JobID
	}
	s.jobs[j.JobID] = j
	return nil
}

// Delete removes a job and its idempotency mapping. It exists
// for retention pruning of terminal jobs; deleting a missing
// job is a no-op.
func (s *MemoryStore) Delete(_ context.Context, id string) error {
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
	old, ok := s.jobs[j.JobID]
	if !ok {
		return ErrNotFound
	}
	if err := checkTerminalUpdate(old, j); err != nil {
		return err
	}
	s.jobs[j.JobID] = j
	return nil
}

// claimable reports whether j may be leased to owner at now:
// no lease, an expired lease, or a lease already held by the
// same owner (renewal). A live foreign lease blocks.
func claimable(j video.VideoJob, owner string, now time.Time) bool {
	if j.LeaseOwner == "" || !j.LeaseUntil.After(now) {
		return true
	}
	return j.LeaseOwner == owner
}

func (s *MemoryStore) TryClaim(_ context.Context, id, owner string, ttl time.Duration, now time.Time) (video.VideoJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return video.VideoJob{}, false, ErrNotFound
	}
	if !claimable(j, owner, now) {
		return video.VideoJob{}, false, nil
	}
	j.LeaseOwner = owner
	j.LeaseUntil = now.Add(ttl)
	s.jobs[id] = j
	return j, true, nil
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
