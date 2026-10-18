package orchestrator

import (
	"context"
	"time"
)

// PruneTerminalJobs deletes terminal jobs whose completion is
// older than the retention window (#420 item 5: retention). A
// job's terminal time is its CompletedAt when recorded, else
// its creation time (a force-failed job without a completion
// stamp still ages from creation). Non-terminal jobs are NEVER
// pruned, however old — an active job is not history. A
// non-positive retention disables pruning (keep forever, the
// historical default). Returns the number of jobs deleted.
func (o *Orchestrator) PruneTerminalJobs(ctx context.Context, now time.Time, retention time.Duration) (int, error) {
	if retention <= 0 || o.Store == nil {
		return 0, nil
	}
	jobs, err := o.Store.List(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-retention)
	pruned := 0
	for _, j := range jobs {
		if !j.IsTerminal() {
			continue
		}
		terminalAt := j.CreatedAt
		if j.CompletedAt != nil {
			terminalAt = *j.CompletedAt
		}
		if terminalAt.After(cutoff) {
			continue
		}
		if err := o.Store.Delete(ctx, j.JobID); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}
