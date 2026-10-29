package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
)

// runDiagnostics implements `nexaroute diagnostics bundle`: an
// operator-invoked, offline diagnostic bundle for incident investigation.
//
// It reuses the doctor checks and the `config validate` path, and it
// deliberately builds a NEW document from an allowlist of fields — it
// never copies the configuration, a log record, or a provider response
// wholesale. Every free-text field is sanitized before serialization,
// the serialized bytes are scanned a second time, and any scan failure
// aborts the write: no partial bundle is left behind. The file is
// written atomically with mode 0600, and nothing is ever sent to an
// external service: the operator reviews the file locally and decides
// what, if anything, to share.
//
// Exit codes: 0 = bundle written, 1 = bundle could not be produced
// safely (or the output could not be written), 2 = usage error. A
// failing doctor check is data inside the bundle, not a command
// failure, so it does not change the exit code.
func runDiagnostics(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "bundle" {
		fmt.Fprintln(stderr, "usage: nexaroute diagnostics bundle --config path --output path [--bundle path] [--max-age duration] [--no-log-errors] [--overwrite]")
		return 2
	}
	fs := flag.NewFlagSet("diagnostics bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON config")
	outputPath := fs.String("output", "", "path of the bundle file to write (required; never printed to stdout)")
	bundlePath := fs.String("bundle", "", "also run the doctor backup-bundle preflight and include its result")
	maxAge := fs.Duration("max-age", 0, "with --bundle, warn when the backup is older than this duration (e.g. 24h); 0 disables the freshness check")
	noLogErrors := fs.Bool("no-log-errors", false, "exclude sanitized recent log errors from the bundle")
	overwrite := fs.Bool("overwrite", false, "allow replacing an existing bundle file")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "unexpected positional arguments")
		}
		return 2
	}
	if strings.TrimSpace(*outputPath) == "" {
		fmt.Fprintln(stderr, "diagnostics bundle requires --output; the bundle is written to a file only, never to stdout or to any external service")
		return 2
	}
	if _, err := os.Stat(*outputPath); err == nil && !*overwrite {
		fmt.Fprintf(stderr, "diagnostics bundle refused: %s already exists; pass --overwrite to replace it or choose a new path\n", *outputPath)
		return 1
	}
	bundle, data, err := buildDiagnosticsBundle(*configPath, *bundlePath, *maxAge, !*noLogErrors)
	if err != nil {
		fmt.Fprintf(stderr, "diagnostics bundle failed: %v\n", err)
		return 1
	}
	// buildDiagnosticsBundle already scanned these exact bytes with
	// the full secret list; this second, pattern-only scan is a
	// defense-in-depth re-check of the bytes about to be written.
	if err := verifyBundleSafe(data, nil); err != nil {
		fmt.Fprintf(stderr, "diagnostics bundle failed safety scan; nothing was written: %v\n", err)
		return 1
	}
	if err := writeDiagnosticsBundleAtomic(*outputPath, data); err != nil {
		fmt.Fprintf(stderr, "diagnostics bundle failed: cannot write %s: %v\n", *outputPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "diagnostics bundle written: %s (mode 0600)\n", *outputPath)
	fmt.Fprintf(stdout, "contents: %s\n", strings.Join(bundle.Contents, ", "))
	fmt.Fprintln(stdout, "excluded: credentials, master keys, tokens, cookies, authorization headers, prompt text, model responses, raw configuration, URL user info and query strings")
	fmt.Fprintln(stdout, "live data: this bundle is offline. It does not include live Prometheus counters, live deployment health, or request/response bodies; on a running gateway those come from /metrics and /admin/api/health.")
	fmt.Fprintln(stdout, "before sharing: review the file yourself. It was not sent anywhere. Provider base URLs appear as scheme://host only, credential presence as a boolean, and log errors as sanitized, truncated lines — but hostnames, file basenames and counts are still metadata, so share only with a party you trust with operational detail.")
	fmt.Fprintln(stdout, "retention: delete the bundle when the incident is closed; it is an incident artifact, not a backup, and it must not be kept alongside backups or committed to a repository.")
	return 0
}

// --- Bundle shape: allowlisted fields only. Adding a field here is a
// deliberate, reviewable decision; nothing outside this struct can
// reach the bundle. ---

type diagnosticsBundle struct {
	Format       string                       `json:"format"`
	GeneratedAt  time.Time                    `json:"generated_at"`
	Gateway      diagnosticsGateway           `json:"gateway"`
	Config       diagnosticsConfigSummary     `json:"config"`
	Providers    []diagnosticsProviderSummary `json:"providers"`
	DoctorChecks []diagnosticsCheck           `json:"doctor_checks"`
	RecentErrors diagnosticsRecentErrors      `json:"recent_errors"`
	Metrics      diagnosticsMetrics           `json:"metrics"`
	Contents     []string                     `json:"contents"`
	SharingNote  string                       `json:"sharing_note"`
}

type diagnosticsGateway struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	CommitTime string `json:"commit_time,omitempty"`
	Modified   bool   `json:"vcs_modified"`
	GoVersion  string `json:"go_version"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	NumCPU     int    `json:"num_cpu"`
}

type diagnosticsConfigSummary struct {
	Valid               bool     `json:"valid"`
	Error               string   `json:"error,omitempty"`
	ProvidersTotal      int      `json:"providers_total"`
	ProvidersEnabled    int      `json:"providers_enabled"`
	ModelsTotal         int      `json:"models_total"`
	ModelsEnabled       int      `json:"models_enabled"`
	RouteProfiles       int      `json:"route_profiles"`
	VirtualEndpoints    int      `json:"virtual_endpoints"`
	Warnings            []string `json:"warnings"`
	TLSEnabled          bool     `json:"tls_enabled"`
	ControlPlaneEnabled bool     `json:"control_plane_enabled"`
	ControlPlaneBackend string   `json:"control_plane_backend"`
	VideoEnabled        bool     `json:"video_enabled"`
	LoggingMode         string   `json:"logging_mode"`
}

type diagnosticsProviderSummary struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Type                 string   `json:"type"`
	AuthMode             string   `json:"auth_mode"`
	Enabled              bool     `json:"enabled"`
	BaseURLHost          string   `json:"base_url_host"`
	ModelsTotal          int      `json:"models_total"`
	ModelsEnabled        int      `json:"models_enabled"`
	CredentialConfigured bool     `json:"credential_configured"`
	CredentialSource     string   `json:"credential_source"`
	HeaderNames          []string `json:"header_names"`
}

type diagnosticsCheck struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type diagnosticsRecentErrors struct {
	LogAvailable bool     `json:"log_available"`
	LinesScanned int      `json:"lines_scanned"`
	Included     []string `json:"included"`
	Dropped      int      `json:"dropped"`
}

type diagnosticsMetrics struct {
	ChecksPass          int  `json:"checks_pass"`
	ChecksWarn          int  `json:"checks_warn"`
	ChecksFail          int  `json:"checks_fail"`
	AuditRecords        int  `json:"audit_records"`
	AuditRecordsKnown   bool `json:"audit_records_known"`
	ProvidersTotal      int  `json:"providers_total"`
	ProvidersEnabled    int  `json:"providers_enabled"`
	ModelsTotal         int  `json:"models_total"`
	ModelsEnabled       int  `json:"models_enabled"`
	RecentErrorsScanned int  `json:"recent_errors_scanned"`
	RecentErrorsKept    int  `json:"recent_errors_kept"`
	RecentErrorsDropped int  `json:"recent_errors_dropped"`
}

const (
	diagnosticsFormat      = "nexaroute-diagnostics/v1"
	diagnosticsMaxLogBytes = 256 << 10
	diagnosticsMaxLogLines = 200
	diagnosticsMaxErrors   = 50
	diagnosticsMaxErrorLen = 400
	diagnosticsMaxDetail   = 300
	diagnosticsMaxWarnings = 20
)

var diagnosticsAuditRecordsPattern = regexp.MustCompile(`audit chain verified: (\d+) record`)

// buildDiagnosticsBundle assembles the bundle from the doctor checks,
// the config-validate path, and a bounded tail of the log file. It
// returns the bundle together with its exact serialized bytes, and it
// never returns bytes whose serialized form fails the safety scan.
func buildDiagnosticsBundle(configPath, backupBundlePath string, maxAge time.Duration, includeLogErrors bool) (*diagnosticsBundle, []byte, error) {
	var doctorOut bytes.Buffer
	d := &doctor{w: &doctorOut}
	d.checkConfig(configPath)
	d.checkMasterKey(configPath)
	d.checkSecurityStore(configPath)
	d.checkControlPlane(configPath)
	d.checkRestoreJournals()
	if strings.TrimSpace(backupBundlePath) != "" {
		d.checkBackupBundle(backupBundlePath, configPath, maxAge)
	}

	cfg, cfgErr := loadCheckedConfig(configPath)
	// Collect redaction secrets from whatever the loader produced,
	// even when validation failed: a failed load can still hold
	// decrypted values in memory, and those values must be redactable
	// from error text. The secrets live only in this sanitizer and are
	// never copied into the bundle.
	san := newDiagnosticsSanitizer(&cfg)

	checks := make([]diagnosticsCheck, 0, 16)
	for _, line := range strings.Split(strings.TrimRight(doctorOut.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		status, name, detail, ok := parseDoctorLine(line)
		if !ok {
			// A doctor line we cannot parse is not a field we can
			// vouch for: fail safely instead of copying it verbatim.
			return nil, nil, fmt.Errorf("doctor output line could not be parsed safely; bundle aborted")
		}
		detail = san.sanitizeText(detail)
		detail = truncateRunes(detail, diagnosticsMaxDetail)
		if diagnosticsDenyScan(detail, san.secrets) {
			detail = "detail withheld by safety scan"
		}
		checks = append(checks, diagnosticsCheck{Status: status, Name: name, Detail: detail})
	}

	bundle := &diagnosticsBundle{
		Format:       diagnosticsFormat,
		GeneratedAt:  time.Now().UTC(),
		Gateway:      diagnosticsGatewayInfo(),
		DoctorChecks: checks,
		Contents: []string{
			"gateway version and runtime",
			"config validation summary",
			"provider readiness summaries",
			"doctor checks",
			"sanitized recent log errors",
			"bounded counts",
		},
		SharingNote: "Operator-invoked incident artifact. Review before sharing; never send automatically. Delete when the incident is closed. Counts, hostnames, file basenames and check statuses are operational metadata and may still be sensitive in context.",
	}
	bundle.Config = diagnosticsConfigSummaryFrom(cfg, cfgErr, san)
	bundle.Providers = diagnosticsProviderSummaries(cfg, cfgErr == nil)
	if includeLogErrors && cfgErr == nil {
		bundle.RecentErrors = diagnosticsRecentErrorsFrom(cfg, configPath, san)
	} else {
		bundle.RecentErrors = diagnosticsRecentErrors{Included: []string{}}
	}
	bundle.Metrics = diagnosticsMetricsFrom(bundle)

	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("cannot serialize bundle for safety scan: %w", err)
	}
	data = append(data, '\n')
	if err := verifyBundleSafe(data, san.secrets); err != nil {
		return nil, nil, err
	}
	return bundle, data, nil
}

func diagnosticsGatewayInfo() diagnosticsGateway {
	info := diagnosticsGateway{
		Version:   version,
		Commit:    "unavailable",
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if s.Value != "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				info.CommitTime = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	return info
}

func diagnosticsConfigSummaryFrom(cfg config.Config, cfgErr error, san *diagnosticsSanitizer) diagnosticsConfigSummary {
	summary := diagnosticsConfigSummary{Warnings: []string{}, LoggingMode: "unknown"}
	if cfgErr != nil {
		summary.Valid = false
		summary.Error = safeDiagnosticsError(cfgErr, san)
		// Even on validation failure the loader may have populated
		// counts; only counts are allowlisted, never raw values, so
		// reporting them is safe. When the load failed outright the
		// counts are zero.
		summary.ProvidersTotal = len(cfg.Providers)
		return summary
	}
	summary.Valid = true
	for _, p := range cfg.Providers {
		summary.ProvidersTotal++
		if p.Enabled {
			summary.ProvidersEnabled++
		}
		for _, m := range p.Models {
			summary.ModelsTotal++
			if m.Enabled {
				summary.ModelsEnabled++
			}
		}
	}
	summary.RouteProfiles = len(cfg.RouteProfiles)
	summary.VirtualEndpoints = len(cfg.VirtualEndpoints)
	summary.TLSEnabled = cfg.TLS.Enabled
	summary.ControlPlaneEnabled = cfg.ControlPlane.Enabled
	summary.ControlPlaneBackend = cfg.ControlPlane.Backend
	summary.VideoEnabled = cfg.Video.Enabled
	switch strings.TrimSpace(cfg.Logging.File) {
	case "", "auto":
		summary.LoggingMode = "auto"
	case "off":
		summary.LoggingMode = "off"
	default:
		summary.LoggingMode = "custom"
	}
	seen := map[string]bool{}
	for _, w := range append(append([]string{}, cfg.Warnings...), cfg.EligibilityWarnings...) {
		clean := san.sanitizeText(w)
		clean = truncateRunes(clean, diagnosticsMaxDetail)
		if clean == "" || diagnosticsDenyScan(clean, san.secrets) || seen[clean] {
			continue
		}
		seen[clean] = true
		summary.Warnings = append(summary.Warnings, clean)
		if len(summary.Warnings) >= diagnosticsMaxWarnings {
			break
		}
	}
	return summary
}

func diagnosticsProviderSummaries(cfg config.Config, valid bool) []diagnosticsProviderSummary {
	out := []diagnosticsProviderSummary{}
	if !valid {
		return out
	}
	for _, p := range cfg.Providers {
		sum := diagnosticsProviderSummary{
			ID:               p.ID,
			Name:             p.Name,
			Type:             p.Type,
			AuthMode:         p.AuthMode,
			Enabled:          p.Enabled,
			BaseURLHost:      diagnosticsBaseURLHost(p.BaseURL),
			HeaderNames:      []string{},
			CredentialSource: "none",
		}
		for _, m := range p.Models {
			sum.ModelsTotal++
			if m.Enabled {
				sum.ModelsEnabled++
			}
		}
		if len(p.ResolvedCredentials()) > 0 {
			sum.CredentialConfigured = true
		}
		switch {
		case strings.TrimSpace(p.APIKeyEnv) != "":
			// The environment variable NAME is configuration metadata;
			// its value is never read for the bundle beyond the
			// presence check inside ResolvedCredentials.
			sum.CredentialSource = "env:" + strings.TrimSpace(p.APIKeyEnv)
		case sum.CredentialConfigured:
			sum.CredentialSource = "configured"
		}
		names := make([]string, 0, len(p.Headers))
		for name := range p.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
		sum.HeaderNames = names
		out = append(out, sum)
	}
	return out
}

// diagnosticsBaseURLHost reduces a provider base URL to scheme://host.
// User info, query, fragment and path are dropped: a base URL is
// routing metadata, and anything after the host can carry credentials
// or deployment-specific paths that do not belong in a shared bundle.
func diagnosticsBaseURLHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func diagnosticsRecentErrorsFrom(cfg config.Config, configPath string, san *diagnosticsSanitizer) diagnosticsRecentErrors {
	result := diagnosticsRecentErrors{Included: []string{}}
	path, ok := diagnosticsLogPath(cfg, configPath)
	if !ok {
		return result
	}
	f, err := os.Open(path)
	if err != nil {
		return result
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return result
	}
	offset := int64(0)
	if info.Size() > diagnosticsMaxLogBytes {
		offset = info.Size() - diagnosticsMaxLogBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return result
	}
	raw, err := io.ReadAll(io.LimitReader(f, diagnosticsMaxLogBytes+1))
	if err != nil {
		return result
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) > diagnosticsMaxLogLines {
		lines = lines[len(lines)-diagnosticsMaxLogLines:]
	}
	result.LogAvailable = true
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		result.LinesScanned++
		// The gateway never logs request/response bodies, so a log
		// line shaped like a payload (prompt, messages, content,
		// choices) is not an operational error line at all: drop it
		// rather than trying to redact prompt text or model output
		// out of it. Fail safely: when in doubt, the line is out.
		if diagnosticsLooksLikePayload(line) {
			result.Dropped++
			continue
		}
		if !diagnosticsLooksLikeError(line) {
			continue
		}
		clean := san.sanitizeText(line)
		clean = truncateRunes(clean, diagnosticsMaxErrorLen)
		if diagnosticsDenyScan(clean, san.secrets) {
			result.Dropped++
			continue
		}
		if len(result.Included) < diagnosticsMaxErrors {
			result.Included = append(result.Included, clean)
		} else {
			result.Dropped++
		}
	}
	return result
}

// diagnosticsLogPath mirrors the gateway startup rule in main.go:
// "off" disables the file, ""/"auto" means nexaroute.log next to the
// config, and a relative path resolves against the config directory.
// Only the existence of the file is reported; the path itself is never
// written into the bundle.
func diagnosticsLogPath(cfg config.Config, configPath string) (string, bool) {
	logPath := strings.TrimSpace(cfg.Logging.File)
	if logPath == "off" {
		return "", false
	}
	if logPath == "" || logPath == "auto" {
		return filepath.Join(filepath.Dir(configPath), "nexaroute.log"), true
	}
	if filepath.IsAbs(logPath) {
		return logPath, true
	}
	return filepath.Join(filepath.Dir(configPath), logPath), true
}

// diagnosticsLooksLikePayload reports whether a log line carries a
// request/response payload shape rather than an operational error.
func diagnosticsLooksLikePayload(line string) bool {
	for _, key := range []string{`"prompt"`, `"messages"`, `"content"`, `"choices"`} {
		if strings.Contains(line, key) {
			return true
		}
	}
	return false
}

func diagnosticsLooksLikeError(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "panic")
}

func diagnosticsMetricsFrom(bundle *diagnosticsBundle) diagnosticsMetrics {
	m := diagnosticsMetrics{
		ProvidersTotal:      bundle.Config.ProvidersTotal,
		ProvidersEnabled:    bundle.Config.ProvidersEnabled,
		ModelsTotal:         bundle.Config.ModelsTotal,
		ModelsEnabled:       bundle.Config.ModelsEnabled,
		RecentErrorsScanned: bundle.RecentErrors.LinesScanned,
		RecentErrorsKept:    len(bundle.RecentErrors.Included),
		RecentErrorsDropped: bundle.RecentErrors.Dropped,
	}
	for _, c := range bundle.DoctorChecks {
		switch c.Status {
		case "PASS":
			m.ChecksPass++
		case "WARN":
			m.ChecksWarn++
		default:
			m.ChecksFail++
		}
		if match := diagnosticsAuditRecordsPattern.FindStringSubmatch(c.Detail); match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil {
				m.AuditRecords = n
				m.AuditRecordsKnown = true
			}
		}
	}
	return m
}

func parseDoctorLine(line string) (status, name, detail string, ok bool) {
	space := strings.Index(line, " ")
	if space <= 0 {
		return "", "", "", false
	}
	status = line[:space]
	rest := line[space+1:]
	colon := strings.Index(rest, ": ")
	if colon <= 0 {
		return "", "", "", false
	}
	return status, rest[:colon], rest[colon+2:], true
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// safeDiagnosticsError reduces a validation error to a sanitized,
// truncated class string. The raw error may echo a configured value
// (for example a malformed URL), so it is never copied verbatim; if
// the sanitized form still fails the deny scan, the detail is
// withheld entirely rather than risked.
func safeDiagnosticsError(err error, san *diagnosticsSanitizer) string {
	if err == nil {
		return ""
	}
	clean := san.sanitizeText(err.Error())
	clean = truncateRunes(clean, diagnosticsMaxDetail)
	if clean == "" || diagnosticsDenyScan(clean, san.secrets) {
		return "validation failed (detail withheld by safety scan)"
	}
	return clean
}

// --- Sanitizer: sanitize before serialization, then scan again. ---

type diagnosticsSanitizer struct {
	redactor *livecanary.Redactor
	secrets  []string
}

func newDiagnosticsSanitizer(cfg *config.Config) *diagnosticsSanitizer {
	secrets := []string{}
	if cfg != nil {
		secrets = append(secrets, strings.TrimSpace(cfg.Admin.APIKey))
		for _, p := range cfg.Providers {
			secrets = append(secrets, p.ResolvedCredentials()...)
			for _, name := range []string{p.SecretKeyEnv, p.SessionTokenEnv} {
				if v := os.Getenv(name); v != "" {
					secrets = append(secrets, v)
				}
			}
			for _, value := range p.Headers {
				secrets = append(secrets, value)
			}
			if u, err := url.Parse(p.ProxyURL); err == nil && u.User != nil {
				secrets = append(secrets, u.User.Username())
				if pw, ok := u.User.Password(); ok {
					secrets = append(secrets, pw)
				}
			}
		}
		if v := os.Getenv(cfg.Admin.OIDC.ClientSecretEnv); v != "" {
			secrets = append(secrets, v)
		}
		if v := os.Getenv(cfg.Video.AuthTokenEnv); v != "" {
			secrets = append(secrets, v)
		}
		if v := os.Getenv(cfg.Video.CodeRenderPlannerAPIKeyEnv); v != "" {
			secrets = append(secrets, v)
		}
	}
	return &diagnosticsSanitizer{redactor: livecanary.NewRedactor(secrets), secrets: secrets}
}

var (
	diagnosticsURLUserInfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
	diagnosticsURLQueryPattern    = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^\s"'?#]+)\?[^\s"'#]*`)
	diagnosticsURLFragmentPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^\s"'#]+)#[^\s"'#]*`)
	diagnosticsKeyValuePattern    = regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|secret|token|password|passwd|pwd|session|cookie|authorization)\b\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)
	diagnosticsJSONSecretPattern  = regexp.MustCompile(`(?i)"(api_key|apiKey|secret|token|password|access_token|refresh_token|session_token|cookie|authorization)"\s*:\s*"[^"]*"`)
	diagnosticsEnvelopePattern    = regexp.MustCompile(`nxs[12]:[A-Za-z0-9+/=_-]+`)
	diagnosticsPathPattern        = regexp.MustCompile(`/(?:[A-Za-z0-9._~+-]+/)+[A-Za-z0-9._~+-]*`)
)

// sanitizeText removes secret material from free text. It is
// deliberately layered: known values and auth headers first (the
// livecanary redactor), then URL user info, query and fragment, then
// secret-looking key=value and JSON pairs, then encrypted envelopes,
// then absolute filesystem paths reduced to their basename. A field
// that still fails the deny scan after this pass is dropped or
// withheld by its caller — sanitization never silently passes a
// suspicious field through.
func (s *diagnosticsSanitizer) sanitizeText(text string) string {
	if s == nil {
		return ""
	}
	out := text
	if s.redactor != nil {
		out = s.redactor.Redact(out)
	}
	out = diagnosticsURLUserInfoPattern.ReplaceAllString(out, "$1")
	out = diagnosticsURLQueryPattern.ReplaceAllString(out, "$1")
	out = diagnosticsURLFragmentPattern.ReplaceAllString(out, "$1")
	out = diagnosticsKeyValuePattern.ReplaceAllString(out, "$1=[REDACTED]")
	out = diagnosticsJSONSecretPattern.ReplaceAllString(out, `"$1":"[REDACTED]"`)
	out = diagnosticsEnvelopePattern.ReplaceAllString(out, "[REDACTED-ENVELOPE]")
	out = diagnosticsStripPaths(out)
	return strings.TrimSpace(out)
}

// diagnosticsStripPaths replaces absolute filesystem paths with their
// basename, so a shared bundle does not leak home directories or
// deployment layouts. A match is only treated as a path when it is
// preceded by a delimiter (start, space, quote, bracket, = or :), so
// URL paths after a host name are left alone — URL user info, query
// and fragment are already stripped above.
func diagnosticsStripPaths(s string) string {
	matches := diagnosticsPathPattern.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	pos := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > 0 {
			prev := s[start-1]
			if prev != ' ' && prev != '\t' && prev != '"' && prev != '\'' && prev != '(' && prev != '[' && prev != '=' && prev != ':' {
				continue
			}
		}
		match := s[start:end]
		base := match[strings.LastIndex(match, "/")+1:]
		if base == "" {
			continue
		}
		b.WriteString(s[pos:start])
		b.WriteString(base)
		pos = end
	}
	b.WriteString(s[pos:])
	return b.String()
}

var (
	diagnosticsDenyUserInfoPattern   = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/\s]+@`)
	diagnosticsDenyQueryPattern      = regexp.MustCompile(`\?[A-Za-z0-9_.\-]+=[^&\s"']+`)
	diagnosticsDenyEnvelopePattern   = regexp.MustCompile(`nxs[12]:[A-Za-z0-9+/=_-]+`)
	diagnosticsDenyAuthPattern       = regexp.MustCompile(`(?i)\b(bearer|authorization)\b\s*[:=]?\s*(?:(?:bearer|basic|digest|token)\s+)?[A-Za-z0-9._~+/=\-]{8,}`)
	diagnosticsDenySecretKVPattern   = regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|secret|token|password|passwd|pwd)\b\s*[:=]\s*("[^"]+"|'[^']*'|[A-Za-z0-9._~+/=\-]{8,})`)
	diagnosticsDenyCookiePattern     = regexp.MustCompile(`(?i)\bcookie\b\s*[:=]\s*[A-Za-z0-9._~+/=\-]{8,}`)
	diagnosticsDenyJSONSecretPattern = regexp.MustCompile(`(?i)"(api_key|apiKey|secret|token|password|access_token|refresh_token|session_token|cookie|authorization)"\s*:\s*"[^"]+"`)
)

// diagnosticsDenyScan is the second scan: it reports true when text
// still looks like it carries secret material. Matches that are only
// the "[REDACTED]" marker itself are not counted. Known secret values
// (length >= 4, as in the redactor) are checked as exact substrings.
func diagnosticsDenyScan(text string, secrets []string) bool {
	for _, secret := range secrets {
		if len(strings.TrimSpace(secret)) >= 4 && strings.Contains(text, secret) {
			return true
		}
	}
	patterns := []*regexp.Regexp{
		diagnosticsDenyUserInfoPattern,
		diagnosticsDenyQueryPattern,
		diagnosticsDenyEnvelopePattern,
		diagnosticsDenyAuthPattern,
		diagnosticsDenySecretKVPattern,
		diagnosticsDenyCookiePattern,
		diagnosticsDenyJSONSecretPattern,
	}
	for _, re := range patterns {
		for _, match := range re.FindAllString(text, -1) {
			if strings.Contains(match, "[REDACTED]") {
				continue
			}
			return true
		}
	}
	return false
}

// verifyBundleSafe scans the final serialized bundle. Any hit aborts
// the write; the caller must not leave a partial file behind. With a
// secret list it also rejects any exact known secret value; without
// one it still rejects structural secret patterns (URL user info and
// query strings, envelopes, auth headers, secret key=value pairs).
func verifyBundleSafe(data []byte, secrets []string) error {
	if diagnosticsDenyScan(string(data), secrets) {
		return fmt.Errorf("final bundle contains a value matching a secret pattern")
	}
	return nil
}

// writeDiagnosticsBundleAtomic mirrors the config/backup atomic-write
// pattern: temp file in the destination directory, mode 0600, fsync,
// rename, directory sync. The destination must already exist.
func writeDiagnosticsBundleAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nexaroute-diagnostics-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if info, err := os.Stat(path); err != nil {
		return err
	} else if info.Mode().Perm() != 0o600 {
		_ = os.Remove(path)
		return fmt.Errorf("bundle was written with mode %04o, want 0600; file removed", info.Mode().Perm())
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
