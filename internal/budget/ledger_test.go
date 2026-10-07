package budget

import (
	"errors"
	"sync"
	"testing"
)

func TestConcurrentReservationsCannotOversubscribe(t *testing.T) {
	l, err := NewLedger([]Limit{{Key: "tenant/acme", Amount: 100}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.Reserve("r"+string(rune('a'+i)), "tenant/acme", 10)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	var ok, rejected int
	for err := range results {
		if err == nil {
			ok++
		}
		if errors.Is(err, ErrBudgetExceeded) {
			rejected++
		}
	}
	if ok != 10 || rejected != 10 {
		t.Fatalf("ok=%d rejected=%d", ok, rejected)
	}
	used, reserved, _ := l.Usage("tenant/acme")
	if used != 0 || reserved != 100 {
		t.Fatalf("used=%d reserved=%d", used, reserved)
	}
}

func TestSettleAndReleaseAndAlerts(t *testing.T) {
	l, err := NewLedger([]Limit{{Key: "project/p1", Amount: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("a", "project/p1", 80); err != nil {
		t.Fatal(err)
	}
	alerts, err := l.Settle("a", 80)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 || alerts[0].Threshold != 50 || alerts[1].Threshold != 80 {
		t.Fatalf("alerts=%+v", alerts)
	}
	if _, err := l.Settle("a", 1); !errors.Is(err, ErrReservationClosed) {
		t.Fatalf("err=%v", err)
	}
	if _, err := l.Reserve("b", "project/p1", 20); err != nil {
		t.Fatal(err)
	}
	if err := l.Release("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("c", "project/p1", 20); err != nil {
		t.Fatal(err)
	}
	alerts, err = l.Settle("c", 20)
	if err != nil || len(alerts) != 2 || alerts[0].Threshold != 95 || alerts[1].Threshold != 100 {
		t.Fatalf("final alerts=%+v err=%v", alerts, err)
	}
	used, reserved, _ := l.Usage("project/p1")
	if used != 100 || reserved != 0 {
		t.Fatalf("used=%d reserved=%d", used, reserved)
	}
}
