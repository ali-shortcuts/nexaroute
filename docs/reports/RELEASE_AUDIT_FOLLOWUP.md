# Release audit follow-up — partial, not an acceptance review

Snapshot reviewed: `8d2ae57f03b95a81d7282eaeeeb6261ab0cda458`, main
`436c4e98618eed0c41e59059eae7eca5f05dd190`. Later follow-up changes require
fresh checks and independent review. The full dashboard, backend and security
audit remains incomplete; this register is not exhaustive.

| Severity | Finding / path | Remediation and verification | Status |
|---|---|---|---|
| High / release-control | A v0.25.0 tag at an old main ancestor can execute that ancestor's old release workflow and gate; the current workflow only checks ancestry. Privileged manual publication is also outside scripts. | The current strict gate checks exact main tip via API and rejects conflicting tag refs; this cannot change historical workflow code or prevent manual publication. Owner must enforce tag/release permissions and independently verify tag target and current gate. | OPEN; repository policy / operator control required. |
| Medium / availability | `.github/scripts/check-v025-acceptance.py` used `gh api --slurp`, unsupported by installed gh 2.23. Real evidence could not be checked even when correct. Existing fake ignored CLI flags. | Parse every concatenated `gh api --paginate` JSON page, reject malformed responses and duplicate keys; fixtures emulate the older CLI and multi-page stream. Actual gh API comment-list parsing was exercised, but no genuine acceptance record was asserted. | Fix proposed; independent review required. |
| Medium / release-control | An explicit version could override a v0.25.0 tag if the ref name were absent but `GITHUB_REF` named the tag. | Derive tag from `GITHUB_REF`, reject ref/name mismatch and alternate-version override; offline regressions cover both. This does not make arbitrary caller-controlled environment trustworthy. | Fix proposed; independent review required. |
| Low / release-control | Strict failure message suggested approved exclusions could resolve v0.25.0; duplicate manifest keys could be parsed last-value-wins. | Strict-specific refusal text and duplicate-key validation; fixture regression. Historical unsigned manifest remains untouched. | Fix proposed; independent review required. |
| Medium / operational handoff | `docs/RELEASE_BLOCKERS.md` described runner-local OIDC, Vault and single-host HA dispatches as if they provided organizational/staging/multi-host evidence. | Corrected workflow evidence classes and added explicit external operator steps and provider-budget stops. | Documentation fix proposed; independent review required. |
| Unassessed | Full authentication, authorization, tenant isolation, SSRF, logging, dependencies, migrations and state-transition audit; all dashboard pages and responsive/accessibility interactions. | Prior static scan covers selected direct request-body reads only; no general taint claim. CodeQL and fixture/browser CI do not replace manual or real-environment review. | OPEN. |

Targeted inspection of `Service.Publish` found it requires `ConditionalPublisher`;
unsupported custom stores fail closed. Memory uses the store lock, file uses
its lock and persistence path, PostgreSQL uses a transaction/row lock. Existing
unit/contract tests and a real two-pool PostgreSQL test exercise the stale-draft
boundary. This inspection does not establish that every migration, persistence
failure or concurrency edge case has been audited. The editor has no caller-
supplied draft revision precondition before the publish request.

The chart maps the backend lifecycle states separately, uses a rolling
half-open interval, escapes its SVG label, and has refresh/error tests. Local
`node scripts/test-activity-refresh.js` and `node scripts/test-workflow-config.js`
passed; these use fixtures, not live providers. Wider page-by-page responsive,
keyboard, screen-reader and real-data checks remain outstanding.
