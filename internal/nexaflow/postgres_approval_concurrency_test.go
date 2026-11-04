package nexaflow

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

// Concurrent first decisions must behave like subsequent idempotent replays,
// including when both writers initially observe the approval row is missing.
func TestPostgresConcurrentFirstApproval(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	s, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for round := 0; round < 12; round++ {
		run := Continue95NexaflowPGID("approval-race")
		appr := Approval{RunID: run, NodeID: "gate", Requester: "alice", Approver: "bob", Decision: "approved"}
		start := make(chan struct{})
		errs := make(chan error, 24)
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := s.RecordApproval(ctx, appr)
				if err == nil && (got.Decision != "approved" || got.DecidedAt.IsZero()) {
					errs <- errors.New("invalid approval replay")
					return
				}
				if err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: concurrent identical approval: %v", round, err)
		}
		stored, err := s.GetApproval(ctx, run, "gate")
		if err != nil || stored.Decision != "approved" {
			t.Fatalf("stored approval: %+v %v", stored, err)
		}
		if _, err := s.RecordApproval(ctx, Approval{RunID: run, NodeID: "gate", Requester: "alice", Approver: "bob", Decision: "rejected"}); !errors.Is(err, ErrApprovalConflict) {
			t.Fatalf("conflict: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+wfApprovalsTable+` WHERE run_id=$1`, run); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresConcurrentConflictingFirstApprovals(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	s, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for round := 0; round < 20; round++ {
		run := Continue95NexaflowPGID("approval-conflict")
		type outcome struct {
			approval Approval
			err      error
		}
		results := make(chan outcome, 2)
		start := make(chan struct{})
		for _, decision := range []string{"approved", "rejected"} {
			go func(decision string) {
				<-start
				a, e := s.RecordApproval(ctx, Approval{RunID: run, NodeID: "gate", Requester: "alice", Approver: "bob", Decision: decision})
				results <- outcome{a, e}
			}(decision)
		}
		close(start)
		first, second := <-results, <-results
		var winner Approval
		if first.err == nil && errors.Is(second.err, ErrApprovalConflict) {
			winner = first.approval
		} else if second.err == nil && errors.Is(first.err, ErrApprovalConflict) {
			winner = second.approval
		} else {
			t.Fatalf("round %d: results %+v %+v", round, first, second)
		}
		stored, err := s.GetApproval(ctx, run, "gate")
		if err != nil || stored.Decision != winner.Decision {
			t.Fatalf("winner not persisted: %+v %+v %v", winner, stored, err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+wfApprovalsTable+` WHERE run_id=$1`, run); err != nil {
			t.Fatal(err)
		}
	}
}
