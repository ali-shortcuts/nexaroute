package eval

import (
	"sort"
	"sync"
)

// Store bounds for stored evaluation runs.
const (
	MaxStoredRuns     = 512
	MaxStateFileBytes = 8 << 20
)

// Store keeps recent evaluation runs in memory, bounded. Durability is a plane
// concern (the evaluation state file), so this store stays a pure, bounded,
// concurrency-safe history and never does I/O on a request path.
type Store struct {
	mu      sync.Mutex
	maxRuns int
	order   []string
	byID    map[string]Result
	saves   uint64
}

// NewStore builds a bounded in-memory store.
func NewStore(maxRuns int) *Store {
	if maxRuns <= 0 || maxRuns > MaxStoredRuns {
		maxRuns = 64
	}
	return &Store{maxRuns: maxRuns, byID: map[string]Result{}}
}

// Save stores a run, evicting the oldest runs beyond the bound. It is
// idempotent per run id: re-saving a run replaces it in place.
func (s *Store) Save(run Result) error {
	if err := run.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	if _, exists := s.byID[run.RunID]; exists {
		// Idempotent re-save of the same run id: keep the newest copy.
		for i, id := range s.order {
			if id == run.RunID {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.byID[run.RunID] = run
	s.order = append(s.order, run.RunID)
	if len(s.order) > s.maxRuns {
		drop := s.order[:len(s.order)-s.maxRuns]
		for _, id := range drop {
			delete(s.byID, id)
		}
		s.order = append([]string(nil), s.order[len(s.order)-s.maxRuns:]...)
	}
	s.saves++
	s.mu.Unlock()
	return nil
}

func (s *Store) snapshotLocked() []Result {
	out := make([]Result, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id])
	}
	return out
}

// Recent returns up to limit runs, newest first.
func (s *Store) Recent(limit int) []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.order) {
		limit = len(s.order)
	}
	out := make([]Result, 0, limit)
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.byID[s.order[i]])
	}
	return out
}

// All returns every stored run, oldest first (deterministic).
func (s *Store) All() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// Get returns one run by id.
func (s *Store) Get(runID string) (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.byID[runID]
	return run, ok
}

// Len returns the number of stored runs.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

// Counts aggregates verdict counters across stored runs, bounded by the verdict
// vocabulary.
func (s *Store) Counts() map[Verdict]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[Verdict]int{}
	for _, id := range s.order {
		for v, n := range s.byID[id].Counts {
			out[v] += n
		}
	}
	return out
}

// SuiteCounts aggregates run counts per suite id.
func (s *Store) SuiteCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, id := range s.order {
		out[s.byID[id].SuiteID]++
	}
	return out
}

// Retain drops runs for deployments that no longer exist.
func (s *Store) Retain(valid map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]string, 0, len(s.order))
	for _, id := range s.order {
		run := s.byID[id]
		if _, ok := valid[run.DeploymentID]; !ok {
			delete(s.byID, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}

// SortedVerdictKeys returns the verdict vocabulary in canonical order, useful for
// deterministic metric emission.
func SortedVerdictKeys() []Verdict {
	out := append([]Verdict(nil), AllVerdicts...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
