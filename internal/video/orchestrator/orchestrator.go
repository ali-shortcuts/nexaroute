package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type ProviderRegistry interface {
	Get(string) (video.VideoProvider, bool)
}
type Registry map[string]video.VideoProvider

func (r Registry) Get(id string) (video.VideoProvider, bool) { p, ok := r[id]; return p, ok }

type Orchestrator struct {
	Store        queue.JobStore
	Queue        *queue.Queue
	Providers    ProviderRegistry
	Ledger       *cost.Ledger
	PollInterval time.Duration
	MaxPolls     int
}

func (o *Orchestrator) Create(ctx context.Context, r video.VideoRequest) (video.VideoJob, error) {
	r = r.Normalized()
	if err := r.Validate(); err != nil {
		return video.VideoJob{}, err
	}
	if r.IdempotencyKey != "" {
		if old, err := o.Store.GetByIdempotency(ctx, r.IdempotencyKey); err == nil {
			return old, nil
		}
	}
	id := fmt.Sprintf("video-%d", time.Now().UnixNano())
	j := video.NewJob(id, r)
	p, ok := o.Providers.Get(r.ProviderPreference)
	if !ok {
		return video.VideoJob{}, errors.New("no eligible video provider")
	}
	j.Provider, j.Model = p.ID(), r.ModelPreference
	e, err := p.EstimateCost(r)
	if err == nil {
		j.EstimatedCostUSD = e.UpperBoundUSD
		if o.Ledger != nil && !r.DryRun {
			if err = o.Ledger.Reserve(e, r.MaxCostUSD); err != nil {
				return video.VideoJob{}, err
			}
		}
	}
	if err = o.Store.Create(ctx, j); err != nil {
		return video.VideoJob{}, err
	}
	if r.DryRun {
		return j, nil
	}
	if err = o.Queue.Enqueue(ctx, j); err != nil {
		j.State, j.LastError = video.StateFailed, err.Error()
		_ = o.Store.Update(ctx, j)
		return j, err
	}
	return j, nil
}

func (o *Orchestrator) RunJob(ctx context.Context, j video.VideoJob) error {
	p, ok := o.Providers.Get(j.Provider)
	if !ok {
		return o.fail(ctx, j, "provider unavailable")
	}
	if err := j.Transition(video.StateAdmitted); err != nil {
		return o.fail(ctx, j, err.Error())
	}
	_ = o.Store.Update(ctx, j)
	req := j.Request
	submitted, err := p.CreateJob(ctx, req)
	if err != nil {
		return o.fail(ctx, j, err.Error())
	}
	now := time.Now().UTC()
	j.ProviderJobID, j.SubmittedAt = submitted.ProviderJobID, &now
	if err := j.Transition(video.StateSubmitted); err != nil {
		return o.fail(ctx, j, err.Error())
	}
	_ = o.Store.Update(ctx, j)
	interval := o.PollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	max := o.MaxPolls
	if max <= 0 {
		max = 300
	}
	for i := 0; i < max; i++ {
		select {
		case <-ctx.Done():
			_ = j.Transition(video.StateCancelRequested)
			_ = o.Store.Update(ctx, j)
			return ctx.Err()
		case <-time.After(interval):
		}
		st, err := p.GetJob(ctx, j.ProviderJobID)
		if err != nil {
			return o.fail(ctx, j, err.Error())
		}
		j.Progress = st.Progress
		if err := j.Transition(st.State); err != nil {
			return o.fail(ctx, j, err.Error())
		}
		_ = o.Store.Update(ctx, j)
		if st.State == video.StateCompleted {
			j.OutputAssets, j.Progress = st.Outputs, 1
			now = time.Now().UTC()
			j.CompletedAt = &now
			_ = o.Store.Update(ctx, j)
			return nil
		}
		if st.State == video.StateFailed || st.State == video.StateCancelled {
			return o.fail(ctx, j, st.Error)
		}
	}
	return o.fail(ctx, j, "poll deadline exceeded")
}

func (o *Orchestrator) fail(ctx context.Context, j video.VideoJob, msg string) error {
	_ = j.Transition(video.StateFailed)
	j.LastError = msg
	_ = o.Store.Update(ctx, j)
	return errors.New(msg)
}

func (o *Orchestrator) Cancel(ctx context.Context, id string) error {
	j, err := o.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	p, ok := o.Providers.Get(j.Provider)
	if ok && j.ProviderJobID != "" {
		if err = p.CancelJob(ctx, j.ProviderJobID); err != nil {
			return err
		}
	}
	if err := j.Transition(video.StateCancelled); err != nil {
		return err
	}
	return o.Store.Update(ctx, j)
}
