package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
)

var ErrForceFailNotActive = errors.New("video job is not active and cannot be force-failed")

// ErrStoreUnavailable marks a failure of the durable job store (or the
// cost ledger behind it) as opposed to a client, validation, or business
// error. The video API maps it to a generic retryable 503: a backend
// outage must never surface as 404 (which would masquerade as a deleted
// job) or leak backend detail in a 400 body (HA acceptance, issue #652).
// Wrap store-originated errors with wrapStoreErr at the call site; keep
// genuine queue.ErrNotFound unwrapped so missing jobs still map to 404.
var ErrStoreUnavailable = errors.New("video job store is temporarily unavailable")

func wrapStoreErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
}

// isCostBusinessErr reports whether err is a cost-ledger business rejection
// (budget exceeded, unknown price, invalid estimate). These are client
// errors, not backend outages, and must reach callers unwrapped: wrapping
// them as store-unavailable would map a budget rejection to a retryable
// 503 instead of a 400.
func isCostBusinessErr(err error) bool {
	return errors.Is(err, cost.ErrBudgetExceeded) || errors.Is(err, cost.ErrUnknownPrice) || errors.Is(err, cost.ErrInvalidEstimate)
}

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
