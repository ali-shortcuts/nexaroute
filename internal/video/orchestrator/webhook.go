package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type WebhookProcessor struct {
	Store interface {
		List(context.Context) ([]video.VideoJob, error)
		Update(context.Context, video.VideoJob) error
	}
	// Ledger is optional. When set, completed webhook events reconcile the
	// estimate → actual transition; when nil, only the persisted job cost
	// fields are updated. Additive and nil-safe.
	Ledger CostLedger
	mu     sync.Mutex
	seen   map[string]time.Time
	MaxAge time.Duration
}

func NewWebhookProcessor(s interface {
	List(context.Context) ([]video.VideoJob, error)
	Update(context.Context, video.VideoJob) error
}) *WebhookProcessor {
	return &WebhookProcessor{Store: s, seen: map[string]time.Time{}, MaxAge: 5 * time.Minute}
}
func VerifyHMAC(req *http.Request, secret string) error {
	ts := req.Header.Get("X-Video-Timestamp")
	sig := req.Header.Get("X-Video-Signature")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(n, 0)) > 5*time.Minute || time.Since(time.Unix(n, 0)) < -5*time.Minute {
		return fmt.Errorf("invalid or expired webhook timestamp")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}
func (p *WebhookProcessor) Apply(ctx context.Context, eventID string, status video.ProviderJobStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string]time.Time{}
	}
	if eventID == "" {
		return fmt.Errorf("event id is required")
	}
	if _, ok := p.seen[eventID]; ok {
		return nil
	}
	jobs, err := p.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.ProviderJobID != status.ProviderJobID {
			continue
		}
		unlock := lockVideoJobOperation(j.JobID)
		defer unlock()
		jobs, err = p.Store.List(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, current := range jobs {
			if current.JobID == j.JobID {
				j, found = current, true
				break
			}
		}
		if !found {
			return fmt.Errorf("webhook job not found")
		}
		if j.OperatorForceFailed {
			p.seen[eventID] = time.Now()
			return nil
		}
		// Idempotent by terminal state and event ID: replaying a completion
		// for an already-completed job is a no-op and never double-settles.
		if j.State == video.StateCompleted && status.State == video.StateCompleted {
			p.seen[eventID] = time.Now()
			return nil
		}
		if status.State != j.State && !video.CanTransition(j.State, status.State) {
			return fmt.Errorf("invalid webhook state transition")
		}
		j.Progress = status.Progress
		j.State = status.State
		j.OutputAssets = status.Outputs
		if status.Error != "" {
			j.LastError = status.Error
		}
		if status.State == video.StateCompleted {
			if actual, ok := webhookActualCost(status); ok {
				delta := actual - j.EstimatedCostUSD
				j.ActualCostUSD = actual
				j.CostDeltaUSD = &delta
				j.CostReconciled = true
				j.CostBasis = "actual"
			} else {
				j.ActualCostUSD = 0
				j.CostDeltaUSD = nil
				j.CostReconciled = false
				j.CostBasis = "estimate"
			}
		}
		if status.State == video.StateCompleted && p.Ledger != nil {
			if j.CostReconciled {
				if err := p.Ledger.SettleActual(j.JobID, j.ActualCostUSD, j.EstimatedCostUSD); err != nil {
					return err
				}
			} else if err := p.Ledger.SettleEstimate(j.JobID, j.EstimatedCostUSD); err != nil {
				return err
			}
		}
		if err = p.Store.Update(ctx, j); err != nil {
			return err
		}
		p.seen[eventID] = time.Now()
		return nil
	}
	return fmt.Errorf("webhook job not found")
}

// webhookActualCost mirrors providerActualCost without creating an import
// cycle risk if the helper ever moves; it prefers the explicit field and
// falls back to the legacy usage entry.
func webhookActualCost(st video.ProviderJobStatus) (float64, bool) {
	if st.ActualCostUSD != nil && *st.ActualCostUSD >= 0 && !math.IsNaN(*st.ActualCostUSD) && !math.IsInf(*st.ActualCostUSD, 0) {
		return *st.ActualCostUSD, true
	}
	if raw := strings.TrimSpace(st.Usage["actual_cost_usd"]); raw != "" {
		if actual, err := strconv.ParseFloat(raw, 64); err == nil && actual >= 0 && !math.IsNaN(actual) && !math.IsInf(actual, 0) {
			return actual, true
		}
	}
	return 0, false
}
