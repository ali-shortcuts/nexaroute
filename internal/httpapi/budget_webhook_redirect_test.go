package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Audit #371 finding F7: the budget webhook's CheckRedirect validated
// the scheme only, so a 307 delivered the alert payload (budget_id,
// spend/limit) to a cross-origin target. The webhook must refuse
// cross-origin redirects; same-origin redirects keep the existing
// scheme discipline.
func TestBudgetAlertRefusesCrossOriginRedirect(t *testing.T) {
	var targetHits atomic.Int32
	var targetGotBody atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if body, _ := io.ReadAll(r.Body); len(body) > 0 {
			targetGotBody.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/hook", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	payload := BudgetAlertPayload{BudgetID: "vk-a", BudgetName: "n", ThresholdPercent: 80, SpendUSD: 8, LimitUSD: 10, Currency: "USD", Period: "lifetime:10", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := deliverBudgetAlert(ctx, payload, source.URL+"/hook", 5*time.Second, config.EgressConfig{})
	if err == nil {
		t.Fatal("budget webhook followed a cross-origin redirect: no error returned")
	}
	if targetHits.Load() != 0 || targetGotBody.Load() {
		t.Fatalf("alert payload reached the cross-origin redirect target (hits=%d body=%v)", targetHits.Load(), targetGotBody.Load())
	}
}

// A same-origin redirect (redirect within the same host) must still
// work: the refusal applies only across origins.
func TestBudgetAlertAllowsSameOriginRedirect(t *testing.T) {
	var gotBody []byte
	// Redirect within the source server must be followed when the
	// scheme discipline allows it.
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/hook", http.StatusTemporaryRedirect)
			return
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer inner.Close()

	payload := BudgetAlertPayload{BudgetID: "vk-b", BudgetName: "n", ThresholdPercent: 90, SpendUSD: 9, LimitUSD: 10, Currency: "USD", Period: "lifetime:10", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := deliverBudgetAlert(ctx, payload, inner.URL+"/start", 5*time.Second, config.EgressConfig{}); err != nil {
		t.Fatalf("same-origin redirect refused: %v", err)
	}
	if len(gotBody) == 0 {
		t.Fatal("same-origin redirect was not followed: payload never delivered")
	}
}
