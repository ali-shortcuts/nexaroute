# NexaRoute Final Acceptance Report

**NEXAROUTE CANONICAL: PASS**

| Field | Value |
|---|---|
| FINAL MAIN SHA | `134921c4395a9b76add1dc3211830622450d662b` (squash merge of PR #33; exact v0.7.0 tag target) |
| RELEASE TAG | `v0.7.0` |
| RELEASE URL | https://github.com/ali-shortcuts/nexaroute/releases/tag/v0.7.0 |
| MAIN CI | PASS |
| PUBLIC INSTALL TEST | PASS |

**INSTALL / UPDATE:**

```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash
```

**START:**

```bash
nexaroute
```

**UI:**

```
http://127.0.0.1:8080/
```

**CLAUDE CODE CONFIG (exact tested final configuration):**

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="local-gateway"
export ANTHROPIC_MODEL="coding"
```

Client auth credentials are never forwarded to upstream providers; each
provider uses its own configured credential. `ANTHROPIC_MODEL` may be a real
model ID, a configured alias (e.g. `coding`), or `claude-auto`.

## Release provenance

- PR #33 (`NexaRoute v0.7.0 — Final Integration Release`) head `4683764fb635c7261b28e856591ae461dc4dd4e9`, CI green, squash-merged into `main`.
- FINAL MAIN SHA `134921c4395a9b76add1dc3211830622450d662b` — main CI PASS (`CI` run 36288515448, `Soak` run 36288515400).
- Tag `v0.7.0` points at that exact commit. The release pipeline (run 36288748472) re-ran the full verification, stress, smoke, and installer-lifecycle suites on the tag, rebuilt both architectures, and published only after checksum verification (`verify: success`, `publish: success`).
- Later `main` commits are documentation/CI metadata only (temporary acceptance harness add/remove, this report).

## Published assets (all four required)

| Asset | Size | SHA256 (GitHub-computed digest of published bytes) |
|---|---|---|
| `nexaroute-linux-amd64` | 8,650,904 | `808a596e784666518a9c820781013de2dec0325f84f2d17d1cb5b8d5eaf687c2` |
| `nexaroute-linux-arm64` | 8,257,688 | `9525b937afb7b39a9156377c49422a4ae2e39d69d7fcd1891bcb3ebd2b616926` |
| `install.sh` | 5,058 | `0fb919823ead6975e6fcfaffeff0a1713c7b918bf6c6f44ce98f0027f8e4d835` |
| `SHA256SUMS` | 253 | `a8daa935b69e3d9a82bef2074a526a420d994a46f0d8ecd8f85d978c7874fd6a` |

Checksum chain verified independently of the CDN:

1. Published `install.sh` is byte-identical to `scripts/install.sh` at tag `v0.7.0` (local sha256 equals the published digest).
2. The published `SHA256SUMS` content was reconstructed from the published binary digests and hashes exactly to the published `SHA256SUMS` digest (253/253 bytes) — the manifest lists exactly the three published assets with their true hashes.
3. The release pipeline ran `sha256sum -c SHA256SUMS` on the uploaded bytes and `cmp scripts/install.sh dist/install.sh` before `gh release create`.
4. `releases/latest/download/install.sh` resolves live to `releases/download/v0.7.0/install.sh`; `v0.7.0` is the only release and is marked **Latest** (not prerelease), target `main`.

## PUBLIC INSTALL TEST: PASS

Executed on GitHub-hosted Ubuntu in a clean `debian:bookworm-slim` container
that contains **no Go and no git**, with a fresh `HOME`
(workflow run 36291191631, job `public-install`, success):

1. Real `releases/latest` URL resolves to `v0.7.0`.
2. The exact public command `curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash` downloads, SHA256-verifies, and atomically installs.
3. `nexaroute` is on PATH at `/usr/local/bin/nexaroute`; `nexaroute -version` reports `v0.7.0`.
4. `nexaroute` starts; `/healthz` and the UI at `http://127.0.0.1:8080/` respond.
5. Config is created at `~/.config/nexaroute/config.json` with mode `0600`.
6. The **same command run a second time** reinstalls safely: config checksum unchanged, canary file intact, binary still `v0.7.0`, restart healthy.

## Verification evidence (all green)

- `gofmt`, `go vet ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`
- `./scripts/verify.sh` (shuffled repeats, race repeats, fuzz targets, benchmarks, amd64+arm64 static builds)
- `./scripts/stress.sh`, `./scripts/smoke-local.sh`, `./scripts/test-install.sh` (installer lifecycle)
- Four-route protocol matrix E2E (Anthropic→Anthropic, Anthropic→OpenAI, OpenAI→OpenAI, OpenAI→Anthropic), streaming cancellation propagation, deadline runs
- Failover E2E with exact attempt ordering; five-attempt recovery; fake-clock 30-minute cooldown and re-entry
- 50/100/200-deployment scale acceptance; Phase H isolation; hedging/decision/cache suites
- Browser launch, headless fallback, single-instance locking; secret canary/redaction suites
- Fuzz and benchmark smoke gates; Docker build + runtime smoke

CI runs: PR #33 CI `36288315578` (head `4683764`, success) · main CI `36288515448` (`134921c`, success) · main CI `36289924824` (`4660af3`, success) · Install acceptance `36291191631` (success) · main CI `36291248504` (`a24ebb0`, success) · Release `36288748472` (`v0.7.0`, verify+publish success).

## DELETED REMOTE BRANCHES (49 — complete list)

```
arena/01a0cf59-nexaroute            arena/01a0d1d4-nexaroute
arena/01a0d36d-nexaroute            arena/01a0d825-nexaroute
arena/01a0d9e5-nexaroute            arena/01a0db74-nexaroute
arena/01a0dbf9-nexaroute            arena/01a0dc29-nexaroute
arena/01a0dd7b-nexaroute            arena/01a0dd9d-nexaroute
arena/01a0dda5-nexaroute            arena/01a0dda8-nexaroute
arena/01a0ddaf-nexaroute            arena/01a0de6d-nexaroute
arena/01a0deab-nexaroute            arena/nexaroute-final-integration
codex/cost-quota-routing-v051       codex/provider-incident-intelligence-v03
codex/provider-intelligence-v041-stage
codex/quota-observation-order-v053  codex/quota-observation-order-v061
codex/quota-reservations-v052       codex/resilience-intelligence-v03
codex/usage-accounting-v042
feat/continuous-ready-routing       feat/supervisor-gateway-audit
fix/admin-rate-bucket-bound         fix/beta-compatibility-install
fix/canonical-v06-regressions       fix/canonical-v06-regressions-rebased
fix/compat-test-upstream-model-name fix/gemini-native-probes
fix/health-donut-half-open          fix/hedge-attempt-budget
fix/hedge-attempt-budget-main       fix/hedge-attempt-budget-r2
fix/hedging-cancellation-race       fix/no-backups-final-audit
fix/provider-edit-selector-regression
fix/provider-editor-protocol-parity fix/release-readiness
fix/responses-admission-limit       fix/responses-path-hot-reload
fix/responses-stateful-semantics    fix/runtime-version-v060
fix/runtime-version-v060-r2         fix/separate-local-provider-presets
fix/stale-quota-observations        fix/universal-installer-release
```

Also removed: release + tag `v0.6.1-beta.3` (obsolete prerelease). Closed stale
worker PRs #5, #10, #12, #13, #20, #21, #26, #31 (PR #33 ended **MERGED**).
The temporary acceptance harness `.github/workflows/install-acceptance.yml`
was added on `main` for the public install test and deleted after it passed.

## DELETED LEGACY FILES (37 — complete list)

```
install-user.sh                     run-local.sh
scripts/smoke-v05.sh
INSTALLATION.md                     QUICKSTART.md
CLAUDE_CODE.md                      TEST_REPORT.md
CHANGELOG.md                        ROADMAP.md
docs/PHASE_A_CURRENT_STATE_REPORT.md
docs/PHASE_B_IMPLEMENTATION_REPORT.md
docs/PHASE_B_RECONCILIATION_NOTE.md
docs/PHASE_C_CURRENT_STATE_NOTE.md
docs/PHASE_C_IMPLEMENTATION_REPORT.md
docs/PHASE_C_REQUEST_INTELLIGENCE.md
docs/PHASE_D_CURRENT_STATE_NOTE.md
docs/PHASE_D_DECISION_ARCHITECTURE.md
docs/PHASE_D_IMPLEMENTATION_REPORT.md
docs/PHASE_E_CURRENT_STATE_NOTE.md
docs/PHASE_E_IMPLEMENTATION_REPORT.md
docs/PHASE_E_POLICY_ENGINE.md
docs/PHASE_F_EXTERNAL_DECISION_PROVIDERS.md
docs/PHASE_F_IMPLEMENTATION_REPORT.md
docs/PHASE_G_CURRENT_STATE_NOTE.md
docs/PHASE_G_DECISION_CHAINS.md
docs/PHASE_G_IMPLEMENTATION_REPORT.md
docs/PHASE_H_CURRENT_STATE_NOTE.md
docs/PHASE_H_EVALUATION_AND_SCORECARDS.md
docs/PHASE_H_IMPLEMENTATION_REPORT.md
docs/INSTALL_RELEASE_REPORT.md
docs/PROTOCOL_COMPATIBILITY_REPORT.md
docs/RUNTIME_FINALIZATION_CURRENT_STATE.md
docs/RUNTIME_FINALIZATION_REPORT.md
docs/ROUTING_LIFECYCLE_AUDIT_FA.md
docs/INSPIRATION_AND_DECISIONS.md
docs/BUG_AVOIDANCE.md
docs/PROVIDER_UI_SPEC.md
```

Plus stale release-version references removed throughout (CI release
validation now `./scripts/build-release.sh v0.7.0`; runtime, tests, and
installer fixtures unified at `0.7.0`).

## REMAINING BRANCHES

```
main
```

## Canonical product invariants

- ONE router, health manager, probe/recovery engine, provider registry,
  protocol translation layer, config persistence, desktop/browser launcher +
  single-instance lock (`internal/desktop`), installer (`scripts/install.sh`,
  shipped identically as release `install.sh`), release pipeline
  (`.github/workflows/release.yml` — real publishing).
- ONE user install/update command and ONE start command (`nexaroute`).
- ONE canonical docs set: `README.md`, `docs/INSTALLATION.md`,
  `docs/QUICKSTART.md`, `docs/CLAUDE_CODE.md`, `docs/CONFIGURATION.md`,
  `docs/COMPATIBILITY.md`, `docs/KNOWN_GAPS.md`, `docs/FINAL_ACCEPTANCE_REPORT.md`,
  `ARCHITECTURE.md`, `SECURITY.md`, `CONTRIBUTING.md`.

## KNOWN LIMITATIONS

1. **Linux-only installer** (amd64/arm64). macOS/Windows users must build from source or use the Docker image.
2. **Plaintext configuration at rest** — config dir `0700`, config file `0600`; no OS keyring or encrypted vault.
3. **Single-process runtime** — health, breaker, affinity, quota and scorecard state are in-memory; no distributed/multi-node coordination.
4. **No built-in TLS** — use a reverse proxy for any non-loopback exposure.
5. **Admin surface is loopback-oriented** — not an internet-hardened control plane (no RBAC/multi-user/CSRF framework).
6. **Cross-protocol reasoning/thinking metadata is capability-filtered** and provider-specific; native passthrough remains the safest path for provider-only fields.
7. **Token counting falls back to a local estimate** (marked `"estimated": true`) when no native count is obtainable.
8. **Cost/quota routing intelligence is advisory** — not invoice-perfect billing or hard budget enforcement.

---

*This report is the closing acceptance record of the canonical v0.7.0
integration. There is no next phase and no further integration branch.*
