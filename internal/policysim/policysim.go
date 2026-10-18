// Package policysim evaluates decision policies offline against
// recorded-shape decision requests (M6-2). It drives the REAL
// policy provider, so a simulation is exactly what production
// would decide for the same eligible set and signals — with no
// traffic served. Reports are canonical (stable ordering, no
// timestamps) and carry a SHA-256 digest: identical inputs always
// produce an identical digest.
package policysim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/policy"
)

// Fixture is one simulation case: a named DecisionRequest in the
// exact shape the orchestrator builds at runtime.
type Fixture struct {
	Name    string                   `json:"name"`
	Request decision.DecisionRequest `json:"request"`
}

// FixtureFile is the simulate input document.
type FixtureFile struct {
	Cases []Fixture `json:"cases"`
}

// ReplayRecord is one recorded production decision: the request
// as built at runtime and the winner production actually chose.
type ReplayRecord struct {
	Request        decision.DecisionRequest `json:"request"`
	RecordedWinner string                   `json:"recorded_winner"`
	// PolicyVersion is the policy version that produced RecordedWinner.
	// When set, replay flags a mismatch against the loaded policy version so a
	// decision can never be silently attributed to the wrong policy revision.
	PolicyVersion string `json:"policy_version,omitempty"`
}

// CaseResult is one evaluated case.
type CaseResult struct {
	Name      string   `json:"name"`
	Winner    string   `json:"winner,omitempty"`
	Ordered   []string `json:"ordered,omitempty"`
	Abstained bool     `json:"abstained,omitempty"`
	// Breakdown is the policy engine's own score explanation
	// (policy.Explain), included verbatim for review.
	Breakdown string `json:"breakdown,omitempty"`
}

// Report is the canonical simulation output.
type Report struct {
	PolicyID      string       `json:"policy_id"`
	PolicyVersion string       `json:"policy_version,omitempty"`
	Cases         []CaseResult `json:"cases"`
	Digest        string       `json:"digest"`
}

// ReplayCase is one replayed decision compared with history.
type ReplayCase struct {
	Name           string `json:"name"`
	Winner         string `json:"winner,omitempty"`
	RecordedWinner string `json:"recorded_winner"`
	Agree          bool   `json:"agree"`
	// VersionMatch is true when the record carried no policy version or the
	// recorded version equals the loaded policy version.
	VersionMatch bool `json:"version_match"`
}

// ReplayReport summarizes a replay run.
type ReplayReport struct {
	PolicyID          string       `json:"policy_id"`
	PolicyVersion     string       `json:"policy_version,omitempty"`
	Total             int          `json:"total"`
	Agreed            int          `json:"agreed"`
	VersionMismatches int          `json:"version_mismatches"`
	Cases             []ReplayCase `json:"cases"`
	Digest            string       `json:"digest"`
}

// LoadPolicies converts the config's decision policies into engine
// policies, failing on the first invalid one.
func LoadPolicies(cfg config.Config) ([]policy.Policy, error) {
	out := make([]policy.Policy, 0, len(cfg.DecisionPolicies))
	for _, pc := range cfg.DecisionPolicies {
		p, err := policy.FromConfig(pc)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func providerFor(cfg config.Config, policyID string) (*policy.Provider, policy.Policy, error) {
	policies, err := LoadPolicies(cfg)
	if err != nil {
		return nil, policy.Policy{}, err
	}
	for _, p := range policies {
		if p.ID == policyID {
			return policy.NewProvider(policies, policyID), p, nil
		}
	}
	return nil, policy.Policy{}, fmt.Errorf("policy %q is not defined in decision_policies", policyID)
}

func evaluate(ctx context.Context, provider *policy.Provider, req decision.DecisionRequest) CaseResult {
	res, err := provider.Decide(ctx, req)
	if err != nil {
		return CaseResult{Abstained: true}
	}
	cr := CaseResult{Abstained: res.Abstained}
	// Effective winner: an explicit selection wins; when the
	// policy ABSTAINS because its top score IS the router's
	// original primary ("preserve existing order"), the trace
	// names the preserved winner. The effective order is the
	// winner first, then the remaining candidates in request
	// order — exactly what the gateway would serve.
	cr.Winner = res.SelectedID
	if cr.Winner == "" && res.PolicyTrace != nil {
		cr.Winner = res.PolicyTrace.SelectedID
	}
	if len(res.RankedIDs) > 0 {
		cr.Ordered = append([]string(nil), res.RankedIDs...)
	} else if cr.Winner != "" {
		cr.Ordered = []string{cr.Winner}
		for _, c := range req.Candidates {
			if c.ID != cr.Winner {
				cr.Ordered = append(cr.Ordered, c.ID)
			}
		}
	}
	// The breakdown is the provider's own policy trace (selected
	// vs original primary, component scores and weights) — the
	// faithful record of why the winner won, canonicalized as JSON
	// (encoding/json emits map keys in sorted order).
	if res.PolicyTrace != nil {
		if raw, err := json.Marshal(res.PolicyTrace); err == nil {
			cr.Breakdown = string(raw)
		}
	}
	return cr
}

func digestOf(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Simulate evaluates every fixture under the named policy.
func Simulate(ctx context.Context, cfg config.Config, policyID string, fixtures []Fixture) (Report, error) {
	provider, pol, err := providerFor(cfg, policyID)
	if err != nil {
		return Report{}, err
	}
	report := Report{PolicyID: policyID, PolicyVersion: pol.Version, Cases: make([]CaseResult, 0, len(fixtures))}
	for _, fx := range fixtures {
		req := fx.Request
		req.PolicyID = policyID
		cr := evaluate(ctx, provider, req)
		cr.Name = fx.Name
		report.Cases = append(report.Cases, cr)
	}
	report.Digest = digestOf(struct {
		PolicyID      string       `json:"policy_id"`
		PolicyVersion string       `json:"policy_version,omitempty"`
		Cases         []CaseResult `json:"cases"`
	}{report.PolicyID, report.PolicyVersion, report.Cases})
	return report, nil
}

// Replay re-evaluates recorded decisions under the named policy
// and diffs the winners against history.
func Replay(ctx context.Context, cfg config.Config, policyID string, records []ReplayRecord) (ReplayReport, error) {
	provider, pol, err := providerFor(cfg, policyID)
	if err != nil {
		return ReplayReport{}, err
	}
	report := ReplayReport{PolicyID: policyID, PolicyVersion: pol.Version, Total: len(records), Cases: make([]ReplayCase, 0, len(records))}
	for i, rec := range records {
		req := rec.Request
		req.PolicyID = policyID
		cr := evaluate(ctx, provider, req)
		versionMatch := rec.PolicyVersion == "" || rec.PolicyVersion == pol.Version
		rc := ReplayCase{
			Name:           fmt.Sprintf("case-%d", i+1),
			Winner:         cr.Winner,
			RecordedWinner: rec.RecordedWinner,
			Agree:          cr.Winner == rec.RecordedWinner,
			VersionMatch:   versionMatch,
		}
		if rc.Agree {
			report.Agreed++
		}
		if !versionMatch {
			report.VersionMismatches++
		}
		report.Cases = append(report.Cases, rc)
	}
	report.Digest = digestOf(struct {
		PolicyID          string       `json:"policy_id"`
		PolicyVersion     string       `json:"policy_version,omitempty"`
		Cases             []ReplayCase `json:"cases"`
		VersionMismatches int          `json:"version_mismatches"`
	}{report.PolicyID, report.PolicyVersion, report.Cases, report.VersionMismatches})
	return report, nil
}
