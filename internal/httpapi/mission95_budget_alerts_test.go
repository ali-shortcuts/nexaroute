package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// fakeBudgetAlertIO is a controllable BudgetAlertSnapshotIO for the
// io-backed store tests. Writes can be forced to conflict once.
type fakeBudgetAlertIO struct {
	mu              sync.Mutex
	data            []byte
	rev             uint64
	conflictOnce    bool
	readFailOnRetry bool
	readErr         error
	writeErr        error
	writes          int
	conflictGiven   bool
}

func (f *fakeBudgetAlertIO) Read(ctx context.Context) ([]byte, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, 0, f.readErr
	}
	if f.readFailOnRetry && f.conflictGiven {
		return nil, 0, errors.New("re-read down")
	}
	if f.data == nil {
		return nil, 0, ErrBudgetAlertStoreNotFound
	}
	return append([]byte(nil), f.data...), f.rev, nil
}

func (f *fakeBudgetAlertIO) Write(ctx context.Context, payload []byte, expected uint64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.conflictOnce && !f.conflictGiven {
		f.conflictGiven = true
		// Simulate a concurrent writer winning first.
		f.data, f.rev = []byte(`{"version":1,"fired":{"winner":7}}`), 9
		return 0, ErrBudgetAlertStoreConflict
	}
	if expected != f.rev {
		return 0, ErrBudgetAlertStoreConflict
	}
	f.data, f.rev = append([]byte(nil), payload...), f.rev+1
	return f.rev, nil
}

func TestMission95BudgetAlertDocParse(t *testing.T) {
	fired, err := parseBudgetAlertDoc(nil)
	if err != nil || len(fired) != 0 {
		t.Fatalf("empty doc = %v, %v", fired, err)
	}
	fired, err = parseBudgetAlertDoc([]byte(`{"version":1,"fired":{"a":5}}`))
	if err != nil || fired["a"] != 5 {
		t.Fatalf("valid doc = %v, %v", fired, err)
	}
	fired, err = parseBudgetAlertDoc([]byte(`{"version":1}`))
	if err != nil || len(fired) != 0 {
		t.Fatalf("nil fired doc = %v, %v", fired, err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"invalid json", []byte(`{`)},
		{"unknown field", []byte(`{"version":1,"bogus":1}`)},
		{"wrong version", []byte(`{"version":2,"fired":{}}`)},
		{"oversize", []byte(`{"version":1,"fired":{"k":1},"pad":"` + strings.Repeat("x", maxBudgetAlertDoc) + `"}`)},
	} {
		if _, err := parseBudgetAlertDoc(tc.data); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
	// Too many entries is rejected.
	var sb strings.Builder
	sb.WriteString(`{"version":1,"fired":{`)
	for i := 0; i < maxBudgetAlertFired+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"k%d":1`, i)
	}
	sb.WriteString(`}}`)
	if _, err := parseBudgetAlertDoc([]byte(sb.String())); err == nil {
		t.Error("over-cap fired set accepted")
	}
	// Marshal round-trip preserves marks.
	data, err := marshalBudgetAlertDoc(map[string]int64{"x": 42})
	if err != nil {
		t.Fatal(err)
	}
	var doc budgetAlertStateDoc
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != 1 || doc.Fired["x"] != 42 {
		t.Fatalf("marshal round-trip = %+v, %v", doc, err)
	}
}

func TestMission95FileBudgetAlertStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")
	st := &fileBudgetAlertStore{path: path}
	fired, rev, err := st.Load()
	if err != nil || rev != 0 || len(fired) != 0 {
		t.Fatalf("missing file load = %v, %d, %v", fired, rev, err)
	}
	if _, err := st.Save(map[string]int64{"budget\x00period\x0080": 123}, 0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, %v", info, err)
	}
	fired, _, err = st.Load()
	if err != nil || fired["budget\x00period\x0080"] != 123 {
		t.Fatalf("round-trip = %v, %v", fired, err)
	}
	// Directory path fails closed on load.
	dirStore := &fileBudgetAlertStore{path: dir}
	if _, _, err := dirStore.Load(); err == nil {
		t.Error("directory load accepted")
	}
	// Corrupt document fails closed.
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Load(); err == nil {
		t.Error("corrupt doc load accepted")
	}
	// Save into a path whose parent component is an existing file fails.
	// path (dir/nested/state.json) is a regular file at this point.
	bad := &fileBudgetAlertStore{path: filepath.Join(path, "child.json")}
	if _, err := bad.Save(map[string]int64{}, 0); err == nil {
		t.Error("save under a file accepted")
	}
}

func TestMission95IOBudgetAlertStore(t *testing.T) {
	// Not-found reads as empty, first write succeeds.
	fake := &fakeBudgetAlertIO{}
	st := NewIOBudgetAlertStore(fake)
	fired, rev, err := st.Load()
	if err != nil || rev != 0 || len(fired) != 0 {
		t.Fatalf("not-found load = %v, %d, %v", fired, rev, err)
	}
	rev, err = st.Save(map[string]int64{"a": 1}, 0)
	if err != nil || rev != 1 {
		t.Fatalf("first save = %d, %v", rev, err)
	}
	fired, rev, err = st.Load()
	if err != nil || fired["a"] != 1 || rev != 1 {
		t.Fatalf("reload = %v, %d, %v", fired, rev, err)
	}

	// Conflict retry merges the winner's marks with ours.
	conflicting := &fakeBudgetAlertIO{conflictOnce: true}
	st2 := NewIOBudgetAlertStore(conflicting)
	if _, err := st2.Save(map[string]int64{"ours": 3}, 0); err != nil {
		t.Fatalf("conflict-retry save failed: %v", err)
	}
	fired, _, err = st2.Load()
	if err != nil || fired["winner"] != 7 || fired["ours"] != 3 {
		t.Fatalf("merged marks = %v, %v", fired, err)
	}
	if conflicting.writes != 2 {
		t.Errorf("expected 2 writes across conflict retry, got %d", conflicting.writes)
	}

	// Non-conflict write errors propagate without retry.
	broken := &fakeBudgetAlertIO{writeErr: errors.New("disk gone")}
	if _, err := NewIOBudgetAlertStore(broken).Save(map[string]int64{"a": 1}, 0); err == nil {
		t.Error("write error swallowed")
	}
	if broken.writes != 1 {
		t.Errorf("non-conflict error retried: %d writes", broken.writes)
	}

	// Non-not-found read errors propagate.
	readBroken := &fakeBudgetAlertIO{readErr: errors.New("unavailable")}
	if _, _, err := NewIOBudgetAlertStore(readBroken).Load(); err == nil {
		t.Error("read error swallowed")
	}
	// Corrupt stored document fails load.
	corrupt := &fakeBudgetAlertIO{data: []byte(`{"version":3}`), rev: 4}
	if _, _, err := NewIOBudgetAlertStore(corrupt).Load(); err == nil {
		t.Error("corrupt doc load accepted")
	}
	// Conflict retry whose re-read fails returns the original conflict error
	// and does not report success.
	reReadBroken := &fakeBudgetAlertIO{conflictOnce: true, readFailOnRetry: true}
	if _, err := NewIOBudgetAlertStore(reReadBroken).Save(map[string]int64{"a": 1}, 0); err == nil {
		t.Error("save with failing conflict re-read accepted")
	}
	if reReadBroken.writes != 1 {
		t.Errorf("failed re-read retried the write: %d writes", reReadBroken.writes)
	}
}

func TestMission95BudgetAlertPeriodDedupPrune(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if got := budgetAlertPeriodID("monthly", 10, now); got != "2026-10" {
		t.Errorf("monthly period = %q", got)
	}
	if got := budgetAlertPeriodID("MONTHLY ", 10, now); got != "lifetime:10" {
		// Note: callers lowercase+trim before calling; raw input is not
		// normalized here, so an unnormalized monthly label is a lifetime id.
		t.Errorf("unnormalized period = %q", got)
	}
	if got := budgetAlertPeriodID("", 2.5, now); got != "lifetime:2.5" {
		t.Errorf("lifetime period = %q", got)
	}
	if budgetAlertDedupKey("b", "p", 80) == budgetAlertDedupKey("b", "p", 90) {
		t.Error("distinct thresholds share a dedup key")
	}
	if budgetAlertDedupKey("b", "p", 80) != budgetAlertDedupKey("b", "p", 80) {
		t.Error("dedup key not deterministic")
	}

	// markFired is exactly-once per key.
	a := &budgetAlerter{fired: map[string]int64{}, now: func() time.Time { return now }}
	if !a.markFired("k1") || a.markFired("k1") {
		t.Error("markFired dedup violated")
	}
	if a.snapshotFired()["k1"] == 0 {
		t.Error("snapshot lost fired mark")
	}
	// Prune drops entries older than 400 days.
	old := time.Now().Add(-500 * 24 * time.Hour).UnixNano()
	a.fired["stale"] = old
	pruneBudgetAlertFired(a.fired)
	if _, ok := a.fired["stale"]; ok {
		t.Error("stale fired mark survived prune")
	}
	// CIDR parsing: prefixes pass, bare IPs become /32 (or /128), junk ignored.
	prefixes := parseBudgetAlertCIDRs([]string{"", " 10.0.0.0/8 ", "192.0.2.7", "not-a-cidr", "2001:db8::1"})
	if len(prefixes) != 3 {
		t.Fatalf("CIDR parse = %v", prefixes)
	}
	if prefixes[1].Bits() != 32 || prefixes[2].Bits() != 128 {
		t.Errorf("bare-IP prefix bits = %v", prefixes)
	}
	// Redirect loopback spelling.
	for _, host := range []string{"localhost", "api.localhost", "127.0.0.1", "[::1]", "LOCALHOST."} {
		if !isBudgetWebhookRedirectLoopback(host) {
			t.Errorf("loopback host %q rejected", host)
		}
	}
	for _, host := range []string{"example.com", "192.0.2.1", "localhost.evil.com"} {
		if isBudgetWebhookRedirectLoopback(host) {
			t.Errorf("non-loopback host %q accepted", host)
		}
	}
}

func TestMission95BudgetAlertReconfigure(t *testing.T) {
	a := newBudgetAlerter(config.Config{}, nil)
	if a.fired == nil {
		t.Fatal("alerter has nil fired map")
	}
	// Same path twice is a no-op; empty path clears the file store.
	path := filepath.Join(t.TempDir(), "alerts.json")
	fs := &fileBudgetAlertStore{path: path}
	// Fired marks are UnixNano timestamps; pruneBudgetAlertFired drops
	// entries older than 400 days, so seed with a fresh timestamp.
	seedAt := time.Now().UnixNano()
	if _, err := fs.Save(map[string]int64{"seed": seedAt}, 0); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.BudgetAlerts.StatePath = path
	a.reconfigure(cfg)
	if a.fired["seed"] != seedAt {
		t.Fatalf("reconfigure did not merge file marks: %v", a.fired)
	}
	a.reconfigure(cfg) // same path: no-op, marks preserved
	if a.fired["seed"] != seedAt {
		t.Error("same-path reconfigure dropped marks")
	}
	cfg.BudgetAlerts.StatePath = ""
	a.reconfigure(cfg)
	if a.fileStore != nil {
		t.Error("empty path did not clear file store")
	}
	if a.fired["seed"] != seedAt {
		t.Error("clearing file store dropped live marks")
	}
	// Unusable state path counts a failure but keeps the alerter alive.
	badDir := t.TempDir()
	cfg.BudgetAlerts.StatePath = badDir // a directory, not a file
	before := a.stateFailures
	a.reconfigure(cfg)
	if a.stateFailures != before+1 {
		t.Errorf("unusable path failures = %d, want %d", a.stateFailures, before+1)
	}
}

func TestMission95DeliverBudgetAlert(t *testing.T) {
	payload := BudgetAlertPayload{BudgetID: "k1", ThresholdPercent: 80, SpendUSD: 8, LimitUSD: 10, Currency: "USD", Period: "lifetime:10", Timestamp: "2026-10-27T00:00:00Z"}
	// Success against a loopback httptest server.
	var gotBody []byte
	var gotCT, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotCT, gotUA = r.Header.Get("Content-Type"), r.Header.Get("User-Agent")
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("alert request carried credentials")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	if err := deliverBudgetAlert(context.Background(), payload, srv.URL, time.Second, config.EgressConfig{}); err != nil {
		t.Fatalf("loopback delivery failed: %v", err)
	}
	var decoded BudgetAlertPayload
	if err := json.Unmarshal(gotBody, &decoded); err != nil || decoded.BudgetID != "k1" || decoded.ThresholdPercent != 80 {
		t.Fatalf("delivered payload = %s, %v", gotBody, err)
	}
	if gotCT != "application/json" || gotUA == "" {
		t.Errorf("headers ct=%q ua=%q", gotCT, gotUA)
	}
	// Non-2xx is an error, not silent success.
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	if err := deliverBudgetAlert(context.Background(), payload, srv500.URL, time.Second, config.EgressConfig{}); err == nil {
		t.Error("500 delivery accepted")
	}
	// SSRF refusal: a non-loopback, non-allowlisted target is refused.
	if err := deliverBudgetAlert(context.Background(), payload, "http://example.com/hook", time.Second, config.EgressConfig{}); err == nil {
		t.Error("public webhook target accepted without allowlist")
	}
	// Malformed URL is refused.
	if err := deliverBudgetAlert(context.Background(), payload, "://bad", time.Second, config.EgressConfig{}); err == nil {
		t.Error("malformed webhook URL accepted")
	}
	// Redirect downgrade to a non-loopback http target is refused.
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/hook", http.StatusFound)
	}))
	defer redirecting.Close()
	if err := deliverBudgetAlert(context.Background(), payload, redirecting.URL, time.Second, config.EgressConfig{}); err == nil {
		t.Error("cross-origin redirect accepted")
	}
	// Zero timeout falls back to the default instead of hanging forever.
	if err := deliverBudgetAlert(context.Background(), payload, srv.URL, 0, config.EgressConfig{}); err != nil {
		t.Fatalf("zero-timeout delivery failed: %v", err)
	}
}
