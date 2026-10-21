# Quality Audit Findings - Issue #593

- Audit timestamp: **2026-10-21T11:33:58Z**
- Audited `main`: `bfa93b69cd9fc5041b2ddb822a49c03a32eeb13a`
- Work branch: `opencode/issue593-20261021110358`
- Checkpoint commit: `41169e9a4c880dd25894b3ff39fe08abbb49cc42` (plus report/final lint
  fixes in the follow-up commit on the same branch)
- Release `v0.23.0` is unchanged.

This report is authoritative for the current checkpoint. Earlier PR #595/#596
findings are summarized under "Already merged" below; full text remains in git
history.

## Summary counts

| Class | Count | Status |
| --- | --- | --- |
| Confirmed code/test defects fixed in this checkpoint | **4** | Fixed with tests or verified behavior preservation |
| Security hardening delivered | **1** | Trusted-proxy scheme configuration |
| CI/evidence workflow defects fixed | **1** | Browser evidence PR publication |
| Staticcheck findings before -> after | **46 -> 23** | 22 ST1005 style, 1 proven SA4017 false positive |
| Gosec findings before -> after | **157 -> 98** | G115/G705/G104 cleared; G101/G124 false positives annotated |
| Confirmed false positives annotated | **21** | 14 G115, 6 G101, 6 G124, 1 SA4017 |
| Unverified / environment-limited | **6 areas** | Live PG/Redis/OIDC, browser, CI for new SHA, load, release signing, provider live APIs |
| Dead code (test-inclusive) | **4** | Unreachable test-only helpers; no production leak found |
| Repository statement coverage | **84.5%** | `go test ./...` profile, whole repository |

Do not read "98 gosec findings" as 98 exploitable vulnerabilities. The remaining
rules are concentrated in operator-controlled file paths and command execution;
their dispositions are below. No finding was closed by disabling a check or
deleting a test.

## Confirmed defects and fixes

### SEC-01: proxy deployments could not mark cookies Secure

**Symptom.** Behind a TLS-terminating reverse proxy, `requestIsSecure` returned
`false` because the gateway saw plain HTTP. Admin session, CSRF, and OIDC flow
cookies therefore lost the `Secure` attribute even though the browser-facing
connection was HTTPS. Enabling `Secure` unconditionally would have broken
plain-HTTP deployments, and trusting `X-Forwarded-Proto` from any peer would let
a direct client turn it on or off at will.

**Fix.** A new explicit list, `tls.trusted_proxy_cidrs`, identifies proxy
networks. `withTrustedProxyContext` marks a request only when its *peer address*
is inside a listed network; `requestIsSecure` honors a single unambiguous
`X-Forwarded-Proto: https` only for those requests. Direct TLS is always secure.
Unlisted peers, `Forwarded`-only requests, duplicate/chained values, and empty
values are rejected.

**Files.** `internal/config/config.go:485`, `internal/config/config.go:505`,
`internal/httpapi/config_snapshot.go:25`, `internal/httpapi/server.go:1438`,
`internal/httpapi/server.go:1459`, `internal/httpapi/server.go:1472`,
`docs/CONFIGURATION.md`, `docs/SECURITY.md`.

**Tests.** `internal/config/trusted_proxy_test.go` covers bare IPs, CIDRs,
IPv4-mapped IPv6, empty lists, >64 entries, malformed values, and the two
"trust everything" prefixes. `internal/httpapi/trusted_proxy_test.go` covers
trusted/untrusted peers, plain HTTP, ambiguity, `Forwarded`-only, direct TLS,
admin session/CSRF/OIDC cookies, and same-origin behavior.

**Residual risk.** A proxy that forwards from an address outside the configured
list behaves as before (cookies not Secure). This is explicit and fail-closed.
The list is configuration, so a compromised config remains inside the trust
boundary.

### BUG-01: nested Anthropic tool-start error was discarded

`anthropicEmitter.Emit` recursively emitted a `StreamToolStart` when a delta
arrived without a start, but ignored the recursive error. A write failure could
therefore be lost while the emitter continued.

**Fix.** Return the recursive error. Regression surface is covered by the
existing canonical stream tests; the full package suite passes.

**File.** `internal/protocol/canonical/stream.go:606`.

### BUG-02: dead cooldown expression in admin snapshot

`internal/httpapi/admin.go` computed
`!time.Now().Before(st.CooldownUntil) == false && ...` and then immediately
overwrote the value with the authoritative `IsCooldown` call. The first line had
no runtime effect and confused the real source of truth.

**Fix.** Removed the dead expression; the authoritative assignment is unchanged.

**File.** `internal/httpapi/admin.go:334` (before fix).

### TEST-01: budget-alert regression loop never iterated

`TestBudgetAlertFailureDoesNotAffectRequests` wrapped its follow-up request in a
`for` loop that always executed `break` after the first request. The test only
ever sent one follow-up request despite claiming to wait for background delivery.

**Fix.** Replaced the fake loop with the single follow-up request it actually
performed. Assertions are unchanged; the test is now honest about its coverage.

**File.** `internal/httpapi/budget_alerts_test.go:405`.

### BUG-03: intentional probe budget looked like caller-controlled input

`httpAdapter.Probe` accepted `maxTokens`, overwrote it with `1`, then used the
variable. Staticcheck correctly flagged the overwritten assignment. The intent
was to ignore the argument permanently.

**Fix.** Added `_ = maxTokens` and a named `probeTokens = 1` constant used by
all provider bodies. Behavior is unchanged and the invariant is explicit.

**File.** `internal/providers/http_adapter.go:1084`.

### CI-01: browser evidence run could fail on PR permission denial

`browser-evidence-pr.yml` used `GH_TOKEN` and failed the whole workflow when
branch push or PR creation was denied, even though the evidence itself had been
produced successfully. It also used a runner-temp path and unpinned action
references.

**Fix.** Evidence is stored under `${{ github.workspace }}/.artifacts/...`,
actions are pinned by full SHA, unrelated inputs are enumerated, and denied
push/PR publication records a `blocked` status plus a manual compare URL without
destroying the evidence run.

**Files.** `.github/workflows/browser-evidence.yml`,
`.github/workflows/browser-evidence-pr.yml`.

**Verification limit.** The workflow syntax and shell paths are reviewed and the
repository's own workflow tests pass, but GitHub-hosted execution cannot be
observed locally. This remains unverified until the next workflow run.

## Staticcheck disposition (46 -> 23)

| Rule | Count remaining | Disposition |
| --- | --- | --- |
| ST1005 | 22 | **Accepted style debt**: user-facing error strings begin with a capital letter. Changing them would alter API/error text. Deferred to an owner-approved copy change. |
| SA4017 | 1 | **False positive**: `internal/protocol/canonical/stream.go:117`. A minimal probe under `.artifacts/scprobe` reproduced staticcheck 2026.2.2 flagging a `switch` branch whose function call does have side effects. No source workaround was added. |

Fixed in this checkpoint: SA4004 (fake test loop), SA4009 (overwritten probe
argument), SA1012 (intentional nil context in tests, justified lint ignore),
SA4006/no-op expression, S1002/S1009/S1011/S1016/S1017/S1030/S1034/ST1018
(style-only cleanups). The `cooldown_active` dead expression and recursive
`Emit` error were also surfaced here and fixed as BUG-01/BUG-02.

Command: `staticcheck ./...` -> exit 1, 23 diagnostics, stored in
`.artifacts/issue593-final/staticcheck.txt`.

## Gosec disposition (157 -> 98)

Command: `gosec -quiet -fmt=json -out=.artifacts/issue593-final/gosec.json ./...`
-> 98 issues, 232 files, 75,654 lines, 28 justified `#nosec` annotations.

| Rule | Count | Disposition |
| --- | --- | --- |
| G304 | 54 | **Operator-controlled paths**: config, media, video, and audit files opened from validated/allow-listed settings. Not attacker-chosen request paths. Needs path-root hardening only if config becomes untrusted. |
| G703 | 28 | **Tainted path analysis**: same operator-configured media/video locations. The code sanitizes names for traversal; gosec cannot see the allow-list. |
| G204 | 12 | **Configured binaries**: `ffmpeg`/`ffprobe`/TTS commands executed with argument arrays, never through a shell. Binary paths come from operator config. |
| G704 | 4 | **CLI SSRF**: `cmd/videogen` intentionally connects to the operator-supplied CLI base URL. It is a client tool, not a server-side fetch boundary. |

Cleared in this checkpoint:

- **G115 (14)**: all are bounded modulo results or intentional two's-complement
  reinterpretation in the EventStream protocol, audit key timestamps, or
  rotation indexes. Each has a line-local justification.
- **G705 (1)**: admin CSRF response now uses `json.NewEncoder` instead of
  `fmt.Fprintf` with a token.
- **G104 (32)**: response body closes, health check writes, and stream buffer
  writes now explicitly discard or check errors.
- **G101 (6)**: provider endpoint URLs, enum/classification names, response map
  keys, and environment-variable *names*; annotated as non-credentials.
- **G124 (6)**: cookie `Secure` is now computed by `requestIsSecure`; gosec
  cannot follow the helper. Annotated with the trusted-proxy invariant.

## Architecture debt and proposals (not implemented here)

These require product/owner decisions or multi-package migrations. This
checkpoint deliberately did not start them without approval; no partial parallel
implementation was left behind.

### ARCH-01: `config.Config.Validate` is ~1,320 lines

**Problem.** One function validates unrelated domains (admin, TLS, routing,
providers, video, logging), making error order and ownership hard to reason
about. Current complexity is a maintenance and review risk.

**Proposed safe split.** Keep `Validate` as the ordered orchestrator and extract
`validateAdmin`, `validateRouting`, `validateProviders`, `validateVideo`, and
`validateLogging`, preserving call order and exact error strings. Add table
tests that assert the *first* error for a config with several invalid fields.

**Status.** OPEN. Deferred because moving 1,300 lines without dedicated
differential tests is a large behavior-preserving refactor, and the task forbids
risky architecture changes without owner approval.

### ARCH-02: virtual-key create/rotate/revoke duplication

**Problem.** `internal/httpapi/client_keys.go` repeats key generation, lookup,
revocation, and config mutation in three handlers. The route layer owns domain
rules that should be reusable by future admin clients.

**Proposed change.** Extract a small `virtualKeyService` (or package-level
functions) with `Create`, `Rotate`, `Revoke`, and a single ID generator;
handlers only decode, call, and render. Add tests for duplicate IDs, expired
keys, and rollback on config validation failure.

**Status.** OPEN. Scoped but not implemented in this checkpoint; it is safe to do
next without changing API shapes.

### ARCH-03: distributed circuit breaker

**Problem.** Circuit-breaker state is process-local. Behind multiple replicas,
one instance can keep routing to a failing provider while another is open.

**Options.** (a) Redis/PostgreSQL shared state with TTL keys; (b) accept
per-replica state and document it; (c) leader-owned breaker with gossip.
Option (a) adds a hard dependency to request routing and needs failure-mode
design; option (b) is current behavior.

**Status.** BLOCKED on owner decision. No distributed state was fabricated or
simulated.

### ARCH-04: durable video job queue

**Problem.** Video jobs are in-process. Restart loses queued work and
multi-replica deployments cannot share progress.

**Options.** (a) PostgreSQL-backed job table with `FOR UPDATE SKIP LOCKED`;
(b) Redis streams; (c) keep in-process and document single-replica constraints.

**Status.** BLOCKED on product decision (durability/HA expectations). The
current behavior is documented, not silently claimed as durable.

### ARCH-05: control-plane/HA ownership

The admin control plane is still single-writer for config mutations. Splitting
config writes from routing would require an explicit consistency model and
revision protocol. **Status: OPEN/owner decision.**

### ARCH-06: browser and live-infrastructure acceptance

Browser acceptance is skipped locally because Chromium + Python Playwright are
absent. PostgreSQL, Redis, OIDC, KMS, and live providers are not available.
`verify.sh` release-artifact trust-chain tests do pass. **Status: PARTIAL;
requires CI or staging evidence.**

## Already merged (PR #595/#596 history)

PR #595 fixed nine confirmed defects: egress TLS `ServerName` pinning, proxy
`allowed_hosts`, media `no_training` bypass, structured-output removal,
Anthropic thinking budget, emergency-key/OIDC precedence, truncated media
responses, hedge capability filtering, and Anthropic cache-usage reporting.
PR #596 recorded the baseline and verification for that batch. Those fixes are
present in `main` at `bfa93b6` and are not re-claimed here.

## Dead code

`deadcode -test ./...` reports four unreachable functions:

- `internal/decision/jev/provider.go:441` `CloneForHotReload`
- `internal/decision/remote/errors.go:70` `IsUnavailable`
- `internal/httpapi/budget_alerts.go:131` `NewFileBudgetAlertStore`
- `internal/scorecards/feedback.go:186` `FeedbackTracker.Cards`

All are test/dispatch-only or reserved API helpers. No production call was
removed; deleting exported/package API needs an owner decision.

## Limitations and unverified areas

1. No authenticated `gh`; cannot observe CI for the new commit or download
   artifacts. CI status must be checked after PR creation.
2. Chromium + Python Playwright unavailable; browser acceptance is a real skip,
   not a pass.
3. No live PostgreSQL, Redis, OIDC, KMS, or provider credentials; integration
   paths are compile/unit-tested only.
4. No load/soak test, no multi-replica failure injection, no destructive
   restart test.
5. Release signatures/assets were not re-verified against the published
   release; `verify.sh` only proves the local trust-chain tests.
6. Gosec G304/G703/G204/G704 remain plausible risks if operator config is ever
   attacker-controlled. They are classified, not proven harmless.
