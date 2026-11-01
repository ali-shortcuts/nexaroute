package httpapi

// TEMPORARY DIAGNOSTIC — remove before the real fix lands.
// Reproduces the intermittent `TestWave2GeminiHTTPFailover: good candidate
// calls=2 want 1` failure and records, per anomaly: the exact requests the
// "good" fake upstream received (method, path, body digest, remote addr,
// arrival offset), the gateway's attempts header, and the event stream.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type debugUpstreamHit struct {
	At      time.Duration
	Method  string
	Path    string
	Digest  string
	Remote  string
	HasBody bool
}

type debugHitLog struct {
	base time.Time
	mu   sync.Mutex
	hits []debugUpstreamHit
}

func (l *debugHitLog) record(r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	sum := sha256.Sum256(body)
	at := time.Since(l.base)
	l.mu.Lock()
	l.hits = append(l.hits, debugUpstreamHit{
		At: at, Method: r.Method, Path: r.URL.Path,
		Digest: hex.EncodeToString(sum[:6]), Remote: r.RemoteAddr, HasBody: len(body) > 0,
	})
	l.mu.Unlock()
}

func (l *debugHitLog) snapshot() []debugUpstreamHit {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]debugUpstreamHit, len(l.hits))
	copy(out, l.hits)
	return out
}

// debugGeminiFailoverScenario runs one isolated copy of the HTTP-failover
// scenario: provider "a" answers 500, provider "b" answers a valid chat
// completion, and the Gemini ingress must call "b" exactly once.
func debugGeminiFailoverScenario(t *testing.T) (code int, events []string, hits []debugUpstreamHit, attempts string) {
	t.Helper()
	var goodCalls atomic.Int32
	log := &debugHitLog{base: time.Now()}

	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer upA.Close()

	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		log.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wave3ChatOK)
	}))
	defer upB.Close()

	s := wave2GeminiServer(t, []config.ProviderConfig{
		wave2GeminiProvider("a", "openai_compatible", upA.URL),
		wave2GeminiProvider("b", "openai_compatible", upB.URL),
	}, nil)

	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	// Capture anything a lingering goroutine adds within a short window.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) && goodCalls.Load() < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	for _, e := range s.bus.Snapshot() {
		events = append(events, fmt.Sprintf("%s@%s", e.Kind, e.Deployment))
	}
	return rr.Code, events, log.snapshot(), rr.Header().Get("X-NexaRoute-Attempts")
}

func debugAnomalyLine(t *testing.T, label string, code int, events []string, hits []debugUpstreamHit, attempts string) string {
	t.Helper()
	parts := make([]string, 0, len(hits))
	for i, h := range hits {
		same := "first"
		if i > 0 && h.Digest == hits[0].Digest {
			same = "sameBody"
		} else if i > 0 {
			same = "otherBody"
		}
		parts = append(parts, fmt.Sprintf("{t=%dms %s %s sha=%s %s remote=%s}", h.At.Milliseconds(), h.Method, h.Path, h.Digest, same, h.Remote))
	}
	return fmt.Sprintf("%s code=%d hits=%d attempts=%q up=%s events=%s",
		label, code, len(hits), attempts, strings.Join(parts, ","), strings.Join(events, ","))
}

func TestDebugGeminiFailoverFlakeProbe(t *testing.T) {
	iters := 60
	if raw := strings.TrimSpace(os.Getenv("NEXAROUTE_DEBUG_ITERS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			iters = n
		}
	}
	var anomalies []string
	for i := 0; i < iters; i++ {
		code, events, hits, attempts := debugGeminiFailoverScenario(t)
		if code == http.StatusOK && len(hits) == 1 && attempts == "2" {
			continue
		}
		anomalies = append(anomalies, debugAnomalyLine(t, fmt.Sprintf("iter=%d", i), code, events, hits, attempts))
		if len(anomalies) >= 6 {
			break
		}
	}
	if len(anomalies) > 0 {
		t.Fatalf("failover anomalies in %d/%d iterations:\n%s", len(anomalies), iters, strings.Join(anomalies, "\n"))
	}
	t.Logf("no anomalies in %d iterations", iters)
}

// TestDebugGeminiFailoverConcurrentStress runs the same scenario from several
// goroutines at once: several gateways and fake upstreams alive together, which
// is the closest in-package analogue of the surrounding suite's goroutines.
func TestDebugGeminiFailoverConcurrentStress(t *testing.T) {
	const workers, rounds = 8, 12
	var mu sync.Mutex
	var anomalies []string
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				code, events, hits, attempts := debugGeminiFailoverScenario(t)
				if code == http.StatusOK && len(hits) == 1 && attempts == "2" {
					continue
				}
				line := debugAnomalyLine(t, fmt.Sprintf("w=%d r=%d", w, r), code, events, hits, attempts)
				mu.Lock()
				if len(anomalies) < 6 {
					anomalies = append(anomalies, line)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(anomalies) > 0 {
		t.Fatalf("concurrent failover anomalies in %d rounds:\n%s", workers*rounds, strings.Join(anomalies, "\n"))
	}
	t.Logf("no anomalies in %d concurrent rounds", workers*rounds)
}
