package video

import (
	"fmt"
	"time"
)

type JobState string

const (
	StateQueued            JobState = "queued"
	StateAdmitted          JobState = "admitted"
	StatePlanning          JobState = "planning"
	StateWaitingForBudget  JobState = "waiting_for_budget"
	StateSubmitted         JobState = "submitted"
	StateProcessing        JobState = "processing"
	StatePolling           JobState = "polling"
	StateComposing         JobState = "composing"
	StateUploading         JobState = "uploading"
	StateCompleted         JobState = "completed"
	StateFailed            JobState = "failed"
	StateCancelRequested   JobState = "cancel_requested"
	StateCancelled         JobState = "cancelled"
	StateExpired           JobState = "expired"
	StateNeedsManualAction JobState = "needs_manual_action"
	StateDryRun            JobState = "dry_run"
)

type AuditEvent struct {
	At       time.Time         `json:"at"`
	Type     string            `json:"type"`
	State    JobState          `json:"state,omitempty"`
	Message  string            `json:"message,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type VideoJob struct {
	SchemaVersion    string            `json:"schema_version"`
	JobID            string            `json:"job_id"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	ProjectID        string            `json:"project_id"`
	EpisodeID        string            `json:"episode_id,omitempty"`
	ParentJobID      string            `json:"parent_job_id,omitempty"`
	Request          VideoRequest      `json:"request"`
	Provider         string            `json:"provider,omitempty"`
	Model            string            `json:"model,omitempty"`
	ProviderJobID    string            `json:"provider_job_id,omitempty"`
	State            JobState          `json:"state"`
	Progress         float64           `json:"progress"`
	CreatedAt        time.Time         `json:"created_at"`
	SubmittedAt      *time.Time        `json:"submitted_at,omitempty"`
	CompletedAt      *time.Time        `json:"completed_at,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
	RetryCount       int               `json:"retry_count"`
	CurrentShot      int               `json:"current_shot,omitempty"`
	TotalShots       int               `json:"total_shots,omitempty"`
	OutputAssets     []Asset           `json:"output_assets,omitempty"`
	SourceAssets     []Asset           `json:"source_assets,omitempty"`
	EstimatedCostUSD float64           `json:"estimated_cost_usd,omitempty"`
	ActualCostUSD    float64           `json:"actual_cost_usd,omitempty"`
	UsageMetadata    map[string]string `json:"usage_metadata,omitempty"`
	AuditEvents      []AuditEvent      `json:"audit_events,omitempty"`
}

func NewJob(id string, req VideoRequest) VideoJob {
	now := time.Now().UTC()
	return VideoJob{SchemaVersion: SchemaVersion, JobID: id, IdempotencyKey: req.IdempotencyKey, ProjectID: req.ProjectID, EpisodeID: req.EpisodeID, Request: req, State: StateQueued, CreatedAt: now, TotalShots: len(req.ShotPlan)}
}

func CanTransition(from, to JobState) bool {
	if from == to {
		return true
	}
	switch from {
	case StateQueued:
		return to == StateAdmitted || to == StateWaitingForBudget || to == StateFailed || to == StateCancelled || to == StateNeedsManualAction
	case StateAdmitted:
		return to == StatePlanning || to == StateSubmitted || to == StateFailed || to == StateCancelRequested || to == StateNeedsManualAction
	case StatePlanning:
		return to == StateSubmitted || to == StateFailed || to == StateNeedsManualAction
	case StateWaitingForBudget:
		return to == StateQueued || to == StateFailed || to == StateNeedsManualAction
	case StateSubmitted:
		return to == StateProcessing || to == StatePolling || to == StateCompleted || to == StateComposing || to == StateUploading || to == StateCancelRequested || to == StateCancelled || to == StateFailed || to == StateNeedsManualAction
	case StateProcessing, StatePolling:
		return to == StateProcessing || to == StatePolling || to == StateComposing || to == StateUploading || to == StateCompleted || to == StateCancelRequested || to == StateCancelled || to == StateFailed || to == StateExpired || to == StateNeedsManualAction
	case StateComposing:
		return to == StateUploading || to == StateCompleted || to == StateFailed || to == StateNeedsManualAction
	case StateUploading:
		return to == StateCompleted || to == StateCancelled || to == StateFailed || to == StateNeedsManualAction
	case StateCancelRequested:
		return to == StateCancelled || to == StateFailed || to == StateNeedsManualAction
	case StateNeedsManualAction:
		return to == StateCancelled
	case StateFailed:
		return to == StateQueued || to == StateCancelled
	default:
		return false
	}
}
func (j *VideoJob) Transition(to JobState) error {
	if !CanTransition(j.State, to) {
		return fmt.Errorf("invalid video job transition %s -> %s", j.State, to)
	}
	j.State = to
	return nil
}
