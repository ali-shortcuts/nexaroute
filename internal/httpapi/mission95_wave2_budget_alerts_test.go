package httpapi

import (
	"bytes"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// wave2SyncBuffer is a bytes.Buffer safe for concurrent log writes and
// polling reads.
type wave2SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *wave2SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *wave2SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type wave2FailingBudgetStore struct{}

func (wave2FailingBudgetStore) Load() (map[string]int64, uint64, error) {
	return map[string]int64{}, 0, nil
}

func (wave2FailingBudgetStore) Save(map[string]int64, uint64) (uint64, error) {
	return 0, errors.New("wave2 store gone")
}

func TestWave2FileBudgetAlertLoadStatError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"fired":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stat under a regular file fails with ENOTDIR, not NotExist: the
	// loader must surface it instead of reporting an empty store.
	st := &fileBudgetAlertStore{path: filepath.Join(file, "child.json")}
	if _, _, err := st.Load(); err == nil || os.IsNotExist(err) {
		t.Fatalf("load err=%v want a non-NotExist stat error", err)
	}
}

func TestWave2FileBudgetAlertLoadSocketReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Stat succeeds on a socket (it is not a directory) but ReadFile
	// cannot open it: the loader must fail closed.
	if _, _, err := (&fileBudgetAlertStore{path: path}).Load(); err == nil {
		t.Fatal("socket load accepted")
	}
}

func TestWave2FileBudgetAlertSaveRenameOntoDir(t *testing.T) {
	dir := t.TempDir()
	// Renaming a temp file onto an existing directory fails: the save
	// must report the error and leave the directory intact.
	if _, err := (&fileBudgetAlertStore{path: dir}).Save(map[string]int64{"k": 1}, 0); err == nil {
		t.Fatal("save onto a directory accepted")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("directory after failed save: info=%v err=%v", info, err)
	}
}

func TestWave2PruneBudgetAlertFiredCap(t *testing.T) {
	now := time.Now().UnixNano()
	fired := make(map[string]int64, 10005)
	for i := 0; i < 10005; i++ {
		// Distinct, increasing timestamps: all survive the 400-day
		// cutoff, so only the oldest-first cap trim applies.
		fired["k"+itoa(i)] = now + int64(i)
	}
	pruneBudgetAlertFired(fired)
	if len(fired) != 10000 {
		t.Fatalf("pruned len=%d want 10000", len(fired))
	}
	for i := 0; i < 5; i++ {
		if _, ok := fired["k"+itoa(i)]; ok {
			t.Fatalf("oldest key k%d survived the cap trim", i)
		}
	}
	for i := 10000; i < 10005; i++ {
		if _, ok := fired["k"+itoa(i)]; !ok {
			t.Fatalf("newest key k%d was trimmed", i)
		}
	}
}

func TestWave2BudgetAlertDeliveryFailureLogged(t *testing.T) {
	s, cap := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "http://127.0.0.1:9/hook"
		cfg.BudgetAlerts.Thresholds = []float64{50}
	})
	a := s.budgetAlerterSnapshot()
	a.deliver = deliverBudgetAlert
	buf := &wave2SyncBuffer{}
	a.log = log.New(buf, "", 0)
	settleSpend(t, s, "vk-a", 10, 6) // 60%: crosses 50.
	s.checkBudgetAlerts("vk-a")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "budget_alert_delivery_failed=true") {
			if cap.count() != 0 {
				t.Fatalf("capture saw %d deliveries, want 0 (real deliver path)", cap.count())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("delivery failure never logged; log=%q", buf.String())
}

func TestWave2BudgetAlertPersistStoreFailureCounted(t *testing.T) {
	a := &budgetAlerter{fired: map[string]int64{"k": 1}, store: wave2FailingBudgetStore{}}
	a.persistBestEffort()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		n := a.stateFailures
		a.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("async store failure was not counted")
}

func TestWave2BudgetAlertsSnapshotDurableFile(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.BudgetAlerts.StatePath = filepath.Join(t.TempDir(), "state.json")
	s := testGateway(t, cfg)
	snap := s.budgetAlertsSnapshot()
	if snap["durable"] != "file" {
		t.Fatalf("durable=%v want file", snap["durable"])
	}
	if snap["state_writes_failed"] != int64(0) {
		t.Fatalf("state_writes_failed=%v want 0", snap["state_writes_failed"])
	}
}

func TestWave2BudgetAlertNilAlerterSilent(t *testing.T) {
	s, cap := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
		cfg.BudgetAlerts.Thresholds = []float64{50}
	})
	settleSpend(t, s, "vk-a", 10, 6) // 60%: crosses 50.
	s.runtimeMu.Lock()
	s.budgetAlerter = nil
	s.runtimeMu.Unlock()
	if s.budgetAlerterSnapshot() != nil {
		t.Fatal("alerter snapshot must be nil after clearing")
	}
	s.checkBudgetAlerts("vk-a")
	if cap.count() != 0 {
		t.Fatalf("deliveries=%d want 0 with no alerter", cap.count())
	}
}
