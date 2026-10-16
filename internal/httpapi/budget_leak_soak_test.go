package httpapi

// Wave C-4 (issue #405), part 2: soak with statistical leak
// detection.
//
// Rather than a single end-state comparison (which slow leaks can
// pass), this test runs sustained mixed in-process traffic and fits
// a linear regression to per-round samples of goroutine count,
// budget-ledger shape, and cache occupancy. A healthy gateway shows
// slopes statistically indistinguishable from zero; any systematic
// per-round growth fails the gate.
//
// Behind the existing NEXAROUTE_SOAK gate (see scripts/soak.sh).

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// leakSlope fits y = a + b*x by least squares and returns the slope
// b (units of y per round).
func leakSlope(xs, ys []float64) float64 {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

func TestSoakBudgetLeakSlope(t *testing.T) {
	requireHTTPSoak(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":100}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	}))
	t.Cleanup(up.Close)
	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1.0,0.0]}]}`)
	}))
	t.Cleanup(emb.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk-soak", KeyHash: keyDigest("soak-key"), BudgetUSD: 500},
	}
	cfg.Cache.Enabled = true
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "emb/e1"
	cfg.Cache.Semantic.Threshold = 0.5
	cfg.Providers = []config.ProviderConfig{
		{ID: "llm", Name: "LLM", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "chat-model", Enabled: true, Weight: 1,
				InputCostPerMTok: 10, OutputCostPerMTok: 30,
				Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "emb", Name: "Emb", Type: "openai_compatible", BaseURL: emb.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "e1", Model: "emb-model", Enabled: true, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	do := func(path, body string) {
		req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
		req.Header.Set("x-api-key", "soak-key")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("soak request %s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	// Fixed prompt pool: caches reach steady state during warmup, so
	// any post-warmup growth is a leak, not fill.
	pool := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}

	const rounds = 30
	const warmup = 8
	var (
		goroutines, rows, reservs, reserved []float64
		exactEntries, semEntries            []float64
		xs                                  []float64
	)
	for r := 0; r < rounds; r++ {
		for i, p := range pool {
			body := fmt.Sprintf(`{"model":"m","max_tokens":50,"messages":[{"role":"user","content":"%s-%d"}]}`, p, r%2)
			do("/v1/chat/completions", body) // miss, settles
			_ = i
		}
		for _, p := range pool[:3] {
			body := fmt.Sprintf(`{"model":"m","max_tokens":50,"messages":[{"role":"user","content":"%s-0"}]}`, p)
			do("/v1/chat/completions", body) // exact hit, releases
		}
		do("/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		do("/v1/chat/completions", `{"model":"m","stream":true,"max_tokens":50,"messages":[{"role":"user","content":"s"}]}`)

		// Let async cleanup (if any) quiesce before sampling.
		time.Sleep(50 * time.Millisecond)
		br, bo, bv := s.budgets.Stats()
		goroutines = append(goroutines, float64(runtime.NumGoroutine()))
		rows = append(rows, float64(br))
		reservs = append(reservs, float64(bo))
		reserved = append(reserved, bv)
		exactEntries = append(exactEntries, float64(s.respCache.Stats().Entries))
		if s.semCache != nil {
			semEntries = append(semEntries, float64(s.semCache.Stats().Entries))
		} else {
			semEntries = append(semEntries, 0)
		}
		xs = append(xs, float64(r))
	}

	// Regression over post-warmup rounds only.
	xs, goroutines = xs[warmup:], goroutines[warmup:]
	rows = rows[warmup:]
	reservs = reservs[warmup:]
	reserved = reserved[warmup:]
	exactEntries = exactEntries[warmup:]
	semEntries = semEntries[warmup:]

	slopes := map[string]float64{
		"goroutines":          leakSlope(xs, goroutines),
		"budget_rows":         leakSlope(xs, rows),
		"budget_open":         leakSlope(xs, reservs),
		"budget_reserved":     leakSlope(xs, reserved),
		"exact_cache_entries": leakSlope(xs, exactEntries),
		"sem_cache_entries":   leakSlope(xs, semEntries),
	}
	// Stated thresholds (units per round). Budget series must be
	// exactly flat: every synchronous path closes its reservation.
	limits := map[string]float64{
		"goroutines":          0.5,
		"budget_rows":         0.05,
		"budget_open":         0.05,
		"budget_reserved":     1e-9,
		"exact_cache_entries": 0.5,
		"sem_cache_entries":   0.5,
	}
	t.Logf("leak-slope report (per-round slope over %d post-warmup rounds):", len(xs))
	failed := false
	for _, name := range []string{"goroutines", "budget_rows", "budget_open", "budget_reserved", "exact_cache_entries", "sem_cache_entries"} {
		t.Logf("  %-18s slope=%+.6f limit=±%.6f", name, slopes[name], limits[name])
		if s := slopes[name]; s > limits[name] || s < -limits[name] {
			t.Errorf("leak slope detected: %s slope=%+.6f exceeds ±%.6f/round", name, s, limits[name])
			failed = true
		}
	}
	// Terminal state: no open reservation may survive the soak.
	if _, open, _ := s.budgets.Stats(); open != 0 {
		t.Errorf("soak left %d budget reservations open", open)
		_ = failed
	}
}
