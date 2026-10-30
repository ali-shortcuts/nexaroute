# Kubernetes / Helm acceptance — live gate re-verified, context guard added — 2026-10-30

Issue #650 remains OPEN. This record covers the live-gate re-verification
on current main and one focused harness hardening. It is NOT a
live-deployment pass: no cluster was created, no release was installed,
and no production resource was touched.

## What changed

Branch `supervisor/issue650-live-context-guard`, based on main `7daa36d`
(merge of #764). Diff vs main: exactly 3 files, no Go code, no chart
change, no production change:

- `scripts/k8s-acceptance.sh` — a shared `assert_kind_context` helper
  that fails closed unless the current kubectl context is exactly the
  kind cluster this run created (an empty context, e.g. a kubectl
  failure, refuses just as a foreign context does). It is now called
  in `live_phase` immediately after `kind create cluster` and the EXIT
  trap are set, and BEFORE any namespace, Secret, or release is
  created. Previously only `safe_failure_phase` had a context guard;
  the lifecycle phases (namespace creation, stub apply, dummy secrets,
  base install, upgrades, rollback) ran on whatever context `kind
  create` left behind. If a `KUBECONFIG` pointing at an operator
  cluster (or a kind that did not switch context) was in play, those
  mutations could have landed on an existing cluster. The trap still
  deletes the just-created kind cluster on refusal when
  `--keep-cluster` is not given.
- `scripts/k8s-acceptance.sh` self-test — new check 4: a stub kind
  that "creates" a cluster plus a stub kubectl reporting a foreign
  current-context. The run must exit 1 with the refusal line, and the
  kubectl invocation log must contain the context query and NO
  namespace or Secret creation. Red/green proof (this VM): with the
  guard call removed, the self-test fails with exactly one failure
  ("foreign kubectl context was not refused before mutation"); with
  the guard present, all 11 checks pass. The safe-failure phase's
  previous inline guard is now the same helper (behavior unchanged).
- `docs/DEPLOYMENT.md` — the image-tag paragraph now records WHY the
  chart `appVersion` bump is still deliberately open (see below),
  instead of only saying an owner decision is pending.

Chart state (unchanged by this change, verified on the branch tree):
`deploy/helm/nexaroute` version 0.1.0, appVersion 0.22.0.

## Chart version reconciliation (deliberate decision, not a bump)

- Latest supported release, verified live: `v0.24.0` (newest tag and
  newest GitHub release; script default `--prev-tag` is already
  `v0.24.0`, so the upgrade test's base install is the current
  supported release).
- The chart default render still resolves to
  `ghcr.io/ali-shortcuts/nexaroute:v0.22.0` (empty `image.tag` means
  `v` + `appVersion`).
- Why `appVersion` was NOT bumped to `0.24.0` here: no workflow in
  this repository publishes container images to `ghcr.io` (zero
  `ghcr` references outside the chart values files), and the
  existence of a `v0.24.0` image there is unverifiable from the
  development environment — the Packages API returns 404 for both
  the user path (`/users/ali-shortcuts/packages/container/nexaroute`)
  and the organization path, which proves nothing about visibility
  versus existence. Bumping the default tag to an unverified image
  would be exactly the blind version bump this task forbids. The
  correct sequence is: establish a verified publish pipeline first,
  then bump `appVersion` and the chart `version` together. Until
  then, every install must pin `image.tag` explicitly (the
  acceptance script always does).

## Versions and environment

- Repo head at implementation: `7daa36d`; branch head: see the PR.
- Helm v3.19.0 (local binary `~/workspace/.local/bin/helm`; CI pins
  the same version). kubectl client v1.34.0 and kind v0.27.0 installed
  for this run under `~/workspace/k8s-bin/` (CLIs only; they create
  nothing on their own).
- Python 3.12.3, git 2.43.0, Go 1.27.3 toolchain present.
- This VM: systemd-nspawn container, root user, systemd running —
  but `/sys/fs/cgroup/cgroup.controllers` is 0 bytes, there are no
  `cpu`/`memory`/`pids` controller directories, creating a directory
  under `/sys/fs/cgroup` fails, no Docker binary, no Docker socket
  (`/var/run/docker.sock` and `/run/docker.sock` both absent), no
  podman/containerd, no `~/.kube`. A container runtime cannot run
  without cgroup controllers, and kind needs Docker, so no kind
  cluster can be created here. This is the same root cause as the
  2026-10-24 k3s failure, re-verified today rather than quoted.

## Results (executed, this VM)

Command: `bash scripts/k8s-acceptance.sh --offline-only`
Result: exit 0, 38 PASS, 0 FAIL (failures=0, not_run=0).

Command: `bash scripts/k8s-acceptance.sh --self-test`
Result: exit 0, 11 PASS, 0 FAIL — the previous 10 checks plus the new
foreign-context guard check. Negative control: guard call removed →
exit 1, exactly 1 failure (the new check), all other checks unchanged.

Command: `bash scripts/k8s-acceptance.sh --live --out-dir /tmp/k650-live-out`
(with kind v0.27.0 + kubectl v1.34.0 + helm v3.19.0 on PATH)
Result: exit 3 NOT_RUN. The offline phases in the same run still pass
(38 PASS); the live phase stops at the tool gate with the single line
`missing required tool: docker`. The gate has narrowed since the
earlier reports (kind and kubectl are now present); the remaining
blocker is exactly one missing component: a Docker daemon, which
itself is blocked by the missing cgroup controllers above. `kind get
clusters` confirms the same: `docker: executable file not found in
$PATH`. Nothing was created: no cluster, no namespace, no Secret, no
release, no kubeconfig, no production contact of any kind.

## What still blocks #650 (unchanged)

A kind-capable, authorized runner: a host with cgroup controller
delegation (cpu, memory, pids at minimum) plus a Docker daemon, or an
operator-authorized disposable-cluster kubeconfig. The owner has
already confirmed this VM is not such a runner. When that runner
exists, the canonical run is:

```
bash scripts/k8s-acceptance.sh --live [--with-postgres]
```

covering, in order: preceding-release (`v0.24.0`) install, upgrade,
rollback, re-upgrade, probes, live pod-spec posture, graceful
termination with PVC persistence across restart, the sentinel
secret-leak check, and then the safe-failure phases (invalid-JSON
config, unreachable PostgreSQL backend, missing referenced Secret,
failed rollout → rollback → `/readyz` 200), with redacted
manifests/events/logs and exact Kubernetes, Helm, chart, and image
versions collected from that run. Until that run passes, #650 stays
OPEN and production-cluster support remains unclaimed.
