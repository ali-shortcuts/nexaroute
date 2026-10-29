# Kubernetes / Helm acceptance — 2026-10-27 (issue #650)

## Verdict

**Offline acceptance: PASS. Live-cluster acceptance: NOT_RUN.**
No live-cluster claim is made from rendering alone, and no
production-cluster or multi-host claim is made at all.

- Base tree: main `e6c3cdf3` (2026-10-27 mission baseline), branch
  `supervisor/issue650-k8s-acceptance` in this PR.
- Offline tool: Helm v3.19.0 (the version CI pins via
  `azure/setup-helm`), run locally from a downloaded release tarball
  because this VM ships no Helm.
- Live blocker (exact): this VM has no `docker`, `kind`, or `kubectl`,
  and no cluster was named or authorized by the task. `kind` needs a
  container runtime, and this sandbox previously could not run k3s
  either (systemd-nspawn container with zero delegated cgroup
  controllers, 2026-10-24 Round-2 finding), so installing kind here
  was not a credible path to a cluster. The script therefore exits
  `NOT_RUN` (exit code 3) for live mode instead of fabricating a run:
  verified on this VM, `scripts/k8s-acceptance.sh --live` prints the
  missing tools and exits 3 having created nothing.

## What was inspected first

- Chart `deploy/helm/nexaroute` (Chart.yaml, values.yaml, all six
  templates), `Dockerfile` (distroless `static-debian12:nonroot`,
  uid/gid 65532, no shell), `docs/DEPLOYMENT.md`, the CI `container`
  job, `/healthz` + `/readyz` handlers
  (`internal/httpapi/server.go`, `internal/httpapi/metrics.go`), the
  readiness funnel (`internal/router/router.go` `Readiness`), and
  SIGTERM handling (`cmd/gateway/main.go`: `srv.Shutdown` with a
  drain cap of `max(request_timeout, stream idle timeouts, 10s) + 5s`).
- Prior state (2026-10-24): `helm lint` + `helm template` PASS with
  5 resources on an acceptance values set; the live install was
  already recorded as unverified. This PR does not re-claim that work;
  it re-runs the checks on the current tree and adds the automation
  that was missing.

## Offline evidence (executed 2026-10-27, this tree)

Command: `bash scripts/k8s-acceptance.sh --offline-only`
(exit 0; 38 PASS lines, 0 failures, 0 not-run). The script renders
twice — once with the least-privilege acceptance values, once with
chart defaults — and asserts on both renders:

| Check | Result |
| --- | --- |
| `helm lint` | PASS (only the informational icon note) |
| Deployment / ServiceAccount / PVC rendered | PASS |
| No Role / RoleBinding / ClusterRole rendered; SA grants nothing | PASS |
| `automountServiceAccountToken: false` (pod + SA) | PASS |
| `runAsNonRoot: true`, `runAsUser: 65532` | PASS |
| `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false` | PASS |
| seccomp `RuntimeDefault`, capabilities drop ALL | PASS |
| Liveness `/healthz`, readiness `/readyz` | PASS |
| `Recreate` strategy (never two pods on one RWO claim) | PASS |
| `terminationGracePeriodSeconds` configured | PASS |
| No plaintext `postgres://` / `redis://` value in renders | PASS |
| Image tag pinned, never `latest` | PASS |
| Database + stub credentials only via `secretKeyRef` | PASS |
| Drain invariant: grace 120s > acceptance-config drain cap 35s | PASS |

The drain cap is computed from the acceptance config actually
shipped (`routing.request_timeout_ms` 30000, stub
`stream_idle_timeout_seconds` 30 → `max(30, 30, 10) + 5 = 35s`).
The same check is what stops an operator from silently shipping a
grace shorter than their config's drain.

## Findings

### F1 — Readiness is 503 by design with no usable deployment (documented, not a defect)

`/readyz` returns 503 when there are zero enabled deployments
(`total == 0`) or zero usable ones, and under the default
`ready_mesh` strategy "usable" means probe-Healthy. The chart's
default config enables no providers, so a default install is
Running but never Ready. Operators must not read that as a failed
install, and acceptance must not expect Ready without a usable
deployment. The live script therefore proves both sides: 503 on the
empty-provider base install, then 200 after upgrade to a config with
a stub provider that the probe engine primes to Healthy. The stub
(`deploy/helm/nexaroute/acceptance-stub.yaml`) is a disposable fake
upstream answering only `GET /v1/models` and
`POST /v1/chat/completions`; it makes no paid or real upstream call.

### F2 — The chart had no termination grace; the gateway drain can exceed the Kubernetes default (fixed in this PR)

Before this PR the chart set no `terminationGracePeriodSeconds`,
so pods got the 30s default while the gateway's drain cap is at
least 15s and, with the example config's timeouts, 185s
(`max(120, 180) + 5`). A pod SIGKILLed at 30s mid-drain cuts
in-flight streams. This PR adds `terminationGracePeriodSeconds`
(default 120s) to values + pod spec and the drain-invariant check
above. The 120s default is NOT universal: it covers the acceptance
config (35s) but not the example config (185s). Operators must set
grace above their own config's drain cap; `docs/DEPLOYMENT.md` says
so explicitly.

### F3 — Chart `appVersion` is stale against the latest release (reported, not changed)

Chart `appVersion` is `0.22.0` at main `e6c3cdf3` and was already
`0.22.0` at the `v0.24.0` tag, so an empty `image.tag` installs
`v0.22.0`, not the latest supported release. The `Dockerfile`
`VERSION` default (`v0.23.0`) is stale the same way. This PR does
not bump either: aligning chart/image versioning with releases is a
release-policy decision for the owner, and release-policy changes
are out of scope for this task. The acceptance script always pins
`image.tag` explicitly (built from the tree under test, and from
the preceding-release tree for the upgrade base), so the upgrade
test does not depend on the stale default. Until the owner aligns
versioning, a default `helm install` must not be described as
installing the current release.

### F4 — `helm rollback` does not roll back an operator-managed config Secret (documented + scripted)

The chart's `existingSecret` config lives outside the release, so a
rollback restores chart + values but keeps the current config. The
live script swaps the config Secret back to the base config before
`helm rollback`, then re-upgrades; a rollback run that skipped the
swap would be testing nothing. This order is now in
`docs/DEPLOYMENT.md` as an operator caveat.

### F5 — What the live script deliberately does not prove (even when it runs)

- In-flight stream draining under a real long-running request: the
  script measures pod termination inside its grace period and the
  replacement pod becoming Ready, but it does not hold a stream open
  across SIGTERM. Drain timing for a real stream is code-verified
  (`srv.Shutdown` with the drain cap), not live-proven.
- Multi-host persistence, production storage classes, and production
  topology: the PVC test is single-node kind with the default
  provisioner; nothing here supports an HA or production claim.
- Backup/restore round-trip across restart: the script proves backup
  succeeds to the PVC and `doctor` passes after restart on the same
  claim; it does not restore the bundle in the live run, because a
  restore into a running gateway's config is a separate operator
  procedure covered by the gateway's own backup/restore tooling.
- Runtime uid from the image config: the script asserts posture from
  the live pod spec; the image `USER` check stays in CI's container
  job (`docker image inspect`, non-root), as before.
- The optional `--with-postgres` phase (real ephemeral PostgreSQL
  control-plane backend via secret reference) is implemented but,
  like the rest of live mode, unexecuted here.

## Live runbook (for an operator with docker + kind + kubectl)

```bash
# Offline first (also what CI runs):
bash scripts/k8s-acceptance.sh --offline-only

# Full disposable-cluster run (creates + deletes kind cluster
# "nexaroute-accept"; add --keep-cluster to inspect afterwards,
# --with-postgres for the database-backend phase):
bash scripts/k8s-acceptance.sh --live

# Evidence lands in ./k8s-acceptance-out/ (git-ignored):
# rendered manifests, live pod spec, events, pod logs, describe
# output, backup/doctor logs. Review before committing anything
# from it; the script never writes Secret objects there.
```

Live exit codes: 0 = all executed phases passed; 1 = a phase
failed; 3 = NOT_RUN (prerequisites missing or the preceding tag
absent from the checkout). A NOT_RUN result must never be reported
as a pass.

## Files in this PR

- `scripts/k8s-acceptance.sh` — offline + live acceptance, sentinel
  leak check, drain invariant, upgrade/rollback/persistence phases.
- `deploy/helm/nexaroute/values-acceptance.yaml` — least-privilege
  acceptance values (secret references only, no secret values).
- `deploy/helm/nexaroute/acceptance-config.json` — stub-provider
  gateway config for the live run (derived from the example config:
  listen `0.0.0.0:8080`, 30s timeouts, one enabled stub provider).
- `deploy/helm/nexaroute/acceptance-stub.yaml` — disposable fake
  upstream (not part of the chart).
- `deploy/helm/nexaroute/values.yaml` + `templates/deployment.yaml`
  — `terminationGracePeriodSeconds` (F2).
- `.github/workflows/ci.yml` — container job now runs the offline
  script instead of inline greps (same assertions, plus the new ones).
- `docs/DEPLOYMENT.md` — readiness semantics, drain rule, image-tag
  note, rollback caveat, acceptance runbook.
- `.gitignore` — `k8s-acceptance-out/` evidence dir.
