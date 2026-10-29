// Package privacyaudit implements the read-only `nexaroute privacy audit`
// checks (checklist item M3-2). It inspects a loaded configuration and the
// runtime policy surfaces derived from it (route profiles, candidate pools,
// virtual keys, egress policy) and reports privacy findings that can be
// determined locally, without sending any provider request.
//
// Findings never contain secret values: scopes name configuration objects
// (provider IDs, profile IDs, key IDs) and messages describe the field and
// the remediation, never the credential, header value, or URL userinfo.
package privacyaudit

import (
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
	"time"
)

// Severity ranks findings. Error findings fail the audit (exit code 1 in
// the CLI); warnings and infos are advisory.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Finding is one actionable audit observation.
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Scope    string   `json:"scope"`
	Message  string   `json:"message"`
}

// Report is the deterministic audit result.
type Report struct {
	Findings []Finding `json:"findings"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
	Infos    int       `json:"infos"`
}

// HasErrors reports whether the audit found any error-severity problem.
func (r Report) HasErrors() bool { return r.Errors > 0 }

func severityRank(s Severity) int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

// Audit runs every local privacy check against cfg. The config is expected
// to be the on-disk representation (secret fields may hold plaintext or
// nxs1:-encrypted values); Audit only reads it and never mutates it.
func Audit(cfg *config.Config) Report {
	a := &auditor{cfg: cfg}
	a.checkProviderSecretsAndURLs()
	a.checkDecisionProviders()
	a.checkDataHandling()
	a.checkNoTrainingGuarantees()
	a.checkEgressPolicy()
	a.checkCacheAndLogging()
	return a.report()
}

type auditor struct {
	cfg      *config.Config
	findings []Finding
}

func (a *auditor) add(code string, sev Severity, scope, format string, args ...any) {
	a.findings = append(a.findings, Finding{
		Code:     code,
		Severity: sev,
		Scope:    scope,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (a *auditor) report() Report {
	sort.SliceStable(a.findings, func(i, j int) bool {
		ri, rj := severityRank(a.findings[i].Severity), severityRank(a.findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if a.findings[i].Code != a.findings[j].Code {
			return a.findings[i].Code < a.findings[j].Code
		}
		return a.findings[i].Scope < a.findings[j].Scope
	})
	rep := Report{Findings: a.findings}
	if rep.Findings == nil {
		rep.Findings = []Finding{}
	}
	for _, f := range rep.Findings {
		switch f.Severity {
		case SeverityError:
			rep.Errors++
		case SeverityWarning:
			rep.Warnings++
		default:
			rep.Infos++
		}
	}
	return rep
}

func isEncryptedValue(v string) bool {
	return secrets.IsEnvelope(v)
}

// checkProviderSecretsAndURLs flags plaintext credentials at rest and
// credential-bearing or cleartext provider endpoints.
func (a *auditor) checkProviderSecretsAndURLs() {
	for _, p := range a.cfg.Providers {
		scope := "provider:" + p.ID
		if p.APIKey != "" && !isEncryptedValue(p.APIKey) {
			a.add("PLAINTEXT_SECRET", SeverityError, scope,
				"provider %s stores api_key as plaintext in the config file; run the gateway once so it is encrypted at rest, or use api_key_env", p.ID)
		}
		for i, c := range p.Credentials {
			if c.APIKey != "" && !isEncryptedValue(c.APIKey) {
				a.add("PLAINTEXT_SECRET", SeverityError, scope,
					"provider %s credentials[%d].api_key is plaintext in the config file; it will be encrypted at rest on next gateway start", p.ID, i)
			}
		}
		// The persistence layer (secrets.TransformConfig) treats EVERY
		// provider header value as secret-bearing and encrypts it at rest,
		// regardless of the header name. The audit mirrors that contract:
		// any non-empty, non-encrypted header value is a plaintext secret
		// in the stored config. Only the header name is reported, never
		// the value.
		for name, v := range p.Headers {
			if v != "" && !isEncryptedValue(v) {
				a.add("PLAINTEXT_SECRET", SeverityError, scope,
					"provider %s header %q stores a plaintext value in the config file; header values are encrypted at rest, so run the gateway once to migrate it", p.ID, name)
			}
		}
		// proxy_url is likewise a secret-bearing field in the persistence
		// contract (the whole value is encrypted at rest).
		if p.ProxyURL != "" && !isEncryptedValue(p.ProxyURL) {
			a.add("PLAINTEXT_SECRET", SeverityError, scope,
				"provider %s proxy_url is plaintext in the config file; it is a secret-bearing field and will be encrypted at rest on next gateway start", p.ID)
		}
		a.checkURL(scope, "base_url", p.BaseURL)
		// For the proxy, only embedded credentials are flagged: the
		// gateway's own egress policy accepts http proxies, so a
		// cleartext scheme alone is not a finding here.
		a.checkURLCredentials(scope, "proxy_url", p.ProxyURL)
	}
	a.checkStoredSecrets()
}

// checkStoredSecrets covers the remaining secret-bearing fields of the
// persistence contract that live outside provider configs: the admin API
// key and the plain client-auth keys. Virtual-key entries store only key
// hashes and are not secret-bearing.
func (a *auditor) checkStoredSecrets() {
	if a.cfg.Admin.APIKey != "" && !isEncryptedValue(a.cfg.Admin.APIKey) {
		a.add("PLAINTEXT_SECRET", SeverityError, "admin",
			"the admin api_key is plaintext in the config file; it will be encrypted at rest on next gateway start")
	}
	for i, k := range a.cfg.ClientAuth.Keys {
		if k != "" && !isEncryptedValue(k) {
			a.add("PLAINTEXT_SECRET", SeverityError, "client_auth",
				"client_auth.keys[%d] is plaintext in the config file; client keys are encrypted at rest, so run the gateway once to migrate it", i)
		}
	}
}

// checkURL flags embedded URL credentials (never printing the URL itself)
// and cleartext http endpoints on non-loopback hosts.
func (a *auditor) checkURL(scope, field, raw string) {
	u := parseURL(raw)
	if u == nil {
		return
	}
	if u.User != nil {
		a.add("URL_CREDENTIALS", SeverityError, scope,
			"%s contains embedded userinfo credentials; move them to the dedicated credential fields", field)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		a.add("CLEARTEXT_ENDPOINT", SeverityError, scope,
			"%s uses http:// with a non-loopback host; credentials and content would travel unencrypted", field)
	}
}

// checkURLCredentials flags only embedded URL credentials.
func (a *auditor) checkURLCredentials(scope, field, raw string) {
	u := parseURL(raw)
	if u == nil {
		return
	}
	if u.User != nil {
		a.add("URL_CREDENTIALS", SeverityError, scope,
			"%s contains embedded userinfo credentials; move them to the dedicated credential fields", field)
	}
}

func parseURL(raw string) *url.URL {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil // malformed URLs are a config-validation concern, reported elsewhere
	}
	return u
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

// checkDecisionProviders applies the stored-secret and URL-credential
// checks to external decision providers (they receive request metadata).
// Stored-configuration checks run regardless of the enabled flag, matching
// the persistence layer, which transforms decision-provider API keys
// without consulting their enabled state.
func (a *auditor) checkDecisionProviders() {
	for _, d := range a.cfg.DecisionProviders {
		scope := "decision_provider:" + d.ID
		if d.APIKey != "" && !isEncryptedValue(d.APIKey) {
			a.add("PLAINTEXT_SECRET", SeverityError, scope,
				"decision provider %s stores api_key as plaintext in the config file", d.ID)
		}
		a.checkURL(scope, "base_url", d.BaseURL)
	}
}

// checkDataHandling notes providers whose data-handling class is undeclared:
// the runtime no_training gate is fail-closed, so an undeclared class can
// never satisfy a no_training requirement.
func (a *auditor) checkDataHandling() {
	for _, p := range a.cfg.Providers {
		if !p.Enabled {
			continue
		}
		trains := strings.ToLower(strings.TrimSpace(p.DataHandling.TrainsOnData))
		if trains == "" || trains == "unknown" {
			a.add("DATA_HANDLING_UNDECLARED", SeverityInfo, "provider:"+p.ID,
				"provider %s does not declare data_handling.trains_on_data; it is treated as not satisfying no_training requirements", p.ID)
		}
	}
}

// deployments builds the runtime deployment set and pool expansion exactly
// as the serving path does, so audit conclusions match runtime behavior.
func (a *auditor) deployments() (*route.Resolver, []router.Deployment) {
	r := router.New(*a.cfg, health.New(5, time.Hour))
	all := r.All()
	return route.NewResolver(*a.cfg, all), all
}

func poolSatisfaction(resolver *route.Resolver, byID map[string]router.Deployment, poolID string) (total, satisfying int, known bool) {
	set, ok := resolver.GetExpanded(poolID)
	if !ok {
		return 0, 0, false
	}
	for id := range set {
		d, ok := byID[id]
		if !ok {
			continue
		}
		total++
		if router.SatisfiesNoTraining(d.TrainsOnData) {
			satisfying++
		}
	}
	return total, satisfying, true
}

// noTrainingVerdict emits the finding for one no_training requirement,
// given the size of its reachable deployment set and how many members
// satisfy the requirement. Route profiles and virtual keys share this
// verdict so both surfaces report partial compliance consistently.
func (a *auditor) noTrainingVerdict(scope, subject string, total, satisfying int, tail string) {
	switch {
	case total == 0:
		a.add("NO_TRAINING_UNSATISFIABLE", SeverityError, scope,
			"%s but it can reach no deployments; %s", subject, tail)
	case satisfying == 0:
		a.add("NO_TRAINING_UNSATISFIABLE", SeverityError, scope,
			"%s but none of its %d reachable deployments declare trains_on_data=no; %s", subject, total, tail)
	case satisfying < total:
		a.add("NO_TRAINING_PARTIAL", SeverityWarning, scope,
			"%s; %d of %d reachable deployments satisfy it and the rest are excluded at runtime", subject, satisfying, total)
	}
}

// checkNoTrainingGuarantees cross-checks no_training requirements from
// route profiles and virtual keys against the deployments they can reach.
func (a *auditor) checkNoTrainingGuarantees() {
	resolver, all := a.deployments()
	byID := make(map[string]router.Deployment, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}
	totalSatisfying := 0
	for _, d := range all {
		if router.SatisfiesNoTraining(d.TrainsOnData) {
			totalSatisfying++
		}
	}

	for _, rp := range a.cfg.RouteProfiles {
		scope := "route_profile:" + rp.ID
		poolID := strings.TrimSpace(rp.CandidatePool)
		if poolID == "" {
			// Candidate pool is required by Config.Validate and by the runtime
			// resolver. Do not treat an absent pool as unrestricted: that would
			// disagree with runtime behavior and could hide a broken route.
			a.add("PROFILE_POOL_UNKNOWN", SeverityError, scope,
				"route profile %s has no candidate_pool configured", rp.ID)
			continue
		}
		if _, known := resolver.GetExpanded(poolID); !known {
			a.add("PROFILE_POOL_UNKNOWN", SeverityError, scope,
				"route profile %s references unknown candidate pool %q", rp.ID, poolID)
			continue
		}
		if rp.Privacy != "no_training" {
			continue
		}
		total, satisfying, _ := poolSatisfaction(resolver, byID, poolID)
		a.noTrainingVerdict(scope,
			fmt.Sprintf("route profile %s requires no_training", rp.ID),
			total, satisfying, "requests will fail closed")
	}

	profilesByID := make(map[string]config.RouteProfileConfig, len(a.cfg.RouteProfiles))
	for _, rp := range a.cfg.RouteProfiles {
		profilesByID[rp.ID] = rp
	}
	for _, k := range a.cfg.ClientAuth.VirtualKeys {
		if k.Revoked || k.RequirePrivacy != "no_training" {
			continue
		}
		scope := "virtual_key:" + k.ID
		if len(k.AllowedRoutes) == 0 {
			// Unrestricted key: the relevant set is every deployment.
			a.noTrainingVerdict(scope,
				fmt.Sprintf("virtual key %s requires no_training", k.ID),
				len(all), totalSatisfying, "its requests will fail closed")
			continue
		}
		// Union over the key's allowed route profiles' pools, counting
		// each deployment once even when routes overlap.
		seen := map[string]bool{}
		total, satisfying := 0, 0
		for _, routeID := range k.AllowedRoutes {
			rp, ok := profilesByID[routeID]
			if !ok {
				a.add("VIRTUAL_KEY_ROUTE_UNKNOWN", SeverityError, scope,
					"virtual key %s lists allowed route %q, which does not match any configured route profile", k.ID, routeID)
				continue
			}
			set, known := resolver.GetExpanded(rp.CandidatePool)
			if !known {
				continue // the profile itself is reported via PROFILE_POOL_UNKNOWN
			}
			for id := range set {
				if seen[id] {
					continue
				}
				seen[id] = true
				if d, ok := byID[id]; ok {
					total++
					if router.SatisfiesNoTraining(d.TrainsOnData) {
						satisfying++
					}
				}
			}
		}
		a.noTrainingVerdict(scope,
			fmt.Sprintf("virtual key %s requires no_training", k.ID),
			total, satisfying, "its requests will fail closed")
	}
}

// checkEgressPolicy notes the absence of an operator egress deny policy.
func (a *auditor) checkEgressPolicy() {
	if len(a.cfg.Egress.DenyHosts) == 0 && len(a.cfg.Egress.DenyCIDRs) == 0 {
		a.add("EGRESS_DENY_POLICY_EMPTY", SeverityInfo, "egress",
			"no operator egress deny policy is configured (egress.deny_hosts/deny_cidrs are empty); the built-in special-purpose IP blocklist still applies")
	}
}

// checkCacheAndLogging surfaces local data-retention relevant settings.
func (a *auditor) checkCacheAndLogging() {
	if a.cfg.Cache.Enabled {
		a.add("RESPONSE_CACHE_ENABLED", SeverityInfo, "cache",
			"the exact-match response cache is enabled; matching responses are stored locally and replayed, so confirm this matches your data-retention requirements")
	}
	if strings.EqualFold(strings.TrimSpace(a.cfg.Logging.AccessMode), "all") {
		a.add("ACCESS_LOG_ALL", SeverityWarning, "logging",
			"logging.access_mode is \"all\"; every request is access-logged, so confirm log storage and retention match your privacy requirements")
	}
}
