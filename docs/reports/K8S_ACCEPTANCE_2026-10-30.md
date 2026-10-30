# Kubernetes / Helm acceptance — safe-failure extension — 2026-10-30

Issue #650 remains OPEN. This record covers the safe-failure extension
to the acceptance harness only; it is not a live-deployment pass.

## What changed

Branch `supervisor/issue650-safe-failure`, commit `75fd133`, rebased
onto main `4b830cd` (merge of #755). Diff vs main: exactly 3 files,
+414/−21, no Go code, no chart change, no production change:

- `scripts/k8s-acceptance.sh` — the existing harness, extended in
  place. No second harness was created; the 2026-10-29 external
  wrapper in `~/workspace/your_files/` is superseded by this change
  and must not be used alongside it.
  - New `--self-test` mode: deterministic, cluster-free tests of the
    failure paths (see below).
  - New live `safe_failure_phase`, run after the lifecycle phases
    and only if they passed: invalid-JSON config, unreachable
    PostgreSQL control-plane backend, and missing referenced Secret
    must each fail closed (a pod is created, none becomes Ready),
    each fault release is uninstalled and verified absent, and a
    failed rollout (unpullable image, pullPolicy Never) must roll
    back to a Ready release with `/readyz` 200 again. A context
    guard refuses the fault phases unless the current context is
    the kind cluster this run created.
  - `check_drain_grace` replaces the bare drain computation in the
    offline phase, so a malformed config fails the check instead of
    aborting the script.
- `.github/workflows/ci.yml` — the container job now runs
  `--offline-only` and `--self-test` (live kind phases still not run
  in CI).
- `docs/DEPLOYMENT.md` — acceptance-automation paragraph updated
  for `--self-test` and the live safe-failure phases; the standing
  statement that a live cluster install is not yet verified is
  unchanged.

Chart state (unchanged by this change, verified on the rebased
tree): `deploy/helm/nexaroute` version 0.1.0, appVersion 0.22.0.
The appVersion staleness vs the v0.24.0 release is an owner
release-policy decision and was deliberately not bumped here.

## Versions and environment

- Repo head at implementation: `2c5f583`; rebased head: `75fd133`
  on main `4b830cd`.
- Helm v3.19.0 (local binary; CI pins the same version).
- Python 3.12.3, git 2.43.0, local Go 1.27.0 (CI uses Go 1.27.3;
  the local govulncheck result below is a toolchain-version
  artifact, not a repo finding).
- This VM: no kind, kubectl, docker, podman, k3d or minikube; no
  `~/.kube`; `/sys/fs/cgroup/cgroup.controllers` is 0 bytes
  (kubelet cannot run locally — the same root cause as the
  2026-10-24 k3s failure). Nothing was created, no production
  infrastructure was touched, zero live calls, zero paid calls.

## Offline results (executed, this VM)

Command: `bash scripts/k8s-acceptance.sh --offline-only`

- Result: failures=0, not_run=0. 38 PASS: helm lint, both renders
  (acceptance values and chart defaults), hardened posture
  (runAsNonRoot 65532, readOnlyRootFilesystem, drop ALL, seccomp
  RuntimeDefault, automountServiceAccountToken false, no RBAC
  objects), probes (/healthz, /readyz), Recreate strategy,
  secretKeyRef-only wiring, no plaintext database URL, pinned
  image tag, secret-leak check, drain invariant (acceptance
  config drain cap 35s < chart grace 120s).

Command: `bash scripts/k8s-acceptance.sh --self-test`

- Result: failures=0, not_run=0, exit 0. 10 PASS:
  1. Missing live tools (kind/kubectl/docker) → exit 3 NOT_RUN.
  2. Missing preceding tag → exit 3 NOT_RUN, stub kind log shows
     no `create cluster` call.
  3. Already-existing kind cluster → exit 1 refusal, stub kind
     log shows no `create cluster` call.
  4. Acceptance config passes the drain invariant (35s < 120s).
  5. Oversized config (request_timeout_ms 300000, drain 305s)
     correctly fails the drain invariant.
  6. A 10s grace correctly fails against the acceptance drain cap.
  7. Malformed config fails the drain check without crashing the
     script (python traceback suppressed; the "unreadable" line is
     the diagnostic).
  8. Missing referenced Secret renders as a secretKeyRef only.
  9. That render contains no plaintext DSN.
  10. Fault-release cleanup helper invokes uninstall + secret
      deletion and is idempotent on a second call.
- A first self-test run caught a real defect in the new code (the
  stub PATH lacked `dirname`/`basename`, so subprocesses computed
  the wrong repo path) and failed 3 checks; the fix (adding the
  missing tools to the stub bin) is in the committed diff, and the
  rerun above is the clean result.
- Usage error: unknown argument → exit 2 with the full header.

## Live results (NOT_RUN — this is the remaining gap)

Command: `bash scripts/k8s-acceptance.sh --live`

- Offline phases: same 38 PASS as above.
- Live phase: `missing required tool: kind`, `missing required
  tool: kubectl`, `missing required tool: docker`,
  `NOT_RUN: live acceptance: required tools missing`.
- Result: `NOT_RUN (live prerequisites missing)`, exit 3. The
  script returns before any cluster creation, so no cluster,
  release, or Secret was created anywhere.

The live safe-failure phases (invalid JSON, dead PostgreSQL
backend, missing Secret, failed rollout + rollback) are therefore
code-complete but UNEXECUTED. Their premises were checked in code,
not in a cluster: the gateway pings a PostgreSQL DSN at startup
and fails closed when unreachable
(`cmd/gateway/controlplane_runtime.go`, `openPostgresControlPlane`),
and Helm renders a secretKeyRef without checking Secret
existence, so the missing-Secret failure is a runtime
never-Ready, not a render failure. That is not a substitute for
running them.

To execute them, on a host with Docker + kind + kubectl + helm:

```bash
bash scripts/k8s-acceptance.sh --live [--with-postgres]
```

Expected live evidence: preceding-release (v0.24.0) install,
upgrade, rollback, re-upgrade, probes, live pod posture, PVC
persistence across a measured graceful restart, the four
safe-failure phases with per-phase cleanup verification, the
sentinel leak check, and a summary of exact Kubernetes/Helm/
chart/image versions. Until that run passes, production-cluster
support remains unclaimed.

## Full repository verification (after the code change)

Command: `bash scripts/verify.sh` (Go 1.27.0, TMPDIR/GOTMPDIR
under `~/workspace/.tmp` per the /tmp tmpfs lesson).

- Exit 1, sole cause: four packages hit verify.sh's 3-minute
  per-package cap with zero `--- FAIL` lines — cmd/gateway
  180.189s, internal/httpapi 180.987s, internal/router 180.110s,
  internal/securitystore 180.098s (goroutine dumps of tests in
  flight, no test failure). 70 packages ok. This is the known VM
  contention artifact, not a defect in this change, which touches
  no Go file.
- Proof: the same four packages rerun individually with
  `-timeout=10m -count=1` all PASS — gateway 275.409s, httpapi
  555.989s, router 451.234s, securitystore 150.917s.
- `go vet ./...`: exit 0, clean.
- `govulncheck ./...`: exit 3, pre-existing stdlib findings only
  (GO-2026-6605..6809 family, fixed in Go 1.27.2/1.27.3) on the
  local Go 1.27.0 toolchain; no finding in any changed file, and
  no Go file was changed. CI (Go 1.27.3) is the authority for
  this check.
- `bash -n scripts/k8s-acceptance.sh`: clean.
  `scripts/check-ha-docs.sh`: OK. `git diff --check`: clean.
- A test-created untracked directory
  (`cmd/gateway/synthetic-stage/`) was removed before commit;
  the committed tree contains only the 3 intended files.

## Secrets

Only fake SENTINEL values exist in the harness code
(`SENTINEL-ACCEPT-650` and derivatives). No real credential,
DSN, or connection string appears in any captured log or in this
report; the leak check reports only whether a match was found,
never the matching line. The script never dumps Secret objects.

## What remains for issue #650

1. A live `--live` run on an authorized disposable cluster (or a
   kind-capable host), including the new safe-failure phases.
2. That run's redacted evidence (versions, manifests, events,
   logs) recorded in a follow-up report.
3. Issue #650 stays OPEN until (1) passes or an explicit
   owner-approved exclusion is recorded. No merge and no close
   are claimed by this change.
