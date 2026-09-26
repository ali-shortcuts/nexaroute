package eval

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// TestConcurrentRunsAndStoreIsRaceFree runs the always-on concurrency check for
// the evaluation plane: parallel suites over one runner plus a shared store.
func TestConcurrentRunsAndStoreIsRaceFree(t *testing.T) {
	r := NewRunner()
	store := NewStore(64)
	suites := []string{"coding", "reasoning", "structured_output", "protocol_compat"}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				suite, ok := LookupSuite(suites[(g+i)%len(suites)])
				if !ok {
					t.Errorf("suite missing")
					return
				}
				outcomes := make([]Outcome, 0, len(suite.Cases))
				for _, c := range suite.Cases {
					outcomes = append(outcomes, Outcome{
						CaseID: c.ID, Status: OutcomeOK, Output: "42",
						UnitTests: &UnitTestResult{Compiled: true, Passed: 1},
						ToolCall:  &ToolCall{Name: "search_files"},
						Stream:    &StreamResult{Terminated: true},
					})
				}
				res, err := r.Run(context.Background(), Request{
					DeploymentID: "p1/m1", SuiteID: suite.ID, Outcomes: outcomes,
				})
				if err != nil {
					t.Errorf("run: %v", err)
					return
				}
				if err := store.Save(res); err != nil {
					t.Errorf("save: %v", err)
					return
				}
				_ = store.Recent(5)
				_ = store.Counts()
				_, _, _ = res.Scorecard()
			}
		}(g)
	}
	wg.Wait()
	if store.Len() == 0 {
		t.Fatal("no runs were stored")
	}
	if store.Len() > 64 {
		t.Fatalf("store exceeded its bound: %d", store.Len())
	}
}

// TestStressEvaluationThroughput is the bounded stress gate member (run by
// scripts/stress.sh with NEXAROUTE_STRESS=1).
func TestStressEvaluationThroughput(t *testing.T) {
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
	r := NewRunner()
	store := NewStore(MaxStoredRuns)
	suite, _ := LookupSuite("coding")
	outcomes := make([]Outcome, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		outcomes = append(outcomes, Outcome{
			CaseID: c.ID, Status: OutcomeOK, LatencyMS: 80,
			UnitTests: &UnitTestResult{Compiled: true, Passed: 2},
		})
	}
	const workers = 32
	const iterations = 250
	deadline := time.Now().Add(60 * time.Second)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if time.Now().After(deadline) {
					t.Errorf("stress pass exceeded its time budget")
					return
				}
				res, err := r.Run(context.Background(), Request{
					DeploymentID: "p1/m" + string(rune('a'+w%4)), SuiteID: suite.ID, Outcomes: outcomes,
				})
				if err != nil {
					t.Errorf("run: %v", err)
					return
				}
				if !res.Scoreable || res.Score != 1 {
					t.Errorf("unexpected run result: scoreable=%v score=%v", res.Scoreable, res.Score)
					return
				}
				if err := store.Save(res); err != nil {
					t.Errorf("save: %v", err)
					return
				}
				sc, _, err := res.Scorecard()
				if err != nil {
					t.Errorf("scorecard: %v", err)
					return
				}
				if _, ok := sc.Quality(suite.Dimension); !ok {
					t.Errorf("suite dimension missing from scorecard")
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if store.Len() > MaxStoredRuns {
		t.Fatalf("store exceeded its bound: %d", store.Len())
	}
}
