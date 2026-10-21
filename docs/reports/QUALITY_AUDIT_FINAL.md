# Quality Audit Final Report - Issue #593

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
