package orchestrator

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
)

type EpisodeComposer interface {
	ComposeEpisode(context.Context, composer.EpisodeSpec) (composer.Manifest, error)
}

func validateShotPlanRequest(r video.VideoRequest, p video.VideoProvider, caps video.Capabilities) (video.VideoRequest, video.CostEstimate, error) {
	if len(r.ShotPlan) < 2 {
		return r, video.CostEstimate{}, errors.New("shot-plan validation requires at least two shots")
	}
	selectedModel := strings.TrimSpace(r.ModelPreference)
	seen := make(map[string]struct{}, len(r.ShotPlan))
	total := video.CostEstimate{
		Provider: p.ID(), Currency: "USD", PriceKnown: true,
		Breakdown: make(map[string]float64),
	}
	for i, shot := range r.ShotPlan {
		shotID := strings.TrimSpace(shot.ShotID)
		if _, ok := seen[shotID]; ok {
			return r, video.CostEstimate{}, fmt.Errorf("shot_plan contains duplicate shot_id %q", shotID)
		}
		seen[shotID] = struct{}{}
		if shot.Provider != "" && shot.Provider != p.ID() {
			return r, video.CostEstimate{}, fmt.Errorf("shot %q selects provider %q but this episode uses %q", shotID, shot.Provider, p.ID())
		}
		if shot.Model != "" {
			if selectedModel != "" && selectedModel != shot.Model {
				return r, video.CostEstimate{}, fmt.Errorf("shot %q selects model %q but this episode uses %q", shotID, shot.Model, selectedModel)
			}
			selectedModel = shot.Model
		}
		request := buildShotRequest(r, shot, i)
		request.ProviderPreference = p.ID()
		request.ModelPreference = selectedModel
		normalized, err := validateProviderRequest(request, caps)
		if err != nil {
			return r, video.CostEstimate{}, fmt.Errorf("shot %q: %w", shotID, err)
		}
		if selectedModel == "" {
			selectedModel = normalized.ModelPreference
		}
		if normalized.ModelPreference != selectedModel {
			return r, video.CostEstimate{}, fmt.Errorf("shot %q selected a different model; one episode currently requires one provider/model", shotID)
		}
		if err := p.ValidateRequest(normalized); err != nil {
			return r, video.CostEstimate{}, fmt.Errorf("provider rejected shot %q: %w", shotID, err)
		}
		estimate, err := p.EstimateCost(normalized)
		if err != nil {
			return r, video.CostEstimate{}, fmt.Errorf("cannot safely estimate cost for shot %q: %w", shotID, err)
		}
		if !validEstimate(estimate) {
			return r, video.CostEstimate{}, fmt.Errorf("shot %q: %w", shotID, cost.ErrInvalidEstimate)
		}
		if estimate.Provider != "" && estimate.Provider != p.ID() {
			return r, video.CostEstimate{}, fmt.Errorf("shot %q cost estimate belongs to another provider", shotID)
		}
		if estimate.Model != "" && estimate.Model != normalized.ModelPreference {
			return r, video.CostEstimate{}, fmt.Errorf("shot %q cost estimate belongs to another model", shotID)
		}
		total.EstimatedUSD += estimate.EstimatedUSD
		total.UpperBoundUSD += estimate.UpperBoundUSD
		total.BillableSeconds += estimate.BillableSeconds
		for key, value := range estimate.Breakdown {
			total.Breakdown[key] += value
		}
	}
	if len(total.Breakdown) == 0 {
		total.Breakdown = nil
	}
	r.ProviderPreference = p.ID()
	r.ModelPreference = selectedModel
	total.Model = selectedModel
	return r, total, nil
}

func buildShotRequest(parent video.VideoRequest, shot video.Shot, index int) video.VideoRequest {
	r := parent
	r.Prompt = shotPrompt(shot)
	if strings.TrimSpace(shot.NegativePrompt) != "" {
		r.NegativePrompt = shot.NegativePrompt
	}
	r.DurationSeconds = shot.DurationSeconds
	r.MaxCostUSD = 0 // the episode-level budget is enforced against the aggregate estimate
	r.ShotPlan = nil
	r.ProviderPreference = parent.ProviderPreference
	if shot.Provider != "" {
		r.ProviderPreference = shot.Provider
	}
	r.ModelPreference = parent.ModelPreference
	if shot.Model != "" {
		r.ModelPreference = shot.Model
	}
	r.ReferenceAssets = append([]video.Asset(nil), parent.ReferenceAssets...)
	r.ReferenceAssets = append(r.ReferenceAssets, shot.RequiredReferences...)
	r.AudioRequested = parent.AudioRequested || len(shot.SoundRequirements) > 0
	if strings.TrimSpace(shot.NarrationSegment) != "" {
		r.NarrationText = shot.NarrationSegment
	}
	if parent.IdempotencyKey != "" {
		r.IdempotencyKey = fmt.Sprintf("%s-shot-%03d", parent.IdempotencyKey, index+1)
	}
	return r
}

func shotPrompt(shot video.Shot) string {
	parts := []string{strings.TrimSpace(shot.Prompt)}
	if value := strings.TrimSpace(shot.CameraMovement); value != "" {
		parts = append(parts, "Camera movement: "+value)
	}
	if value := strings.TrimSpace(shot.SubjectAction); value != "" {
		parts = append(parts, "Subject action: "+value)
	}
	if value := strings.TrimSpace(shot.StartState); value != "" {
		parts = append(parts, "Starting state: "+value)
	}
	if value := strings.TrimSpace(shot.EndState); value != "" {
		parts = append(parts, "Ending state: "+value)
	}
	for _, value := range shot.ContinuityConstraints {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, "Continuity constraint: "+value)
		}
	}
	return strings.Join(parts, "\n")
}

func (o *Orchestrator) runEpisodeJob(ctx context.Context, j video.VideoJob) error {
	if j.State == video.StateCompleted || j.State == video.StateFailed || j.State == video.StateCancelled || j.State == video.StateDryRun {
		return nil
	}
	if j.State == video.StateNeedsManualAction {
		return errors.New("video episode requires manual action")
	}
	if len(j.Request.ShotPlan) < 2 || len(j.ShotExecutions) != len(j.Request.ShotPlan) {
		return o.markNeedsManualAndError(ctx, j, "multi-shot job has no complete durable shot checkpoint list")
	}
	if o.Assets == nil || o.Composer == nil {
		return o.markNeedsManualAndError(ctx, j, "multi-shot job requires an asset sink and episode composer")
	}
	var p video.VideoProvider
	needsProvider := j.State == video.StateCancelRequested
	for _, shot := range j.Request.ShotPlan {
		if shot.OutputAsset == nil {
			needsProvider = true
			break
		}
	}
	if needsProvider {
		if o.Providers == nil {
			return o.markNeedsManualAndError(ctx, j, "provider registry is unavailable while episode shots remain")
		}
		var ok bool
		p, ok = o.Providers.Get(j.Provider)
		if !ok {
			return o.fail(ctx, j, "provider unavailable")
		}
	}
	var caps video.Capabilities
	capsLoaded := false
	if j.State == video.StateCancelRequested {
		if strings.TrimSpace(j.ProviderJobID) == "" {
			return o.markNeedsManualAndError(ctx, j, "episode cancellation was requested without a known active provider job ID")
		}
		if err := p.CancelJob(ctx, j.ProviderJobID); err != nil {
			return err
		}
		if err := j.Transition(video.StateCancelled); err != nil {
			return err
		}
		j.ProviderJobID = ""
		j.LastError = ""
		if err := o.Store.Update(ctx, j); err != nil {
			return err
		}
		if o.Ledger != nil {
			o.Ledger.ReleaseForJob(j.JobID)
		}
		return nil
	}
	if j.State == video.StateQueued {
		if err := j.Transition(video.StateAdmitted); err != nil {
			return err
		}
		if err := o.Store.Update(ctx, j); err != nil {
			return fmt.Errorf("persist episode admission: %w", err)
		}
	}

	for i := range j.Request.ShotPlan {
		shot := &j.Request.ShotPlan[i]
		run := &j.ShotExecutions[i]
		j.CurrentShot = i + 1
		if shot.OutputAsset != nil {
			if strings.TrimSpace(shot.OutputAsset.URI) == "" || shot.OutputAsset.SizeBytes <= 0 || len(shot.OutputAsset.SHA256) != 64 {
				return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q has an invalid persisted output asset checkpoint", run.ShotID))
			}
			run.State = video.StateCompleted
			run.Progress = 1
			addSourceAsset(&j, *shot.OutputAsset)
			continue
		}
		if run.ProviderJobID == "" && strings.TrimSpace(j.ProviderJobID) != "" {
			return o.markNeedsManualAndError(ctx, j, "parent job has an active provider ID without a matching shot checkpoint")
		}
		if run.State == video.StateCompleted && run.ProviderJobID == "" {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q is marked complete without a persisted output asset", run.ShotID))
		}
		if run.State == video.StatePlanning && run.ProviderJobID == "" {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q may have been submitted before a crash; refusing automatic resubmission", run.ShotID))
		}
		if run.ProviderJobID == "" {
			if p == nil {
				return o.markNeedsManualAndError(ctx, j, "provider is unavailable while a shot still requires generation")
			}
			if !capsLoaded {
				var err error
				caps, err = p.Capabilities(ctx)
				if err != nil {
					j.LastError = "provider capabilities unavailable; episode remains resumable"
					if saveErr := o.Store.Update(ctx, j); saveErr != nil {
						return fmt.Errorf("provider capabilities unavailable and episode state could not be saved: %w", saveErr)
					}
					o.scheduleRetry(ctx, j, o.pollInterval())
					return fmt.Errorf("read provider capabilities while submitting episode shot: %w", err)
				}
				capsLoaded = true
			}
			request := buildShotRequest(j.Request, *shot, i)
			request.ProviderPreference = j.Provider
			request.ModelPreference = j.Model
			normalized, err := validateProviderRequest(request, caps)
			if err != nil {
				return o.fail(ctx, j, fmt.Sprintf("shot %q is no longer supported: %v", run.ShotID, err))
			}
			if err := p.ValidateRequest(normalized); err != nil {
				return o.fail(ctx, j, fmt.Sprintf("provider rejected shot %q: %v", run.ShotID, err))
			}
			run.State = video.StatePlanning
			run.Provider = p.ID()
			run.Model = normalized.ModelPreference
			run.LastError = ""
			if err := o.Store.Update(ctx, j); err != nil {
				return fmt.Errorf("persist shot %q submission intent: %w", run.ShotID, err)
			}
			submitted, submitErr := p.CreateJob(ctx, normalized)
			if submitErr != nil {
				return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q submission outcome is unknown; automatic resubmission is disabled", run.ShotID))
			}
			if strings.TrimSpace(submitted.ProviderJobID) == "" {
				return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("provider returned no job ID for shot %q", run.ShotID))
			}
			run.ProviderJobID = strings.TrimSpace(submitted.ProviderJobID)
			run.Provider = p.ID()
			if submitted.Model != "" {
				run.Model = submitted.Model
			}
			run.State = video.StateSubmitted
			now := time.Now().UTC()
			run.SubmittedAt = &now
			j.ProviderJobID = run.ProviderJobID
			j.Provider = p.ID()
			j.Model = run.Model
			if j.State == video.StateAdmitted {
				if err := j.Transition(video.StateSubmitted); err != nil {
					return err
				}
			}
			if err := o.Store.Update(ctx, j); err != nil {
				return fmt.Errorf("shot %q provider ID was returned but not durably saved; do not resubmit: %w", run.ShotID, err)
			}
			if !submitted.Accepted || (submitted.Provider != "" && submitted.Provider != p.ID()) || (submitted.Model != "" && submitted.Model != normalized.ModelPreference) {
				return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("provider submission for shot %q was inconsistent; known job ID retained", run.ShotID))
			}
		} else {
			j.ProviderJobID = run.ProviderJobID
			j.Provider = run.Provider
			j.Model = run.Model
		}

		status, err := o.pollEpisodeShot(ctx, p, &j, i)
		if err != nil {
			return err
		}
		if status.State == video.StateCancelled {
			return nil
		}
		if status.State != video.StateCompleted {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q ended without a completed provider status", run.ShotID))
		}
		if len(status.Outputs) == 0 {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("provider completed shot %q without output assets", run.ShotID))
		}
		run.State = video.StateUploading
		if err := o.Store.Update(ctx, j); err != nil {
			return fmt.Errorf("persist shot %q output-upload state: %w", run.ShotID, err)
		}
		for k := range status.Outputs {
			meta := make(map[string]string, len(status.Outputs[k].Metadata)+4)
			for key, value := range status.Outputs[k].Metadata {
				meta[key] = value
			}
			meta["project_id"] = j.ProjectID
			meta["episode_id"] = j.EpisodeID
			meta["job_id"] = j.JobID
			meta["shot_id"] = run.ShotID
			meta["shot_index"] = fmt.Sprint(i + 1)
			status.Outputs[k].Metadata = meta
		}
		assets, err := p.DownloadOutputs(ctx, status, o.Assets)
		if err != nil {
			run.LastError = "shot output download failed; checkpoint remains resumable"
			j.LastError = run.LastError
			if saveErr := o.Store.Update(ctx, j); saveErr != nil {
				return fmt.Errorf("shot output download failed and checkpoint could not be saved: %w", saveErr)
			}
			o.scheduleRetry(ctx, j, o.pollInterval())
			return fmt.Errorf("download shot %q output: %w", run.ShotID, err)
		}
		if len(assets) != 1 {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q must produce exactly one video asset; got %d", run.ShotID, len(assets)))
		}
		asset := assets[0]
		if strings.TrimSpace(asset.URI) == "" || strings.TrimSpace(asset.ContentType) == "" || asset.SizeBytes <= 0 || len(asset.SHA256) != 64 {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q output was not durably persisted with URI, type, size, and SHA-256", run.ShotID))
		}
		if _, err := hex.DecodeString(asset.SHA256); err != nil {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q output checksum is malformed", run.ShotID))
		}
		maxBytes := o.MaxAssetBytes
		if maxBytes <= 0 {
			maxBytes = 512 << 20
		}
		if asset.SizeBytes > maxBytes {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q output exceeds the configured asset limit", run.ShotID))
		}
		if !strings.HasPrefix(strings.ToLower(asset.ContentType), "video/") {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q output is not a video asset", run.ShotID))
		}
		info, err := os.Stat(asset.URI)
		if err != nil || !info.Mode().IsRegular() || info.Size() != asset.SizeBytes {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q asset is not a valid local file usable by the episode composer", run.ShotID))
		}
		// Re-store every clip under a unique filename before the next provider
		// job runs. Providers commonly reuse names such as output.mp4; without
		// this copy, a later shot could overwrite an earlier checkpoint.
		rawClip, err := os.ReadFile(asset.URI)
		if err != nil {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q asset could not be read for checkpointing", run.ShotID))
		}
		baseName := filepath.Base(strings.TrimSpace(asset.Filename))
		if baseName == "" || baseName == "." {
			baseName = "clip.mp4"
		}
		asset.Filename = fmt.Sprintf("shot-%03d-%s", i+1, baseName)
		baseID := strings.TrimSpace(asset.ID)
		if baseID == "" {
			baseID = j.JobID
		}
		asset.ID = fmt.Sprintf("%s-shot-%03d", baseID, i+1)
		metadata := make(map[string]string, len(asset.Metadata)+5)
		for key, value := range asset.Metadata {
			metadata[key] = value
		}
		metadata["project_id"] = j.ProjectID
		metadata["episode_id"] = j.EpisodeID
		metadata["job_id"] = j.JobID
		metadata["shot_id"] = run.ShotID
		metadata["shot_index"] = fmt.Sprint(i + 1)
		asset.Metadata = metadata
		asset.Kind = "video"
		savedClip, err := o.Assets.Put(ctx, asset, rawClip)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			run.LastError = "unique shot checkpoint persistence failed; remote job remains resumable"
			j.LastError = run.LastError
			if saveErr := o.Store.Update(ctx, j); saveErr != nil {
				return fmt.Errorf("shot checkpoint persistence failed and state could not be saved: %w", saveErr)
			}
			o.scheduleRetry(ctx, j, o.pollInterval())
			return fmt.Errorf("persist unique output for shot %q: %w", run.ShotID, err)
		}
		asset = savedClip
		if strings.TrimSpace(asset.URI) == "" || asset.SizeBytes != int64(len(rawClip)) || asset.SizeBytes <= 0 || asset.SizeBytes > maxBytes || len(asset.SHA256) != 64 {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q unique checkpoint was not persisted with valid metadata", run.ShotID))
		}
		if _, err := hex.DecodeString(asset.SHA256); err != nil {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q unique checkpoint checksum is malformed", run.ShotID))
		}
		info, err = os.Stat(asset.URI)
		if err != nil || !info.Mode().IsRegular() || info.Size() != asset.SizeBytes {
			return o.markNeedsManualAndError(ctx, j, fmt.Sprintf("shot %q unique checkpoint file is invalid", run.ShotID))
		}
		shot.OutputAsset = &asset
		run.State = video.StateCompleted
		run.Progress = 1
		run.LastError = ""
		now := time.Now().UTC()
		run.CompletedAt = &now
		addSourceAsset(&j, asset)
		j.ProviderJobID = ""
		j.Progress = math.Max(j.Progress, float64(i+1)/float64(len(j.Request.ShotPlan))*0.8)
		if i+1 == len(j.Request.ShotPlan) {
			if j.State != video.StateComposing {
				if err := j.Transition(video.StateComposing); err != nil {
					return err
				}
			}
		} else if j.State == video.StateSubmitted {
			if err := j.Transition(video.StateProcessing); err != nil {
				return err
			}
		}
		j.LastError = ""
		if err := o.Store.Update(ctx, j); err != nil {
			return fmt.Errorf("persist completed shot %q checkpoint: %w", run.ShotID, err)
		}
	}

	if j.State != video.StateComposing {
		if err := j.Transition(video.StateComposing); err != nil {
			return err
		}
	}
	j.ProviderJobID = ""
	j.Progress = math.Max(j.Progress, 0.85)
	j.LastError = ""
	if err := o.Store.Update(ctx, j); err != nil {
		return fmt.Errorf("persist episode composition stage: %w", err)
	}
	return o.composeEpisode(ctx, &j)
}

func (o *Orchestrator) pollEpisodeShot(ctx context.Context, p video.VideoProvider, j *video.VideoJob, index int) (video.ProviderJobStatus, error) {
	interval := o.pollInterval()
	max := o.MaxPolls
	if max <= 0 {
		max = 300
	}
	run := &j.ShotExecutions[index]
	for i := 0; i < max; i++ {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return video.ProviderJobStatus{}, ctx.Err()
		case <-timer.C:
		}
		status, err := p.GetJob(ctx, run.ProviderJobID)
		if err != nil {
			j.RetryCount++
			run.LastError = "provider status lookup failed; shot remains resumable"
			j.LastError = run.LastError
			if saveErr := o.Store.Update(ctx, *j); saveErr != nil {
				return video.ProviderJobStatus{}, fmt.Errorf("shot status lookup failed and checkpoint could not be saved: %w", saveErr)
			}
			continue
		}
		if status.ProviderJobID != "" && status.ProviderJobID != run.ProviderJobID {
			return video.ProviderJobStatus{}, o.markNeedsManualAndError(ctx, *j, "provider returned a different shot job ID")
		}
		if math.IsNaN(status.Progress) || math.IsInf(status.Progress, 0) || status.Progress < 0 || status.Progress > 1 {
			return video.ProviderJobStatus{}, o.markNeedsManualAndError(ctx, *j, "provider returned invalid shot progress metadata")
		}
		run.Progress = status.Progress
		run.UsageMetadata = mergeUsage(run.UsageMetadata, status.Usage)
		for key, value := range status.Usage {
			if j.UsageMetadata == nil {
				j.UsageMetadata = make(map[string]string)
			}
			j.UsageMetadata[fmt.Sprintf("shot_%03d.%s", index+1, key)] = value
		}
		switch status.State {
		case video.StateFailed, video.StateExpired:
			run.State = status.State
			return video.ProviderJobStatus{}, o.fail(ctx, *j, fmt.Sprintf("provider reported shot %q ended without a video output", run.ShotID))
		case video.StateCancelled:
			if j.State != video.StateCancelled {
				if err := j.Transition(video.StateCancelled); err != nil {
					return video.ProviderJobStatus{}, err
				}
			}
			j.ProviderJobID = ""
			j.LastError = ""
			if err := o.Store.Update(ctx, *j); err != nil {
				return video.ProviderJobStatus{}, err
			}
			if o.Ledger != nil {
				o.Ledger.ReleaseForJob(j.JobID)
			}
			return status, nil
		case video.StateCompleted:
			run.State = video.StateCompleted
			run.LastError = ""
			if err := o.Store.Update(ctx, *j); err != nil {
				return video.ProviderJobStatus{}, fmt.Errorf("persist completed shot status: %w", err)
			}
			return status, nil
		case video.StateQueued, video.StateSubmitted, video.StateProcessing, video.StatePolling:
			if status.State == video.StateQueued || status.State == video.StateSubmitted {
				run.State = video.StateSubmitted
			} else {
				run.State = status.State
			}
			if j.State == video.StateSubmitted && status.State != video.StateQueued && status.State != video.StateSubmitted {
				if err := j.Transition(video.StateProcessing); err != nil {
					return video.ProviderJobStatus{}, err
				}
			}
			total := float64(len(j.Request.ShotPlan))
			j.Progress = math.Max(j.Progress, math.Min(0.8, (float64(index)+status.Progress)/total*0.8))
			j.LastError = ""
			if err := o.Store.Update(ctx, *j); err != nil {
				return video.ProviderJobStatus{}, fmt.Errorf("persist shot progress: %w", err)
			}
		default:
			return video.ProviderJobStatus{}, o.markNeedsManualAndError(ctx, *j, "provider returned an unsupported shot state")
		}
	}
	j.RetryCount++
	j.LastError = fmt.Sprintf("shot %q polling deadline reached; remote job remains resumable", run.ShotID)
	run.LastError = j.LastError
	if err := o.Store.Update(ctx, *j); err != nil {
		return video.ProviderJobStatus{}, fmt.Errorf("shot polling deadline reached and checkpoint could not be saved: %w", err)
	}
	o.scheduleRetry(ctx, *j, interval)
	return video.ProviderJobStatus{}, errors.New(j.LastError)
}

func (o *Orchestrator) composeEpisode(ctx context.Context, j *video.VideoJob) error {
	clips := make([]string, 0, len(j.Request.ShotPlan))
	for _, shot := range j.Request.ShotPlan {
		if shot.OutputAsset == nil || strings.TrimSpace(shot.OutputAsset.URI) == "" {
			return o.markNeedsManualAndError(ctx, *j, "episode composition is missing a persisted shot asset")
		}
		clips = append(clips, shot.OutputAsset.URI)
	}
	if len(clips) < 2 {
		return o.markNeedsManualAndError(ctx, *j, "episode composition requires at least two persisted shot assets")
	}
	width, height := episodeDimensions(j.Request)
	fps := j.Request.FPS
	if fps < 1 {
		fps = 24
	}
	tempDir, err := os.MkdirTemp("", "nexaroute-episode-")
	if err != nil {
		return o.fail(ctx, *j, "could not create episode composition workspace")
	}
	defer os.RemoveAll(tempDir)
	outputPath := filepath.Join(tempDir, "episode.mp4")
	_, err = o.Composer.ComposeEpisode(ctx, composer.EpisodeSpec{
		Shots: clips, OutputPath: outputPath, Width: width, Height: height, FPS: fps,
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j.CompositionRetryCount++
		j.LastError = "episode composition failed; generated shot checkpoints are preserved"
		if saveErr := o.Store.Update(ctx, *j); saveErr != nil {
			return fmt.Errorf("episode composition failed and checkpoint could not be saved: %w", saveErr)
		}
		if j.CompositionRetryCount >= 3 {
			return o.fail(ctx, *j, "episode composition failed after repeated attempts")
		}
		o.scheduleRetry(ctx, *j, o.pollInterval())
		return fmt.Errorf("compose episode: %w", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return o.fail(ctx, *j, "episode composer reported success without an output file")
	}
	maxBytes := o.MaxAssetBytes
	if maxBytes <= 0 {
		maxBytes = 512 << 20
	}
	if info.Size() <= 0 || info.Size() > maxBytes {
		return o.fail(ctx, *j, "composed episode is empty or exceeds the configured asset limit")
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return o.fail(ctx, *j, "could not read composed episode output")
	}
	asset := video.Asset{
		ID: j.JobID + "-episode", Kind: "video", Filename: j.JobID + ".mp4",
		ContentType: "video/mp4",
		Metadata:    map[string]string{"project_id": j.ProjectID, "episode_id": j.EpisodeID, "job_id": j.JobID, "asset_role": "final_episode"},
	}
	saved, err := o.Assets.Put(ctx, asset, data)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j.LastError = "final episode persistence failed; shot checkpoints are preserved"
		if saveErr := o.Store.Update(ctx, *j); saveErr != nil {
			return fmt.Errorf("final episode persistence failed and checkpoint could not be saved: %w", saveErr)
		}
		o.scheduleRetry(ctx, *j, o.pollInterval())
		return fmt.Errorf("persist final episode: %w", err)
	}
	if strings.TrimSpace(saved.URI) == "" || saved.SizeBytes <= 0 || saved.SizeBytes > maxBytes || len(saved.SHA256) != 64 || !strings.HasPrefix(strings.ToLower(saved.ContentType), "video/") {
		return o.markNeedsManualAndError(ctx, *j, "final episode was not persisted with valid URI, content type, size, and SHA-256")
	}
	if _, err := hex.DecodeString(saved.SHA256); err != nil {
		return o.markNeedsManualAndError(ctx, *j, "final episode checksum is malformed")
	}
	j.OutputAssets = []video.Asset{saved}
	j.Progress = 1
	j.LastError = ""
	now := time.Now().UTC()
	j.CompletedAt = &now
	if err := j.Transition(video.StateCompleted); err != nil {
		return err
	}
	if err := o.Store.Update(ctx, *j); err != nil {
		return fmt.Errorf("final episode was persisted but completion state could not be saved: %w", err)
	}
	if o.Ledger != nil {
		if err := o.Ledger.SettleEstimate(j.JobID, j.EstimatedCostUSD); err != nil {
			return fmt.Errorf("episode completed but cost settlement failed: %w", err)
		}
	}
	return nil
}

func episodeDimensions(r video.VideoRequest) (int, int) {
	if r.Width > 0 && r.Height > 0 {
		return r.Width, r.Height
	}
	switch r.AspectRatio {
	case video.Aspect9x16:
		return 720, 1280
	case video.Aspect1x1:
		return 1024, 1024
	default:
		return 1280, 720
	}
}

func addSourceAsset(j *video.VideoJob, asset video.Asset) {
	for _, existing := range j.SourceAssets {
		if existing.ID == asset.ID && existing.URI == asset.URI {
			return
		}
	}
	j.SourceAssets = append(j.SourceAssets, asset)
}

func (o *Orchestrator) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return 100 * time.Millisecond
}
