package orchestrator

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type ProviderRegistry interface {
	Get(string) (video.VideoProvider, bool)
}
type Registry map[string]video.VideoProvider

type Narrator interface {
	Synthesize(context.Context, string, string) error
}

func (r Registry) Get(id string) (video.VideoProvider, bool) { p, ok := r[id]; return p, ok }

type Orchestrator struct {
	Store         queue.JobStore
	Queue         *queue.Queue
	Providers     ProviderRegistry
	Ledger        CostLedger
	Assets        video.AssetSink
	Composer      EpisodeComposer
	Narrator      Narrator
	MaxAssetBytes int64
	PollInterval  time.Duration
	MaxPolls      int
}

func (o *Orchestrator) Create(ctx context.Context, r video.VideoRequest) (video.VideoJob, error) {
	if err := ctx.Err(); err != nil {
		return video.VideoJob{}, err
	}
	// OutputAsset is a server-owned checkpoint. Never trust caller-supplied
	// values here, or a request could skip generation and point the composer at
	// an arbitrary local path.
	for i := range r.ShotPlan {
		r.ShotPlan[i].OutputAsset = nil
	}
	if len(r.ShotPlan) > 1 && strings.TrimSpace(r.NarrationText) == "" {
		segments := make([]string, 0, len(r.ShotPlan))
		for _, shot := range r.ShotPlan {
			if segment := strings.TrimSpace(shot.NarrationSegment); segment != "" {
				segments = append(segments, segment)
			}
		}
		r.NarrationText = strings.Join(segments, "\n\n")
	}
	if len(r.ShotPlan) == 1 {
		shot := r.ShotPlan[0]
		if strings.TrimSpace(shot.ShotID) == "" {
			return video.VideoJob{}, errors.New("single shot requires shot_id")
		}
		if shot.Prompt == "" {
			shot.Prompt = r.Prompt
		}
		if shot.NegativePrompt == "" {
			shot.NegativePrompt = r.NegativePrompt
		}
		if shot.DurationSeconds <= 0 || math.IsNaN(shot.DurationSeconds) || math.IsInf(shot.DurationSeconds, 0) {
			return video.VideoJob{}, fmt.Errorf("shot %q has an invalid duration", shot.ShotID)
		}
		if r.DurationSeconds > 0 && math.Abs(r.DurationSeconds-shot.DurationSeconds) > 0.05 {
			return video.VideoJob{}, fmt.Errorf("duration_seconds %.2f does not match shot duration %.2f", r.DurationSeconds, shot.DurationSeconds)
		}
		if shot.Provider != "" {
			if r.ProviderPreference != "" && r.ProviderPreference != shot.Provider {
				return video.VideoJob{}, errors.New("single shot provider conflicts with provider_preference")
			}
			r.ProviderPreference = shot.Provider
		}
		if shot.Model != "" {
			if r.ModelPreference != "" && r.ModelPreference != shot.Model {
				return video.VideoJob{}, errors.New("single shot model conflicts with model_preference")
			}
			r.ModelPreference = shot.Model
		}
		r.Prompt = shotPrompt(shot)
		r.NegativePrompt = shot.NegativePrompt
		r.DurationSeconds = shot.DurationSeconds
		r.ReferenceAssets = append(r.ReferenceAssets, shot.RequiredReferences...)
		r.AudioRequested = r.AudioRequested || len(shot.SoundRequirements) > 0
		if shot.NarrationSegment != "" {
			r.NarrationText = shot.NarrationSegment
		}
		r.ShotPlan = nil
	}
	if len(r.ShotPlan) > 1 {
		totalDuration := 0.0
		for _, shot := range r.ShotPlan {
			if shot.DurationSeconds <= 0 || math.IsNaN(shot.DurationSeconds) || math.IsInf(shot.DurationSeconds, 0) {
				return video.VideoJob{}, fmt.Errorf("shot %q has an invalid duration", shot.ShotID)
			}
			totalDuration += shot.DurationSeconds
		}
		if r.DurationSeconds < 0 || math.IsNaN(r.DurationSeconds) || math.IsInf(r.DurationSeconds, 0) {
			return video.VideoJob{}, errors.New("duration_seconds is invalid")
		}
		if r.DurationSeconds > 0 && math.Abs(r.DurationSeconds-totalDuration) > 0.05 {
			return video.VideoJob{}, fmt.Errorf("duration_seconds %.2f does not match total shot duration %.2f", r.DurationSeconds, totalDuration)
		}
		r.DurationSeconds = totalDuration
	}
	r = r.Normalized()
	if err := r.Validate(); err != nil {
		return video.VideoJob{}, err
	}
	if o.Store == nil || o.Queue == nil || o.Providers == nil {
		return video.VideoJob{}, errors.New("video orchestrator is not fully configured")
	}
	// A provider that handles narration itself (for example code-render,
	// which composes Piper narration inside its own render pipeline) is
	// exempt from the multi-shot Gateway narration gate; it validates
	// narration support in its own ValidateRequest instead.
	providerHandlesNarration := false
	if o.Providers != nil && strings.TrimSpace(r.ProviderPreference) != "" {
		if p, ok := o.Providers.Get(r.ProviderPreference); ok {
			if hn, ok := p.(interface{ HandlesNarration() bool }); ok && hn.HandlesNarration() {
				providerHandlesNarration = true
			}
		}
	}
	if strings.TrimSpace(r.SubtitleLanguage) != "" && strings.TrimSpace(r.NarrationText) == "" && !providerHandlesNarration {
		return video.VideoJob{}, errors.New("subtitle_language requires narration_text or per-shot narration_segment values")
	}
	if strings.TrimSpace(r.NarrationText) != "" && !providerHandlesNarration {
		if len(r.ShotPlan) < 2 {
			return video.VideoJob{}, errors.New("Gateway narration requires a multi-shot episode with at least two shots")
		}
		if o.Narrator == nil {
			return video.VideoJob{}, errors.New("narration_text requires video.piper_enabled with a valid local Piper model")
		}
		if r.AudioRequested || r.DialogueRequested {
			return video.VideoJob{}, errors.New("local Piper supports narration only; provider audio and dialogue generation are not configured")
		}
		for _, shot := range r.ShotPlan {
			if len(shot.SoundRequirements) > 0 {
				return video.VideoJob{}, fmt.Errorf("shot %q requests sound effects but only narration synthesis is configured", shot.ShotID)
			}
		}
	}
	if len(r.ShotPlan) > 1 {
		width, height := episodeDimensions(r)
		if width%2 != 0 || height%2 != 0 {
			return video.VideoJob{}, errors.New("multi-shot episode dimensions must be even for H.264 composition")
		}
	}
	if o.Store == nil || o.Queue == nil || o.Providers == nil {
		return video.VideoJob{}, errors.New("video orchestrator is not fully configured")
	}
	if r.IdempotencyKey != "" {
		old, err := o.Store.GetByIdempotency(ctx, r.IdempotencyKey)
		switch {
		case err == nil:
			return old, nil
		case errors.Is(err, queue.ErrNotFound):
		case err != nil:
			return video.VideoJob{}, fmt.Errorf("lookup idempotency key: %w", err)
		}
	}
	p, ok := o.Providers.Get(r.ProviderPreference)
	if !ok {
		return video.VideoJob{}, errors.New("no eligible video provider")
	}
	caps, err := p.Capabilities(ctx)
	if err != nil {
		return video.VideoJob{}, fmt.Errorf("read provider capabilities: %w", err)
	}
	var estimate video.CostEstimate
	if len(r.ShotPlan) > 1 {
		if !r.DryRun && (o.Composer == nil || o.Assets == nil) {
			return video.VideoJob{}, errors.New("multi-shot video jobs require an episode composer and asset sink")
		}
		if !r.DryRun {
			if preflight, ok := o.Composer.(interface{ Preflight(context.Context) error }); ok {
				if err := preflight.Preflight(ctx); err != nil {
					return video.VideoJob{}, fmt.Errorf("episode composer preflight failed before provider submission: %w", err)
				}
			}
		}
		r, estimate, err = validateShotPlanRequest(r, p, caps)
		if err != nil {
			return video.VideoJob{}, err
		}
	} else {
		r, err = validateProviderRequest(r, caps)
		if err != nil {
			return video.VideoJob{}, err
		}
		if err := p.ValidateRequest(r); err != nil {
			return video.VideoJob{}, fmt.Errorf("provider rejected request: %w", err)
		}
		estimate, err = p.EstimateCost(r)
		if err != nil {
			return video.VideoJob{}, fmt.Errorf("cannot safely estimate video cost; refusing submission: %w", err)
		}
	}
	if !validEstimate(estimate) {
		return video.VideoJob{}, cost.ErrInvalidEstimate
	}
	if estimate.Provider != "" && estimate.Provider != p.ID() {
		return video.VideoJob{}, errors.New("provider returned a cost estimate for a different provider")
	}
	if estimate.Model != "" && estimate.Model != r.ModelPreference {
		return video.VideoJob{}, errors.New("provider returned a cost estimate for a different model")
	}
	if r.MaxCostUSD > 0 && o.Ledger == nil {
		return video.VideoJob{}, errors.New("max_cost_usd requires an enabled cost ledger")
	}

	j := video.NewJob(fmt.Sprintf("video-%d", time.Now().UnixNano()), r)
	j.Provider, j.Model = p.ID(), r.ModelPreference
	j.EstimatedCostUSD = estimate.EstimatedUSD
	if r.DryRun {
		j.State = video.StateDryRun
		j.LastError = "estimate only; no provider job was submitted"
		if err := o.Store.Create(ctx, j); err != nil {
			return video.VideoJob{}, err
		}
		return j, nil
	}
	reserved := false
	if o.Ledger != nil {
		if err := o.Ledger.ReserveForJob(j.JobID, estimate, r.MaxCostUSD); err != nil {
			return video.VideoJob{}, err
		}
		reserved = true
	}
	if err := o.Store.Create(ctx, j); err != nil {
		if reserved {
			o.Ledger.ReleaseForJob(j.JobID)
		}
		return video.VideoJob{}, err
	}
	if err := o.Queue.Enqueue(ctx, j); err != nil {
		j.State, j.LastError = video.StateFailed, "queue admission failed"
		persistErr := o.Store.Update(ctx, j)
		if reserved {
			o.Ledger.ReleaseForJob(j.JobID)
		}
		if persistErr != nil {
			return video.VideoJob{}, fmt.Errorf("queue admission failed: %v; failure state not durable: %w", err, persistErr)
		}
		return video.VideoJob{}, fmt.Errorf("queue admission failed: %w", err)
	}
	return j, nil
}

func validEstimate(e video.CostEstimate) bool {
	return e.PriceKnown && e.EstimatedUSD >= 0 && e.UpperBoundUSD >= e.EstimatedUSD &&
		!math.IsNaN(e.EstimatedUSD) && !math.IsInf(e.EstimatedUSD, 0) &&
		!math.IsNaN(e.UpperBoundUSD) && !math.IsInf(e.UpperBoundUSD, 0) &&
		strings.EqualFold(strings.TrimSpace(e.Currency), "USD")
}

func validateProviderRequest(r video.VideoRequest, caps video.Capabilities) (video.VideoRequest, error) {
	modeOK := false
	for _, mode := range caps.Modes {
		if mode == r.Mode {
			modeOK = true
			break
		}
	}
	if !modeOK {
		return r, fmt.Errorf("provider does not advertise support for video mode %q", r.Mode)
	}
	if caps.MaxDurationSeconds <= 0 || math.IsNaN(caps.MaxDurationSeconds) || math.IsInf(caps.MaxDurationSeconds, 0) {
		return r, errors.New("provider has no verified maximum duration; refusing to assume support")
	}
	if r.DurationSeconds > caps.MaxDurationSeconds {
		return r, fmt.Errorf("requested clip duration %.2fs exceeds provider maximum %.2fs", r.DurationSeconds, caps.MaxDurationSeconds)
	}
	if r.ModelPreference == "" {
		if len(caps.Models) == 0 {
			return r, errors.New("provider did not advertise a selectable model")
		}
		r.ModelPreference = caps.Models[0]
	} else {
		if len(caps.Models) == 0 {
			return r, errors.New("provider did not advertise its model capabilities")
		}
		found := false
		for _, model := range caps.Models {
			if model == r.ModelPreference {
				found = true
				break
			}
		}
		if !found {
			return r, fmt.Errorf("provider does not advertise model %q", r.ModelPreference)
		}
	}
	if len(caps.AspectRatios) == 0 {
		return r, errors.New("provider did not advertise supported aspect ratios")
	}
	ratioOK := false
	for _, ratio := range caps.AspectRatios {
		if ratio == r.AspectRatio {
			ratioOK = true
			break
		}
	}
	if !ratioOK {
		return r, fmt.Errorf("provider does not support aspect ratio %q", r.AspectRatio)
	}
	if r.AudioRequested && !caps.SupportsAudio {
		return r, errors.New("provider does not support requested audio generation")
	}
	if r.DialogueRequested && !caps.SupportsAudio {
		return r, errors.New("provider does not support requested dialogue/audio")
	}
	return r, nil
}

func mergeUsage(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	// Never mutate dst in place: job stores may hand out shallow copies
	// that share this map with concurrent readers (for example the API
	// handler JSON-encoding a job while a worker merges provider usage).
	// Merging into a fresh map keeps those readers race-free.
	out := make(map[string]string, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

// validActualCost reports whether a provider-reported actual is billable.
func validActualCost(v float64) bool {
	return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// providerActualCost extracts the provider-reported actual from a completed
// job status. The explicit ActualCostUSD field takes precedence; the legacy
// Usage["actual_cost_usd"] entry is honored for backwards compatibility.
// Invalid or absent values report ok=false so the job stays explicitly
// unreconciled instead of failing or being misrepresented as an actual.
func providerActualCost(st video.ProviderJobStatus) (float64, bool) {
	if st.ActualCostUSD != nil && validActualCost(*st.ActualCostUSD) {
		return *st.ActualCostUSD, true
	}
	if raw := strings.TrimSpace(st.Usage["actual_cost_usd"]); raw != "" {
		if actual, err := strconv.ParseFloat(raw, 64); err == nil && validActualCost(actual) {
			return actual, true
		}
	}
	return 0, false
}

// applyCostReconciliation records the estimate → actual transition on the
// job. Reconciled jobs carry ActualCostUSD, CostDeltaUSD (actual − estimate)
// and CostBasis "actual"; unreconciled jobs keep the estimate with
// CostReconciled=false, no delta, no actual, and CostBasis "estimate".
func applyCostReconciliation(j *video.VideoJob, actual *float64) {
	if actual != nil && validActualCost(*actual) {
		delta := *actual - j.EstimatedCostUSD
		j.ActualCostUSD = *actual
		j.CostDeltaUSD = &delta
		j.CostReconciled = true
		j.CostBasis = "actual"
		return
	}
	j.ActualCostUSD = 0
	j.CostDeltaUSD = nil
	j.CostReconciled = false
	j.CostBasis = "estimate"
}

func (o *Orchestrator) Recover(ctx context.Context) error {
	if o.Store == nil || o.Queue == nil {
		return errors.New("video recovery requires a store and queue")
	}
	jobs, err := o.Store.List(ctx)
	if err != nil {
		return fmt.Errorf("list persisted video jobs for recovery: %w", err)
	}
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	for _, job := range jobs {
		if job.Request.DryRun || job.State == video.StateDryRun || job.State == video.StateCompleted ||
			job.State == video.StateFailed || job.State == video.StateCancelled || job.State == video.StateNeedsManualAction {
			continue
		}
		if len(job.Request.ShotPlan) > 1 {
			if len(job.ShotExecutions) != len(job.Request.ShotPlan) {
				if err := o.markNeedsManual(ctx, job, "multi-shot job has no complete durable shot checkpoint list"); err != nil {
					return err
				}
				continue
			}
			ambiguous := false
			if job.ProviderJobID != "" {
				current := job.CurrentShot - 1
				if current < 0 || current >= len(job.ShotExecutions) || job.ShotExecutions[current].ProviderJobID != job.ProviderJobID {
					if err := o.markNeedsManual(ctx, job, "parent provider job ID does not match the current shot checkpoint"); err != nil {
						return err
					}
					continue
				}
			}
			for i, run := range job.ShotExecutions {
				if job.Request.ShotPlan[i].OutputAsset == nil && run.ProviderJobID == "" && run.State != video.StateQueued {
					if err := o.markNeedsManual(ctx, job, fmt.Sprintf("shot %q has an unknown submission checkpoint; automatic resubmission is disabled", run.ShotID)); err != nil {
						return err
					}
					ambiguous = true
					break
				}
			}
			if ambiguous {
				continue
			}
			switch job.State {
			case video.StateQueued, video.StateAdmitted, video.StateSubmitted, video.StateProcessing, video.StatePolling, video.StateUploading, video.StateComposing, video.StateCancelRequested:
				if err := o.enqueueWithBackpressure(ctx, job); err != nil {
					return fmt.Errorf("enqueue recovered episode job %s: %w", job.JobID, err)
				}
			default:
				if err := o.markNeedsManual(ctx, job, "multi-shot episode state is not restart-resumable"); err != nil {
					return err
				}
			}
			continue
		}
		switch job.State {
		case video.StateQueued:
			if err := o.enqueueWithBackpressure(ctx, job); err != nil {
				return fmt.Errorf("recover queued video job %s: %w", job.JobID, err)
			}
		case video.StateAdmitted:
			if job.ProviderJobID == "" {
				if err := o.markNeedsManual(ctx, job, "previous submission outcome is unknown; automatic resubmission is disabled to avoid duplicate provider charges"); err != nil {
					return err
				}
				continue
			}
			if err := job.Transition(video.StateSubmitted); err != nil {
				return fmt.Errorf("recover video job %s state: %w", job.JobID, err)
			}
			if err := o.Store.Update(ctx, job); err != nil {
				return fmt.Errorf("persist recovered video job %s: %w", job.JobID, err)
			}
			if err := o.enqueueWithBackpressure(ctx, job); err != nil {
				return fmt.Errorf("enqueue recovered video job %s: %w", job.JobID, err)
			}
		case video.StateSubmitted, video.StateProcessing, video.StatePolling, video.StateUploading, video.StateCancelRequested:
			if job.ProviderJobID == "" {
				if err := o.markNeedsManual(ctx, job, "active job has no provider job ID; automatic resubmission is disabled"); err != nil {
					return err
				}
				continue
			}
			if err := o.enqueueWithBackpressure(ctx, job); err != nil {
				return fmt.Errorf("enqueue recovered video job %s: %w", job.JobID, err)
			}
		case video.StatePlanning, video.StateWaitingForBudget, video.StateComposing:
			if err := o.markNeedsManual(ctx, job, "this pipeline stage is not restart-resumable yet"); err != nil {
				return err
			}
		default:
			if err := o.markNeedsManual(ctx, job, "unknown or unsupported persisted job state"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *Orchestrator) enqueueWithBackpressure(ctx context.Context, job video.VideoJob) error {
	for {
		err := o.Queue.Enqueue(ctx, job)
		if !errors.Is(err, queue.ErrFull) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (o *Orchestrator) scheduleRetry(ctx context.Context, job video.VideoJob, delay time.Duration) {
	if o.Queue == nil || ctx.Err() != nil {
		return
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		_ = o.enqueueWithBackpressure(ctx, job)
	}()
}

func (o *Orchestrator) markNeedsManual(ctx context.Context, j video.VideoJob, reason string) error {
	if j.State != video.StateNeedsManualAction {
		if err := j.Transition(video.StateNeedsManualAction); err != nil {
			return fmt.Errorf("cannot mark video job for manual action: %w", err)
		}
	}
	j.LastError = reason
	return o.Store.Update(ctx, j)
}

func (o *Orchestrator) markNeedsManualAndError(ctx context.Context, j video.VideoJob, reason string) error {
	if err := o.markNeedsManual(ctx, j, reason); err != nil {
		return err
	}
	return errors.New(reason)
}

func (o *Orchestrator) RunJob(ctx context.Context, queued video.VideoJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.Store == nil {
		return errors.New("video orchestrator is not fully configured")
	}
	j, err := o.Store.Get(ctx, queued.JobID)
	if err != nil {
		return fmt.Errorf("load video job before execution: %w", err)
	}
	if len(j.Request.ShotPlan) > 1 {
		return o.runEpisodeJob(ctx, j)
	}
	if o.Providers == nil {
		return errors.New("video orchestrator is not fully configured")
	}
	switch j.State {
	case video.StateCompleted, video.StateFailed, video.StateCancelled, video.StateDryRun:
		return nil
	case video.StateNeedsManualAction:
		return errors.New("video job requires manual action")
	case video.StateQueued:
		if err := j.Transition(video.StateAdmitted); err != nil {
			return err
		}
		if err := o.Store.Update(ctx, j); err != nil {
			return fmt.Errorf("persist provider-admission state: %w", err)
		}
		p, ok := o.Providers.Get(j.Provider)
		if !ok {
			return o.fail(ctx, j, "provider unavailable")
		}
		submitted, submitErr := p.CreateJob(ctx, j.Request)
		if submitErr != nil {
			if err := o.markNeedsManual(ctx, j, "provider submission outcome is unknown; automatic resubmission is disabled"); err != nil {
				return err
			}
			return fmt.Errorf("provider submission outcome is unknown: %w", submitErr)
		}
		if strings.TrimSpace(submitted.ProviderJobID) == "" {
			if err := o.markNeedsManual(ctx, j, "provider returned no confirmed job ID; automatic resubmission is disabled"); err != nil {
				return err
			}
			return errors.New("provider did not confirm a job ID; manual action is required")
		}
		// Keep the selected provider as the canonical owner of this submission.
		// Even an inconsistent response must not discard a returned job ID and
		// accidentally trigger another billable submission after restart.
		j.ProviderJobID = strings.TrimSpace(submitted.ProviderJobID)
		j.Provider = p.ID()
		if submitted.Model != "" {
			j.Model = submitted.Model
		}
		now := time.Now().UTC()
		j.SubmittedAt = &now
		if err := j.Transition(video.StateSubmitted); err != nil {
			return err
		}
		if err := o.Store.Update(ctx, j); err != nil {
			bestEffortCancelProvider(ctx, p, j.ProviderJobID)
			return fmt.Errorf("provider returned a job ID but it was not durably saved; do not resubmit: %w", err)
		}
		if !submitted.Accepted || (submitted.Provider != "" && submitted.Provider != p.ID()) || (submitted.Model != "" && submitted.Model != j.Request.ModelPreference) {
			if err := o.markNeedsManual(ctx, j, "provider submission response was inconsistent; the known provider job ID is retained and automatic retry is disabled"); err != nil {
				return err
			}
			return errors.New("provider submission response was inconsistent; manual action is required")
		}
	case video.StateAdmitted:
		if j.ProviderJobID == "" {
			if err := o.markNeedsManual(ctx, j, "provider submission outcome is unknown; automatic resubmission is disabled"); err != nil {
				return err
			}
			return errors.New("provider submission outcome is unknown; manual action is required")
		}
		if err := j.Transition(video.StateSubmitted); err != nil {
			return err
		}
		if err := o.Store.Update(ctx, j); err != nil {
			return err
		}
	case video.StateSubmitted, video.StateProcessing, video.StatePolling, video.StateUploading, video.StateCancelRequested:
		if j.ProviderJobID == "" {
			if err := o.markNeedsManual(ctx, j, "active job has no provider job ID; automatic resubmission is disabled"); err != nil {
				return err
			}
			return errors.New("active video job has no provider job ID")
		}
	default:
		if err := o.markNeedsManual(ctx, j, "unsupported job state for automatic execution"); err != nil {
			return err
		}
		return errors.New("unsupported video job state")
	}
	p, ok := o.Providers.Get(j.Provider)
	if !ok {
		return o.fail(ctx, j, "provider unavailable")
	}
	if j.State == video.StateCancelRequested {
		if err := p.CancelJob(ctx, j.ProviderJobID); err != nil {
			return err
		}
		if err := j.Transition(video.StateCancelled); err != nil {
			return err
		}
		j.LastError = ""
		if err := o.Store.Update(ctx, j); err != nil {
			return err
		}
		if o.Ledger != nil {
			_ = o.Ledger.SettleEstimate(j.JobID, j.EstimatedCostUSD)
		}
		return nil
	}
	return o.pollAndFinalize(ctx, p, j)
}

func (o *Orchestrator) pollAndFinalize(ctx context.Context, p video.VideoProvider, j video.VideoJob) error {
	interval := o.PollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	max := o.MaxPolls
	if max <= 0 {
		max = 300
	}
	for i := 0; i < max; i++ {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Shutdown is not a user cancellation. Preserve state for recovery.
			return ctx.Err()
		case <-timer.C:
		}
		st, err := p.GetJob(ctx, j.ProviderJobID)
		if err != nil {
			j.RetryCount++
			j.LastError = "provider status lookup failed; job remains resumable"
			if saveErr := o.Store.Update(ctx, j); saveErr != nil {
				return fmt.Errorf("provider status lookup failed and job state could not be saved: %w", saveErr)
			}
			continue
		}
		if st.ProviderJobID != "" && st.ProviderJobID != j.ProviderJobID {
			return o.markNeedsManualAndError(ctx, j, "provider returned a different job ID")
		}
		if math.IsNaN(st.Progress) || math.IsInf(st.Progress, 0) || st.Progress < 0 || st.Progress > 1 {
			return o.markNeedsManualAndError(ctx, j, "provider returned invalid progress metadata")
		}
		if st.Progress > j.Progress {
			j.Progress = st.Progress
		}
		j.UsageMetadata = mergeUsage(j.UsageMetadata, st.Usage)
		switch st.State {
		case video.StateFailed:
			return o.fail(ctx, j, "provider reported job failure")
		case video.StateCancelled:
			if err := j.Transition(video.StateCancelled); err != nil {
				return err
			}
			j.LastError = ""
			if err := o.Store.Update(ctx, j); err != nil {
				return err
			}
			if o.Ledger != nil {
				o.Ledger.ReleaseForJob(j.JobID)
			}
			return nil
		case video.StateCompleted:
			if err := o.downloadAndComplete(ctx, p, j, st); err != nil {
				latest, getErr := o.Store.Get(ctx, j.JobID)
				if getErr == nil && latest.State == video.StateUploading {
					o.scheduleRetry(ctx, latest, interval)
				}
				return err
			}
			return nil
		case video.StateQueued, video.StateSubmitted, video.StateProcessing, video.StatePolling:
			if j.State == video.StateUploading {
				return o.markNeedsManualAndError(ctx, j, "provider status regressed after output upload began")
			}
			if st.State == video.StateQueued {
				// Queued is a valid remote status; never regress the persisted parent job.
			} else if st.State == video.StateSubmitted && (j.State == video.StateProcessing || j.State == video.StatePolling) {
				// A stale provider response must not regress persisted state.
			} else if err := j.Transition(st.State); err != nil {
				return o.markNeedsManualAndError(ctx, j, "provider returned an unsupported state transition")
			}
			j.LastError = ""
			if err := o.Store.Update(ctx, j); err != nil {
				return fmt.Errorf("persist provider job progress: %w", err)
			}
		default:
			return o.markNeedsManualAndError(ctx, j, "provider returned an unsupported job state")
		}
	}
	j.LastError = "polling deadline reached; remote job remains resumable"
	if err := o.Store.Update(ctx, j); err != nil {
		return fmt.Errorf("polling deadline reached and job state could not be saved: %w", err)
	}
	o.scheduleRetry(ctx, j, interval)
	return errors.New(j.LastError)
}

func (o *Orchestrator) downloadAndComplete(ctx context.Context, p video.VideoProvider, j video.VideoJob, st video.ProviderJobStatus) error {
	// Idempotent by job ID and terminal state: a replayed completion for an
	// already-completed job must not re-persist outputs or double-settle
	// the ledger.
	if j.State == video.StateCompleted {
		return nil
	}
	if len(st.Outputs) == 0 {
		return o.markNeedsManualAndError(ctx, j, "provider marked job complete but supplied no output assets")
	}
	if o.Assets == nil {
		return o.markNeedsManualAndError(ctx, j, "no output asset sink is configured")
	}
	if j.State != video.StateUploading {
		if err := j.Transition(video.StateUploading); err != nil {
			return o.markNeedsManualAndError(ctx, j, "could not enter output-upload stage")
		}
	}
	j.LastError = ""
	if err := o.Store.Update(ctx, j); err != nil {
		return fmt.Errorf("persist output-upload state: %w", err)
	}
	for i := range st.Outputs {
		meta := make(map[string]string, len(st.Outputs[i].Metadata)+3)
		for k, v := range st.Outputs[i].Metadata {
			meta[k] = v
		}
		meta["project_id"], meta["episode_id"], meta["job_id"] = j.ProjectID, j.EpisodeID, j.JobID
		st.Outputs[i].Metadata = meta
	}
	assets, err := p.DownloadOutputs(ctx, st, o.Assets)
	if err != nil {
		j.LastError = "output download or persistence failed; job remains resumable"
		if saveErr := o.Store.Update(ctx, j); saveErr != nil {
			return fmt.Errorf("output download failed and job state could not be saved: %w", saveErr)
		}
		return fmt.Errorf("output download or persistence failed: %w", err)
	}
	if len(assets) == 0 {
		return o.markNeedsManualAndError(ctx, j, "provider returned no persisted output assets")
	}
	for _, a := range assets {
		if strings.TrimSpace(a.URI) == "" || strings.TrimSpace(a.ContentType) == "" || a.SizeBytes <= 0 {
			return o.markNeedsManualAndError(ctx, j, "provider output was not persisted with URI, content type, and non-empty size")
		}
		if len(a.SHA256) != 64 {
			return o.markNeedsManualAndError(ctx, j, "provider output was not persisted with a SHA-256 checksum")
		}
		if _, err := hex.DecodeString(a.SHA256); err != nil {
			return o.markNeedsManualAndError(ctx, j, "provider output checksum is malformed")
		}
	}
	unlock := lockVideoJobOperation(j.JobID)
	defer unlock()
	latest, err := o.Store.Get(ctx, j.JobID)
	if err != nil {
		return fmt.Errorf("reload video job before completion commit: %w", err)
	}
	if latest.IsTerminal() || latest.State == video.StateFailed {
		// A force-fail or competing terminal transition won while the provider
		// output was being downloaded. Ignore this late result; never settle it.
		return nil
	}
	j = latest
	var actualCost *float64
	if actual, ok := providerActualCost(st); ok {
		actualCost = &actual
	}
	if o.Ledger != nil {
		var settleErr error
		if actualCost != nil {
			settleErr = o.Ledger.SettleActual(j.JobID, *actualCost, j.EstimatedCostUSD)
		} else {
			settleErr = o.Ledger.SettleEstimate(j.JobID, j.EstimatedCostUSD)
		}
		if settleErr != nil {
			applyCostReconciliation(&j, nil)
			j.LastError = "cost settlement failed; completed provider output remains resumable"
			if saveErr := o.Store.Update(ctx, j); saveErr != nil {
				return fmt.Errorf("cost settlement failed (%v) and resumable job state could not be saved: %w", settleErr, saveErr)
			}
			return fmt.Errorf("video completion cost settlement failed: %w", settleErr)
		}
	}
	applyCostReconciliation(&j, actualCost)
	j.OutputAssets = assets
	j.Progress = 1
	j.LastError = ""
	now := time.Now().UTC()
	j.CompletedAt = &now
	if err := j.Transition(video.StateCompleted); err != nil {
		return err
	}
	if err := o.Store.Update(ctx, j); err != nil {
		return fmt.Errorf("outputs were persisted but completion state could not be saved: %w", err)
	}
	return nil
}

// ForceFail is the explicit operator escape hatch for jobs whose provider
// refuses ordinary cancellation. Provider cancellation is best-effort; the
// durable local failed state and reservation release are authoritative. The
// returned booleans report whether cancellation was attempted and acknowledged.
func (o *Orchestrator) ForceFail(ctx context.Context, id string) (attempted, confirmed bool, err error) {
	if o == nil || o.Store == nil {
		return false, false, errors.New("video orchestrator is not fully configured")
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	j, err := o.Store.Get(ctx, id)
	if err != nil {
		return false, false, err
	}
	if j.IsTerminal() || j.State == video.StateFailed {
		return false, false, ErrForceFailNotActive
	}
	providerID := j.ProviderJobID
	if providerID != "" && o.Providers != nil {
		if p, ok := o.Providers.Get(j.Provider); ok {
			attempted = true
			cancelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cancelErr := p.CancelJob(cancelCtx, providerID)
			cancel()
			confirmed = cancelErr == nil
		}
	}

	unlock := lockVideoJobOperation(id)
	defer unlock()
	// Re-read under the per-job commit lock. If completion or a normal cancel
	// won while provider cancellation was in flight, do not override it.
	j, err = o.Store.Get(ctx, id)
	if err != nil {
		return attempted, confirmed, err
	}
	if j.IsTerminal() || j.State == video.StateFailed {
		return attempted, confirmed, ErrForceFailNotActive
	}
	j.State = video.StateFailed // privileged override; ordinary transitions stay strict.
	j.OperatorForceFailed = true
	switch {
	case !attempted:
		j.LastError = "Force-failed by operator before provider cancellation could be attempted; remote work may continue and costs may accrue."
	case confirmed:
		j.LastError = "Force-failed by operator; provider acknowledged cancellation."
	default:
		j.LastError = "Force-failed by operator; provider cancellation was not confirmed, so remote work may continue and costs may accrue."
	}
	if err := o.Store.Update(ctx, j); err != nil {
		return attempted, confirmed, fmt.Errorf("persist operator force-fail: %w", err)
	}
	if o.Ledger != nil {
		o.Ledger.ReleaseForJob(j.JobID)
	}
	return attempted, confirmed, nil
}

func (o *Orchestrator) fail(ctx context.Context, j video.VideoJob, msg string) error {
	if j.State != video.StateFailed {
		if err := j.Transition(video.StateFailed); err != nil {
			return fmt.Errorf("cannot mark video job failed: %w", err)
		}
	}
	j.LastError = msg
	if err := o.Store.Update(ctx, j); err != nil {
		return fmt.Errorf("persist failed video job: %w", err)
	}
	if o.Ledger != nil {
		_ = o.Ledger.SettleEstimate(j.JobID, j.EstimatedCostUSD)
	}
	return errors.New(msg)
}

func (o *Orchestrator) Cancel(ctx context.Context, id string) error {
	j, err := o.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	switch j.State {
	case video.StateCompleted, video.StateFailed, video.StateCancelled, video.StateDryRun:
		return errors.New("video job is already terminal")
	}
	if len(j.Request.ShotPlan) > 1 && j.State == video.StateComposing {
		return errors.New("episode is composing locally; cancellation during FFmpeg composition is not supported")
	}
	if j.ProviderJobID != "" {
		if o.Providers == nil {
			return errors.New("provider registry for active video job is unavailable; refusing to mark remote work cancelled")
		}
		p, ok := o.Providers.Get(j.Provider)
		if !ok {
			return errors.New("provider for active video job is unavailable; refusing to mark remote work cancelled")
		}
		if err = p.CancelJob(ctx, j.ProviderJobID); err != nil {
			return err
		}
	}
	if err := j.Transition(video.StateCancelled); err != nil {
		return err
	}
	j.LastError = ""
	if err := o.Store.Update(ctx, j); err != nil {
		return err
	}
	if o.Ledger != nil {
		if j.ProviderJobID == "" {
			o.Ledger.ReleaseForJob(j.JobID)
		} else {
			_ = o.Ledger.SettleEstimate(j.JobID, j.EstimatedCostUSD)
		}
	}
	return nil
}
