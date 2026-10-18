package video

import (
	"strings"
	"testing"
	"time"
)

// #414 item 6: the retry-safety assessment derives only from
// recorded job state and names the duplicate-billing risk
// honestly in every branch.
func TestRetrySafetyFor(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		job     VideoJob
		verdict RetrySafetyVerdict
		dup     bool
		idSeen  bool
	}{
		{"failed before submission", VideoJob{State: StateFailed}, RetrySafe, false, false},
		{"failed after provider accept", VideoJob{State: StateFailed, ProviderJobID: "pj-1", SubmittedAt: &now}, RetryDuplicateRisk, true, true},
		{"failed ambiguous submission", VideoJob{State: StateFailed, SubmittedAt: &now}, RetryDuplicateRisk, true, false},
		{"cancelled before submission", VideoJob{State: StateCancelled}, RetrySafe, false, false},
		{"cancelled after provider accept", VideoJob{State: StateCancelled, ProviderJobID: "pj-2"}, RetryDuplicateRisk, true, true},
		{"manual action", VideoJob{State: StateNeedsManualAction, ProviderJobID: "pj-3"}, RetryManualAction, true, true},
		{"manual action no submission", VideoJob{State: StateNeedsManualAction}, RetryManualAction, false, false},
		{"force failed", VideoJob{State: StateFailed, OperatorForceFailed: true, ProviderJobID: "pj-4"}, RetryBlocked, true, true},
		{"completed", VideoJob{State: StateCompleted, ProviderJobID: "pj-5"}, RetryNotApplicable, false, true},
		{"in flight", VideoJob{State: StateProcessing, ProviderJobID: "pj-6"}, RetryNotApplicable, false, true},
		{"episode with submitted shot", VideoJob{State: StateFailed, ShotExecutions: []ShotExecution{{Index: 1, ProviderJobID: "shot-pj"}}}, RetryDuplicateRisk, true, true},
	}
	for _, tc := range cases {
		got := RetrySafetyFor(tc.job)
		if got.Verdict != tc.verdict {
			t.Errorf("%s: verdict = %q, want %q", tc.name, got.Verdict, tc.verdict)
		}
		if got.DuplicateBillingPossible != tc.dup {
			t.Errorf("%s: duplicate = %v, want %v", tc.name, got.DuplicateBillingPossible, tc.dup)
		}
		if got.ProviderJobIDPresent != tc.idSeen {
			t.Errorf("%s: providerJobIDPresent = %v, want %v", tc.name, got.ProviderJobIDPresent, tc.idSeen)
		}
		if strings.TrimSpace(got.Reason) == "" {
			t.Errorf("%s: empty reason", tc.name)
		}
	}
	// The ambiguous case must say the ID is missing, not imply one.
	got := RetrySafetyFor(VideoJob{State: StateFailed, ShotExecutions: []ShotExecution{{Index: 2, SubmittedAt: &now}}})
	if got.Verdict != RetryDuplicateRisk || !strings.Contains(got.Reason, "shot") {
		t.Fatalf("shot-ambiguous case = %+v", got)
	}
}
