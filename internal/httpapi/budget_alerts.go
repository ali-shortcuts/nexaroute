package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/egress"
)

// Budget webhook alerts. When a virtual-key budget's RECORDED spend
// crosses a configured threshold, an operator-configured webhook
// endpoint is notified asynchronously.
//
// Guarantee (honest): exactly once per (budget, threshold, budget
// period) WITHIN the gateway process lifetime. Across restarts the
// fired marks survive only where a durable store is configured:
// budget_alerts.state_path (synchronous file write) or a durable
// control-plane backend wired via SetBudgetAlertStore (best-effort
// background write). With the in-memory default a restart may re-fire
// alerts for the same period. Spend itself is runtime state and resets
// on restart (documented with the budget backend), so a persisted
// fired-set also means a post-restart crossing in the SAME period
// stays silent — exactly-once per period, not at-least-once.
//
// Delivery never blocks or fails the data plane: the request path only
// marks the alert and queues a goroutine; transport timeouts, refused
// SSRF targets and non-2xx responses are logged and dropped. Delivery
// is at-most-once per period: a failed attempt is NOT retried later in
// the same period.

// BudgetAlertPayload is the webhook JSON contract. It carries budget
// identity and spend figures only — never secrets, credentials, key
// hashes, or request content.
type BudgetAlertPayload struct {
	BudgetID         string  `json:"budget_id"`
	BudgetName       string  `json:"budget_name,omitempty"`
	ThresholdPercent float64 `json:"threshold_percent"`
	SpendUSD         float64 `json:"spend_usd"`
	LimitUSD         float64 `json:"limit_usd"`
	Currency         string  `json:"currency"`
	Period           string  `json:"period"`
	Timestamp        string  `json:"timestamp"`
}

// budgetAlertNamespace is the control-plane namespace used when the
// gateway wires its durable control-plane store into the alerter.
const budgetAlertNamespace = "budgets/alerts"

const (
	budgetAlertStateVersion = 1
	// maxBudgetAlertFired bounds the in-memory + persisted fired-set so
	// rotated key IDs cannot grow it without bound.
	maxBudgetAlertFired = 10000
	maxBudgetAlertDoc   = 1 << 20
)

// ErrBudgetAlertStoreNotFound reports a durable store with no record yet.
var ErrBudgetAlertStoreNotFound = errors.New("budget alert store: not found")

// ErrBudgetAlertStoreConflict reports a concurrent-modification conflict.
var ErrBudgetAlertStoreConflict = errors.New("budget alert store: conflict")

// BudgetAlertSnapshotIO is the minimal durable namespace read/write
// contract. cmd/gateway adapts controlplane.Manager to it, so the
// alerter deduplicates across restarts on file, postgres and redis
// control-plane backends without importing them here.
type BudgetAlertSnapshotIO interface {
	Read(ctx context.Context) (payload []byte, revision uint64, err error)
	Write(ctx context.Context, payload []byte, expectedRevision uint64) (revision uint64, err error)
}

// BudgetAlertStore persists fired alert marks.
type BudgetAlertStore interface {
	Load() (fired map[string]int64, revision uint64, err error)
	Save(fired map[string]int64, expectedRevision uint64) (revision uint64, err error)
}

// budgetAlertStateDoc is the persisted document shape (file + snapshot IO).
type budgetAlertStateDoc struct {
	Version int              `json:"version"`
	Fired   map[string]int64 `json:"fired,omitempty"`
}

func marshalBudgetAlertDoc(fired map[string]int64) ([]byte, error) {
	return json.Marshal(budgetAlertStateDoc{Version: budgetAlertStateVersion, Fired: fired})
}

func parseBudgetAlertDoc(data []byte) (map[string]int64, error) {
	if len(data) == 0 {
		return map[string]int64{}, nil
	}
	if len(data) > maxBudgetAlertDoc {
		return nil, fmt.Errorf("budget alert state exceeds %d bytes", maxBudgetAlertDoc)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc budgetAlertStateDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid budget alert state: %w", err)
	}
	if doc.Version != budgetAlertStateVersion {
		return nil, fmt.Errorf("unsupported budget alert state version %d", doc.Version)
	}
	if doc.Fired == nil {
		return map[string]int64{}, nil
	}
	if len(doc.Fired) > maxBudgetAlertFired {
		return nil, fmt.Errorf("budget alert state exceeds %d entries", maxBudgetAlertFired)
	}
	return doc.Fired, nil
}

// fileBudgetAlertStore is an atomically written, mode-0600 JSON document
// (budget_alerts.state_path), mirroring the evaluation state-file pattern.
type fileBudgetAlertStore struct {
	mu   sync.Mutex
	path string
}

// NewFileBudgetAlertStore returns a file-backed fired-mark store.
func NewFileBudgetAlertStore(path string) BudgetAlertStore {
	return &fileBudgetAlertStore{path: path}
}

func (f *fileBudgetAlertStore) Load() (map[string]int64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int64{}, 0, nil
		}
		return nil, 0, err
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("%s is a directory", f.path)
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, 0, err
	}
	fired, err := parseBudgetAlertDoc(data)
	if err != nil {
		return nil, 0, err
	}
	return fired, 0, nil
}

func (f *fileBudgetAlertStore) Save(fired map[string]int64, _ uint64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := marshalBudgetAlertDoc(fired)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(f.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return 0, err
		}
	}
	tmp, err := os.CreateTemp(dir, ".budget-alerts-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return 0, err
	}
	return 0, nil
}

// ioBudgetAlertStore adapts a BudgetAlertSnapshotIO (control-plane
// namespace) to BudgetAlertStore with one conflict retry.
type ioBudgetAlertStore struct {
	io BudgetAlertSnapshotIO
}

// NewIOBudgetAlertStore wraps a snapshot IO handle (e.g. a
// control-plane namespace) as a fired-mark store.
func NewIOBudgetAlertStore(io BudgetAlertSnapshotIO) BudgetAlertStore {
	return &ioBudgetAlertStore{io: io}
}

func (s *ioBudgetAlertStore) Load() (map[string]int64, uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, rev, err := s.io.Read(ctx)
	if err != nil {
		if errors.Is(err, ErrBudgetAlertStoreNotFound) {
			return map[string]int64{}, 0, nil
		}
		return nil, 0, err
	}
	fired, err := parseBudgetAlertDoc(data)
	if err != nil {
		return nil, 0, err
	}
	return fired, rev, nil
}

func (s *ioBudgetAlertStore) Save(fired map[string]int64, expectedRevision uint64) (uint64, error) {
	data, err := marshalBudgetAlertDoc(fired)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rev, err := s.io.Write(ctx, data, expectedRevision)
	if err == nil {
		return rev, nil
	}
	if !errors.Is(err, ErrBudgetAlertStoreConflict) {
		return 0, err
	}
	// One conflict retry: re-read the winner, merge our marks (fired
	// marks are add-only, so a union cannot un-fire anything), and
	// write once more under the fresh revision.
	data2, rev2, rerr := s.io.Read(ctx)
	if rerr != nil && !errors.Is(rerr, ErrBudgetAlertStoreNotFound) {
		return 0, err
	}
	merged := map[string]int64{}
	if other, perr := parseBudgetAlertDoc(data2); perr == nil {
		for k, v := range other {
			merged[k] = v
		}
	}
	for k, v := range fired {
		merged[k] = v
	}
	mergedData, merr := marshalBudgetAlertDoc(merged)
	if merr != nil {
		return 0, merr
	}
	return s.io.Write(ctx, mergedData, rev2)
}

// budgetAlerter tracks fired (budget, threshold, period) marks and
// delivers webhook notifications asynchronously.
type budgetAlerter struct {
	mu            sync.Mutex
	fired         map[string]int64
	store         BudgetAlertStore
	storeRev      uint64
	statePath     string
	fileStore     *fileBudgetAlertStore
	now           func() time.Time
	deliver       func(ctx context.Context, payload BudgetAlertPayload, webhookURL string, timeout time.Duration, egressCfg config.EgressConfig) error
	stateFailures int64
	log           *log.Logger
}

func newBudgetAlerter(cfg config.Config, l *log.Logger) *budgetAlerter {
	a := &budgetAlerter{
		fired:   map[string]int64{},
		now:     time.Now,
		deliver: deliverBudgetAlert,
		log:     l,
	}
	a.reconfigure(cfg)
	return a
}

func (a *budgetAlerter) printf(format string, args ...any) {
	if a.log != nil {
		a.log.Printf(format, args...)
	}
}

// reconfigure picks up alert policy + state-path changes on config
// reload. Fired marks are never dropped by a reload: a state-path
// change MERGES the newly loaded marks into the live set.
func (a *budgetAlerter) reconfigure(cfg config.Config) {
	path := strings.TrimSpace(cfg.BudgetAlerts.StatePath)
	a.mu.Lock()
	defer a.mu.Unlock()
	if path == a.statePath {
		return
	}
	a.statePath = path
	if path == "" {
		a.fileStore = nil
		return
	}
	a.fileStore = &fileBudgetAlertStore{path: path}
	fired, _, err := a.fileStore.Load()
	if err != nil {
		a.stateFailures++
		a.printf("budget_alert_state_unusable=true path=%q err=%v", path, err)
		return
	}
	for k, v := range fired {
		if _, ok := a.fired[k]; !ok {
			a.fired[k] = v
		}
	}
	pruneBudgetAlertFired(a.fired)
}

// SetBudgetAlertStore wires a durable store (e.g. the control-plane
// namespace on file/postgres/redis backends) and merges its marks into
// the live set. A nil store reverts to memory/file only.
func (s *Server) SetBudgetAlertStore(store BudgetAlertStore) {
	a := s.budgetAlerterSnapshot()
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store = store
	a.storeRev = 0
	if store == nil {
		return
	}
	fired, rev, err := store.Load()
	if err != nil {
		a.stateFailures++
		a.printf("budget_alert_store_unusable=true err=%v", err)
		return
	}
	for k, v := range fired {
		if _, ok := a.fired[k]; !ok {
			a.fired[k] = v
		}
	}
	a.storeRev = rev
	pruneBudgetAlertFired(a.fired)
}

func (s *Server) budgetAlerterSnapshot() *budgetAlerter {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.budgetAlerter
}

// budgetAlertDedupKey is the exactly-once identity: one fire per
// (budget, threshold, budget period).
func budgetAlertDedupKey(budgetID, period string, threshold float64) string {
	return budgetID + "\x00" + period + "\x00" + fmt.Sprintf("%g", threshold)
}

// budgetAlertPeriodID maps a budget's period policy to the current
// period label. "" (default) is the lifetime of the current limit
// value: editing budget_usd starts a new period and re-arms fired
// thresholds. "monthly" is the UTC calendar month.
func budgetAlertPeriodID(period string, limit float64, now time.Time) string {
	if period == "monthly" {
		return now.UTC().Format("2006-01")
	}
	return "lifetime:" + fmt.Sprintf("%g", limit)
}

// pruneBudgetAlertFired keeps the fired-set bounded: entries older than
// 400 days go first (monthly periods rotate), then oldest-first down to
// the cap. Callers hold a.mu.
func pruneBudgetAlertFired(fired map[string]int64) {
	cutoff := time.Now().Add(-400 * 24 * time.Hour).UnixNano()
	for k, v := range fired {
		if v < cutoff {
			delete(fired, k)
		}
	}
	for len(fired) > maxBudgetAlertFired {
		oldest := ""
		var oldestAt int64
		first := true
		for k, v := range fired {
			if first || v < oldestAt {
				oldest, oldestAt, first = k, v, false
			}
		}
		delete(fired, oldest)
	}
}

func (a *budgetAlerter) snapshotFired() map[string]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int64, len(a.fired))
	for k, v := range a.fired {
		out[k] = v
	}
	return out
}

// checkBudgetAlerts evaluates a budget's recorded spend against its
// effective threshold ladder and fires newly crossed thresholds. It
// runs on the usage-settle path and must never block, fail, or delay
// the data plane: marking is in-memory and synchronous, the file
// write is a small local atomic rename, the remote-store write (when
// wired) goes to a background goroutine, and delivery is always
// asynchronous.
func (s *Server) checkBudgetAlerts(keyID string) {
	now := time.Now()
	cfg := s.currentConfig()
	var key *config.VirtualKeyConfig
	for i := range cfg.ClientAuth.VirtualKeys {
		if cfg.ClientAuth.VirtualKeys[i].ID == keyID {
			key = &cfg.ClientAuth.VirtualKeys[i]
			break
		}
	}
	if key == nil || key.BudgetUSD <= 0 {
		return
	}
	webhookURL := strings.TrimSpace(key.BudgetWebhookURL)
	if webhookURL == "" {
		webhookURL = strings.TrimSpace(cfg.BudgetAlerts.WebhookURL)
	}
	if webhookURL == "" {
		return // alerts disabled: the zero-service default stays silent
	}
	thresholds := key.BudgetAlertThresholds
	if len(thresholds) == 0 {
		thresholds = cfg.BudgetAlerts.Thresholds
	}
	if len(thresholds) == 0 {
		thresholds = append([]float64(nil), config.DefaultBudgetAlertThresholds...)
	}
	if s.budgets == nil {
		return
	}
	committed, _ := s.budgets.Snapshot(keyID, now)
	a := s.budgetAlerterSnapshot()
	if a == nil {
		return
	}
	period := budgetAlertPeriodID(strings.ToLower(strings.TrimSpace(key.BudgetPeriod)), key.BudgetUSD, a.now())
	timeout := time.Duration(cfg.BudgetAlerts.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	for _, t := range thresholds {
		if committed+1e-9 < key.BudgetUSD*t/100 {
			continue
		}
		dedup := budgetAlertDedupKey(keyID, period, t)
		payload := BudgetAlertPayload{
			BudgetID:         keyID,
			BudgetName:       key.Name,
			ThresholdPercent: t,
			SpendUSD:         committed,
			LimitUSD:         key.BudgetUSD,
			Currency:         "USD",
			Period:           period,
			Timestamp:        a.now().UTC().Format(time.RFC3339),
		}
		if !a.markFired(dedup) {
			continue
		}
		a.persistBestEffort()
		egressCfg := cfg.Egress
		deliver := a.deliver
		go func(p BudgetAlertPayload, target string) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := deliver(ctx, p, target, timeout, egressCfg); err != nil {
				a.printf("budget_alert_delivery_failed=true budget=%q threshold=%g period=%q err=%v", p.BudgetID, p.ThresholdPercent, p.Period, err)
			}
		}(payload, webhookURL)
	}
}

// markFired records a dedup key. It reports false when the key already
// fired (exactly-once per budget/threshold/period in this process).
func (a *budgetAlerter) markFired(dedup string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.fired[dedup]; ok {
		return false
	}
	a.fired[dedup] = a.now().UnixNano()
	pruneBudgetAlertFired(a.fired)
	return true
}

// persistBestEffort writes fired marks to the configured durable
// stores. The file write (when configured) is synchronous and local;
// the snapshot-IO write (control-plane namespace, possibly remote)
// runs in the background so a slow database can never stall the data
// plane. Failures are counted for the admin snapshot and logged.
func (a *budgetAlerter) persistBestEffort() {
	a.mu.Lock()
	fileStore := a.fileStore
	store := a.store
	rev := a.storeRev
	fired := make(map[string]int64, len(a.fired))
	for k, v := range a.fired {
		fired[k] = v
	}
	a.mu.Unlock()
	if fileStore != nil {
		if _, err := fileStore.Save(fired, 0); err != nil {
			a.mu.Lock()
			a.stateFailures++
			a.mu.Unlock()
			a.printf("budget_alert_state_write_failed=true err=%v", err)
		}
	}
	if store != nil {
		go func() {
			if _, err := store.Save(fired, rev); err != nil {
				a.mu.Lock()
				a.stateFailures++
				a.mu.Unlock()
				a.printf("budget_alert_store_write_failed=true err=%v", err)
				return
			}
		}()
	}
}

// parseBudgetAlertCIDRs converts operator egress CIDR strings into
// prefixes. Bare IPs become single-host prefixes; invalid entries are
// ignored here because config validation already rejects them.
func parseBudgetAlertCIDRs(cidrs []string) []netip.Prefix {
	var out []netip.Prefix
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(c); err == nil {
			out = append(out, prefix)
		} else if addr, err := netip.ParseAddr(c); err == nil {
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return out
}

// deliverBudgetAlert POSTs one alert payload through the project's
// egress/SSRF policy machinery: operator deny-hosts/deny-CIDRs take
// precedence, a configured host allowlist gates the target, loopback
// http is allowed only for loopback-spelled hosts, redirects cannot
// downgrade to non-loopback http, and no credentials, cookies, or
// request content ever leave the gateway on this path.
func deliverBudgetAlert(ctx context.Context, payload BudgetAlertPayload, webhookURL string, timeout time.Duration, egressCfg config.EgressConfig) error {
	policy := egress.Policy{
		AllowedHosts:  append([]string(nil), egressCfg.AllowHosts...),
		AllowLoopback: true,
		DenyHosts:     append([]string(nil), egressCfg.DenyHosts...),
		DenyCIDRs:     parseBudgetAlertCIDRs(egressCfg.DenyCIDRs),
	}
	tr, err := policy.NewTransport(webhookURL, "")
	if err != nil {
		return fmt.Errorf("budget webhook refused: %w", err)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("budget webhook: stopped after 5 redirects")
			}
			target := req.URL.String()
			u, perr := url.Parse(target)
			if perr != nil || u.Hostname() == "" {
				return fmt.Errorf("budget webhook: redirect target is malformed")
			}
			if u.Scheme != "https" && !(u.Scheme == "http" && isBudgetWebhookRedirectLoopback(u.Hostname())) {
				return fmt.Errorf("budget webhook: redirect target must use https (http only for loopback hosts)")
			}
			return nil
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("budget webhook: cannot encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("budget webhook: cannot build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nexaroute-budget-alert/1.0")
	// No Authorization, no cookies, no forwarded client headers: the
	// payload carries no secrets, so nothing secret leaves here.
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("budget webhook delivery failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("budget webhook delivery failed: status %d", resp.StatusCode)
	}
	return nil
}

// isBudgetWebhookRedirectLoopback mirrors the loopback spelling rule
// for redirect targets (localhost, *.localhost, loopback literals).
func isBudgetWebhookRedirectLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return ip.WithZone("").Unmap().IsLoopback()
	}
	return false
}

// budgetAlertsSnapshot renders the admin-visible alert state: policy,
// per-budget spend vs. ladder, and fired marks for the current period.
// It exposes operator configuration and spend figures only — no
// secrets, key hashes, or request content.
func (s *Server) budgetAlertsSnapshot() map[string]any {
	cfg := s.currentConfig()
	a := s.budgetAlerterSnapshot()
	now := time.Now()
	var nowFn func() time.Time
	if a != nil {
		nowFn = a.now
	} else {
		nowFn = time.Now
	}
	budgets := make([]map[string]any, 0, len(cfg.ClientAuth.VirtualKeys))
	for _, k := range cfg.ClientAuth.VirtualKeys {
		if k.BudgetUSD <= 0 {
			continue
		}
		thresholds := k.BudgetAlertThresholds
		if len(thresholds) == 0 {
			thresholds = cfg.BudgetAlerts.Thresholds
		}
		if len(thresholds) == 0 {
			thresholds = append([]float64(nil), config.DefaultBudgetAlertThresholds...)
		}
		period := budgetAlertPeriodID(strings.ToLower(strings.TrimSpace(k.BudgetPeriod)), k.BudgetUSD, nowFn())
		var spend float64
		if s.budgets != nil {
			committed, _ := s.budgets.Snapshot(k.ID, now)
			spend = committed
		}
		fired := []float64{}
		if a != nil {
			live := a.snapshotFired()
			for _, t := range thresholds {
				if _, ok := live[budgetAlertDedupKey(k.ID, period, t)]; ok {
					fired = append(fired, t)
				}
			}
		}
		target := strings.TrimSpace(k.BudgetWebhookURL)
		if target == "" {
			target = strings.TrimSpace(cfg.BudgetAlerts.WebhookURL)
		}
		budgets = append(budgets, map[string]any{
			"id": k.ID, "name": k.Name,
			"limit_usd": k.BudgetUSD, "spend_usd": spend,
			"currency": "USD", "period": period,
			"thresholds": thresholds, "fired": fired,
			"webhook_configured": target != "",
		})
	}
	enabled := strings.TrimSpace(cfg.BudgetAlerts.WebhookURL) != ""
	for _, k := range cfg.ClientAuth.VirtualKeys {
		if strings.TrimSpace(k.BudgetWebhookURL) != "" {
			enabled = true
			break
		}
	}
	durable := "memory"
	if a != nil {
		a.mu.Lock()
		hasStore := a.store != nil
		hasFile := a.fileStore != nil
		failures := a.stateFailures
		a.mu.Unlock()
		switch {
		case hasStore:
			durable = "controlplane"
		case hasFile:
			durable = "file"
		}
		return map[string]any{
			"enabled":             enabled,
			"webhook_configured":  strings.TrimSpace(cfg.BudgetAlerts.WebhookURL) != "",
			"thresholds":          cfg.BudgetAlerts.Thresholds,
			"timeout_seconds":     cfg.BudgetAlerts.TimeoutSeconds,
			"durable":             durable,
			"state_writes_failed": failures,
			"budgets":             budgets,
		}
	}
	return map[string]any{
		"enabled":             enabled,
		"webhook_configured":  strings.TrimSpace(cfg.BudgetAlerts.WebhookURL) != "",
		"thresholds":          cfg.BudgetAlerts.Thresholds,
		"timeout_seconds":     cfg.BudgetAlerts.TimeoutSeconds,
		"durable":             durable,
		"state_writes_failed": 0,
		"budgets":             budgets,
	}
}
