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

type ShotExecution struct {
	Index         int               `json:"index"`
	ShotID        string            `json:"shot_id"`
	State         JobState          `json:"state"`
	Provider      string            `json:"provider,omitempty"`
	Model         string            `json:"model,omitempty"`
	ProviderJobID string            `json:"provider_job_id,omitempty"`
	Progress      float64           `json:"progress"`
	SubmittedAt   *time.Time        `json:"submitted_at,omitempty"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
	UsageMetadata map[string]string `json:"usage_metadata,omitempty"`
	// ActualCostUSD is the provider-reported actual for this shot, when the
	// provider reports one. Additive: nil means the shot reported no actual.
	ActualCostUSD *float64 `json:"actual_cost_usd,omitempty"`
}

type VideoJob struct {
	SchemaVersion  string       `json:"schema_version"`
	JobID          string       `json:"job_id"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	LeaseOwner     string       `json:"lease_owner,omitempty"`
	LeaseUntil     time.Time    `json:"lease_until,omitempty"`
	ProjectID      string       `json:"project_id"`
	EpisodeID      string       `json:"episode_id,omitempty"`
	ParentJobID    string       `json:"parent_job_id,omitempty"`
	Request        VideoRequest `json:"request"`
	Provider       string       `json:"provider,omitempty"`
	Model          string       `json:"model,omitempty"`
	ProviderJobID  string       `json:"provider_job_id,omitempty"`
	State          JobState     `json:"state"`
	Progress       float64      `json:"progress"`
	CreatedAt      time.Time    `json:"created_at"`
	SubmittedAt    *time.Time   `json:"submitted_at,omitempty"`
	CompletedAt    *time.Time   `json:"completed_at,omitempty"`
	LastError      string       `json:"last_error,omitempty"`
	// OperatorForceFailed marks a privileged force-fail transition. Unlike an
	// ordinary failed job (which may be retried), this state is immutable so a
	// late provider result cannot overwrite the operator's decision.
	OperatorForceFailed   bool `json:"operator_force_failed,omitempty"`
	RetryCount            int  `json:"retry_count"`
	CompositionRetryCount int  `json:"composition_retry_count,omitempty"`
	CurrentShot           int  `json:"current_shot,omitempty"`
	TotalShots            int  `json:"total_shots,omitempty"`
	// JobType is the brief classification stamped at creation
	// (#510 item 6): text_to_video | image_to_video | edit_extend |
	// compose, with the reason codes that produced it. Additive.
	JobType          JobType         `json:"job_type,omitempty"`
	JobTypeReasons   []string        `json:"job_type_reasons,omitempty"`
	ShotExecutions   []ShotExecution `json:"shot_executions,omitempty"`
	OutputAssets     []Asset         `json:"output_assets,omitempty"`
	SourceAssets     []Asset         `json:"source_assets,omitempty"`
	NarrationAsset   *Asset          `json:"narration_asset,omitempty"`
	SubtitleAsset    *Asset          `json:"subtitle_asset,omitempty"`
	EstimatedCostUSD float64         `json:"estimated_cost_usd,omitempty"`
	ActualCostUSD    float64         `json:"actual_cost_usd,omitempty"`
	// CostReconciled reports whether the ledger entry for this job was
	// reconciled from the admission estimate to a provider-reported actual.
	// It has no omitempty so unreconciled jobs are explicitly marked false
	// and never silently presented as actuals.
	CostReconciled bool `json:"cost_reconciled"`
	// CostDeltaUSD is actual − estimate, recorded only when CostReconciled
	// is true. Additive: absent for unreconciled jobs.
	CostDeltaUSD *float64 `json:"cost_delta_usd,omitempty"`
	// CostBasis names which figure operator surfaces should treat as
	// authoritative for this job: "actual" when reconciled, "estimate"
	// otherwise. Additive.
	CostBasis     string            `json:"cost_basis,omitempty"`
	UsageMetadata map[string]string `json:"usage_metadata,omitempty"`
	AuditEvents   []AuditEvent      `json:"audit_events,omitempty"`
}

func NewJob(id string, req VideoRequest) VideoJob {
	now := time.Now().UTC()
	cls := ClassifyBrief(req)
	j := VideoJob{
		SchemaVersion: SchemaVersion, JobID: id, IdempotencyKey: req.IdempotencyKey,
		ProjectID: req.ProjectID, EpisodeID: req.EpisodeID, Request: req,
		State: StateQueued, CreatedAt: now, TotalShots: len(req.ShotPlan),
		JobType: cls.Type, JobTypeReasons: cls.Reasons,
	}
	if len(req.ShotPlan) > 0 {
		j.ShotExecutions = make([]ShotExecution, len(req.ShotPlan))
		for i, shot := range req.ShotPlan {
			j.ShotExecutions[i] = ShotExecution{Index: i + 1, ShotID: shot.ShotID, State: StateQueued}
		}
	}
	return j
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

// IsTerminalState reports states that cannot be left through the ordinary
// state machine. Failed jobs are intentionally excluded because they may be
// retried; operator-forced failures are separately fenced on VideoJob.
func IsTerminalState(state JobState) bool {
	switch state {
	case StateCompleted, StateCancelled, StateExpired, StateDryRun:
		return true
	default:
		return false
	}
}

func (j VideoJob) IsTerminal() bool { return IsTerminalState(j.State) || j.OperatorForceFailed }

func (j *VideoJob) Transition(to JobState) error {
	if !CanTransition(j.State, to) {
		return fmt.Errorf("invalid video job transition %s -> %s", j.State, to)
	}
	j.State = to
	return nil
}
