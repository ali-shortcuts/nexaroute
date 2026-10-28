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

## Upgrades and rollback

1. `nexaroute backup --config <path> --out <bundle>` (see OPERATIONS §2).
2. Deploy the new binary/image/unit; the gateway migrates state
   fail-closed on startup.
3. `nexaroute doctor --config <path>` and check `/readyz`.
4. Rollback = restore the previous binary and
   `nexaroute restore --bundle <bundle> --config <path> --force`.

### NexaFlow workflows (Phase 8)

NexaFlow is opt-in (`nexaflow.enabled`) and single-host in this
phase: one gateway process owns one store path. Do not run two
gateway processes against the same store directory, and do not
share the store over a network filesystem expecting cross-process
safety — the FileStore's atomicity is single-host.

- Back up the workflow store directory alongside the gateway
  config. It holds definitions, versions, runs, schedule claims,
  webhook delivery records, and audit events.
- Upgrading with in-flight runs: `running` runs at shutdown are
  interrupted and park on the next startup with their ambiguous
  step named. They do not auto-resume; an operator resumes each
  parked run explicitly. `awaiting_approval` and `parked` runs
  survive restarts unchanged.
- Store format upgrades run at startup (`Migrate`) and are
  additive; see [NexaFlow migration notes](nexaflow-migration.md)
  for the format 1 → 2 procedure and its backup-first rule.
- Webhook deliveries should pass through the operator's reverse
  proxy, which owns rate limiting for `/nexaflow/webhooks/*` in
  this phase (the gateway enforces HMAC, the timestamp window,
  and replay rejection, but no fixed rate limit).
