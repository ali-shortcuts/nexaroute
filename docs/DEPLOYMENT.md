# Deployment

NexaRoute is a single static Go binary; every deployment form below runs
the same artifact and the same configuration model. Local state lives next
to the config file (the auto-managed master key, the bbolt session/audit
store, and — for the file backend — control-plane state). When the optional
PostgreSQL or Redis backends are selected, their state lives in those
services instead. In every case the config itself is a per-replica local
file, so the M4-3
`nexaroute backup` / `nexaroute restore` / `nexaroute doctor` commands
work against that local installation and an application backup never
substitutes for backing up external services.

## Container image

The repository `Dockerfile` builds a distroless image
(`gcr.io/distroless/static-debian12:nonroot`, no shell, CGO-free static
binary) and seeds `/config/config.json` (mode 0600 in a 0700 directory)
from `configs/config.example.json`.

```bash
docker build -t nexaroute --build-arg VERSION=v0.24.0 .
docker run -d --name nexaroute -p 8080:8080 \
  -v nexaroute-config:/config nexaroute
```

CI builds the image for `linux/amd64` and `linux/arm64` on every run
and smoke-runs `--version` (build only — image publishing stays with
the release workflow). The container listens on `0.0.0.0:8080`
(`NEXAROUTE_LISTEN`), runs as uid/gid 65532, and its root filesystem
needs no writes outside `/config` and `/tmp`.

## systemd

Two units ship in `deploy/systemd/`:

- `nexaroute.service` — a **user unit** for single-operator installs
  (config under `%h/.config/nexaroute`), sandboxed with
  `NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=strict` and
  `ProtectHome=read-only`.
- `nexaroute-system.service` — a **system unit** for servers: dedicated
  `nexaroute` system user, config at `/etc/nexaroute/config.json`,
  systemd-managed `StateDirectory` (`/var/lib/nexaroute`) and
  `ConfigurationDirectory` (both mode 0700), an empty capability set,
  `@system-service` syscall filtering and restricted address families.
  Both units pass `systemd-analyze verify`.

```bash
sudo useradd --system --home /var/lib/nexaroute --shell /usr/sbin/nologin nexaroute
sudo install -m 0755 nexaroute /usr/local/bin/nexaroute
sudo install -d -m 0700 -o nexaroute -g nexaroute /etc/nexaroute
sudo install -m 0600 -o nexaroute -g nexaroute config.json /etc/nexaroute/config.json
sudo cp deploy/systemd/nexaroute-system.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now nexaroute-system
nexaroute doctor --config /etc/nexaroute/config.json
```

Because the auto-managed master key lives next to the config file, the
service user must be able to write `/etc/nexaroute` (and nothing else
outside its state directory) — the unit's `ReadWritePaths` encodes
exactly that.

## Kubernetes (Helm)

The chart at `deploy/helm/nexaroute` renders a hardened single-replica
deployment of the distroless image: non-root security contexts at pod
and container level, read-only root filesystem, all capabilities
dropped, seccomp `RuntimeDefault`, no service-account token, `/healthz`
liveness and `/readyz` readiness probes, and resource requests/limits.

```bash
helm install nexaroute deploy/helm/nexaroute \
  --set config.existingSecret=my-nexaroute-config
```

- **Configuration**: supply a Secret with a `config.json` key via
  `config.existingSecret` (preferred), or let the chart render one from
  `config.configJSON` — values files are not a secret store, and the
  rendered Secret is only base64-encoded.
- **Persistence**: local state is on one PVC mounted at `/config`
  (default 1Gi RWO; `persistence.existingClaim` to reuse one). The
  config Secret is overlaid as the single file `/config/config.json`;
  the master-key file, bbolt security store and file-backend
  control-plane state are written beside it on the PVC. External
  PostgreSQL/Redis state is not on this volume.
- **Database control plane**: set `controlPlane.postgresDSN.secretName`
  / `controlPlane.redisURL.secretName` to reference operator Secrets;
  the chart injects them as `NEXAROUTE_CONTROL_PLANE_PG_DSN` /
  `NEXAROUTE_CONTROL_PLANE_REDIS_URL` environment variables — point the
  config's `postgres_dsn_env` / `redis_url_env` at those names. No
  connection value ever appears in values or rendered manifests.
- **Scaling truth**: `replicaCount` defaults to 1. More replicas can share
  control-plane state with the Postgres/Redis backends and can share the
  session/audit store by selecting the PostgreSQL or Redis security-store
  backend. They do **not** share configuration: the config file remains
  local to each replica, and admin or CLI edits do not propagate. Distribute
  the same config to every replica through deployment tooling. The chart does
  not provide full HA, and the upgrade strategy is `Recreate` so two pods never
  share one PVC claim.

Chart correctness is evidenced by `helm lint` + `helm template` with
rendered-output assertions in CI; **a live cluster install is not yet
verified** (no cluster in the development environment) and is tracked
as an explicit gap, not a claim.

- **Readiness semantics — read before judging an install**: `/healthz`
  is liveness (the process answers), while `/readyz` is 503 until at
  least one deployment is usable. An install whose config enables no
  providers (the chart default) therefore stays un-Ready forever, by
  design: the gateway refuses to receive traffic it cannot route. A
  bare install that is Running but not Ready is not a broken install;
  a Ready install needs at least one usable deployment.
- **Graceful termination**: on SIGTERM the gateway drains in-flight
  work for `max(request_timeout, stream idle timeouts, 10s) + 5s` and
  then exits, so `terminationGracePeriodSeconds` (chart default 120s)
  must stay above the drain cap of the config actually installed —
  the chart cannot read that cap back out of the config Secret. The
  acceptance config drains in 35s, inside the 120s default; the
  example config's 120s request / 180s stream timeouts drain in 185s
  and need a grace above 185s. Set the value from your config, not
  from the default.
- **Image tag**: an empty `image.tag` resolves to `v` + the chart's
  `appVersion`, which is currently `0.22.0` while the latest supported
  release is `v0.24.0`. A default `helm install` therefore does not
  install the latest release; pin `image.tag` explicitly (the
  acceptance script always does) until chart versioning is aligned
  with releases by an owner decision.
- **Rollback caveat**: `helm rollback` restores the previous chart and
  values, but it does not touch an operator-managed config Secret.
  Rolling back a release whose config changed means restoring the
  previous config Secret yourself first, or the rolled-back pods boot
  with the new config. The acceptance script exercises exactly that
  order.
- **Acceptance automation**: `scripts/k8s-acceptance.sh --offline-only`
  runs the lint/render/posture/leak/drain checks (this is what CI
  runs, together with `--self-test`). `scripts/k8s-acceptance.sh
  --self-test` runs the deterministic, cluster-free safe-failure
  checks: the missing-prerequisite NOT_RUN gates (missing tools,
  missing preceding tag, already-existing kind cluster — all exit
  without creating a cluster), the drain invariant against oversized
  and malformed configs, the missing-Secret reference-only render,
  and the fault-release cleanup helper. `scripts/k8s-acceptance.sh
  --live` runs the full disposable kind-cluster acceptance:
  preceding-release install, upgrade, rollback, re-upgrade, probes,
  posture from the live pod spec, PVC persistence across a measured
  graceful restart, the sentinel secret-leak check over events, logs
  and describe output, and then the safe-failure phases: an
  invalid-JSON config, an unreachable PostgreSQL control-plane
  backend, and a missing referenced Secret must each fail closed
  (a pod is created, none becomes Ready) and each fault release is
  uninstalled and verified absent, and a failed rollout (unpullable
  image) must roll back to a Ready release. It creates and deletes
  its own kind cluster, refuses to touch an existing one, and uses
  only fake sentinel secret values. Live evidence for this release
  is recorded in `docs/reports/K8S_ACCEPTANCE_2026-10-27.md`; until a
  live run passes there, production-cluster support remains
  unclaimed. The safe-failure extension itself is recorded in
  `docs/reports/K8S_ACCEPTANCE_2026-10-30.md`, where the live run
  is still NOT_RUN for the same missing-cluster reason.

## Upgrades and rollback

1. `nexaroute backup --config <path> --out <bundle>` (see OPERATIONS §2).
2. Deploy the new binary/image/unit; the gateway migrates state
   fail-closed on startup.
3. `nexaroute doctor --config <path>` and check `/readyz`.
4. Rollback = restore the previous binary and
   `nexaroute restore --bundle <bundle> --config <path> --force`.
