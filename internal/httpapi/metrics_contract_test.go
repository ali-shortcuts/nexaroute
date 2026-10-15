package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// The observability contract (M4-6): the dashboards and alert rules in
// deploy/observability may only reference series the gateway actually
// exports, the histogram exposition must be well-formed, and the label
// names on the whole exposition must stay inside the audited allowlist
// (all config- or enum-derived — never request-scoped values such as
// users, keys, prompts or client IPs).

var metricsBaselineLatency, metricsBaselineTTFT health.Histogram

func scrapeMetrics(t *testing.T) string {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false // startup probes would add their own samples
	cfg.Providers = []config.ProviderConfig{{
		ID: "prov1", Name: "prov1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 10, Weight: 1}},
	}}
	srv := testGateway(t, cfg)
	// One deployment id is formed provider/model in the runtime table;
	// discover it instead of assuming the format.
	ds := srv.rt.All()
	if len(ds) == 0 {
		t.Fatal("test gateway has no deployments")
	}
	dep := ds[0].ID
	// Startup machinery may record an initial observation; wait for the
	// histogram to settle and take a baseline so the assertions below
	// measure exactly the samples this test adds.
	for deadline := time.Now().Add(2 * time.Second); ; {
		lat, ttft, _ := srv.hm.Histograms(dep)
		time.Sleep(30 * time.Millisecond)
		lat2, ttft2, _ := srv.hm.Histograms(dep)
		metricsBaselineLatency, metricsBaselineTTFT = lat2, ttft2
		if lat.Count == lat2.Count && lat.SumMS == lat2.SumMS && ttft.Count == ttft2.Count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("histograms did not settle after gateway startup")
		}
	}
	srv.hm.RecordSuccess(dep, 120*time.Millisecond)
	srv.hm.RecordSuccess(dep, 1500*time.Millisecond)
	srv.hm.RecordFailure(dep, "boom", 300*time.Millisecond)
	srv.hm.RecordTTFT(dep, 45*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "http://gateway/metrics", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status %d", rr.Code)
	}
	return rr.Body.String()
}

var seriesLineRE = regexp.MustCompile(`^(nexaroute_[a-z0-9_]+)(\{[^}]*\})? `)
var helpLineRE = regexp.MustCompile(`^# HELP (nexaroute_[a-z0-9_]+) `)

func expositionSeries(body string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if m := seriesLineRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
		// Some series render samples only once data exists (e.g.
		// nexaroute_errors_total); their HELP declaration is always
		// emitted and proves the series is part of the exposition.
		if m := helpLineRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

func TestMetricsHistogramExposition(t *testing.T) {
	body := scrapeMetrics(t)
	for _, name := range []string{"nexaroute_deployment_request_latency_ms", "nexaroute_deployment_ttft_ms"} {
		buckets := map[float64]uint64{}
		var sum, count float64
		var sawSum, sawCount bool
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, name+"_") {
				continue
			}
			fields := strings.Fields(line)
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				t.Fatalf("unparsable value in line %q", line)
			}
			switch {
			case strings.HasPrefix(line, name+"_bucket"):
				le := line[strings.Index(line, `le="`)+4:]
				le = le[:strings.Index(le, `"`)]
				if le == "+Inf" {
					buckets[1e18] = uint64(value)
				} else {
					b, err := strconv.ParseFloat(le, 64)
					if err != nil {
						t.Fatalf("unparsable le in %q", line)
					}
					buckets[b] = uint64(value)
				}
			case strings.HasPrefix(line, name+"_sum"):
				sum, sawSum = value, true
			case strings.HasPrefix(line, name+"_count"):
				count, sawCount = value, true
			}
		}
		if !sawSum || !sawCount {
			t.Fatalf("%s: missing _sum or _count", name)
		}
		if len(buckets) != len(health.LatencyHistogramBoundsMS)+1 {
			t.Fatalf("%s: %d buckets, want %d", name, len(buckets), len(health.LatencyHistogramBoundsMS)+1)
		}
		keys := make([]float64, 0, len(buckets))
		for k := range buckets {
			keys = append(keys, k)
		}
		sort.Float64s(keys)
		var prev uint64
		for _, k := range keys {
			if buckets[k] < prev {
				t.Fatalf("%s: buckets not cumulative at le=%v", name, k)
			}
			prev = buckets[k]
		}
		if float64(prev) != count {
			t.Fatalf("%s: +Inf bucket %d != count %v", name, prev, count)
		}
		wantSum, wantCount := 1920.0+metricsBaselineLatency.SumMS, 3.0+float64(metricsBaselineLatency.Count)
		if strings.HasSuffix(name, "ttft_ms") {
			wantSum, wantCount = 45.0+metricsBaselineTTFT.SumMS, 1.0+float64(metricsBaselineTTFT.Count)
		}
		if sum != wantSum || count != wantCount {
			t.Fatalf("%s: sum=%v count=%v, want %v/%v", name, sum, count, wantSum, wantCount)
		}
	}
}

var metricNameRE = regexp.MustCompile(`nexaroute_[a-z0-9_]+`)

func TestObservabilityAssetsReferenceRealSeries(t *testing.T) {
	body := scrapeMetrics(t)
	series := expositionSeries(body)
	for _, asset := range []string{
		"../../deploy/observability/prometheus-alerts.yaml",
		"../../deploy/observability/grafana-dashboard.json",
	} {
		raw, err := os.ReadFile(asset)
		if err != nil {
			t.Fatalf("read %s: %v", asset, err)
		}
		refs := map[string]bool{}
		for _, m := range metricNameRE.FindAllString(string(raw), -1) {
			refs[m] = true
		}
		if len(refs) == 0 {
			t.Fatalf("%s references no nexaroute_* series", asset)
		}
		for ref := range refs {
			if !series[ref] {
				t.Errorf("%s references %s, which the gateway does not export", asset, ref)
			}
		}
	}
}

// The audited label-name allowlist for the whole exposition. Every name
// here is config- or enum-derived; adding a label requires updating
// this list deliberately (and re-auditing its cardinality).
var metricLabelAllowlist = map[string]bool{
	"deployment": true, "provider": true, "status": true, "kind": true,
	"error_type": true, "le": true, "task": true, "complexity": true,
	"outcome": true, "type": true, "verdict": true, "dimension": true,
	"provenance": true, "source": true, "mode": true, "evaluator": true,
}

var labelPairRE = regexp.MustCompile(`(?:^|[{,])([a-z_]+)="`)

func TestMetricsLabelNamesStayAudited(t *testing.T) {
	body := scrapeMetrics(t)
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		m := seriesLineRE.FindStringSubmatch(line)
		if m == nil || m[2] == "" {
			continue
		}
		for _, lm := range labelPairRE.FindAllStringSubmatch(m[2], -1) {
			seen[lm[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("no labels found in exposition; allowlist check is vacuous")
	}
	for name := range seen {
		if !metricLabelAllowlist[name] {
			t.Errorf("label %q is not in the audited allowlist; audit its cardinality before adding it", name)
		}
	}
}
