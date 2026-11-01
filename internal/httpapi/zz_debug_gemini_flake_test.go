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
	mu   sync.Mutex
	base time.Time
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
	// Let any still-running goroutine finish so a late request is captured,
	// but keep the observation window tiny.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) && goodCalls.Load() < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	for _, e := range s.bus.Snapshot() {
		events = append(events, fmt.Sprintf("%s@%s", e.Kind, e.Deployment))
	}
	return rr.Code, events, log.snapshot(), rr.Header().Get("X-NexaRoute-Attempts")
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
		if code == http.StatusOK && len(hits) == 1 {
			continue
		}
		parts := make([]string, 0, len(hits))
		for _, h := range hits {
			parts = append(parts, fmt.Sprintf("{t=%dms %s %s sha=%s remote=%s body=%v}", h.At.Milliseconds(), h.Method, h.Path, h.Digest, h.Remote, h.HasBody))
		}
		anomalies = append(anomalies, fmt.Sprintf("iter=%d code=%d hits=%d attempts=%q up=%s events=%s",
			i, code, len(hits), attempts, strings.Join(parts, ","), strings.Join(events, ",")))
	}
	if len(anomalies) > 0 {
		t.Fatalf("gemini failover anomalies in %d/%d iterations:\n%s", len(anomalies), iters, strings.Join(anomalies[:min(len(anomalies), 12)], "\n"))
	}
	t.Logf("no anomalies in %d iterations", iters)
}
