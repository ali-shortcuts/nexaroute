package probe

import (
	"context"
	"net/http"
	"testing"
)

func TestRetiredDeploymentIsNeverProbed(t *testing.T) {
	e, hm, _, calls, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}, 5, 1800)
	defer cancel()

	hm.Retire("p/m", "EOL", "model_retired")
	result := e.RunOnce(context.Background())
	if result.SkippedRetired != 1 {
		t.Fatalf("SkippedRetired=%d want 1; result=%+v", result.SkippedRetired, result)
	}
	if calls.Load() != 0 {
		t.Fatalf("retired deployment was probed %d times", calls.Load())
	}

	// Even an explicit recovery request must be ignored for retired models.
	e.Start(ctx)
	e.Recover("p/m")
	if calls.Load() != 0 {
		t.Fatalf("retired deployment entered recovery: calls=%d", calls.Load())
	}
}
