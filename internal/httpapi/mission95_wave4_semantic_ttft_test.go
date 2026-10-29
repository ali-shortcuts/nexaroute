package httpapi

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func itoaWave4(i int) string { return strconv.Itoa(i) }

func TestWave4SemanticPromptText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"invalid-json", `not json`, "", false},
		{"no-messages", `{}`, "", false},
		{"empty-messages", `{"messages":[]}`, "", false},
		{"plain-strings", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`, "user: hello\nassistant: hi", true},
		{"empty-plain-skipped", `{"messages":[{"role":"user","content":""}]}`, "", false},
		{"blocks-mixed", `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"input_text","text":"b"},{"type":"image","text":"no"},{"type":"text","text":""}]}]}`, "user: a\nuser: b", true},
		{"non-string-non-array-skipped", `{"messages":[{"role":"user","content":42}]}`, "", false},
		{"object-content-skipped", `{"messages":[{"role":"user","content":{"type":"text"}}]}`, "", false},
		{"oversize", `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 40<<10) + `"}]}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := semanticPromptText([]byte(tc.raw))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("semanticPromptText = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func wave4TTFTServer(t *testing.T, enabled, ingest bool) *Server {
	t.Helper()
	plane := newEvaluationPlane(config.EvaluationConfig{Enabled: enabled, TelemetryIngestEnabled: ingest}, nil)
	return &Server{evaluation: plane}
}

func wave4TTFTBuckets(t *testing.T, s *Server) map[string]*telemetryBucket {
	t.Helper()
	plane := s.evaluationSnapshot()
	if plane == nil || plane.telemetry == nil {
		t.Fatal("expected a live evaluation plane with a telemetry aggregator")
	}
	return plane.telemetry.buckets
}

// Disabled planes record nothing: evaluation off, ingest off, and a nil
// aggregator all return before touching any bucket.
func TestWave4ObserveTTFTGates(t *testing.T) {
	(&Server{}).observeTelemetryTTFT("p/m", 100*time.Millisecond)
	off := wave4TTFTServer(t, false, true)
	off.observeTelemetryTTFT("p/m", 100*time.Millisecond)
	if len(wave4TTFTBuckets(t, off)) != 0 {
		t.Fatal("disabled evaluation must not record TTFT")
	}
	gated := wave4TTFTServer(t, true, false)
	gated.observeTelemetryTTFT("p/m", 100*time.Millisecond)
	if len(wave4TTFTBuckets(t, gated)) != 0 {
		t.Fatal("disabled ingest must not record TTFT")
	}
	nilAgg := wave4TTFTServer(t, true, true)
	nilAgg.evaluationSnapshot().telemetry = nil
	nilAgg.observeTelemetryTTFT("p/m", 100*time.Millisecond)
}

// Negative and zero durations never create a sample; the negative case
// clamps to zero and then takes the same early return as zero.
func TestWave4ObserveTTFTNonPositiveDropped(t *testing.T) {
	s := wave4TTFTServer(t, true, true)
	s.observeTelemetryTTFT("p/m", -5*time.Second)
	s.observeTelemetryTTFT("p/m", 0)
	if len(wave4TTFTBuckets(t, s)) != 0 {
		t.Fatalf("buckets = %v, want none for non-positive durations", wave4TTFTBuckets(t, s))
	}
}

// Durations above the 10-minute cap are recorded at exactly the cap,
// and a nil buckets map is initialized on first use.
func TestWave4ObserveTTFTCapAndLazyBuckets(t *testing.T) {
	s := wave4TTFTServer(t, true, true)
	s.evaluationSnapshot().telemetry.buckets = nil
	s.observeTelemetryTTFT("p/m", 700*time.Second)
	buckets := wave4TTFTBuckets(t, s)
	b, ok := buckets["p/m"]
	if !ok || len(b.ttfts) != 1 || b.ttfts[0] != 600000 {
		t.Fatalf("bucket = %+v, want a single 600000 sample", b)
	}
	s.observeTelemetryTTFT("p/m", 150*time.Millisecond)
	if len(b.ttfts) != 2 || b.ttfts[1] != 150 {
		t.Fatalf("bucket samples = %v, want [600000 150]", b.ttfts)
	}
}

// The per-deployment cap refuses a 4097th deployment while existing
// deployments keep recording.
func TestWave4ObserveTTFTDeploymentCap(t *testing.T) {
	s := wave4TTFTServer(t, true, true)
	buckets := wave4TTFTBuckets(t, s)
	for i := 0; i < maxTelemetryDeployments; i++ {
		buckets["dep-"+itoaWave4(i)] = &telemetryBucket{}
	}
	s.observeTelemetryTTFT("new-deployment", 100*time.Millisecond)
	if _, ok := buckets["new-deployment"]; ok {
		t.Fatal("deployment beyond the cap must not create a bucket")
	}
	before := len(buckets["dep-0"].ttfts)
	s.observeTelemetryTTFT("dep-0", 100*time.Millisecond)
	if len(buckets["dep-0"].ttfts) != before+1 {
		t.Fatalf("existing bucket samples did not grow: %d -> %d", before, len(buckets["dep-0"].ttfts))
	}
}

// Beyond 256 samples the oldest sample is dropped: the window slides,
// it does not grow.
func TestWave4ObserveTTFTSampleWindowSlides(t *testing.T) {
	s := wave4TTFTServer(t, true, true)
	buckets := wave4TTFTBuckets(t, s)
	samples := make([]float64, maxTelemetrySamplesPerDeployment)
	for i := range samples {
		samples[i] = float64(i + 1)
	}
	buckets["p/m"] = &telemetryBucket{deploymentID: "p/m", ttfts: samples}
	s.observeTelemetryTTFT("p/m", 999*time.Millisecond)
	b := buckets["p/m"]
	if len(b.ttfts) != maxTelemetrySamplesPerDeployment {
		t.Fatalf("len = %d, want %d", len(b.ttfts), maxTelemetrySamplesPerDeployment)
	}
	if b.ttfts[0] != 2 || b.ttfts[len(b.ttfts)-1] != 999 {
		t.Fatalf("window = [%v ... %v], want [2 ... 999]", b.ttfts[0], b.ttfts[len(b.ttfts)-1])
	}
}
