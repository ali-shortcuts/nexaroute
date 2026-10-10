package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type WebhookProcessor struct {
	Store interface {
		List(context.Context) ([]video.VideoJob, error)
		Update(context.Context, video.VideoJob) error
	}
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
		if status.State != j.State && !video.CanTransition(j.State, status.State) {
			return fmt.Errorf("invalid webhook state transition")
		}
		j.Progress = status.Progress
		j.State = status.State
		j.OutputAssets = status.Outputs
		if status.Error != "" {
			j.LastError = status.Error
		}
		if err = p.Store.Update(ctx, j); err != nil {
			return err
		}
		p.seen[eventID] = time.Now()
		return nil
	}
	return fmt.Errorf("webhook job not found")
}
