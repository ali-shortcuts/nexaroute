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
	return strings.HasPrefix(v, secrets.Prefix)
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
		for name, v := range p.Headers {
			lname := strings.ToLower(strings.TrimSpace(name))
			if (lname == "authorization" || lname == "x-api-key" || lname == "api-key") && v != "" && !isEncryptedValue(v) {
				a.add("PLAINTEXT_SECRET", SeverityError, scope,
					"provider %s header %q carries a plaintext credential in the config file", p.ID, name)
			}
		}
		a.checkURL(scope, "base_url", p.BaseURL)
		a.checkURL(scope, "proxy_url", p.ProxyURL)
	}
}

// checkURL flags embedded URL credentials (never printing the URL itself)
// and cleartext http endpoints on non-loopback hosts.
func (a *auditor) checkURL(scope, field, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return // malformed URLs are a config-validation concern, reported elsewhere
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

// checkDecisionProviders applies the same secret/URL checks to external
// decision providers (they receive request metadata).
func (a *auditor) checkDecisionProviders() {
	for _, d := range a.cfg.DecisionProviders {
		if !d.IsEnabled() {
			continue
		}
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
		if strings.TrimSpace(rp.CandidatePool) != "" {
			if _, known := resolver.GetExpanded(rp.CandidatePool); !known {
				a.add("PROFILE_POOL_UNKNOWN", SeverityError, scope,
					"route profile %s references unknown candidate pool %q", rp.ID, rp.CandidatePool)
				continue
			}
		}
		if rp.Privacy != "no_training" {
			continue
		}
		var total, satisfying int
		if strings.TrimSpace(rp.CandidatePool) == "" {
			// No pool restriction: the runtime gate applies to every
			// deployment, so evaluate the whole set.
			total, satisfying = len(all), totalSatisfying
		} else {
			total, satisfying, _ = poolSatisfaction(resolver, byID, rp.CandidatePool)
		}
		switch {
		case total == 0:
			a.add("NO_TRAINING_UNSATISFIABLE", SeverityError, scope,
				"route profile %s requires no_training but it can reach no deployments", rp.ID)
		case satisfying == 0:
			a.add("NO_TRAINING_UNSATISFIABLE", SeverityError, scope,
				"route profile %s requires no_training but none of its %d candidate deployments declare trains_on_data=no; requests will fail closed", rp.ID, total)
		case satisfying < total:
			a.add("NO_TRAINING_PARTIAL", SeverityWarning, scope,
				"route profile %s requires no_training; %d of %d candidate deployments satisfy it and the rest are excluded at runtime", rp.ID, satisfying, total)
		}
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
			if totalSatisfying == 0 {
				a.add("NO_TRAINING_UNSATISFIABLE", SeverityError, scope,
					"virtual key %s requires no_training but no enabled deployment declares trains_on_data=no; its requests will fail closed", k.ID)
			}
			continue
		}
		// Union over the key's allowed route profiles' pools.
		seen := map[string]bool{}
		total, satisfying := 0, 0
		for _, routeID := range k.AllowedRoutes {
			rp, ok := profilesByID[routeID]
			if !ok {
				continue
			}
			set, known := resolver.GetExpanded(rp.CandidatePool)
			if !known {
				continue
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
		if satisfying == 0 {
			a.add("NO_TRAINING_UNSATISFIABLE", SeverityError, scope,
				"virtual key %s requires no_training but none of the deployments reachable through its allowed routes declare trains_on_data=no; its requests will fail closed", k.ID)
		}
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
