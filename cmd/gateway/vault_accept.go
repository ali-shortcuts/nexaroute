package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

// vault-accept is the opt-in acceptance harness for the shipped Vault KV v2
// key-provider helper. It has no built-in Vault address and never prints or
// stores token values or key bytes. A run is only allowed with --confirm-live
// and an explicitly named dedicated KV v2 data path that is empty before the
// run. Dev-mode Vault (in-memory storage) is rejected unless --allow-dev is
// supplied, and even then the report is labelled evidence_class=dev so it
// cannot be mistaken for production or HA evidence.
const (
	vaultAcceptSchema     = "nexaroute.vault-acceptance.v1"
	vaultAcceptMaxTimeout = 60 * time.Second
	vaultAcceptMaxOverall = 5 * time.Minute
	vaultAcceptMaxBody    = 1 << 20
)

type vaultAcceptReport struct {
	Schema        string             `json:"schema"`
	Gateway       string             `json:"gateway_version,omitempty"`
	GeneratedAt   string             `json:"generated_at"`
	Mode          string             `json:"mode"`
	EvidenceClass string             `json:"evidence_class"`
	Outcome       string             `json:"outcome"`
	Target        vaultAcceptTarget  `json:"target"`
	Vault         vaultAcceptVault   `json:"vault"`
	Bounds        vaultAcceptBounds  `json:"bounds"`
	Checks        []vaultAcceptCheck `json:"checks"`
	Totals        vaultAcceptTotals  `json:"totals"`
	Limitations   []string           `json:"limitations,omitempty"`
	Warnings      []string           `json:"warnings,omitempty"`
}

type vaultAcceptTarget struct {
	Addr       string `json:"addr"`
	Ref        string `json:"ref"`
	AuthMethod string `json:"auth_method"`
	TokenEnv   string `json:"token_env"`
	Helper     bool   `json:"helper_configured"`
}

type vaultAcceptVault struct {
	Version          string   `json:"version,omitempty"`
	StorageType      string   `json:"storage_type,omitempty"`
	HAEnabled        bool     `json:"ha_enabled"`
	HAKnown          bool     `json:"ha_known"`
	Sealed           bool     `json:"sealed"`
	Standby          bool     `json:"standby"`
	DevMode          bool     `json:"dev_mode"`
	Policies         []string `json:"policies,omitempty"`
	TokenTTLSeconds  int64    `json:"token_ttl_seconds,omitempty"`
	TokenRenewable   bool     `json:"token_renewable,omitempty"`
	TokenMetadataOK  bool     `json:"token_metadata_known"`
	BaselineObserved bool     `json:"baseline_observed"`
}

type vaultAcceptBounds struct {
	CallTimeoutSeconds    float64 `json:"call_timeout_seconds"`
	OverallTimeoutSeconds float64 `json:"overall_timeout_seconds"`
	// HelperTimeoutSeconds is the per-request bound actually handed to the
	// helper script via VAULT_HELPER_TIMEOUT (see vaultAcceptHelperTimeout).
	HelperTimeoutSeconds int64 `json:"helper_timeout_seconds"`
}

type vaultAcceptCheck struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Selected   bool   `json:"selected"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`

	vault vaultAcceptVault
}

type vaultAcceptTotals struct {
	Checks  int `json:"checks"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Blocked int `json:"blocked"`
	NotRun  int `json:"not_run"`
}

// newVaultAcceptClient builds the Vault HTTP client. Redirects are
// refused (http.ErrUseLastResponse, same posture as the Transit
// provider): a redirect target must never receive the operator's
// X-Vault-Token, and a 3xx response surfaces as a failed check
// instead of being followed silently. Every vaultHTTP caller treats a
// non-2xx status as failure, so refusing redirects is fail-closed.
func newVaultAcceptClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func runVaultAccept(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vault-accept", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addrEnv := fs.String("addr-env", "VAULT_ADDR", "environment variable containing the Vault base URL")
	tokenEnv := fs.String("token-env", "VAULT_TOKEN", "environment variable containing the Vault token")
	ref := fs.String("ref", "", "empty, dedicated KV v2 data path (for example secret/data/nexaroute/acceptance/master-key)")
	helper := fs.String("helper", "scripts/key-provider-vault-kv.sh", "path to the Vault KV key-provider helper")
	outPath := fs.String("out", "", "redacted JSON report path")
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout (max 60s); when explicitly set, also bounds helper requests via VAULT_HELPER_TIMEOUT")
	overallTimeout := fs.Duration("overall-timeout", 2*time.Minute, "overall run timeout (max 5m)")
	confirmLive := fs.Bool("confirm-live", false, "required acknowledgment that this run writes to the selected Vault path")
	allowDev := fs.Bool("allow-dev", false, "allow in-memory (dev-mode) Vault; the report is still labelled non-production")
	skipCleanup := fs.Bool("skip-cleanup", false, "leave the acceptance key at the selected path instead of deleting its KV metadata")
	outageAddr := fs.String("outage-addr", "http://127.0.0.1:1", "deliberately unreachable Vault URL used to prove helper fail-closed behavior")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *timeout <= 0 || *timeout > vaultAcceptMaxTimeout {
		fmt.Fprintf(stderr, "nexaroute vault-accept: --timeout must be between 1ns and %s\n", vaultAcceptMaxTimeout)
		return 2
	}
	if *overallTimeout < *timeout || *overallTimeout > vaultAcceptMaxOverall {
		fmt.Fprintf(stderr, "nexaroute vault-accept: --overall-timeout must be between --timeout and %s\n", vaultAcceptMaxOverall)
		return 2
	}
	timeoutSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			timeoutSet = true
		}
	})
	helperTimeout, err := vaultAcceptHelperTimeout(*timeout, timeoutSet)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute vault-accept: %v\n", err)
		return 2
	}
	if err := validateVaultAcceptRef(*ref); err != nil {
		fmt.Fprintf(stderr, "nexaroute vault-accept: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*addrEnv) == "" || strings.TrimSpace(*tokenEnv) == "" {
		fmt.Fprintln(stderr, "nexaroute vault-accept: --addr-env and --token-env must name environment variables")
		return 2
	}
	if !*confirmLive {
		fmt.Fprintln(stderr, "nexaroute vault-accept: refusing to run without --confirm-live.")
		fmt.Fprintln(stderr, "This command writes a throwaway key to the selected Vault path; use a disposable/staging Vault.")
		return 2
	}
	helperAbs, err := validateVaultAcceptHelper(*helper)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute vault-accept: %v\n", err)
		return 2
	}
	outageURL, err := validateVaultAcceptAddr(*outageAddr)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute vault-accept: --outage-addr %v\n", err)
		return 2
	}
	if strings.TrimSpace(*outPath) == "" {
		*outPath = "vault-acceptance-" + time.Now().UTC().Format("20060102T150405Z") + ".json"
	}

	addr := strings.TrimSpace(os.Getenv(*addrEnv))
	token := strings.TrimSpace(os.Getenv(*tokenEnv))
	report := vaultAcceptReport{
		Schema:      vaultAcceptSchema,
		Gateway:     version,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Mode:        "live",
		Target: vaultAcceptTarget{
			Ref:        *ref,
			AuthMethod: "static token from environment (helper contract); token acquisition is operator-owned",
			TokenEnv:   *tokenEnv,
			Helper:     true,
		},
		Bounds: vaultAcceptBounds{
			CallTimeoutSeconds:    timeout.Seconds(),
			OverallTimeoutSeconds: overallTimeout.Seconds(),
			HelperTimeoutSeconds:  helperTimeout,
		},
		Limitations: []string{
			"This harness verifies Vault KV v2 custody only. Vault Transit envelope mode is a separate provider path and is not tested by this harness.",
			"Real Vault HA failover, long-running token renewal, and auth methods other than a static token are NOT exercised by this run.",
			"This harness does not start a full gateway process; it verifies helper key load and the nxs1 decrypt path with that key.",
			"Dev-mode Vault is never production evidence; evidence_class=dev is reported for explicit --allow-dev runs.",
			"Reports contain no token values or key bytes; they record only statuses, redacted endpoints, policy names, and topology metadata.",
		},
	}
	report.Checks = append(report.Checks, vaultAcceptNotRun("vault.ha_failover",
		"requires an operator-controlled Vault HA cluster and a failover event; not automated by this harness"))
	report.Checks = append(report.Checks, vaultAcceptNotRun("vault.transit",
		"Vault Transit envelope mode is a separate provider path and is not tested by this harness; this run does not claim Transit support"))

	if addr == "" || token == "" {
		missing := *addrEnv
		if addr != "" {
			missing = *tokenEnv
		}
		report.Target.Addr = sanitizeAcceptanceURL(addr)
		report.Checks = append(report.Checks, vaultAcceptCheck{
			ID: "vault.config", Kind: "config", Selected: true, Status: externalStatusBlocked,
			Reason: fmt.Sprintf("environment variable %s is not set; no Vault request or write was attempted", missing),
		})
		report.Warnings = append(report.Warnings, "No Vault endpoint/token was supplied; this report is BLOCKED, not acceptance evidence.")
		return finishVaultAccept(report, *outPath, stdout, stderr)
	}
	parsedAddr, err := validateVaultAcceptAddr(addr)
	if err != nil {
		report.Checks = append(report.Checks, vaultAcceptCheck{
			ID: "vault.config", Kind: "config", Selected: true, Status: externalStatusFail,
			Reason: "Vault address is not a safe absolute HTTP(S) URL: " + err.Error(),
		})
		return finishVaultAccept(report, *outPath, stdout, stderr)
	}
	report.Target.Addr = sanitizeAcceptanceURL(addr)
	report.EvidenceClass = vaultAcceptEvidenceClass(parsedAddr)
	report.Bounds.CallTimeoutSeconds = timeout.Seconds()

	runCtx, cancel := context.WithTimeout(context.Background(), *overallTimeout)
	defer cancel()
	client := newVaultAcceptClient(*timeout)

	health := runVaultAcceptHealth(runCtx, client, addr, token)
	report.Checks = append(report.Checks, health)
	if health.Status != externalStatusPass {
		report.Checks = append(report.Checks, vaultAcceptSkipped("vault.kv_put_get", "Vault health check did not pass"),
			vaultAcceptSkipped("vault.kv_version", "Vault health check did not pass"),
			vaultAcceptSkipped("vault.kv_rotation", "Vault health check did not pass"),
			vaultAcceptSkipped("vault.wrong_token", "Vault health check did not pass"),
			vaultAcceptSkipped("vault.revoked_token", "Vault health check did not pass"),
			vaultAcceptSkipped("vault.outage_fail_closed", "Vault health check did not pass"))
		return finishVaultAccept(report, *outPath, stdout, stderr)
	}
	report.Vault = health.vault
	if report.Vault.DevMode && !*allowDev {
		report.Checks = append(report.Checks, vaultAcceptCheck{
			ID: "vault.dev_mode", Kind: "guard", Selected: true, Status: externalStatusBlocked,
			Reason: "Vault reports in-memory storage (dev mode); rerun with --allow-dev only for non-production harness checks",
		})
		report.Checks = append(report.Checks, vaultAcceptSkipped("vault.kv_put_get", "dev mode is not production evidence"),
			vaultAcceptSkipped("vault.kv_version", "dev mode is not production evidence"),
			vaultAcceptSkipped("vault.kv_rotation", "dev mode is not production evidence"),
			vaultAcceptSkipped("vault.wrong_token", "dev mode is not production evidence"),
			vaultAcceptSkipped("vault.revoked_token", "dev mode is not production evidence"),
			vaultAcceptSkipped("vault.outage_fail_closed", "dev mode is not production evidence"))
		report.Warnings = append(report.Warnings, "Dev-mode Vault rejected by default; no key was written.")
		return finishVaultAccept(report, *outPath, stdout, stderr)
	}
	if report.Vault.DevMode {
		report.EvidenceClass = "dev"
		report.Warnings = append(report.Warnings, "This run used in-memory (dev-mode) Vault storage. It is harness evidence only and MUST NOT be cited as production, staging, or HA acceptance evidence.")
	}

	policy := runVaultAcceptTokenMetadata(runCtx, client, addr, token, &report)
	report.Checks = append(report.Checks, policy)
	if policy.Status == externalStatusFail {
		// A root token must never be used to write acceptance keys.
		return finishVaultAccept(report, *outPath, stdout, stderr)
	}

	preflight := runVaultAcceptRefPreflight(runCtx, client, addr, token, *ref)
	report.Checks = append(report.Checks, preflight)
	if preflight.Status != externalStatusPass {
		return finishVaultAccept(report, *outPath, stdout, stderr)
	}
	report.Vault.BaselineObserved = true

	key1 := make([]byte, 32)
	if _, err := rand.Read(key1); err != nil {
		report.Checks = append(report.Checks, vaultAcceptCheck{
			ID: "vault.kv_put_get", Kind: "kv", Selected: true, Status: externalStatusFail,
			Reason: "cannot generate acceptance key",
		})
		return finishVaultAccept(report, *outPath, stdout, stderr)
	}
	putGet := runVaultAcceptPutGet(runCtx, helperAbs, *ref, key1, addr, token, helperTimeout)
	report.Checks = append(report.Checks, putGet)
	if putGet.Status == externalStatusPass {
		// The metadata probe runs only after a successful round-trip: on
		// a failed round-trip the key is absent, and a metadata 404
		// would falsely report a KV v1 mount.
		report.Checks = append(report.Checks, runVaultAcceptKVVersion(runCtx, client, addr, token, *ref))
		report.Checks = append(report.Checks, runVaultAcceptDecrypt(key1))
	} else {
		report.Checks = append(report.Checks, vaultAcceptSkipped("vault.kv_version", "key round-trip did not pass"))
		report.Checks = append(report.Checks, vaultAcceptSkipped("vault.gateway_decrypt", "key round-trip did not pass"))
	}

	rotation := runVaultAcceptRotation(runCtx, helperAbs, *ref, addr, token, helperTimeout)
	report.Checks = append(report.Checks, rotation)

	report.Checks = append(report.Checks, runVaultAcceptWrongToken(runCtx, helperAbs, *ref, addr, helperTimeout))
	report.Checks = append(report.Checks, runVaultAcceptRevokedToken(runCtx, client, helperAbs, *ref, addr, token, *timeout, helperTimeout))
	report.Checks = append(report.Checks, runVaultAcceptOutage(runCtx, helperAbs, *ref, outageURL.String(), token, helperTimeout))

	if *skipCleanup {
		report.Checks = append(report.Checks, vaultAcceptNotRun("vault.cleanup", "cleanup skipped by --skip-cleanup; acceptance key remains at the selected path"))
	} else {
		report.Checks = append(report.Checks, runVaultAcceptCleanup(runCtx, client, addr, token, *ref))
	}
	return finishVaultAccept(report, *outPath, stdout, stderr)
}

// vaultAcceptHelperTimeout resolves the per-request bound handed to the
// helper script via VAULT_HELPER_TIMEOUT. An explicitly set --timeout wins,
// because that is what an operator setting --timeout expects to bound;
// otherwise the operator's exported VAULT_HELPER_TIMEOUT is honored; otherwise
// the script default (30s) applies. An exported but invalid value is a usage
// error, not something the helper discovers mid-run with a confusing failure.
func vaultAcceptHelperTimeout(timeout time.Duration, timeoutSet bool) (int64, error) {
	if timeoutSet {
		// Round up: a sub-second --timeout must still hand the helper a
		// positive integer bound instead of 0, which the script rejects.
		seconds := int64(math.Ceil(timeout.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		return seconds, nil
	}
	raw := strings.TrimSpace(os.Getenv("VAULT_HELPER_TIMEOUT"))
	if raw == "" {
		return 30, nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("VAULT_HELPER_TIMEOUT must be a positive integer (seconds), got %q", raw)
	}
	return seconds, nil
}

func validateVaultAcceptRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("--ref is required and must name an empty, dedicated KV v2 data path")
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "..") ||
		strings.ContainsAny(ref, " \t\r\n?#") {
		return errors.New("--ref must be a relative KV v2 data path without traversal, whitespace, query, or fragment characters")
	}
	if !strings.Contains(ref, "/data/") {
		return errors.New("--ref must be a KV v2 data path containing /data/ (for example secret/data/nexaroute/acceptance/master-key)")
	}
	return nil
}

func validateVaultAcceptHelper(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("--helper must name the key-provider helper")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("cannot resolve --helper path")
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", errors.New("--helper does not point to a readable file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("--helper is not executable")
	}
	return abs, nil
}

func validateVaultAcceptAddr(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("must be an absolute HTTP(S) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not contain user info, query, or fragment components")
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if loopbackHTTPURL(raw) {
			return u, nil
		}
		return nil, errors.New("plain HTTP is allowed only for loopback addresses; use HTTPS for staging or production")
	default:
		return nil, errors.New("scheme must be https (or http on loopback)")
	}
}

func vaultAcceptEvidenceClass(u *url.URL) string {
	if u == nil {
		return "none"
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.") {
		return "loopback"
	}
	return "external"
}

func runVaultAcceptHealth(ctx context.Context, client *http.Client, addr, token string) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.health", Kind: "health", Selected: true}
	status, body, err := vaultHTTP(ctx, client, addr, token, http.MethodGet, "sys/seal-status", nil)
	check.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		check.Status = externalStatusFail
		check.Reason = "Vault health request failed: " + vaultAcceptSafeError(err)
		return check
	}
	if status < 200 || status >= 300 {
		check.Status = externalStatusFail
		check.Reason = fmt.Sprintf("Vault health endpoint returned HTTP %d", status)
		return check
	}
	var seal struct {
		Version     string `json:"version"`
		StorageType string `json:"storage_type"`
		Sealed      bool   `json:"sealed"`
		Standby     bool   `json:"standby"`
	}
	if err := json.Unmarshal(body, &seal); err != nil {
		check.Status = externalStatusFail
		check.Reason = "Vault health response is not valid JSON"
		return check
	}
	check.vault.Version = seal.Version
	check.vault.StorageType = seal.StorageType
	check.vault.Sealed = seal.Sealed
	check.vault.Standby = seal.Standby
	check.vault.DevMode = strings.EqualFold(seal.StorageType, "inmem") || strings.EqualFold(seal.StorageType, "in-memory")

	if lstatus, lbody, lerr := vaultHTTP(ctx, client, addr, token, http.MethodGet, "sys/leader", nil); lerr == nil && lstatus >= 200 && lstatus < 300 {
		var leader struct {
			HAEnabled bool `json:"ha_enabled"`
		}
		if json.Unmarshal(lbody, &leader) == nil {
			check.vault.HAEnabled = leader.HAEnabled
			check.vault.HAKnown = true
		}
	}
	if check.vault.Sealed {
		check.Status = externalStatusFail
		check.Reason = "Vault is sealed; no key custody operation can succeed"
		return check
	}
	check.Status = externalStatusPass
	check.Detail = fmt.Sprintf("version=%s storage_type=%s dev_mode=%t ha_enabled_known=%t",
		safeVaultAcceptString(check.vault.Version), safeVaultAcceptString(check.vault.StorageType), check.vault.DevMode, check.vault.HAKnown)
	return check
}

func runVaultAcceptTokenMetadata(ctx context.Context, client *http.Client, addr, token string, report *vaultAcceptReport) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.token_metadata", Kind: "token", Selected: true}
	status, body, err := vaultHTTP(ctx, client, addr, token, http.MethodGet, "auth/token/lookup-self", nil)
	check.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		check.Status = externalStatusBlocked
		check.Reason = "Vault token lookup failed: " + vaultAcceptSafeError(err)
		return check
	}
	if status == http.StatusForbidden {
		check.Status = externalStatusBlocked
		check.Reason = "token lacks auth/token/lookup-self; policy names cannot be recorded (token value was not logged)"
		return check
	}
	if status < 200 || status >= 300 {
		check.Status = externalStatusBlocked
		check.Reason = fmt.Sprintf("Vault token lookup returned HTTP %d; policy names cannot be recorded", status)
		return check
	}
	var lookup struct {
		Data struct {
			Policies  []string `json:"policies"`
			TTL       int64    `json:"ttl"`
			Renewable bool     `json:"renewable"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &lookup); err != nil {
		check.Status = externalStatusBlocked
		check.Reason = "Vault token lookup response is not valid JSON; policy names cannot be recorded"
		return check
	}
	policies := append([]string(nil), lookup.Data.Policies...)
	sort.Strings(policies)
	report.Vault.Policies = policies
	report.Vault.TokenTTLSeconds = lookup.Data.TTL
	report.Vault.TokenRenewable = lookup.Data.Renewable
	report.Vault.TokenMetadataOK = true
	check.Detail = fmt.Sprintf("policies=%s ttl_seconds=%d renewable=%t", strings.Join(policies, ","), lookup.Data.TTL, lookup.Data.Renewable)
	// A root token would otherwise PASS this check and then write
	// acceptance keys with unlimited privilege. Acceptance requires a
	// least-privilege token; non-root is necessary (not sufficient) for
	// that claim, and staging still needs a manual policy review.
	for _, policy := range policies {
		if policy == "root" {
			check.Status = externalStatusFail
			check.Reason = "token carries the root policy; acceptance requires a least-privilege token (only policy names are recorded)"
			return check
		}
	}
	check.Status = externalStatusPass
	return check
}

func runVaultAcceptRefPreflight(ctx context.Context, client *http.Client, addr, token, ref string) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.ref_preflight", Kind: "kv", Selected: true}
	status, _, err := vaultHTTP(ctx, client, addr, token, http.MethodGet, ref, nil)
	check.DurationMS = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		check.Status = externalStatusFail
		check.Reason = "cannot read the selected Vault path: " + vaultAcceptSafeError(err)
	case status == http.StatusNotFound:
		check.Status = externalStatusPass
		check.Detail = "selected path is empty"
	case status >= 200 && status < 300:
		check.Status = externalStatusFail
		check.Reason = "selected path already contains data; refusing to overwrite it (use a dedicated empty acceptance path)"
	case status == http.StatusForbidden:
		check.Status = externalStatusFail
		check.Reason = "token cannot read the selected path (HTTP 403); no write was attempted"
	default:
		check.Status = externalStatusFail
		check.Reason = fmt.Sprintf("selected path preflight returned HTTP %d; no write was attempted", status)
	}
	return check
}

func runVaultAcceptPutGet(ctx context.Context, helper, ref string, key []byte, addr, token string, helperTimeout int64) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.kv_put_get", Kind: "kv", Selected: true}
	out, err := runVaultAcceptHelper(ctx, helper, "put", ref, key, addr, token, helperTimeout)
	check.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		check.Status = externalStatusFail
		check.Reason = "helper put failed; no key bytes were logged"
		return check
	}
	if len(out) != 0 {
		check.Status = externalStatusFail
		check.Reason = "helper put unexpectedly wrote to stdout"
		return check
	}
	start = time.Now()
	got, err := runVaultAcceptHelperGet(ctx, helper, ref, addr, token, helperTimeout)
	check.DurationMS += time.Since(start).Milliseconds()
	if err != nil {
		check.Status = externalStatusFail
		check.Reason = "helper get failed after a successful put"
		return check
	}
	if len(got) != 32 || subtle.ConstantTimeCompare(got, key) != 1 {
		check.Status = externalStatusFail
		check.Reason = "helper get did not return the same 32-byte key"
		return check
	}
	check.Status = externalStatusPass
	check.Detail = "helper put/get returned byte-identical 32-byte key material (bytes not logged)"
	return check
}

func runVaultAcceptDecrypt(key []byte) vaultAcceptCheck {
	check := vaultAcceptCheck{ID: "vault.gateway_decrypt", Kind: "kv", Selected: true}
	raw := []byte(`{"providers":[{"id":"acceptance","type":"openai_compatible","api_key":"acceptance-plaintext"}]}`)
	encrypted, n, err := secrets.TransformConfig(raw, key, true)
	if err != nil || n != 1 {
		check.Status = externalStatusFail
		check.Reason = "cannot encrypt an acceptance config field with the Vault-held key"
		return check
	}
	plain, _, err := secrets.TransformConfig(encrypted, key, false)
	if err != nil {
		check.Status = externalStatusFail
		check.Reason = "cannot decrypt an acceptance config field with the Vault-held key"
		return check
	}
	var decoded struct {
		Providers []struct {
			APIKey string `json:"api_key"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(plain, &decoded); err != nil || len(decoded.Providers) != 1 ||
		decoded.Providers[0].APIKey != "acceptance-plaintext" {
		check.Status = externalStatusFail
		check.Reason = "decrypted acceptance config did not match the original plaintext"
		return check
	}
	wrongKey := make([]byte, 32)
	if _, err := rand.Read(wrongKey); err != nil {
		check.Status = externalStatusFail
		check.Reason = "cannot generate a wrong-key control for decrypt verification"
		return check
	}
	if _, _, err := secrets.TransformConfig(encrypted, wrongKey, false); err == nil {
		check.Status = externalStatusFail
		check.Reason = "a different 32-byte key decrypted the acceptance envelope; wrong-key fail-closed is broken"
		return check
	}
	check.Status = externalStatusPass
	check.Detail = "nxs1 envelope encrypt/decrypt succeeded with the Vault-held key and failed closed with a different key; plaintext and key bytes were not logged"
	return check
}

func runVaultAcceptRotation(ctx context.Context, helper, ref, addr, token string, helperTimeout int64) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.kv_rotation", Kind: "kv", Selected: true}
	next := make([]byte, 32)
	if _, err := rand.Read(next); err != nil {
		check.Status = externalStatusFail
		check.Reason = "cannot generate a replacement acceptance key"
		return check
	}
	if _, err := runVaultAcceptHelper(ctx, helper, "put", ref, next, addr, token, helperTimeout); err != nil {
		check.DurationMS = time.Since(start).Milliseconds()
		check.Status = externalStatusFail
		check.Reason = "helper put of the replacement key failed"
		return check
	}
	got, err := runVaultAcceptHelperGet(ctx, helper, ref, addr, token, helperTimeout)
	check.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		check.Status = externalStatusFail
		check.Reason = "helper get after replacement failed"
		return check
	}
	if len(got) != 32 || subtle.ConstantTimeCompare(got, next) != 1 {
		check.Status = externalStatusFail
		check.Reason = "rotated key was not read back byte-identically"
		return check
	}
	check.Status = externalStatusPass
	check.Detail = "KV entry was replaced with a new 32-byte key and the new key was read back (bytes not logged)"
	return check
}

func runVaultAcceptWrongToken(ctx context.Context, helper, ref, addr string, helperTimeout int64) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.wrong_token", Kind: "fail-closed", Selected: true}
	wrong := []byte("nexaroute-acceptance-invalid-" + time.Now().UTC().Format("150405.000000000"))
	out, err := runVaultAcceptHelper(ctx, helper, "get", ref, nil, addr, string(wrong), helperTimeout)
	check.DurationMS = time.Since(start).Milliseconds()
	if err == nil {
		check.Status = externalStatusFail
		check.Reason = "helper accepted an invalid token"
		return check
	}
	if len(out) != 0 {
		check.Status = externalStatusFail
		check.Reason = "helper wrote key material to stdout after an invalid-token failure"
		return check
	}
	check.Status = externalStatusPass
	check.Detail = "invalid token produced a nonzero helper exit and empty stdout"
	return check
}

func runVaultAcceptRevokedToken(ctx context.Context, client *http.Client, helper, ref, addr, token string, timeout time.Duration, helperTimeout int64) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.revoked_token", Kind: "fail-closed", Selected: true}
	status, body, err := vaultHTTP(ctx, client, addr, token, http.MethodPost, "auth/token/create",
		map[string]any{"ttl": "60s", "renewable": false, "type": "service"})
	check.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		check.Status = externalStatusBlocked
		check.Reason = "cannot create a disposable child token: " + vaultAcceptSafeError(err)
		return check
	}
	if status == http.StatusForbidden {
		check.Status = externalStatusBlocked
		check.Reason = "token lacks auth/token/create; revoked-token check did not run (token value was not logged)"
		return check
	}
	if status < 200 || status >= 300 {
		check.Status = externalStatusBlocked
		check.Reason = fmt.Sprintf("disposable child token creation returned HTTP %d", status)
		return check
	}
	var created struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &created); err != nil || strings.TrimSpace(created.Auth.ClientToken) == "" {
		check.Status = externalStatusFail
		check.Reason = "Vault child-token response did not contain a client token"
		return check
	}
	child := created.Auth.ClientToken

	if _, err := runVaultAcceptHelperGet(ctx, helper, ref, addr, child, helperTimeout); err != nil {
		check.Status = externalStatusFail
		check.Reason = "disposable child token could not read the acceptance path; revocation behavior was not tested"
		return check
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rstatus, _, rerr := vaultHTTP(rctx, client, addr, child, http.MethodPost, "auth/token/revoke-self", nil)
	if rerr != nil || rstatus < 200 || rstatus >= 300 {
		check.Status = externalStatusFail
		check.Reason = "could not revoke the disposable child token; it will expire at its 60s TTL"
		return check
	}
	out, err := runVaultAcceptHelper(ctx, helper, "get", ref, nil, addr, child, helperTimeout)
	check.DurationMS = time.Since(start).Milliseconds()
	if err == nil {
		check.Status = externalStatusFail
		check.Reason = "helper accepted a revoked child token"
		return check
	}
	if len(out) != 0 {
		check.Status = externalStatusFail
		check.Reason = "helper wrote key material to stdout after a revoked-token failure"
		return check
	}
	check.Status = externalStatusPass
	check.Detail = "disposable child token was revoked, then produced a nonzero helper exit and empty stdout"
	return check
}

func runVaultAcceptOutage(ctx context.Context, helper, ref, outageURL, token string, helperTimeout int64) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.outage_fail_closed", Kind: "fail-closed", Selected: true}
	out, err := runVaultAcceptHelper(ctx, helper, "get", ref, nil, outageURL, token, helperTimeout)
	check.DurationMS = time.Since(start).Milliseconds()
	if err == nil {
		check.Status = externalStatusFail
		check.Reason = "helper succeeded against the deliberately unreachable outage endpoint"
		return check
	}
	if len(out) != 0 {
		check.Status = externalStatusFail
		check.Reason = "helper wrote key material to stdout after an outage failure"
		return check
	}
	check.Status = externalStatusPass
	check.Detail = "deliberately unreachable endpoint produced a nonzero helper exit and empty stdout; no fallback key source was used"
	return check
}

func runVaultAcceptCleanup(ctx context.Context, client *http.Client, addr, token, ref string) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.cleanup", Kind: "kv", Selected: true}
	metaRef := vaultMetadataPath(ref)
	if metaRef == "" {
		check.Status = externalStatusFail
		check.Reason = "cannot derive the KV v2 metadata path for cleanup"
		return check
	}
	status, _, err := vaultHTTP(ctx, client, addr, token, http.MethodDelete, metaRef, nil)
	check.DurationMS = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		check.Status = externalStatusFail
		check.Reason = "cleanup request failed: " + vaultAcceptSafeError(err)
	case status == http.StatusNotFound || (status >= 200 && status < 300):
		check.Status = externalStatusPass
		check.Detail = "acceptance key metadata was deleted from the dedicated path"
	case status == http.StatusForbidden:
		check.Status = externalStatusBlocked
		check.Reason = "token lacks delete on the KV metadata path; the acceptance key remains at the selected path"
	default:
		check.Status = externalStatusFail
		check.Reason = fmt.Sprintf("cleanup returned HTTP %d; the acceptance key may remain at the selected path", status)
	}
	return check
}

// runVaultAcceptKVVersion proves the mount behaves as KV v2 instead of
// trusting the ref's path shape. After a successful round-trip, the key's
// KV v2 metadata endpoint must answer: a 200 confirms v2 semantics, a 404
// means the metadata endpoint does not exist (KV v1 behavior) and fails the
// run even though the helper round-trip alone would have passed, and a 403
// blocks the check because the token lacks metadata read — the documented
// runbook policy shape already grants that read, so a 403 means the staging
// policy needs extending, not that the mount is v2. No metadata body is
// recorded in the report.
func runVaultAcceptKVVersion(ctx context.Context, client *http.Client, addr, token, ref string) vaultAcceptCheck {
	start := time.Now()
	check := vaultAcceptCheck{ID: "vault.kv_version", Kind: "kv", Selected: true}
	meta := vaultMetadataPath(ref)
	if meta == "" {
		check.Status = externalStatusBlocked
		check.Reason = "cannot derive the KV metadata path from the selected ref"
		return check
	}
	status, _, err := vaultHTTP(ctx, client, addr, token, http.MethodGet, meta, nil)
	check.DurationMS = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		check.Status = externalStatusBlocked
		check.Reason = "cannot read the KV metadata path: " + vaultAcceptSafeError(err)
	case status >= 200 && status < 300:
		check.Status = externalStatusPass
		check.Detail = "KV v2 metadata endpoint confirms the mount behaves as KV v2"
	case status == http.StatusNotFound:
		check.Status = externalStatusFail
		check.Reason = "KV metadata endpoint is absent; the mount does not behave as KV v2 (check mount type/version)"
	case status == http.StatusForbidden:
		check.Status = externalStatusBlocked
		check.Reason = "token lacks read on the KV metadata path; extend the policy with metadata read per the runbook, then rerun"
	default:
		check.Status = externalStatusBlocked
		check.Reason = fmt.Sprintf("KV metadata probe returned HTTP %d; mount version cannot be determined", status)
	}
	return check
}

func vaultMetadataPath(ref string) string {
	i := strings.Index(ref, "/data/")
	if i < 0 {
		return ""
	}
	return ref[:i] + "/metadata/" + ref[i+len("/data/"):]
}

func vaultHTTP(ctx context.Context, client *http.Client, addr, token, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errors.New("cannot encode Vault request body")
		}
		reader = bytes.NewReader(raw)
	}
	reqCtx, cancel := context.WithTimeout(ctx, client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, strings.TrimRight(addr, "/")+"/v1/"+path, reader)
	if err != nil {
		return 0, nil, errors.New("cannot create Vault request")
	}
	req.Header.Set("X-Vault-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New("Vault request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, vaultAcceptMaxBody+1))
	if err != nil {
		return 0, nil, errors.New("cannot read Vault response")
	}
	if len(raw) > vaultAcceptMaxBody {
		return resp.StatusCode, nil, errors.New("Vault response exceeded the size bound")
	}
	return resp.StatusCode, raw, nil
}

func vaultAcceptSafeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if u, ok := err.(*url.Error); ok {
		msg = u.Err.Error()
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if msg == "" {
		return "request failed"
	}
	return msg
}

func safeVaultAcceptString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func runVaultAcceptHelperGet(ctx context.Context, helper, ref, addr, token string, helperTimeout int64) ([]byte, error) {
	out, err := runVaultAcceptHelper(ctx, helper, "get", ref, nil, addr, token, helperTimeout)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("helper returned invalid key material")
	}
	return decoded, nil
}

func runVaultAcceptHelper(ctx context.Context, helper, op, ref string, key []byte, addr, token string, helperTimeout int64) ([]byte, error) {
	var input bytes.Buffer
	fmt.Fprintf(&input, "%s %s\n", op, ref)
	if op == "put" {
		if len(key) != 32 {
			return nil, errors.New("acceptance key is not 32 bytes")
		}
		input.WriteString(base64.StdEncoding.EncodeToString(key))
		input.WriteByte('\n')
	}
	cmd := exec.CommandContext(ctx, helper)
	cmd.Stdin = &input
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "VAULT_ADDR=") || strings.HasPrefix(entry, "VAULT_TOKEN=") || strings.HasPrefix(entry, "VAULT_HELPER_TIMEOUT=") {
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env, "VAULT_ADDR="+addr, "VAULT_TOKEN="+token, "VAULT_HELPER_TIMEOUT="+strconv.FormatInt(helperTimeout, 10))
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func vaultAcceptNotRun(id, reason string) vaultAcceptCheck {
	return vaultAcceptCheck{ID: id, Kind: "not-run", Selected: false, Status: externalStatusNotRun, Reason: reason}
}

func vaultAcceptSkipped(id, reason string) vaultAcceptCheck {
	return vaultAcceptCheck{ID: id, Kind: "blocked", Selected: true, Status: externalStatusBlocked, Reason: reason}
}

func finishVaultAccept(report vaultAcceptReport, outPath string, stdout, stderr io.Writer) int {
	redactor := vaultAcceptRedactor(report)
	for i := range report.Checks {
		report.Checks[i].Detail = redactor.Redact(report.Checks[i].Detail)
		report.Checks[i].Reason = redactor.Redact(report.Checks[i].Reason)
	}
	finalizeVaultAcceptReport(&report)
	if err := writeVaultAcceptReport(outPath, report); err != nil {
		fmt.Fprintf(stderr, "nexaroute vault-accept: cannot write report: %v\n", err)
		return 2
	}
	printVaultAcceptSummary(stdout, report, outPath)
	return externalAcceptExitCode(report.Outcome)
}

func vaultAcceptRedactor(report vaultAcceptReport) *livecanary.Redactor {
	secrets := []string{}
	if report.Target.TokenEnv != "" {
		secrets = append(secrets, os.Getenv(report.Target.TokenEnv))
	}
	return livecanary.NewRedactor(secrets)
}

func finalizeVaultAcceptReport(report *vaultAcceptReport) {
	selected := make([]string, 0, len(report.Checks))
	for _, check := range report.Checks {
		report.Totals.Checks++
		switch check.Status {
		case externalStatusPass:
			report.Totals.Passed++
		case externalStatusFail:
			report.Totals.Failed++
		case externalStatusBlocked:
			report.Totals.Blocked++
		case externalStatusNotRun:
			report.Totals.NotRun++
		}
		if check.Selected {
			selected = append(selected, check.Status)
		}
	}
	report.Outcome = externalOutcome(selected)
	if report.EvidenceClass == "" {
		report.EvidenceClass = "none"
	}
	if report.Outcome == externalStatusBlocked {
		report.Limitations = append(report.Limitations, "One or more selected checks were BLOCKED by missing infrastructure, credentials, or permissions.")
	}
	if report.Outcome == externalStatusNotRun {
		report.Limitations = append(report.Limitations, "No selected check produced a live observation; this report is not acceptance evidence.")
	}
}

func writeVaultAcceptReport(path string, report vaultAcceptReport) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".vault-accept-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func printVaultAcceptSummary(w io.Writer, report vaultAcceptReport, outPath string) {
	fmt.Fprintf(w, "vault acceptance: outcome=%s checks=%d evidence_class=%s\n", report.Outcome, report.Totals.Checks, report.EvidenceClass)
	for _, check := range report.Checks {
		fmt.Fprintf(w, "  %-7s %s", check.Status, check.ID)
		if check.Reason != "" {
			fmt.Fprintf(w, " — %s", check.Reason)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "report: %s\n", outPath)
}
