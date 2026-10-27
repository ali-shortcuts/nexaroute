package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Isolated restore verification (M4-3b, issue #438).
//
// `nexaroute restore --verify` performs the whole restore READ path —
// bundle structure and per-file SHA-256, config decryption/validation,
// audit-chain integrity, control-plane schema, master-key pairing and
// external-store ownership — without writing to a live installation.
// With `--into` it also materializes the bundle into a disposable
// directory the operator chooses, so the restored bytes are inspected
// in place rather than overwriting production.
//
// The result is a machine-readable report (format
// "nexaroute-restore-verify/1") whose per-check status is one of
// PASS, FAIL, BLOCKED or UNVERIFIED. Verification FAILS CLOSED: a
// bundle whose encrypted config cannot be decrypted, or whose audit
// chain does not verify, exits nonzero and never reports PASS.
const restoreVerifyFormat = "nexaroute-restore-verify/1"

const (
	statusPass       = "PASS"
	statusFail       = "FAIL"
	statusBlocked    = "BLOCKED"
	statusUnverified = "UNVERIFIED"
)

// verifyCheck is one named, timed verification step.
type verifyCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// restoreVerifyReport is the machine-readable evidence artifact of one
// isolated restore drill. It contains no secret material: only hashes,
// statuses, versions and timings.
type restoreVerifyReport struct {
	Format            string        `json:"format"`
	GeneratedAt       time.Time     `json:"generated_at"`
	Bundle            string        `json:"bundle"`
	BundleSHA256      string        `json:"bundle_sha256"`
	BundleBytes       int64         `json:"bundle_bytes"`
	BackupCreatedAt   time.Time     `json:"backup_created_at,omitempty"`
	BackupAgeSeconds  int64         `json:"backup_age_seconds"`
	GatewayVersion    string        `json:"gateway_version,omitempty"`
	MasterKeyIncluded bool          `json:"master_key_included"`
	MaterializedInto  string        `json:"materialized_into,omitempty"`
	Checks            []verifyCheck `json:"checks"`
	Status            string        `json:"status"`
	DurationMS        int64         `json:"total_duration_ms"`
}

// runRestoreVerify implements the isolated verification path. It returns
// a process exit code: 0 when nothing failed and nothing is blocked, 1
// when any check FAILed or was BLOCKED (the restore cannot be relied on
// yet), and 2 on usage errors. It never writes to the restore target.
func runRestoreVerify(bundlePath, configPath, intoDir, reportPath string, maxAge time.Duration, stdout, stderr io.Writer) int {
	if strings.TrimSpace(bundlePath) == "" {
		fmt.Fprintln(stderr, "nexaroute restore --verify: --bundle is required")
		return 2
	}
	if intoDir != "" {
		if err := prepareDisposableDir(intoDir); err != nil {
			fmt.Fprintf(stderr, "nexaroute restore --verify: %v\n", err)
			return 2
		}
	}

	report := restoreVerifyReport{
		Format:         restoreVerifyFormat,
		GeneratedAt:    time.Now().UTC(),
		Bundle:         filepath.Base(bundlePath),
		GatewayVersion: version,
		Checks:         []verifyCheck{},
	}
	start := time.Now()

	if sum, size, err := sha256File(bundlePath); err != nil {
		report.Checks = append(report.Checks, verifyCheck{Name: "bundle_readable", Status: statusFail, Detail: fsErrorClass(err)})
		report.Status = statusFail
		report.DurationMS = time.Since(start).Milliseconds()
		emitVerifyReport(report, reportPath, stdout, stderr)
		return 1
	} else {
		report.BundleSHA256 = sum
		report.BundleBytes = size
	}

	stage, manifest, payloads, err := readAndVerifyBundle(bundlePath)
	if err != nil {
		report.Checks = append(report.Checks, verifyCheck{Name: "bundle_integrity", Status: statusFail, Detail: "bundle failed format/hash verification; refusing to verify"})
		report.Status = statusFail
		report.DurationMS = time.Since(start).Milliseconds()
		emitVerifyReport(report, reportPath, stdout, stderr)
		return 1
	}
	defer func() { _ = os.RemoveAll(stage) }()
	report.Checks = append(report.Checks, verifyCheck{Name: "bundle_integrity", Status: statusPass, Detail: fmt.Sprintf("%d file(s) verified against the manifest", len(manifest.Files))})
	report.BackupCreatedAt = manifest.CreatedAt
	report.BackupAgeSeconds = int64(time.Since(manifest.CreatedAt).Seconds())
	report.MasterKeyIncluded = manifest.MasterKeyIncluded

	// Freshness is only asserted when the operator supplies a bound.
	freshStart := time.Now()
	report.Checks = append(report.Checks, checkFreshness(manifest, maxAge, freshStart))

	// Config decryption and load/validation (route usability is part of
	// config validation: route profiles are checked against pools,
	// providers and policies by the real loader).
	cfg, cfgStatus, cfgDetail := verifyConfigDecryption(stage, payloads, configPath)
	report.Checks = append(report.Checks, verifyCheck{Name: "config_decryption", Status: cfgStatus, Detail: cfgDetail})
	if cfgStatus == statusPass {
		report.Checks = append(report.Checks, verifyCheck{Name: "route_usability", Status: statusPass, Detail: "config loads and validates route profiles, pools and providers"})
	} else {
		report.Checks = append(report.Checks, verifyCheck{Name: "route_usability", Status: statusBlocked, Detail: "not evaluated: config did not load"})
	}

	// Master-key pairing: the bundle's recorded fingerprint against the
	// key material available on this host.
	report.Checks = append(report.Checks, checkMasterKeyPairing(manifest, payloads, configPath))

	// Audit-chain integrity from the bundled security store.
	report.Checks = append(report.Checks, checkBundledAuditChain(payloads))

	// Control-plane file-store schema compatibility.
	report.Checks = append(report.Checks, checkControlPlaneState(payloads))

	// External stores and video assets are operator-owned: the
	// application bundle does not snapshot an external database, Redis,
	// or video assets/jobs, so a restore cannot be claimed complete
	// while the config depends on them.
	report.Checks = append(report.Checks, checkExternalOwnership(cfg, cfgStatus))

	// Optional materialization into a disposable directory.
	if intoDir != "" {
		st := time.Now()
		if err := materializeBundle(intoDir, payloads); err != nil {
			report.Checks = append(report.Checks, verifyCheck{Name: "isolated_materialization", Status: statusFail, Detail: err.Error(), DurationMS: time.Since(st).Milliseconds()})
		} else {
			report.MaterializedInto = intoDir
			report.Checks = append(report.Checks, verifyCheck{Name: "isolated_materialization", Status: statusPass, Detail: "bundle materialized into a disposable directory (production targets untouched)", DurationMS: time.Since(st).Milliseconds()})
		}
	} else {
		report.Checks = append(report.Checks, verifyCheck{Name: "isolated_materialization", Status: statusUnverified, Detail: "no --into directory supplied; bundle verified in a private staging area only"})
	}

	report.Status = aggregateVerifyStatus(report.Checks)
	report.DurationMS = time.Since(start).Milliseconds()
	emitVerifyReport(report, reportPath, stdout, stderr)
	if report.Status == statusFail || report.Status == statusBlocked {
		return 1
	}
	return 0
}

func checkFreshness(manifest backupManifest, maxAge time.Duration, start time.Time) (c verifyCheck) {
	c = verifyCheck{Name: "bundle_freshness"}
	defer func() { c.DurationMS = time.Since(start).Milliseconds() }()
	if maxAge <= 0 {
		c.Status = statusUnverified
		c.Detail = "no --max-age bound given; freshness not asserted"
		return c
	}
	age := time.Since(manifest.CreatedAt)
	if age > maxAge {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("backup is %s old, exceeding the %s bound", age.Round(time.Second), maxAge)
		return c
	}
	c.Status = statusPass
	c.Detail = fmt.Sprintf("backup is %s old, within the %s bound", age.Round(time.Second), maxAge)
	return c
}

// verifyConfigDecryption stages the bundled config (paired with the best
// available key) and runs the real loader. It never returns secret
// material. A config with encrypted values that has no key available is
// BLOCKED, not PASS: verification cannot prove decryption.
func verifyConfigDecryption(stage string, payloads map[string][]byte, configPath string) (config.Config, string, string) {
	dir := filepath.Join(stage, "verify-config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return config.Config{}, statusFail, "cannot stage bundled config for verification"
	}
	stagedConfig := filepath.Join(dir, "config.json")
	if err := os.WriteFile(stagedConfig, payloads[backupEntryConfig], 0o600); err != nil {
		return config.Config{}, statusFail, "cannot stage bundled config for verification"
	}
	hasEnvelopes := strings.Contains(string(payloads[backupEntryConfig]), "nxs1:") || strings.Contains(string(payloads[backupEntryConfig]), "nxs2:")
	keyAvailable := false
	if key, ok := payloads[backupEntryKey]; ok {
		if err := os.WriteFile(stagedConfig+".key", key, 0o600); err != nil {
			return config.Config{}, statusFail, "cannot stage bundled master key for verification"
		}
		keyAvailable = true
	} else if b, err := os.ReadFile(configPath + ".key"); err == nil {
		if err := os.WriteFile(stagedConfig+".key", b, 0o600); err != nil {
			return config.Config{}, statusFail, "cannot stage the host master key for verification"
		}
		keyAvailable = true
	}
	if !keyAvailable {
		keyAvailable = envKeyMaterialPresent()
	}

	cfg, err := config.LoadBase(stagedConfig)
	if err == nil {
		if hasEnvelopes {
			return cfg, statusPass, "encrypted values decrypted and config validated"
		}
		return cfg, statusPass, "config parses and validates"
	}
	if !keyAvailable {
		return config.Config{}, statusBlocked, "encrypted config with no key material available on this host; cannot verify decryption (restore would fail closed)"
	}
	return config.Config{}, statusFail, "key material was available but the config did not decrypt/validate; refusing to treat the backup as restorable"
}

// envKeyMaterialPresent reports whether any environment-provided key
// source is configured. It never reads or returns the key.
func envKeyMaterialPresent() bool {
	if os.Getenv("NEXAROUTE_MASTER_KEY") != "" || os.Getenv("NEXAROUTE_MASTER_KEY_FILE") != "" {
		return true
	}
	kp := strings.TrimSpace(os.Getenv("NEXAROUTE_KEY_PROVIDER"))
	return kp != "" && kp != "file"
}

// availableMasterKeyBytes returns the key material a restore could use
// on this host and a non-secret label describing its source. It is only
// used to compare against the manifest fingerprint; the bytes are never
// logged or written into the report.
func availableMasterKeyBytes(payloads map[string][]byte, configPath string) ([]byte, string) {
	if k, ok := payloads[backupEntryKey]; ok {
		return k, "bundled"
	}
	if v := os.Getenv("NEXAROUTE_MASTER_KEY"); v != "" {
		if b, err := base64.StdEncoding.DecodeString(v); err == nil {
			return b, "environment:NEXAROUTE_MASTER_KEY"
		}
	}
	if f := os.Getenv("NEXAROUTE_MASTER_KEY_FILE"); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			return b, "environment:NEXAROUTE_MASTER_KEY_FILE"
		}
	}
	if b, err := os.ReadFile(configPath + ".key"); err == nil {
		return b, "host:" + filepath.Base(configPath) + ".key"
	}
	return nil, ""
}

func checkMasterKeyPairing(manifest backupManifest, payloads map[string][]byte, configPath string) (c verifyCheck) {
	c = verifyCheck{Name: "master_key_pairing"}
	start := time.Now()
	defer func() { c.DurationMS = time.Since(start).Milliseconds() }()
	if manifest.MasterKeySHA256 == "" {
		c.Status = statusUnverified
		c.Detail = "backup recorded no file-backed master key fingerprint (external key provider or plaintext config)"
		return c
	}
	key, source := availableMasterKeyBytes(payloads, configPath)
	if len(key) == 0 {
		c.Status = statusBlocked
		c.Detail = "no master key available on this host to compare against the backup fingerprint"
		return c
	}
	if sha256Hex(key) != manifest.MasterKeySHA256 {
		c.Status = statusFail
		c.Detail = "the available master key (" + source + ") does not match the backup fingerprint; encrypted values would not decrypt"
		return c
	}
	c.Status = statusPass
	c.Detail = "master key from " + source + " matches the backup fingerprint"
	return c
}

func checkBundledAuditChain(payloads map[string][]byte) (c verifyCheck) {
	c = verifyCheck{Name: "audit_chain"}
	start := time.Now()
	defer func() { c.DurationMS = time.Since(start).Milliseconds() }()
	payload, ok := payloads[backupEntrySecurity]
	if !ok {
		c.Status = statusUnverified
		c.Detail = "no security store was present at backup time"
		return c
	}
	dir, err := os.MkdirTemp("", "nexaroute-verify-audit-*")
	if err != nil {
		c.Status = statusFail
		c.Detail = "cannot stage the bundled security store for verification"
		return c
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "security.db")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		c.Status = statusFail
		c.Detail = "cannot stage the bundled security store for verification"
		return c
	}
	store, err := securitystore.OpenReadOnly(path)
	if err != nil {
		c.Status = statusFail
		c.Detail = "bundled security store does not open (corrupt or incompatible schema)"
		return c
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := store.VerifyAuditChain(ctx)
	if err != nil {
		c.Status = statusFail
		c.Detail = "audit-chain verification errored; refusing to treat the backup as restorable"
		return c
	}
	if !report.Verified {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("audit chain FAILED verification (%d problem(s)); refusing to treat the backup as restorable", len(report.Problems))
		return c
	}
	c.Status = statusPass
	c.Detail = fmt.Sprintf("audit chain verified: %d record(s), %d chained", report.TotalRecords, report.ChainedRecords)
	return c
}

// checkControlPlaneState validates the bundled file-store schema. It
// rejects an unknown/older schema before a restore could install state
// the current build refuses to load.
func checkControlPlaneState(payloads map[string][]byte) (c verifyCheck) {
	c = verifyCheck{Name: "control_plane_state"}
	start := time.Now()
	defer func() { c.DurationMS = time.Since(start).Milliseconds() }()
	payload, ok := payloads[backupEntryControl]
	if !ok {
		c.Status = statusUnverified
		c.Detail = "no file-backend control-plane state was present at backup time"
		return c
	}
	var state struct {
		Schema    int             `json:"schema"`
		Snapshots json.RawMessage `json:"snapshots"`
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		c.Status = statusFail
		c.Detail = "bundled control-plane state is not valid JSON"
		return c
	}
	if state.Schema != 1 || state.Snapshots == nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("bundled control-plane schema %d is not supported by this build (migration required before restore)", state.Schema)
		return c
	}
	c.Status = statusPass
	c.Detail = "control-plane file-store schema is compatible with this build"
	return c
}

// checkExternalOwnership makes the recovery boundary explicit: an
// application backup never snapshots an external database, Redis, or
// video assets/jobs. A config that depends on them cannot be restored
// to a working installation from the bundle alone.
func checkExternalOwnership(cfg config.Config, cfgStatus string) (c verifyCheck) {
	c = verifyCheck{Name: "external_store_ownership"}
	start := time.Now()
	defer func() { c.DurationMS = time.Since(start).Milliseconds() }()
	if cfgStatus != statusPass {
		c.Status = statusUnverified
		c.Detail = "not evaluated: config did not load"
		return c
	}
	var external []string
	if cfg.ControlPlane.Enabled {
		switch cfg.ControlPlane.Backend {
		case "postgres":
			external = append(external, "control-plane postgres")
		case "redis":
			external = append(external, "control-plane redis")
		}
	}
	switch cfg.Admin.SecurityStoreBackend {
	case "postgres":
		external = append(external, "security store postgres")
	case "redis":
		external = append(external, "security store redis")
	}
	if cfg.Video.Enabled {
		external = append(external, "video assets/jobs")
	}
	if len(external) == 0 {
		c.Status = statusPass
		c.Detail = "no external store or video assets are referenced; the bundle is self-contained"
		return c
	}
	c.Status = statusBlocked
	c.Detail = "the bundle does NOT snapshot external state (" + strings.Join(external, ", ") + "); restore them with the database's own backup and verify separately"
	return c
}

func aggregateVerifyStatus(checks []verifyCheck) string {
	status := statusPass
	for _, c := range checks {
		switch c.Status {
		case statusFail:
			return statusFail
		case statusBlocked:
			status = statusBlocked
		case statusUnverified:
			if status == statusPass {
				status = statusUnverified
			}
		}
	}
	return status
}

// prepareDisposableDir refuses to materialize into a directory that is
// not private, is a symlink, or already holds entries: `--into` must
// never overwrite production-like state.
func prepareDisposableDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return fmt.Errorf("inspect --into directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("--into %s is not a real directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect --into directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("--into %s is not empty; refusing to materialize a restore over existing files (choose a disposable directory)", dir)
	}
	return nil
}

// materializeBundle writes the bundle payloads under a disposable
// directory using fixed names. It is a read of the bundle, not a
// production restore: no journal, no rollback, and the operator-chosen
// directory must be empty.
func materializeBundle(dir string, payloads map[string][]byte) error {
	names := map[string]string{
		backupEntryConfig:   "config.json",
		backupEntrySecurity: "security.db",
		backupEntryControl:  "controlplane.json",
		backupEntryKey:      "config.json.key",
	}
	for _, entry := range []string{backupEntryConfig, backupEntrySecurity, backupEntryControl, backupEntryKey} {
		payload, ok := payloads[entry]
		if !ok {
			continue
		}
		target := filepath.Join(dir, names[entry])
		if err := writeFileAtomic(target, payload, 0o600); err != nil {
			return fmt.Errorf("materialize %s: %w", names[entry], err)
		}
		got, err := os.ReadFile(target)
		if err != nil || sha256Hex(got) != sha256Hex(payload) {
			return fmt.Errorf("materialized %s failed hash verification", names[entry])
		}
	}
	return nil
}

// emitVerifyReport prints the human-readable summary and, when a path
// is supplied, writes the machine-readable JSON report (mode 0600).
func emitVerifyReport(report restoreVerifyReport, reportPath string, stdout, stderr io.Writer) {
	for _, c := range report.Checks {
		line := fmt.Sprintf("%s %s", c.Status, c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintf(stdout, "restore verify status: %s (%dms)\n", report.Status, report.DurationMS)
	if reportPath != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "nexaroute restore --verify: cannot encode report: %v\n", err)
			return
		}
		if err := writeFileAtomic(reportPath, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintf(stderr, "nexaroute restore --verify: cannot write report: %v\n", err)
			return
		}
		fmt.Fprintf(stdout, "restore verify report written to %s\n", reportPath)
	}
}

// sha256File streams a file through SHA-256 so an evidence hash of a
// multi-gigabyte bundle never has to be buffered.
func sha256File(path string) (string, int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = fh.Close() }()
	info, err := fh.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), info.Size(), nil
}
