# Architecture slice: shared-config snapshot ADR

- Issue: #612 (parent audit #593; related #599 / PR #601)
- Reviewed baseline: `main` at `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d`
- Branch work: ADR plus documentation/check corrections; no shared-config
  runtime writes, secret changes, or schema migrations
- Status: implementation slice complete for the ADR scope; shared-config
  implementation remains **not started** pending owner decisions

## Scope and baseline

The task was to re-read #612 and current `main`, verify the code and HA
documentation, write an actionable ADR with real options and secret-key
custody/migration/conflict/rollback analysis, correct remaining contradictory
documentation without behavior changes, add checks, and open one focused PR
while keeping #593 open.

Baseline checks performed before editing:

| Check | Result |
|---|---|
| `git rev-parse HEAD` / `origin/main` | `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d` (both) |
| Open pull requests | none (`GET /repos/ali-shortcuts/nexaroute/pulls?state=open` returned `[]`) |
| Reviewed issue threads | #593, #612, #599 and PR #601 |
| Published `v0.23.0` | not modified; tag ref still resolves to `c421cac3d0ad817dc5432afa5515865d0550e7dd` |

## Code review: what actually happens today

| Finding | Evidence |
|---|---|
| Config load is the local file. | `internal/config/config.go:1171` (`LoadBase`), `:1238` (`Load`), `cmd/gateway/main.go:149`. |
| One process is bound to a config path on one host. | `cmd/gateway/main.go:135` acquires the config lock. |
| Admin mutations build from the local base config and save the local file atomically. | `internal/httpapi/server.go:843` (`mutateConfig`), `:608` (`config.SaveAtomic`), `internal/config/config.go:3547`, `:3649`. |
| Admin config revisions are process-local. | `internal/httpapi/server.go:67`, `:260`, `:862`; `internal/httpapi/settings.go:23-51` (in-process `ETag`/`If-Match`). |
| The control-plane store has only runtime/budget namespaces in this build. | `cmd/gateway/controlplane_runtime.go:22` (`gateway/runtime`), `:27` (`budgets/alerts`), `:121-123`, `:145`, `:159`. |
| The control-plane store is CAS-capable but is not wired to config. | `internal/controlplane/types.go:34`, `:11`; `internal/controlplane/manager.go:31`, `:58`, `:86`. |
| Secret values are `nxs1` envelopes with a separately held master key. | `internal/secrets/secrets.go:27`, `:40`, `:47`, `:263`, `:378`; `internal/secrets/key_provider.go:79`. |
| Backup captures local config, local security state, file control-plane state, and optionally the key. | `cmd/gateway/backup.go:33-37`, `:163`, `:552`; `docs/OPERATIONS.md:120`, `:127`. |
| The HA inventory and `/api/hello` now report config as `per-instance`. | `internal/httpapi/deployment.go:44-50`; `docs/HA_STATE_INVENTORY.md:20`; regression `TestConfigSnapshotReportMatchesLocalStore` at `internal/httpapi/deployment_test.go:94`. |

Conclusion: the implementation and current HA documentation agree that
configuration is local and per-instance. There is no shared-config write,
migration, or secret change to verify because none exists.

## ADR and recommendation

Created `docs/adr/0001-shared-config-snapshot.md` (first ADR in the
repository). It compares:

1. local config stays authoritative;
2. PostgreSQL/Redis config namespace with revision CAS;
3. read-only control-plane observability.

It analyzes secret-key custody (shared KEK, references-only shared config,
per-replica re-encryption), bootstrap/migration, mixed-version rollout, admin
versus CLI writes, backup/restore, revision conflicts and split-brain, rollback,
availability, and security failure modes.

**Recommendation: Option 1 for the current release.** Keep local config
authoritative and do not implement shared-config writes or migrations until the
owner resolves the listed decisions. Option 2 is the only acceptable target if
propagation becomes a product requirement; Option 3 is useful observability but
does not provide shared config. The ADR lists nine owner decisions covering
product need, source of truth, secret custody, migration, mixed-version rollout,
availability, conflict/rollback, backup/restore ownership, and security/audit.

## Documentation corrections

The ADR slice also corrects wording that could mislead operators:

- `docs/CONFIGURATION.md` now states explicitly that the control-plane store
  carries runtime metadata and budget-alert marks, never config, and that admin
  or CLI edits do not propagate.
- `docs/DEPLOYMENT.md` now distinguishes local state from external
  PostgreSQL/Redis state and replaces the stale "M4-2 adapters land" claim with
  the actual security-store options. It states that replicas do **not** share
  configuration.
- `deploy/helm/nexaroute/values.yaml` carries the same truthful scaling note.
- `docs/HA_STATE_INVENTORY.md` and `docs/KNOWN_GAPS.md` link the ADR and keep the
  existing per-instance statement.

No file in this slice writes config to the control plane, changes secret
handling, or adds a migration.

## Added check

`scripts/check-ha-docs.sh` is a narrow documentation contract check. It requires
the HA inventory, known-gaps, configuration and deployment docs to keep stating
that config is local/per-instance and that the ADR remains the decision record.
It also rejects the stale "until the M4-2" wording. `scripts/verify.sh` runs it
after shell-syntax validation.

## Local verification

Commands run on the working tree:

```bash
./scripts/check-ha-docs.sh
```

Result: `HA documentation consistency OK`.

```bash
go test ./internal/httpapi -run 'TestConfigSnapshotReportMatchesLocalStore|TestDeploymentReport|TestHelloExposesDeploymentReport' -count=1
```

Result:

```text
ok  	github.com/ali-shortcuts/nexaroute/internal/httpapi	0.043s
```

```bash
go test ./internal/config ./internal/controlplane ./cmd/gateway -count=1
```

Result:

```text
ok  	github.com/ali-shortcuts/nexaroute/internal/config	0.722s
ok  	github.com/ali-shortcuts/nexaroute/internal/controlplane	1.209s
ok  	github.com/ali-shortcuts/nexaroute/cmd/gateway	36.376s
```

Full repository verification (`./scripts/verify.sh`) and exact-SHA GitHub
Actions CI/Security results are recorded in the PR description for the pushed
head. This report does not claim a live multi-replica PostgreSQL/Redis test for
shared config because such a feature does not exist in this slice.

## Remaining blockers and next actions

- **Owner decision required:** whether cross-replica config propagation is a
  product requirement. Until then, Option 1 is the correct production behavior.
- If approved, a separate implementation issue must resolve ADR decisions
  1–9, especially secret custody (references-only is recommended), explicit
  migration, mixed-version gating, CAS conflict UX, and restore semantics.
- #593 remains open as the parent audit issue. No release or tag was changed.
