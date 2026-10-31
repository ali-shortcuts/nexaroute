# Independent-environment verification, 2026-10-31

Base audited: main `9d2ce2f21f61bc6a8eb0531086d062c82566c97c`
(merge of PR 794). This branch (PR 795) head at writing time:
`8cc91055dfd68eee40f888197bf20042a4ab6cc0` (verify.sh wiring
via the web UI, after the gate-script fix). Nothing in this
report merges a PR, closes an issue, or announces a release.

## 1. Why the workflow is a patch, not a pushed file

The GitHub App connection used by this agent cannot push files
under `.github/workflows/` (attempted push: 403 "Resource not
accessible by integration"), and its file push writes mode 644,
which would break directly executed scripts. The complete
workflow therefore ships as an apply-able patch in this branch:

- `docs/reports/independent-environment-workflow.patch`
  (creates `.github/workflows/independent-environment.yml`)

Apply it with a mode-preserving, workflows-permitted push
(owner's git checkout, not the connector):

```sh
git checkout main
git apply docs/reports/independent-environment-workflow.patch
git add .github/workflows/independent-environment.yml
git commit -m "ci: independent-environment workflow"
git push origin main   # or a PR branch, then merge by the normal gates
```

After it lands on main, dispatch it from Actions:
`Independent environment` -> Run workflow. Until that dispatch
runs, the workflow itself is **NOT_RUN**; this report does not
claim its results.

## 2. What the workflow runs (one manual dispatch, fresh runner)

| Job | What it proves | Evidence artifact |
|---|---|---|
| versions-and-build | Tool versions recorded; `gofmt`, `go vet`; gateway + videogen built for amd64 and arm64 | versions.txt, binaries |
| unit-and-race | Full `go test ./...` and `go test -race ./...` | unit-test.log, race-test.log |
| integration-services | `integration`-tagged tests against disposable Postgres 16-alpine and Redis 7-alpine, using the same DSN/URL variables as CI | integration-test.log (SKIP lines visible, never counted as passes) |
| gateway-runtime | Real binary via `scripts/smoke-local.sh`; restart + healthz on both starts; container build + healthz; log check that a real disposable admin key does not appear in logs | gateway logs, container-build.log |
| fuzz-short | Every `Fuzz` target, auto-discovered, 10 s each by default | fuzz-log.txt, corpora |
| browser | Playwright 1.55.0 + Chromium, `run-browser-acceptance.py`; a skipped suite fails the job | browser diagnostics |
| security | govulncheck + CodeQL (Go, manual build) | CodeQL results |
| coverage | `go test -coverprofile`, gate `scripts/coverage-gate.sh <profile> 95.0`, no exclusions | coverage.out |
| stress | `scripts/stress.sh` + `scripts/ci-soak-smoke.sh` | stress logs |
| soak-full (opt-in only) | `scripts/soak.sh` + `check-soak-evidence.py` bound to the dispatched SHA; 2 h with timeout 180 m when `full_soak` is true | soak summary + raw evidence |
| real-services-report | Prints the BLOCKED list below; it never fakes these as passes | step log |

Tool pins (recorded in the workflow, matching existing CI):
Go `1.27.x` on ubuntu-24.04 runners (note: `go.mod` says
`go 1.25.0`; CI already standardizes on 1.27.x, and that
version difference is exactly what exposed the PR 794
mapped-loopback bug, so the pin is deliberate), Postgres
`16-alpine`, Redis `7-alpine`, Playwright `1.55.0`, Docker
from the runner. No install happens on any operator machine;
no real provider, production, or operator credential is used.
The only credentials in the workflow are the disposable
CI-only values already used by existing CI service containers.

The workflow is `workflow_dispatch` only, on purpose: it is
not a second PR gate. The PR gates (CI, Security, Coverage,
real-system acceptance) already run the same ground per PR.

## 3. Architecture and structure findings (from the audit)

No new production defect was found by reading alone, and
none is claimed here. What the audit confirmed, file by file:

- Gateway execution: `cmd/gateway` builds two binaries
  (`gateway`, `videogen`); `main.go` calls
  `maybeAttachNexaflow` before capturing the handler, and
  NexaFlow attaches only when its opt-in flag is on
  (default-off proven by wiring tests).
- Config/security: loopback-only gateway validation at
  config, wiring, and client layers (PR 794, exact spelling);
  secret custody is envelope-encrypted with Vault Transit
  as an optional path; admin surfaces are loopback-first
  with OIDC + API-key options.
- Data plane: control-plane state over Postgres and/or
  Redis, with `integration`-tagged tests that skip (not
  pass) when the service variables are absent.
- Protocol/routing: canonical Anthropic/OpenAI translation
  in both directions, routing/failover/budget/quota as
  separate concerns from translation, with fuzz targets
  across translate, egress, budget, cache, and canonical
  codecs.
- Frontend: browser suites (e2e, e2e-502, live-visual-agent)
  with a fail-on-skip runner; no committed visual baseline
  exists, so visual regression stays NOT_RUN.

## 4. Bugs found and fixed in this cycle

- No new production bug was fixed by this report. The one
  P0 in this area (IPv4-mapped loopback acceptance) was
  fixed on main by PR 794 (`c654c1a`), and PR 795 hardens
  its tests. Two Release Gate script flaws found in review
  (argument order making `<sha> --rc` break, and
  latest-vs-any-run status) were fixed in this branch
  before this report (commits `259e4573`..`a498bd61`
  and the verify.sh wiring `8cc9105`).
- A local-only root-permission test fix (8 tests that
  fail only when run as root, because root bypasses
  chmod) exists as an unpushed local commit in another
  cycle; it is NOT part of this branch, NOT on main, and
  must not be quoted as CI evidence. GitHub runners do
  not run as root, so it does not affect this workflow.

## 5. Open issues: what is provable, what is BLOCKED

From the live inventory (10 open issues, main `9d2ce2f`):

| Issue | Status for this mission |
|---|---|
| 765 NexaFlow production wiring | Code on main verified; main recheck (real binary + browser studio script) is recorded in the parallel tracking file. Closing still needs owner confirmation; this report does not close it. |
| 653 Coverage 95% + stress/soak | IN_PROGRESS: coverage and any soak must be fresh on the SHA under review. Old-SHA numbers do not carry over. The coverage job in the new workflow and the already-dispatched 2h Soak on main `9d2ce2f` (run 41174481182, in progress at last check, not yet validated) are the evidence paths, not verdicts yet. |
| 649 Vault Transit | Implemented on main; live acceptance BLOCKED on a disposable staging Vault with operator approval. Fake-Vault tests are not acceptance. |
| 646 live providers, 647 OIDC tenant, 648 Vault KV staging, 650 Kubernetes, 651 ComfyUI, 652 multi-host HA | BLOCKED on operator tenant/environment/cluster/model decisions. None is marked passed by any mock in this workflow. |
| 654 umbrella | Last: closes only when its children are done or explicitly excepted in a reviewed release note. |

## 6. Evidence status at writing time (honest)

- Independent-environment workflow: NOT_RUN (patch not yet
  applied or dispatched; see section 1).
- PR 795 CI on head `8cc9105`: NOT verified in this turn;
  re-check its runs before any merge decision. Its earlier
  head showed 10 checks successful with Security
  (CodeQL/govulncheck) absent from the PR-triggered list,
  because Security's `pull_request` trigger is `main`
  only; a dispatch on the exact head is required for a
  full security verdict.
- Soak on main `9d2ce2f`: dispatched (run 41174481182,
  2h, rounds 3), in progress at last check, NOT validated:
  validation still needs artifact proof of SHA binding,
  sustained duration >= 7200 s, and stage completeness.
- Coverage on main `9d2ce2f`: a local recomputation
  exists in another cycle's working files, but a local
  total is never a CI total; the authoritative figure
  must come from the Coverage workflow artifact on
  that SHA.
- No secret, credential, or paid-provider call was made
  or stored by this work.

## 7. Decisions needed from the owner

1. Apply the workflow patch via a permitted push (or
   ask for a PR carrying it) and dispatch it once on main.
2. Decide the verify.sh/CI fuzz-smoke wiring for the new
   loopback fuzz target (mode/workflow push limitation
   above applies there too).
3. Provide or decline the operator environments for
   issues 646-652 and 649; until then they stay BLOCKED
   with the reasons in section 5.
4. Confirm or decline closing issue 765 after the main
   recheck, and confirm the deletion of the abandoned
   first-attempt branch from the previous cycle.
