package video

// WorkflowStep is one stage of the Video Studio production
// pipeline (issue #414 item 1), derived purely from the job's
// recorded state — never from client-side guesses. Status is one
// of: done, active, pending, skipped, failed, attention,
// cancelled.
type WorkflowStep struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// WorkflowSteps projects a job onto the nine-step workflow the
// Studio renders: request validation → provider selection →
// queue/admission → per-shot submit → generation → output
// validation → composition → narration/subtitles → final assets.
//
// The projection is deliberately conservative: a step is "done"
// only when the job's recorded state proves it happened (a
// provider job ID, shot executions, assets, terminal state).
// Steps that do not apply to the brief (composition for a
// single-shot job, narration when none was requested) are
// "skipped", never silently shown as done.
func WorkflowSteps(j VideoJob) []WorkflowStep {
	steps := []WorkflowStep{
		{Key: "validate", Label: "Request validation"},
		{Key: "select", Label: "Provider selection"},
		{Key: "admit", Label: "Queue / admission"},
		{Key: "submit", Label: "Shot submission"},
		{Key: "generate", Label: "Generation"},
		{Key: "validate_output", Label: "Output validation"},
		{Key: "compose", Label: "Composition"},
		{Key: "narrate", Label: "Narration / subtitles"},
		{Key: "deliver", Label: "Final assets"},
	}
	set := func(key, status string) {
		for i := range steps {
			if steps[i].Key == key {
				steps[i].Status = status
			}
		}
	}
	detail := func(key, d string) {
		for i := range steps {
			if steps[i].Key == key {
				steps[i].Detail = d
			}
		}
	}
	for i := range steps {
		steps[i].Status = "pending"
	}

	// A persisted job exists only because creation validated the
	// request, so validation is always done.
	set("validate", "done")

	if j.State == StateDryRun {
		for _, k := range []string{"select", "admit", "submit", "generate", "validate_output", "compose", "narrate", "deliver"} {
			set(k, "skipped")
		}
		return steps
	}

	// Applicability.
	composeApplies := j.TotalShots > 1 || j.JobType == JobCompose
	narrateApplies := j.Request.NarrationText != "" || j.Request.SubtitleLanguage != "" ||
		j.NarrationAsset != nil || j.SubtitleAsset != nil
	if !composeApplies {
		set("compose", "skipped")
	}
	if !narrateApplies {
		set("narrate", "skipped")
	}

	submitted := j.SubmittedAt != nil || j.ProviderJobID != ""
	shotsDone := len(j.ShotExecutions) > 0
	for _, sh := range j.ShotExecutions {
		if sh.State != StateCompleted {
			shotsDone = false
		}
	}
	pastGeneration := j.State == StateComposing || j.State == StateUploading || j.State == StateCompleted

	// Provider selection: the provider is stamped at admission.
	if j.Provider != "" {
		set("select", "done")
	} else if j.State != StateQueued {
		set("select", "active")
	}
	// Admission.
	switch j.State {
	case StateAdmitted, StatePlanning, StateWaitingForBudget:
		set("admit", "active")
		if j.State == StateWaitingForBudget {
			detail("admit", "waiting for budget")
		}
	default:
		if submitted || pastGeneration || j.State == StateSubmitted || j.State == StateProcessing || j.State == StatePolling {
			set("admit", "done")
		}
	}
	// Submission.
	if submitted {
		set("submit", "done")
	} else if j.State == StateSubmitted {
		set("submit", "active")
	}
	// Generation.
	if shotsDone || pastGeneration {
		set("generate", "done")
	} else if j.State == StateProcessing || j.State == StatePolling {
		set("generate", "active")
	}
	if j.TotalShots > 1 && (j.State == StateProcessing || j.State == StatePolling) {
		detail("generate", "shot progress recorded per shot")
	}
	// Output validation happens before composition/delivery.
	if pastGeneration {
		set("validate_output", "done")
	} else if shotsDone {
		set("validate_output", "active")
	}
	// Composition.
	if composeApplies {
		if j.State == StateCompleted || j.State == StateUploading {
			set("compose", "done")
		} else if j.State == StateComposing {
			set("compose", "active")
		}
	}
	// Narration / subtitles.
	if narrateApplies {
		if j.NarrationAsset != nil || j.SubtitleAsset != nil || j.State == StateCompleted {
			set("narrate", "done")
		} else if j.State == StateComposing || j.State == StateUploading {
			set("narrate", "active")
		}
	}
	// Delivery of final assets.
	if j.State == StateCompleted {
		if len(j.OutputAssets) > 0 {
			set("deliver", "done")
		} else {
			// A completed job must carry a validated asset
			// (#414 acceptance); its absence is an inconsistency
			// to surface, not to hide.
			set("deliver", "attention")
			detail("deliver", "completed without a recorded output asset")
		}
	} else if j.State == StateUploading {
		set("deliver", "active")
	}

	// Terminal overlays: the first step that never completed
	// carries the outcome.
	overlay := ""
	switch j.State {
	case StateFailed, StateExpired:
		overlay = "failed"
	case StateNeedsManualAction:
		overlay = "attention"
	case StateCancelled:
		overlay = "cancelled"
	}
	if overlay != "" {
		for i := range steps {
			if steps[i].Status == "active" || steps[i].Status == "pending" {
				steps[i].Status = overlay
				break
			}
		}
	}
	return steps
}
