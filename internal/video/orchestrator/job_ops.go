package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var ErrForceFailNotActive = errors.New("video job is not active and cannot be force-failed")

// CostLedger is the small ledger contract the orchestrator needs. Keeping it
// interface-based makes settlement failures injectable in regression tests.
type CostLedger interface {
	ReserveForJob(string, video.CostEstimate, float64) error
	ReleaseForJob(string)
	SettleActual(string, float64, float64) error
	SettleEstimate(string, float64) error
	Entries() []video.CostEstimate
	Reserved() float64
	Spent() float64
}

func bestEffortCancelProvider(ctx context.Context, provider video.VideoProvider, providerJobID string) {
	if provider == nil || providerJobID == "" {
		return
	}
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = provider.CancelJob(cancelCtx, providerJobID)
}

type jobOperationLock struct {
	mu   sync.Mutex
	refs int
}

type jobOperationLockSet struct {
	mu      sync.Mutex
	entries map[string]*jobOperationLock
}

var videoJobOperationLocks jobOperationLockSet

// lockVideoJobOperation serializes only terminal/finalization commits for one
// job, not provider polling or rendering. This lets an operator force-fail a
// live job promptly while still preventing completion from winning afterward.
func lockVideoJobOperation(jobID string) func() {
	videoJobOperationLocks.mu.Lock()
	if videoJobOperationLocks.entries == nil {
		videoJobOperationLocks.entries = make(map[string]*jobOperationLock)
	}
	entry := videoJobOperationLocks.entries[jobID]
	if entry == nil {
		entry = &jobOperationLock{}
		videoJobOperationLocks.entries[jobID] = entry
	}
	entry.refs++
	videoJobOperationLocks.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		videoJobOperationLocks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(videoJobOperationLocks.entries, jobID)
		}
		videoJobOperationLocks.mu.Unlock()
	}
}
