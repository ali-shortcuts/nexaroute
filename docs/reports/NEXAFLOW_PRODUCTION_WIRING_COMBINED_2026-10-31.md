# NexaFlow production wiring — combined landing (issue #765)

Date: 2026-10-31. Base: main `ff0aee29`. Branch:
`supervisor/nexaflow-combined-wiring`. This PR supersedes the three
colliding candidates #781, #779, and #771: all three pairs conflict
(verified first-hand with `git merge-tree`, centered on
`internal/config/config.go`), so no two of them can land together.
This PR takes each candidate's proven part into one coherent wiring
and is the single landing for #765. It does NOT close #765 by itself:
#765 closes only after this wiring merges and the merged main is
rechecked.

## What each candidate contributed

| Candidate | Taken into the combined wiring |
|---|---|
| #781 | Store semantics (file default, memory explicit-only), `store_backend` key naming, fail-closed store open (lock, corrupt file), `Service.Recover` before serving, `maybeAttachNexaflow` structure, wiring + httpapi production test shapes, browser-script production class. |
| #779 | Strict validation shape (disabled section inert, enabled section strictly validated, unknown backend rejected), empty-store-means-file default, fail-closed `StorePath` requirement. |
| #771 | Real backend execution (loopback `GatewayClient` + `EnvSecretResolver` + `SecretResolver`), base-URL derivation from the listener, `CompositeBackend` with direct health/compat, engine `Audit` wiring, production-binary E2E (run to `Succeeded` + store persistence), `EnvSecretResolver` + its test. |

## Deliberate deviations (decided here, not inherited)

1. **Store path is required for the file backend.** #771's implicit
   default path under the config dir is NOT kept: silent file
   creation under the config dir is the wrong default for a
   production gateway. The browser script and the E2E set an
   explicit path.
2. **Worker id defaults to the fixed worker `"gateway"`, not
   `gateway-<pid>`.** A pid changes every restart, so leases lose
   attribution across restarts. The fixed id is correct ONLY for
   the single-process file/memory stores wired here (the file lock
   forbids a second live process on the same store). A future
   multi-process store (e.g. PostgreSQL) must require explicit
   distinct worker ids; this constraint is stated in the wiring
   comment and in `docs/CONFIGURATION.md`.
3. **One recovery call.** `Service.Recover` delegates to
   `Engine.Recover`, so #781's service-level call and #771's
   engine-level call are the same operation. The combined wiring
   calls `Service.Recover` once before serving, fail-closed.
4. **Same-worker continuation replaces the random-worker stall.**
   With a fixed worker, a restarted gateway continues its own
   attempt at once (no 30s stall). The foreign-lease rule is
   preserved and still tested: a different worker id must not steal
   a live lease (see the restart test's second run).

## Preserved unsupported list (unchanged by this PR)

No scheduler daemon (no tick loop), no webhook listener (no secret
source configured), no PostgreSQL store, no `workflow.*` permissions
(interim `config.read`/`config.write` mapping), no tenant dimension
(single-tenant admin deployment; runs filtered by principal subject
only), no BudgetGuard (`budget_check` stays report-only).

## Verification evidence (local, this branch)

All commands run from the branch checkout with
`TMPDIR=$HOME/workspace/.tmp GOTMPDIR=$HOME/workspace/.tmp` and Go
from `~/go/bin` (CI Go is 1.27.3; local Go is 1.27.0 — CI is the
gate for the exact toolchain):

- `go vet ./cmd/gateway/ ./internal/config/ ./internal/httpapi/
  ./internal/nexaflow/` — clean.
- `gofmt -l` over all touched Go files — clean.
- `go test -count=1 -run TestValidateNexaflow ./internal/config/` —
  ok (store table + gateway-field cases).
- `go test -count=1 -run TestEnvSecretResolver
  ./internal/nexaflow/` — ok.
- `go test -count=1 -run TestNexaflowProductionWiring
  ./internal/httpapi/` — ok (studio routes on file store, restart
  persistence, llm.chat node without token fails closed with no
  output).
- `go test -count=1 -run
  'TestNexaflowWiring|TestMaybeAttachNexaflow|TestNexaflowProductionGatewayWiringE2E'
  ./cmd/gateway/` — ok (disabled 404, enabled routes 200, config
  validation, file-store fail-closed incl. lock + corrupt file,
  restart recovery with same-worker continuation + foreign-lease
  stall + lease-expiry completion, principal filtering,
  loopback derivation, worker-id default, gateway-field
  validation, production-binary E2E to `Succeeded` with store
  persistence).
- `go test -count=1 -race -run
  'TestNexaflowWiringRestartRecoversApprovalRun|TestNexaflowProductionGatewayWiringE2E'
  ./cmd/gateway/` — ok.
- Browser studio script
  (`python3 scripts/test-browser-nexaflow-studio.py`, production
  class, 15 cases against a booted production gateway): see
  outcome below.
- Full `./scripts/verify.sh` NOT run locally (known environmental
  `internal/httpapi` timeout on this 2-vCPU VM; CI is the gate).

## Browser studio outcome

Production class, 15 cases against a booted production gateway on
this branch: **PASS** (`nexaflow studio browser acceptance: PASS`,
script exit 0; no SKIP path taken — the gateway attached the
service, so the nodes probe reached the real API). Re-run after the
final script-message polish: same PASS. Note: the script's PASS line
prints only after every case assertion and the console-error check
pass; a SKIP would print `SKIP: ...` instead.
