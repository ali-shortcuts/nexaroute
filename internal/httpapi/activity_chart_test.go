package httpapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Overview throughput panel is the only chart in the control plane. It must
// be built from real lifecycle records, so its model builder is dependency-free
// and executed here verbatim under node (same pattern as the cost page): the
// scenarios below run production app.js code, not a copy.
//
// Two classes of defect are pinned:
//
//  1. data honesty — no smoothing, no interpolation, no clamping of records
//     that fall outside the window into an edge bar, and no fabricated bar when
//     the feed is empty;
//  2. rendering safety — the SVG carries no raw colour literal (the token layer
//     is the single source of truth) and the accessible label is escaped, so a
//     provider or model name can never inject markup.
func TestActivityChartLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(app)

	start := strings.Index(src, "/* activity-chart-logic:start")
	end := strings.Index(src, "/* activity-chart-logic:end */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("activity-chart-logic block not found in app.js")
	}
	escLine := ""
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "const esc = ") {
			escLine = line
			break
		}
	}
	if escLine == "" {
		t.Fatal("app.js is missing the esc() helper the chart renders through")
	}

	script := escLine + "\n" + src[start:end] + `
function eq(got, want, msg) { if (JSON.stringify(got) !== JSON.stringify(want)) { console.error('FAIL ' + msg + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want)); process.exit(1); } }
function ok(cond, msg) { if (!cond) { console.error('FAIL ' + msg); process.exit(1); } }

const now = Date.UTC(2026, 10, 1, 12, 0, 30);
const opts = { bucketMs: 30000, windowMs: 300000 };

// An empty feed renders no bars at all — the panel shows its empty state, it
// never invents movement to look alive.
let m = activityChartModel([], now, opts);
eq(m.total, 0, 'empty feed has no requests');
eq(m.empty, true, 'empty feed is flagged empty');
eq(m.bucketCount, 10, 'a 5 minute window at 30s buckets is 10 buckets');
eq(m.buckets.length, 10, 'one bucket object per bucket');
eq(m.max, 0, 'empty feed has no peak');
eq(activityChartBars(m).filter(b => b.height > 0).length, 0, 'empty feed draws no bars');
ok(m.windowStart <= now && now === m.windowEnd, 'the window contains now');

// Records outside the window are reported as skipped, not clamped into an
// edge bar, and unparsable timestamps are never treated as "now".
const rows = [
  { started_at: new Date(now - 1000).toISOString(), state: 'succeeded' },
  { started_at: new Date(now - 2000).toISOString(), state: 'failed' },
  { started_at: new Date(now - 3600000).toISOString(), state: 'succeeded' },
  { started_at: 'not-a-date', state: 'active' },
];
m = activityChartModel(rows, now, opts);
eq(m.total, 2, 'only in-window records are counted');
eq(m.skipped, 2, 'an old record and a bad timestamp are both skipped');
eq(m.retained, 4, 'the retained count still reports every record seen');
eq(m.buckets.reduce((s, b) => s + b.ok, 0), 1, 'one succeeded start');
eq(m.buckets.reduce((s, b) => s + b.failed, 0), 1, 'one failed start');
eq(m.max, 2, 'the shared bucket holds both');
eq(m.buckets[9].count, 2, 'both starts land in the bucket they belong to');
eq(activityChartBars(m)[9].count, 2, 'geometry agrees with the model');
eq(activityChartBars(m).filter(b => b.height > 0).length, 1, 'one bar for one non-empty bucket');
eq(activityChartBars(m)[9].height, 30, 'the peak bucket fills the geometry');
for (const b of activityChartBars(m)) ok(Number.isFinite(b.height) && b.height >= 0 && b.height <= 30, 'bar height stays finite and bounded');
eq(JSON.stringify(activityChartModel(rows, now, opts)), JSON.stringify(m), 'the model is deterministic for the same input and clock');
for (const b of activityChartBars(activityChartModel([], now, opts))) ok(Number.isFinite(b.height), 'a zero peak never produces NaN geometry');

// Every lifecycle state is accounted for, including cancelled and unknown.
const lifecycle = activityChartModel(['succeeded', 'failed', 'active', 'stale', 'cancelled', 'unknown'].map(state => ({started_at: new Date(now - 1).toISOString(), state})), now, opts);
const last = lifecycle.buckets[9];
eq([last.ok,last.failed,last.active,last.stale,last.cancelled,last.other], [1,1,1,1,1,1], 'states retain their identity');
eq(last.ok+last.failed+last.active+last.stale+last.cancelled+last.other, last.count, 'status counts reconcile');
const lifeBar = activityChartBars(lifecycle)[9];
for (const segment of lifeBar.segments) eq(segment.height, 5, 'each of six equal statuses receives exactly one sixth');
const lifeSvg = activityChartSvg(lifecycle, 'states');
for (const kind of ['ok','failed','active','stale','cancelled','other']) ok(lifeSvg.includes('class="trend-bar '+kind+'"'), 'renders '+kind);
const offBoundary = activityChartModel([], now + 12345, opts);
eq(offBoundary.windowEnd, now + 12345, 'window ends at observation time inside bucket');
eq(offBoundary.windowStart, now + 12345 - 300000, 'window has 300 seconds of history');
eq(m.windowEnd, now, 'window ends at exact boundary');

// Window edges: start inclusive, end exclusive.
const edges = activityChartModel([
  { started_at: new Date(m.windowStart).toISOString(), state: 'succeeded' },
  { started_at: new Date(m.windowEnd - 1).toISOString(), state: 'succeeded' },
  { started_at: new Date(m.windowStart - 1).toISOString(), state: 'succeeded' },
  { started_at: new Date(m.windowEnd).toISOString(), state: 'active' },
  { started_at: new Date(m.windowEnd + 1).toISOString(), state: 'failed' },
], now, opts);
eq(edges.buckets[0].count, 1, 'the window start is inclusive');
eq(edges.buckets[9].count, 1, 'the window end is exclusive');
eq(edges.skipped, 3, 'before-start, end and future records are skipped');

// Bucket count is bounded in both directions regardless of caller input.
eq(activityChartModel([], now, { bucketMs: 1000, windowMs: 3600000 }).bucketCount, 60, 'bucket count is capped at 60');
eq(activityChartModel([], now, { bucketMs: 600000, windowMs: 60000 }).bucketCount, 1, 'bucket count never drops below 1');

// Rendering: no raw colour literal, escaped accessible name, no markup
// injection from any label the backend could supply.
const svg = activityChartSvg(m, '<script>alert(1)</script>');
ok(svg.indexOf('<script') === -1, 'the svg must not contain an injected script tag');
ok(svg.indexOf('&lt;script&gt;') >= 0, 'the accessible label is escaped');
ok(/role="img"/.test(svg) && /aria-label="/.test(svg), 'the svg carries an accessible name');
ok(/class="trend-bar ok"/.test(svg) && /class="trend-bar failed"/.test(svg), 'succeeded and failed portions are visually distinct');
ok(!/#[0-9a-fA-F]{3,8}\b|rgb\(|hsl\(/.test(svg), 'the svg embeds no raw colour literal; it must consume tokens by class');
ok((svg.match(/<rect/g) || []).length === 2, 'one rect per non-empty segment');
console.log('activity chart behavior OK');
`
	path := filepath.Join(t.TempDir(), "activity_chart_behavior_test.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node behavior test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "activity chart behavior OK") {
		t.Fatalf("unexpected node output: %s", out)
	}
}

// TestActivityChartWiring pins the panel to real backend data and to working
// controls: the Overview must render it, the refresh/retry button must resolve
// to a handler, and the panel must never be fed a client-side constant.
func TestActivityChartWiring(t *testing.T) {
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)

	if !strings.Contains(js, "${trendPanel()}") {
		t.Error("overviewView does not render the throughput panel")
	}
	for _, want := range []string{
		"data-action=\"refresh-trend\"",
		"'refresh-trend': () => refreshTrend(true)",
		"'trend-activity': () =>",
		"/admin/api/activity/requests?limit=",
		"function refreshTrend(",
		"function ensureTrendPolling(",
		"function trendState(",
		"w: Math.round((model.windowEnd - model.windowStart) / 1000)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js is missing %q", want)
		}
	}
	if strings.Contains(js, "activityChartModel([{") || strings.Contains(js, "activityChartModel(FIXTURE") {
		t.Error("the chart must not be fed hard-coded sample data")
	}
	// Error, empty and forbidden outcomes must all be distinguished, otherwise
	// an unauthorized operator would read a permission failure as "no traffic".
	for _, key := range []string{"tr_forbidden", "tr_error", "tr_empty", "tr_retry", "tr_caption"} {
		if !strings.Contains(js, "'"+key+"':") {
			t.Errorf("app.js is missing the %q body-copy key", key)
		}
	}
}
