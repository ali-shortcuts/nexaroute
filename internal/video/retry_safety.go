package video

import "fmt"

// Retry-safety assessment (#414 item 6): for a job in a terminal
// or attention state, say — from recorded state only — whether
// submitting the work again is safe, whether a provider job ID
// exists, and whether a duplicate bill is possible. The Studio
// renders this verbatim; it never guesses client-side, and this
// function never guesses server-side either: ambiguity is
// reported as ambiguity.

type RetrySafetyVerdict string

const (
	// RetrySafe: nothing was ever accepted by a provider, so a
	// fresh submission cannot double-bill.
	RetrySafe RetrySafetyVerdict = "safe"
	// RetryDuplicateRisk: a provider accepted (or may have
	// accepted) the work; resubmitting can bill twice.
	RetryDuplicateRisk RetrySafetyVerdict = "duplicate_risk"
	// RetryManualAction: the orchestrator already stopped
	// automation on this job; a human must decide.
	RetryManualAction RetrySafetyVerdict = "manual_action"
	// RetryBlocked: the job can never be retried (operator
	// force-fail is immutable).
	RetryBlocked RetrySafetyVerdict = "blocked"
	// RetryNotApplicable: the job is in flight or completed;
	// retry is not a meaningful action.
	RetryNotApplicable RetrySafetyVerdict = "not_applicable"
)

type RetrySafety struct {
	Verdict                  RetrySafetyVerdict `json:"verdict"`
	ProviderJobIDPresent     bool               `json:"provider_job_id_present"`
	DuplicateBillingPossible bool               `json:"duplicate_billing_possible"`
	Reason                   string             `json:"reason"`
}

// RetrySafetyFor assesses one job. Shot-level submissions count:
// an episode whose third shot reached a provider carries
// duplicate risk even when the job-level provider ID is empty.
func RetrySafetyFor(j VideoJob) RetrySafety {
	submittedShots := 0
	for _, s := range j.ShotExecutions {
		if s.ProviderJobID != "" || s.SubmittedAt != nil {
			submittedShots++
		}
	}
	providerJobPresent := j.ProviderJobID != "" || submittedShots > 0
	submissionAttempted := providerJobPresent || j.SubmittedAt != nil

	switch {
	case j.OperatorForceFailed:
		return RetrySafety{
			Verdict: RetryBlocked, ProviderJobIDPresent: providerJobPresent,
			DuplicateBillingPossible: providerJobPresent,
			Reason:                   "an operator force-failed this job; its state is immutable — create a new job instead of retrying",
		}
	case j.State == StateNeedsManualAction:
		return RetrySafety{
			Verdict: RetryManualAction, ProviderJobIDPresent: providerJobPresent,
			DuplicateBillingPossible: submissionAttempted,
			Reason:                   "automation stopped on this job; a human must inspect the provider state before any resubmission",
		}
	case j.State == StateFailed || j.State == StateCancelled:
		if !submissionAttempted {
			return RetrySafety{
				Verdict: RetrySafe, ProviderJobIDPresent: false,
				Reason: "the job never reached a provider (no provider job ID, no submission recorded), so resubmitting cannot double-bill",
			}
		}
		reason := "a provider accepted this work; resubmitting may bill twice — check the provider job before resubmitting"
		if submittedShots > 0 && j.ProviderJobID == "" {
			reason = fmt.Sprintf("%d shot submission(s) reached a provider; resubmitting may bill those shots twice — check the provider jobs before resubmitting", submittedShots)
		}
		if j.ProviderJobID == "" && submittedShots == 0 {
			reason = "a submission was attempted but no provider job ID was recorded; the provider may have accepted it — verify with the provider before resubmitting"
		}
		return RetrySafety{
			Verdict: RetryDuplicateRisk, ProviderJobIDPresent: providerJobPresent,
			DuplicateBillingPossible: true, Reason: reason,
		}
	default:
		return RetrySafety{
			Verdict: RetryNotApplicable, ProviderJobIDPresent: providerJobPresent,
			Reason: "the job is not in a failed or cancelled state",
		}
	}
}
