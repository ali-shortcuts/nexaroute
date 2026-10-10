# Phase 1a Closeout Report — 2026-10-09

Repository: `ali-shortcuts/nexaroute`  
Base context: `main` at `8aa9d37` (`v0.16.3`)  
Working branch: `phase1a-encrypted-secrets` (continued in place as explicitly requested; no history rewrite)  
PR: [#211](https://github.com/ali-shortcuts/nexaroute/pull/211) — remains open; not merged.

## Acceptance matrix

| Acceptance item | Status | Evidence / disposition |
|---|---|---|
| Audit encrypted secrets, config migration, key custody/modes, nonce and authenticated-encryption behavior, rotation crash recovery, and missing/wrong-key behavior | **DONE** | Key loading now fails closed when an encrypted config has no matching key; it does not silently mint a replacement. Staged-key recovery promotes `.key.next` only when it authenticates the atomically committed config. GCM envelope parsing rejects truncated wrapped-key payloads before slicing. Regression tests cover staged/uncommitted rotation, interrupted rotation recovery, missing key, and truncation. Existing transform tests cover ciphertext authentication and migration behavior. |
| Ensure no plaintext credential disclosure via Admin/API, logs, snapshots, or metrics | **DONE** | Provider/pool canary test checks Admin detail/list, snapshot, metrics, mutation response, encrypted config-at-rest and mode `0600`. New upstream-failure canary checks the data-plane response, Admin providers/snapshot/config-history, metrics, production log buffer and event bus. Both canary tests pass. |
| Make security/configuration/runbook/gap docs agree with implementation | **DONE** | Updated root `SECURITY.md` as a short entry point; `docs/SECURITY.md` is the single canonical security model. Updated `README.md`, `docs/CONFIGURATION.md`, `docs/OPERATIONS.md`, `docs/KNOWN_GAPS.md`, and the capability matrix. Remaining keyring/external-key rotation, distributed-state, TLS/mTLS, and Admin CSRF/session limits are stated as gaps rather than completed features. |
| Add meaningful tests to `internal/guardrail`; raise five requested packages above 70% | **DONE** | Added behavioral suites (not assertion-free padding) for guardrail, control-plane stores/migrations, gateway CLI/server lifecycle, provider evaluation, compatibility contracts, and usage identity. Every requested package exceeds 70%; see table below. |
| Record acceptance, raw outputs, coverage, and open one PR | **DONE** | This report and the verbatim gate transcript are checked in under `docs/reports/`. Existing PR #211 is the single Phase 1a PR and remains open; it was continued on its current branch as directed. |

## Security audit findings and changes

- **Key custody and permissions:** auto-managed key material remains a 32-byte master key in the sibling `.key` file. Key-file writes use restrictive `0600` permissions; encrypted config saves remain `0600`. Explicit environment and key-file overrides retain precedence and are not silently replaced.
- **Authenticated encryption and nonce use:** secret values use AES-256-GCM envelopes with fresh random nonces and field-bound associated data; the envelope also wraps per-secret data keys. `Open` now rejects a ciphertext too short to contain the wrapped key and its authentication tag before indexing into the payload.
- **Rotation durability:** rotation stages `.key.next`, atomically replaces the encrypted config, then promotes the staged key. If the process stops after config replacement but before promotion, key loading authenticates the config against the staged key and completes promotion. A staged key that does not authenticate the current config remains uncommitted. The previous auto-managed key is preserved as `.key.previous` for the documented recovery workflow.
- **Migration and recovery:** legacy plaintext config is atomically rewritten as encrypted data and retains an authenticated, encrypted timestamped `.enc.bak`. Missing/wrong key and tampered envelope cases fail closed; losing the only matching key remains an unrecoverable operator recovery event, not a reason to create a replacement key.
- **Disclosure boundaries:** secrets remain write-only on Admin reads. The canary regression tests check API/Admin responses, event/log paths, snapshot and metrics, and verify encrypted persistence. Decrypt-to-stdout remains an explicit, doubly gated plaintext export.

## Package coverage before / after

The before profile/log were captured on the PR branch before closeout additions. After percentages are from `go test ./... -coverprofile=/tmp/nexaroute-phase1a-closeout-cover.out`.

| Package | Before | After | Change |
|---|---:|---:|---:|
| `cmd/gateway` | 36.5% | 71.1% | +34.6 pp |
| `internal/guardrail` | 0.0% | 100.0% | +100.0 pp |
| `internal/controlplane` | 40.2% | 83.8% | +43.6 pp |
| `internal/providers` | 64.0% | 79.9% | +15.9 pp |
| `internal/compat` | 64.5% | 70.2% | +5.7 pp |
| `internal/usage` | 65.3% | 95.8% | +30.5 pp |
| **Repository total** | **75.1%** | **77.3%** | **+2.2 pp** |

## Required validation gates

All commands below exited `0`. The exact stdout/stderr transcript—including package-by-package race, verify, and coverage results—is in [the raw gate transcript](PHASE1A_GATES_RAW_2026-10-09.txt).

- `gofmt -l .` — empty stdout; exit `0`. (The runner wrapped this command to fail if output was non-empty; the repository verify script’s formatting check also passed.)
- `go vet ./...` — empty stdout; exit `0`.
- `go test -race -count=1 ./...` — all packages `ok`; exit `0`.
- `./scripts/verify.sh` — `VERIFY PASS`; exit `0`. Includes clean unit/integration and race suites, JavaScript syntax, real-browser control-plane and Live Visual Agent acceptance, short fuzz checks, and Linux amd64/arm64 builds.
- `go test ./... -coverprofile=/tmp/nexaroute-phase1a-closeout-cover.out` — all packages passed; exit `0`. Requested-package coverage is shown above.
- `go tool cover -func=/tmp/nexaroute-phase1a-closeout-cover.out | tail -1` — `total: (statements) 77.3%`; exit `0`.
- `git diff --check` — empty stdout; exit `0`.

## GitHub CI follow-up

GitHub CI exposed two stale harness assumptions that expected literal plaintext secrets on disk, contrary to Phase 1a's encrypted-at-rest behavior: one in `scripts/smoke-local.sh`, and one in the installer lifecycle test. Both now require the `nxs1:` ciphertext prefix, verify the plaintext canaries are absent, and check the 32-byte master-key file is mode `0600`. After the corrections, `./scripts/verify.sh`, `./scripts/smoke-local.sh`, `./scripts/build-release.sh v0.7.0`, and `./scripts/test-install.sh` all passed locally. The exact rerun transcripts are included in [PHASE1A_CI_FIX_RAW_2026-10-09.txt](PHASE1A_CI_FIX_RAW_2026-10-09.txt). GitHub's checks are pending until this follow-up commit is pushed and the workflows rerun.

## Remaining limits

Phase 1a does not add OS-keyring integration or an externally managed key-rotation workflow; the CLI rotates only its auto-managed sibling key. `.key.previous` retains only the most recent previous key unless the operator backs it up. Routine runtime/Admin saves are atomic but do not create backup copies. TLS/mTLS and the Admin CSRF/cookie-session protections remain Phase 1b work, not Phase 1a claims. No PR was merged.
