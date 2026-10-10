package cost

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func knownEstimate(amount float64) video.CostEstimate {
	return video.CostEstimate{
		Provider:      "fake",
		Model:         "model-1",
		Currency:      "USD",
		EstimatedUSD:  amount,
		UpperBoundUSD: amount,
		PriceKnown:    true,
	}
}

func TestBookEstimateSuccessUnknownAndInvalidPrices(t *testing.T) {
	book := NewBook()
	req := video.VideoRequest{DurationSeconds: 12}

	if _, err := book.Estimate("fake", "model-1", req); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("missing price error=%v, want ErrUnknownPrice", err)
	}

	book.Set("fake", "model-1", Price{PerSecondUSD: 0.25, FixedUSD: 1, Known: true})
	got, err := book.Estimate("fake", "model-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "fake" || got.Model != "model-1" || got.Currency != "USD" || got.EstimatedUSD != 4 || got.UpperBoundUSD != 4 || !got.PriceKnown || got.BillableSeconds != 12 {
		t.Fatalf("estimate=%+v", got)
	}

	book.Set("fake", "unknown", Price{PerSecondUSD: 1, Known: false})
	if _, err := book.Estimate("fake", "unknown", req); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("unknown price error=%v", err)
	}
	for name, price := range map[string]Price{
		"negative-rate": {PerSecondUSD: -1, Known: true},
		"nan-fixed":     {FixedUSD: math.NaN(), Known: true},
		"infinite-rate": {PerSecondUSD: math.Inf(1), Known: true},
	} {
		book.Set("fake", name, price)
		if _, err := book.Estimate("fake", name, req); !errors.Is(err, ErrInvalidEstimate) {
			t.Errorf("%s error=%v, want ErrInvalidEstimate", name, err)
		}
	}

	book.Set("fake", "overflow", Price{PerSecondUSD: math.MaxFloat64, Known: true})
	if _, err := book.Estimate("fake", "overflow", video.VideoRequest{DurationSeconds: math.MaxFloat64}); !errors.Is(err, ErrInvalidEstimate) {
		t.Fatalf("overflow error=%v, want ErrInvalidEstimate", err)
	}

	var zeroBook Book
	zeroBook.Set("zero", "model", Price{FixedUSD: 2, Known: true})
	if got, err := zeroBook.Estimate("zero", "model", video.VideoRequest{}); err != nil || got.EstimatedUSD != 2 {
		t.Fatalf("zero-value book estimate=%+v err=%v", got, err)
	}
}

func TestLedgerReserveLimitsDuplicateReleaseAndEntries(t *testing.T) {
	var ledger Ledger
	e := knownEstimate(2)
	var legacy Ledger
	if err := legacy.Reserve(e, 0); err != nil {
		t.Fatalf("legacy reservation error=%v", err)
	}
	if legacy.Reserved() != 2 {
		t.Fatalf("legacy reservation reserved=%v", legacy.Reserved())
	}
	legacy.ReleaseForJob("legacy-1")

	if err := ledger.ReserveForJob("job-1", e, 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over-limit reservation error=%v", err)
	}
	if err := ledger.ReserveForJob("job-1", e, 2); err != nil {
		t.Fatal(err)
	}
	if ledger.Reserved() != 2 || len(ledger.Entries()) != 1 {
		t.Fatalf("after reserve: reserved=%v entries=%d", ledger.Reserved(), len(ledger.Entries()))
	}
	if err := ledger.ReserveForJob("job-1", e, 2); err != nil {
		t.Fatalf("idempotent duplicate reservation error=%v", err)
	}
	if len(ledger.Entries()) != 1 {
		t.Fatalf("duplicate changed entries=%d", len(ledger.Entries()))
	}
	if err := ledger.ReserveForJob("", e, 2); err == nil {
		t.Fatal("empty job ID unexpectedly accepted")
	}
	if err := ledger.ReserveForJob("bad", video.CostEstimate{PriceKnown: false}, 2); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("unknown estimate error=%v", err)
	}
	if err := ledger.ReserveForJob("invalid", video.CostEstimate{PriceKnown: true, EstimatedUSD: -1, UpperBoundUSD: 0}, 2); !errors.Is(err, ErrInvalidEstimate) {
		t.Fatalf("invalid estimate error=%v", err)
	}
	if err := ledger.ReserveForJob("negative-max", e, -1); !errors.Is(err, ErrInvalidEstimate) {
		t.Fatalf("negative max error=%v", err)
	}

	ledger.ReleaseForJob("does-not-exist")
	ledger.ReleaseForJob("job-1")
	if ledger.Reserved() != 0 {
		t.Fatalf("released reservation=%v, want 0", ledger.Reserved())
	}
	ledger.ReleaseForJob("job-1")
	if ledger.Reserved() != 0 {
		t.Fatalf("repeated release reservation=%v", ledger.Reserved())
	}
	if err := ledger.ReserveForJob("job-1", e, 2); err == nil {
		t.Fatal("re-reserving finalized job unexpectedly succeeded")
	}

	entries := ledger.Entries()
	entries[0].EstimatedUSD = 999
	if ledger.Entries()[0].EstimatedUSD == 999 {
		t.Fatal("Entries did not return a defensive copy")
	}
}

func TestLedgerSettlementPathsAndValidation(t *testing.T) {
	var ledger Ledger
	reserved := knownEstimate(3)
	if err := ledger.ReserveForJob("actual", reserved, 0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SettleActual("actual", 2.5, 3); err != nil {
		t.Fatal(err)
	}
	if ledger.Reserved() != 0 || ledger.Spent() != 2.5 {
		t.Fatalf("actual settlement: reserved=%v spent=%v", ledger.Reserved(), ledger.Spent())
	}
	if err := ledger.SettleActual("actual", 2.5, 3); err != nil {
		t.Fatalf("idempotent actual settlement error=%v", err)
	}
	if err := ledger.SettleActual("actual", 2.6, 3); err == nil {
		t.Fatal("conflicting actual settlement unexpectedly succeeded")
	}

	if err := ledger.SettleEstimate("estimated", 1.25); err != nil {
		t.Fatal(err)
	}
	if ledger.EstimatedSpent() != 1.25 {
		t.Fatalf("estimated spent=%v", ledger.EstimatedSpent())
	}
	if err := ledger.SettleEstimate("estimated", 99); err != nil {
		t.Fatalf("repeated estimate settlement error=%v", err)
	}
	if ledger.EstimatedSpent() != 1.25 {
		t.Fatalf("repeated estimate settlement changed total=%v", ledger.EstimatedSpent())
	}
	if err := ledger.ReserveForJob("estimate-to-actual", reserved, 0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SettleEstimate("estimate-to-actual", 1.5); err != nil {
		t.Fatal(err)
	}
	if ledger.Reserved() != 0 || ledger.EstimatedSpent() != 2.75 {
		t.Fatalf("reserved estimate settlement: reserved=%v estimated=%v", ledger.Reserved(), ledger.EstimatedSpent())
	}
	if err := ledger.SettleActual("estimate-to-actual", 2, 1.5); err != nil {
		t.Fatal(err)
	}
	if ledger.EstimatedSpent() != 1.25 || ledger.Spent() != 4.5 {
		t.Fatalf("estimate-to-actual settlement: estimated=%v spent=%v", ledger.EstimatedSpent(), ledger.Spent())
	}

	if err := ledger.SettleActual("unreserved", 4, 0.5); err != nil {
		t.Fatal(err)
	}
	if ledger.Spent() != 8.5 {
		t.Fatalf("fallback actual spent=%v", ledger.Spent())
	}
	if err := ledger.SettleEstimate("estimate-to-actual", 2); err != nil {
		t.Fatal(err)
	}
	if ledger.EstimatedSpent() != 1.25 {
		t.Fatalf("actual settlement should not add estimate total=%v", ledger.EstimatedSpent())
	}

	invalidActuals := []struct {
		job, label string
		actual     float64
	}{
		{"", "empty job", 1},
		{"bad", "negative actual", -1},
		{"bad", "nan actual", math.NaN()},
		{"bad", "infinite actual", math.Inf(1)},
	}
	for _, tc := range invalidActuals {
		if err := ledger.SettleActual(tc.job, tc.actual, 1); !errors.Is(err, ErrInvalidEstimate) {
			t.Errorf("%s error=%v", tc.label, err)
		}
	}
	for _, amount := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err := ledger.SettleEstimate("bad-estimate", amount); !errors.Is(err, ErrInvalidEstimate) {
			t.Errorf("estimate %v error=%v", amount, err)
		}
	}
	if err := ledger.SettleActual("bad-fallback", 1, -1); !errors.Is(err, ErrInvalidEstimate) {
		t.Fatalf("negative fallback error=%v", err)
	}
}

func TestLedgerConcurrentReservationsAndSettlement(t *testing.T) {
	const jobs = 64
	ledger := &Ledger{}
	estimate := knownEstimate(0.5)
	var reserveWG sync.WaitGroup
	reserveWG.Add(jobs)
	for i := 0; i < jobs; i++ {
		go func(i int) {
			defer reserveWG.Done()
			if err := ledger.ReserveForJob(jobID(i), estimate, 0.5); err != nil {
				t.Errorf("reserve %d: %v", i, err)
			}
		}(i)
	}
	reserveWG.Wait()
	if ledger.Reserved() != jobs*0.5 || len(ledger.Entries()) != jobs {
		t.Fatalf("concurrent reserve: reserved=%v entries=%d", ledger.Reserved(), len(ledger.Entries()))
	}

	var settleWG sync.WaitGroup
	settleWG.Add(jobs)
	for i := 0; i < jobs; i++ {
		go func(i int) {
			defer settleWG.Done()
			if err := ledger.SettleActual(jobID(i), 0.25, 0.5); err != nil {
				t.Errorf("settle %d: %v", i, err)
			}
		}(i)
	}
	settleWG.Wait()
	if ledger.Reserved() != 0 || ledger.Spent() != jobs*0.25 {
		t.Fatalf("concurrent settlement: reserved=%v spent=%v", ledger.Reserved(), ledger.Spent())
	}
}

func jobID(i int) string {
	return "job-" + string(rune('a'+i))
}
