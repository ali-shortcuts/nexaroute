# Quality Audit Final Report - Issue #593

> **Historical document — superseded.** The body of this report is the
> 2026-10-21 checkpoint result, written before the work merged. Since
> then: the audit-tail program and the 2026-10-22 remaining-work
> mission merged (PRs #624–#634), ADRs 0001–0006 were **accepted**,
> issue #593 was **closed on 2026-10-22**, and **v0.24.0 was released**
> on 2026-10-22. Statements below such as "#593 remains open", "the
> branch is not merged", or "ADR … Proposed" describe the checkpoint
> moment only. The current state is recorded in the final addendum at
> the end of this file.

- Audit timestamp: **2026-10-21T11:33:58Z**
- Audited `main`: `bfa93b69cd9fc5041b2ddb822a49c03a32eeb13a`
- Work branch: `opencode/issue593-20261021110358`
- Code checkpoint: `41169e9a4c880dd25894b3ff39fe08abbb49cc42`
- Report/scanner-annotation follow-up: committed after the checkpoint on the same branch
- Release: `v0.23.0` unchanged; no new tag, release, or asset mutation
- Issue: [#593](https://github.com/ali-shortcuts/nexaroute/issues/593) remains open

## Outcome

The safe, high-confidence parts of Issue #593 are implemented on one branch:

1. **Trusted-proxy scheme support** (`tls.trusted_proxy_cidrs`) so admin cookies
   stay `Secure` behind an explicit TLS-terminating proxy without trusting
   forwarded headers from arbitrary peers.
2. **Scanner debt reduction**: staticcheck 46 -> 23, gosec 157 -> 98, dead-code
   inventory documented, all with triage rather than blanket suppression.
3. **Real defect fixes** surfaced by the scanners: recursive stream error loss,
   dead cooldown expression, overwritten probe budget, and a fake test loop.
4. **CI evidence workflow hardening**: repo-owned artifact paths, pinned actions,
   and permission-tolerant PR publication.
5. **Durable reports**: this file, the baseline, and the findings report now
   agree on SHA, counts, commands, and limitations.

The branch is **not merged**, and no claim is made that CI is green for the new
commit. The infrastructure creates the PR after the final response; CI must be
observed on that PR.

## What changed

### Security: trusted proxy

- `internal/config/config.go`: `TLSConfig.TrustedProxyCIDRs`,
  `ParseTrustedProxyCIDRs`, validation and limits.
- `internal/httpapi/config_snapshot.go`: parsed once per config revision.
- `internal/httpapi/server.go`: peer-address trust marker, strict
  `X-Forwarded-Proto` parsing, context-aware `requestIsSecure`.
- `internal/config/trusted_proxy_test.go` and
  `internal/httpapi/trusted_proxy_test.go`: parser, spoofing, cookie, and
  same-origin regression tests.
- `docs/CONFIGURATION.md` and `docs/SECURITY.md`: deployment contract.

### Defect and test fixes

- `internal/protocol/canonical/stream.go`: propagate recursive tool-start emit
  errors.
- `internal/httpapi/admin.go`: remove the dead `cooldown_active` expression.
- `internal/providers/http_adapter.go`: explicit `probeTokens = 1`, no
  caller-controlled probe budget.
- `internal/httpapi/budget_alerts_test.go`: remove the loop that always broke
  after one request.

### Scanner and workflow cleanup

- staticcheck: SA4004, SA4009, SA1012 (justified test ignore), SA4006/no-op, and
  style rules S1002/S1009/S1011/S1016/S1017/S1030/S1034/ST1018.
- gosec: G115 bounded/two's-complement conversions annotated; G705 fixed with
  `json.Encoder`; G104 error handling made explicit; G101/G124 false positives
  annotated with reasons.
- `.github/workflows/browser-evidence.yml`: `.artifacts/` under
  `${{ github.workspace }}` instead of `runner.temp`.
- `.github/workflows/browser-evidence-pr.yml`: pinned action SHAs and
  permission-tolerant publication that reports `blocked` with a manual compare
  URL instead of failing a successful evidence run.

## Verification evidence (final tree)

All commands were run in `/home/runner/work/nexaroute/nexaroute` and their full
output is under `.artifacts/issue593-final/`.

| Command | Result |
| --- | --- |
| `./scripts/verify.sh` | **EXIT 0**, `VERIFY PASS`; includes unit tests, `go vet`, mandatory race pass, release-artifact/SBOM trust-chain tests, short fuzz, and linux amd64/arm64 builds |
| `go test -timeout=40m -count=1 -coverprofile=.artifacts/issue593-final/coverage.out ./...` | **EXIT 0**, 57 packages `ok`, total **84.5%** statements |
| `staticcheck ./...` | **EXIT 1**, **23** diagnostics: 22 ST1005 style, 1 SA4017 proven false positive |
| `gosec -quiet -fmt=json ... ./...` | **98** findings: G304 54, G703 28, G204 12, G704 4; 28 justified `#nosec` |
| `deadcode -test ./...` | **EXIT 0**, **4** unreachable test/dispatch helpers |
| `GOTOOLCHAIN=go1.27.3 govulncheck ./...` | **EXIT 0**, **0 called vulnerabilities**; 1 module-level advisory not called |
| `gofmt -l .` | empty |
| `git diff --check` | clean (EXIT 0) |

### What is genuinely verified

- Unit, race, vet, fuzz-window, builds, and release trust-chain tests pass on the
  final tree.
- The trusted-proxy behavior is tested end to end at the HTTP handler level,
  including spoofing attempts and every affected cookie.
- Scanner counts are reproducible from the stored JSON/text artifacts.

### What is not verified

- **Browser acceptance**: `WARN: Chromium + Python Playwright unavailable;
  skipping browser acceptance`. This is a skip, not a pass.
- **CI for the new commit**: not observable locally; previous PR #595/#596 runs
  were successful, but that is not evidence for this branch.
- **Live integrations**: no PostgreSQL, Redis, OIDC, KMS, or provider
  credentials. Those paths were compiled and unit-tested only.
- **Load/HA/failure injection**: not run. No multi-replica breaker or durable
  video queue behavior is claimed.
- **Published release assets/signatures**: not re-verified; only the local
  trust-chain tests ran.
- **Gosec path/exec findings**: classified as operator-controlled, not proven
  harmless under a hostile-config threat model.

## Finding totals

| Classification | Count | Notes |
| --- | --- | --- |
| Confirmed defects fixed | **4** | Stream error loss, dead cooldown expression, probe budget, fake test loop |
| Security hardening delivered | **1** | Trusted-proxy scheme support |
| CI/evidence defects fixed | **1** | Permission-tolerant browser evidence publication |
| Confirmed false positives annotated | **21** | 14 G115, 6 G101, 6 G124, 1 SA4017 |
| Accepted style debt | **22** | ST1005 only; behavior-changing copy edits need owner approval |
| Accepted/classified scanner risk | **98** | G304/G703/G204/G704; operator-controlled boundary |
| Unverified areas | **6** | Browser, CI, live services, load/HA, release signing, live providers |

## Pull request and CI status

- Branch: `opencode/issue593-20261021110358`.
- No PR URL exists yet at report time because the local environment has no
  `GH_TOKEN`. The infrastructure will push and open the PR.
- Do **not** merge until the PR's required CI and security checks are green.
- Do **not** retag, re-release, or modify `v0.23.0` as part of this work.

## Remaining limitations and risks

1. The branch's CI has not been observed; any claim of green CI would be
   fabricated.
2. ST1005 errors remain; changing them alters client-visible error text and
   needs an explicit compatibility decision.
3. The 98 gosec findings are real code locations. They are accepted because the
   inputs are operator configuration, but a future "untrusted config" mode
   would need path rooting and process sandboxing.
4. Architecture debt (validate monolith, virtual-key duplication, distributed
   breaker, durable video queue, config write ownership) is documented with
   options, not implemented. No parallel half-migration was left behind.
5. Browser and staging evidence must be produced in CI/staging before a
   production-sufficiency claim.

## Owner decisions

1. **ST1005 copy change**: approve a client-visible error-string cleanup, or
   keep the current wording.
2. **Distributed breaker**: choose Redis/PostgreSQL shared state versus
   documented per-replica behavior.
3. **Durable video jobs**: choose PostgreSQL/Redis durability versus documented
   single-replica constraints.
4. **Config write ownership/HA**: approve a revision/consistency model before
   splitting the control plane.
5. **Release**: decide separately whether a new release is warranted. This audit
   made no release change.

## Recommendation

Merge this branch after CI and security checks are green. Then schedule, in
order:

1. ARCH-02 virtual-key service extraction (small, safe, testable).
2. ARCH-01 `Config.Validate` ordered extraction with first-error tests.
3. ARCH-03/ARCH-04 only after owner decisions on distributed state and video
   durability.
4. Browser/staging evidence and a hostile-config review of G304/G703/G204/G704.

The repository is materially cleaner and better tested than `bfa93b6`, but this
audit does **not** certify production readiness or zero vulnerabilities.

---

## Addendum — 2026-10-22 remaining-work mission outcome

- Mission date: **2026-10-22**
- Final `main`: `6ca8ac5f5d6949c1c7a5832df6472dbce375e784`
- Merged PRs: #624 (golangci triage), #625 (deadcode), #626 (breaker
  sync, section A), #627 (video cost ledger + asset integrity,
  section B), #628 (ADRs 0004–0006, sections C/D), #629 (coverage)
- Release: `v0.23.0` unchanged; no tag, release, or asset mutation.
  Issue #593 remains open — closure is the owner's decision.

### Verdict

Every completable item in the mission scope is complete and verified on
the final tree. The repository now measures **85.1%** coverage (target
≥85% met), **0** golangci-lint / staticcheck / deadcode findings, and
**53** gosec findings (all G304, all triaged). The two architecture
proposals this report previously left open as owner decisions 2 and 3
were resolved by implementation where the evidence supported it:
durable video jobs (already landed via #609/#623) plus a durable
exactly-once cost ledger (#627), and distributed breaker state as an
opt-in, default-off synchronization (#626, ADR 0003) rather than a
silent behavior change. Owner decision 4 (config write ownership) is
recorded in ADR 0001 as Proposed with a recommendation to keep local
config authoritative; three further reserved decisions are recorded in
ADRs 0004–0006 (multi-node video, autopilot, lint ratchet gating).
None of the four was settled by default.

### What is genuinely verified (this mission, new since the checkpoint)

- Breaker synchronization against **real PostgreSQL and real Redis**
  in CI: two-instance propagation, partition, heal, restart — pass,
  verified from job logs (not fixtures).
- Video cost-ledger exactly-once semantics against **real PostgreSQL**
  in CI: concurrent duplicates, conflicting settlement, restart
  recovery, invalid amounts, store failure — pass.
- External acceptance harness against **real public IdPs**: Google
  discovery + JWKS PASS; the GitHub Actions issuer correctly rejected
  (its discovery document has no authorization/token endpoints).
- Key-custody exercises with the shipped binary: master-key rotate /
  verify / decrypt round-trip, wrong-key fail-closed, keyring absence
  fail-closed (no fallback), Vault KV helper fail-closed, command
  key-provider protocol round-trip.
- Backup/restore drill on disposable state: backup, `doctor`
  preflight (10 pass / 4 accurate warnings / 0 fail), `restore
  --verify` PASS set, corrupted bundle and wrong key both FAIL with
  exit 1, real restore into a disposable directory verifies.
- Browser acceptance and axe accessibility scans green in CI on every
  mission PR head; SSE error propagation, no-mid-stream-failover,
  reasoning mapping and estimate labeling verified at source (F/H).

### What is still not verified (unchanged boundaries, stated plainly)

- Live tenant OIDC login (Authorization Code + PKCE with a real
  tenant), ID-token verification with an operator-issued token: NOT_RUN
  (no tenant credentials exist in this environment).
- A real Vault server round-trip and a real OS keyring round-trip:
  BLOCKED here (no Vault, no `secret-tool`/keyring daemon on this
  host); fail-closed behavior is verified, the happy path is not.
- Live LLM provider probes / paid calls: NOT_RUN (no provider
  credentials; zero calls made by design).
- Live Kubernetes install, real OTLP collector ingestion, live
  Azure/Vertex/Bedrock tenants, production multi-hour soak: UNVERIFIED.
- Vault Transit envelope mode: not implemented (documented limitation
  in `docs/EXTERNAL_VERIFICATION.md`); the provider contract loads raw
  key material into the gateway process.

### Remaining owner decisions

1. The four reserved consistency decisions (ADRs 0001, 0004, 0005, 0006).
2. Whether Activity needs a durable cross-process log (noted in
   `docs/HA_STATE_INVENTORY.md`).
3. Closure of issue #593 itself.
4. Any new release. This mission made no release change, per directive.

---

## Closure correction — 2026-10-22 (same day, after the addendum above)

The addendum above was written before the owner's delegation later that
day. For the record, its open items were then resolved:

- "Final `main` `6ca8ac5f…`" was the final commit of the remaining-work
  mission only; PRs #631–#634 followed the same day.
- "Release: `v0.23.0` unchanged" — superseded: **v0.24.0 was released
  2026-10-22T13:33:57Z** under the owner's explicit instruction, with
  download-side verification (checksums, SBOM, Sigstore) complete.
- "Issue #593 remains open" — superseded: **#593 was closed on
  2026-10-22** with a full evidence comment.
- "**53** gosec findings (all G304)" — retracted: the complete analysis
  (see the correction in `QUALITY_AUDIT_FINDINGS.md`) finds **87**
  (53 G304 + 30 G703 + 4 G704), all triaged into the checked-in
  baseline `docs/reports/gosec-baseline.txt`; the audit job is pinned
  to Go 1.27.0 and fails loudly on incomplete analysis.
- "Remaining owner decisions" items 1, 3, 4 — resolved: ADRs 0001,
  0004, 0005, 0006 were **accepted** on 2026-10-22 under the owner's
  delegation (PRs #631–#633); #593 closed; v0.24.0 released. Item 2
  (durable cross-process Activity log) remains open as a documented
  per-instance limitation in `docs/HA_STATE_INVENTORY.md`; it is a
  known boundary, not a defect found by this audit.

---

## Final completion addendum — 2026-10-23

The owner ordered a fresh zero-to-hundred completion pass on
2026-10-23 ("complete anything that is not complete"). The mission
re-established state from live sources rather than trusting prior
reports, re-audited the tree in five lanes (breaker sync / docs /
security / video durability / routing-runtime), fixed every confirmed
defect test-first, and reconciled the documentation.

- Final `main`: `79df87d5` (after PRs #638, #639, #640, #641 on top
  of `8fcd2499`). Open PRs: 0. Open issues: 0. CodeQL open alerts: 0.
  Dependabot open alerts: 0.
- Work completed: PR #635 (CodeQL cookie fix, earlier the same day),
  PR #636 (config `Validate` refactor with golden-corpus equivalence),
  PR #637 (`evaluation.live_enabled` coherence), PR #638 (video bearer
  fail-closed hardening), PR #639 (video durability: download checksum,
  PG terminal-replay guard, loud ledger reads), PR #640 (protocol
  fail-closed: D1–D3, S2–S4), PR #641 (blank-system round-trip fix,
  F-11, found by the extended-fuzz job) — see
  `QUALITY_AUDIT_FINDINGS.md`, 2026-10-23 addendum, for the full
  register.
- Verification on the final tree: CI + Security workflows green on
  the exact merge heads; Architecture & Quality Audit workflow run
  `39945085701` on `79df87d5`: all seven jobs SUCCESS — golangci-lint
  0, staticcheck 0, deadcode 0, gosec at the checked-in baseline of
  87 (0 new / 0 resolved), coverage `85.3%` (gate ≥ 85%), extended
  fuzz SUCCESS, metrics SUCCESS. (The earlier audit run `39941279599`
  on `1ba6ea32` failed only in the extended-fuzz job; that failure is
  finding F-11, fixed by PR #641.) Local `scripts/verify.sh` on the
  final tree `79df87d5`: full PASS, including the mandatory race
  pass. (On more heavily loaded runs of this VM the local race pass
  can stop at the 3-minute cap in `internal/httpapi` with zero
  assertion failures — the capacity artifact A/B-proven against
  pristine main during PR #636; post-merge CI on `79df87d5` is
  SUCCESS and corroborates the race pass.)
- Acceptance detail: `docs/reports/ACCEPTANCE_MATRIX_2026-10-23.md`.
- Standing UNVERIFIED boundaries are unchanged and are recorded in
  `docs/EXTERNAL_VERIFICATION.md`: live tenant OIDC login, real
  Vault/OS-keyring round-trips, live commercial provider probes
  (zero paid calls), live Bedrock/Vertex/Azure tenants, live K8s
  install, real OTLP ingestion, production soak, multi-host HA.
  Milestone closeouts M3-5 / M4-3 / M4-4 still require the owner's
  explicit approval each.
- Verdict of the 2026-10-23 mission: **IMPLEMENTATION COMPLETE,
  EXTERNAL ACCEPTANCE BLOCKED** — implementation is complete within
  the declared scope; the external-acceptance items above remain
  blocked on credentials/infrastructure the mission did not have.
