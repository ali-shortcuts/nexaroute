# ADR 0001: Shared configuration snapshots and secret-key custody

- Status: **Accepted — Option 1 (local configuration stays authoritative).**
  Decided 2026-10-22 by the supervising engineer under the owner's
  explicit delegation of the remaining decisions ("fix the remaining
  sections yourself", 2026-10-22). Of the owner decisions listed at the
  end of this ADR, decision 1 is answered **no**: cross-replica
  configuration propagation is not a product requirement; local config
  plus deployment tooling is accepted indefinitely. Decisions 2–9 are
  therefore moot unless a future owner decision reopens propagation,
  in which case Option 2 with the references-only secret model is the
  only acceptable target and decisions 2–9 must be resolved first.
  **No shared-config implementation is approved by this acceptance.**
- Date: 2026-10-21 (accepted 2026-10-22)
- Owner/deciders: repository owner (`ali-shortcuts`), NexaRoute maintainers
- Related: issue #612 (this decision), parent audit #593, defect #599 / PR #601
- Reviewed baseline: `main` at `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d`

## Context

NexaRoute is a self-hosted gateway that can run more than one replica against
an optional PostgreSQL or Redis control plane and an optional shared security
store. Configuration, however, is still a single JSON document on each host.
That document can contain secret-bearing values: provider API keys, proxied
URLs, client keys, admin credentials and custom headers. Secret values are
stored as `nxs1` AES-256-GCM envelopes; the master key is separate from the
config file.

Operators reasonably ask whether enabling the "shared control plane" also
shares configuration. It does not. The control-plane store currently carries
only the runtime marker and budget-alert marks. There is no config namespace,
no config CAS, and no remote read path. An admin edit or a CLI edit changes
only the local file on the replica that served the edit.

This ADR records the real options and the conditions under which shared
configuration could ever be implemented. It deliberately does **not** add
runtime writes, schema migrations, or secret changes. The goal is to make the
current behavior explicit and to give the owner a decision record before any
implementation work starts.

### Verified current behavior

| Claim | Evidence on the reviewed baseline |
|---|---|
| The durable config is the local file. | `internal/config/config.go:1171` (`LoadBase` opens the path), `:1238` (`Load`), `cmd/gateway/main.go:149` (`config.Load(*configPath)`). |
| Admin saves rewrite the same local file atomically. | `internal/httpapi/server.go:608` calls `config.SaveAtomic`; implementation at `internal/config/config.go:3547`; writer at `:3649`. |
| One process per config path is enforced on a host. | `cmd/gateway/main.go:135` acquires the desktop/config lock. |
| The control-plane store has no config namespace. | `cmd/gateway/controlplane_runtime.go:22` (`gateway/runtime`) and `:27` (`budgets/alerts`) are the only namespaces wired by the gateway; the runtime marker is read/written at `:121-123` and budget marks at `:145` and `:159`. |
| The control plane is revision-CAS capable. | `internal/controlplane/types.go:34` (`Store` with `Get`/`Put(expectedRevision)`/`Delete`), `:11` (`ErrConflict`); `internal/controlplane/manager.go:31`, `:58`, `:86`. |
| Config edits are not reconciled across the CAS store. | No config read or write exists in `controlplane_runtime.go`; the only config save path is the local `SaveAtomic` above. |
| Admin revisions are process-local, not cross-replica CAS. | `internal/httpapi/server.go:67`, `:260`, `:862`; `internal/httpapi/settings.go:23-29` uses an in-process `ETag`/`If-Match` for settings only. |
| Secret fields are encrypted with a separately held master key. | `internal/secrets/secrets.go:27` (`Key`), `:40` (`NEXAROUTE_MASTER_KEY`), `:47` (`NEXAROUTE_MASTER_KEY_FILE`), `:263` (`TransformConfig`), `:378` (`EncryptedBackup`); key-provider selection at `internal/secrets/key_provider.go:79`. |
| Backup captures the local config, security snapshot, local file control-plane state, and optionally the key. | `cmd/gateway/backup.go:33-37`, `:163`, `:552`; external PostgreSQL/Redis state is explicitly outside the bundle (`docs/OPERATIONS.md:120`, `:127`). |
| The documented HA scope is per-instance. | `docs/HA_STATE_INVENTORY.md:20`, `docs/KNOWN_GAPS.md:175-177`; regression test `TestConfigSnapshotReportMatchesLocalStore` at `internal/httpapi/deployment_test.go:94`. |

The implementation and the current HA documentation therefore agree: config
is per-instance. The remaining defect class is any documentation that still
implies config follows a shared control plane or that external security-store
adapters are still future work.

## Decision

**Recommendation: keep Option 1 (local config stays authoritative) for the
current release.** Do not add a config namespace, shared writes, automatic
migration, or cross-replica config conflict handling until the owner explicitly
approves an implementation issue with the decisions in this ADR resolved.

This is the only option that is safe without new key-custody and rollout
contracts. It preserves the existing encryption boundary, backup procedure,
rollback story and zero-service default. It also matches the verified behavior
and the `/api/hello` contract.

The recommendation does **not** claim that local config is convenient at scale.
If the owner decides propagation is a product requirement, **Option 2 is the
only acceptable target**; Option 3 is useful observability but not a substitute
for shared configuration.

## Options considered

### Option 1 — local configuration stays authoritative (recommended)

Each replica loads its own file at startup and writes its own file on an admin
save. Operators distribute the same file through their deployment tooling
(Secret, ConfigMap, configuration management, image bake) and restart or
hot-reload as they do today. The control plane may be enabled for runtime
metadata, but it never becomes the config source.

**Advantages**

- No new secret distribution path: the config and its master key stay paired
  per replica exactly as today.
- No migration, no mixed-version hazard, no split-brain config writer: there is
  only one writer per file and no remote authority to disagree with.
- Backup/restore remains truthful: the bundle captures the config and key; an
  external database remains operator-owned.
- The zero-service default is unchanged. A single process with no control plane
  behaves exactly as before.
- Rollback is a file/binary rollback, tested by the existing
  `nexaroute backup`/`restore`/`restore --verify` workflows.

**Disadvantages**

- Admin edits are replica-local and can drift. A load balancer can send the
  next admin request to a different replica.
- CLI edits and admin edits are not coordinated across hosts.
- Operators must repeat config changes N times or use deployment automation.
- There is no central config history, drift report, or global revision.
- A lost replica loses unbacked local edits.

**Required operating rule:** deploy the same config to every replica through
deployment tooling; never expect the control plane to propagate config.
`/api/hello` and the HA inventory must continue to say `per-instance`.

### Option 2 — PostgreSQL/Redis config namespace with revision CAS

Add a new control-plane namespace (for example `config/snapshot`) containing a
versioned, canonical config document. Replicas read it at startup and on a
bounded refresh interval. Admin and CLI writes go through
`Store.Put(namespace, expectedRevision, payload)`; a stale writer receives
`ErrConflict` and must re-read and reapply its change. The local file becomes a
cache/seed or is removed, depending on the chosen source-of-truth model.

This option is technically possible with the existing store abstraction:
`internal/controlplane/types.go:34` already exposes revision-aware `Get`/`Put`,
and the PostgreSQL and Redis adapters already implement conditional writes
(`internal/controlplane/sqlstore.go`, `internal/controlplane/redisstore.go`).
The security store proves the team can build cross-instance CAS, but it is not
the right carrier for routing configuration.

**Advantages**

- One logical config for all replicas; admin edits can propagate consistently.
- Revision CAS rejects lost updates instead of silently overwriting a newer
  document.
- Reuses existing PostgreSQL/Redis operators, backups, migrations and failure
  modes; no new datastore is introduced.
- Enables central config history and drift detection if the payload includes
  revision/actor metadata.

**Disadvantages and open design work**

- Secret custody becomes a cluster problem. The config can contain `nxs1`
  envelopes; every replica must resolve the same 32-byte key-encryption key, or
  the shared document is unusable. A second copy of that key in a database is a
  materially larger blast radius.
- The config is a whole document, not a set of independent keys. Revision CAS
  detects a stale write but does not merge two valid edits to different fields.
  The writer must either re-read and reapply a typed patch or reject with a
  conflict; a naive full-document `Put` loses work.
- Mixed-version rollout is dangerous: an older binary that does not know the
  namespace will read and write its local file while newer replicas read the
  shared document. There is no safe "some replicas ignore the source of truth"
  mode without an explicit feature gate and a minimum-version rule.
- Bootstrap and migration need explicit ownership: first-write race, local-file
  seed, remote-wins versus local-wins, and what to do when the namespace exists
  but is malformed.
- Backup/restore semantics change: an application bundle that includes only
  the local file would silently omit the authoritative config. A restore of an
  older bundle could roll back the cluster unless restore is CAS-guarded.
- Availability policy must be chosen: fail closed at startup (consistent with
  the current PostgreSQL/Redis control-plane posture) or last-known-good local
  cache (available but possibly stale). Config write failure must be distinct
  from data-plane failure.
- Split-brain is reduced by CAS but not eliminated. A partition can leave a
  replica serving a stale in-memory config; if local files remain writable,
  two sources of truth reappear. A single writer path and a clear stale-read
  policy are mandatory.
- The control-plane database must be treated as sensitive configuration
  storage, with access control, audit of writes, payload size bounds, strict
  validation on read, and no secret material in logs/metrics.

**Secret-key custody sub-options (must be decided before implementation)**

1. **Shared key-encryption key (KEK).** All replicas resolve the same KEK from
   the existing key-provider mechanisms (`NEXAROUTE_KEY_PROVIDER=env|command|
   keyring`; `internal/secrets/key_provider.go:79`). The shared document can
   contain normal `nxs1` envelopes. This is the smallest code change but gives
   anyone with both database read access and KEK access all upstream secrets.
   Rotation must coordinate the KEK and every envelope; a half-rotated fleet
   cannot decrypt a shared snapshot.
2. **References only in shared mode (recommended if Option 2 is approved).**
   The shared document stores only environment-variable references, key-provider
   references and other non-secret connection metadata; inline secret values are
   rejected in shared mode. Each replica continues to receive actual credentials
   from its own secret injection. This keeps the database from becoming a secret
   vault and preserves per-replica key custody. It requires a config restriction
   and migration of existing inline-secret configs.
3. **Per-replica envelope re-encryption.** Store secrets encrypted to each
   replica's public key and re-encrypt on write/rotation. This requires new
   crypto, key discovery, revocation and recovery design. It is not implemented
   and is not recommended for the first slice.

**Bootstrap/migration outline (not executed by this ADR)**

- Add an explicit config source field (for example `config_source:
  local|control_plane`) defaulting to `local`.
- Add a one-time, explicit migration command that reads the current local file,
  validates it, and creates the namespace only if it is absent. It must preserve
  a protected local backup and report the created revision. No automatic
  migration on startup.
- Define first-write behavior, schema version, maximum payload size, canonical
  serialization and malformed-payload handling before writing anything.
- Decide whether the local file remains a read-only seed, a last-known-good
  cache, or is removed. Do not silently keep two writable sources.
- Document that mixed-version fleets must not enable the shared source until
  every replica supports the schema and write path.

**Conflict, split-brain and rollback behavior (must be specified)**

- All writers use CAS and return a typed conflict to the admin/CLI. The client
  re-reads and replays a typed patch; there is no silent last-writer-wins for
  whole-document saves.
- A stale replica may serve its in-memory snapshot until refresh; it must never
  write without a fresh revision. The UI must show the revision it is editing.
- During a partition, reads may be stale but writes either reach the shared
  primary or fail; they never fall back to a local authoritative write.
- Config rollback is a new CAS revision that restores an older document, not a
  deletion of history. Binary rollback requires the older binary to understand
  the schema; otherwise the rollback is blocked, not silently run in local mode.
- Restore from backup must either (a) restore into a local/file target with
  explicit operator intent, or (b) perform a CAS write against the current
  shared revision. It must refuse to overwrite a newer shared revision silently.

**Availability and security failure modes (must be specified)**

- Startup read failure: choose fail closed (consistent with the current
  database control plane) or last-known-good local cache. A `fail_open` empty
  config is not acceptable.
- Runtime refresh failure: keep the last validated in-memory config and keep
  serving; surface staleness. Never replace a validated config with a partial or
  malformed one.
- Write failure/conflict: reject the admin/CLI operation with a clear error;
  the data plane continues on the last validated config.
- Security: authenticate/authorize control-plane access, bound payload size,
  reject unknown schema and unknown fields where practical, redact secrets from
  errors/events, and audit who changed which revision. Database compromise must
  not automatically disclose upstream credentials under the references-only
  model.

### Option 3 — read-only control-plane observability

Publish a redacted config fingerprint (schema/revision/hash/timestamp and
non-secret metadata) to the control plane, or expose each replica's config
identity through `/api/hello`. The local file remains the only authority; the
shared copy is never read back as config.

**Advantages**

- No secret custody change and no config write path; the blast radius is small.
- Operators can detect drift between replicas and verify which config revision
  each replica is running.
- Useful for dashboards and incident response without pretending to provide HA.

**Disadvantages**

- Does not propagate edits, so it does not satisfy "shared configuration".
- Adds writes and a new redaction surface; a hash can still be a correlation
  signal, and a careless payload could leak provider names or URLs.
- Can create a false sense of synchronization unless the UI labels it
  observability-only.
- Still needs a schema, retention and failure policy; a read-only feature is not
  free, only smaller.

Option 3 is a reasonable **future additive observability slice** if operators
need drift detection. It must not be described as shared config or HA.

## Comparison

| Concern | Option 1: local authoritative | Option 2: shared CAS | Option 3: read-only observability |
|---|---|---|---|
| Source of truth | Local file per replica | Config namespace | Local file |
| Propagates admin/CLI edits | No | Yes (with CAS) | No |
| Secret custody | Existing per-replica key/config pair | New cluster key or references-only decision | Unchanged |
| Bootstrap/migration | None | Explicit, one-time, risky | None |
| Mixed-version rollout | Safe (existing behavior) | Must gate and order | Safe, but no config change |
| Conflict handling | Process-local revisions only | Store-level CAS + typed conflict | N/A |
| Split-brain | No shared writer; replicas may drift | Reduced by CAS, not eliminated | No writer |
| Backup/restore | Existing bundle contract | Must include source/revision and CAS restore | Existing plus redacted metadata |
| Rollback | File/binary rollback | Revision CAS + schema compatibility | N/A |
| Availability | Local file availability | New startup/refresh policy required | Store failure is non-fatal |
| Security blast radius | Existing key/config pair | Larger unless references-only | Small, with redaction |

## Consequences of the recommendation

- Configuration remains explicitly per-instance; `/api/hello` continues to
  report `config_snapshot: "per-instance"` for every control-plane backend.
- Operators must distribute the config through deployment tooling and must not
  expect admin edits to reach other replicas.
- Backup/restore remains scoped to local config, local security state and local
  file control-plane state; external PostgreSQL/Redis state remains
  operator-owned.
- Any future Option 2 work starts as a separate approved implementation issue
  with its own ADR update/superseding record, tests and migration plan. It is
  not a documentation fix.

## Owner decisions required

**Answered 2026-10-22** (decision recorded at the top of this ADR):
decision 1 — **No**, cross-replica propagation is not required;
Option 1 is accepted. Decisions 2–9 are moot unless an owner decision
reopens propagation; they are preserved below as the required agenda
for any such reopening.

1. **Product need:** Is cross-replica configuration propagation required, or is
   local-plus-deployment-tooling acceptable indefinitely? If not required, close
   the shared-config thread and keep Option 1.
2. **Source of truth:** If propagation is required, is the config namespace the
   only authority, or can a replica start from a local file when the store is
   unavailable? A dual writable source is not acceptable.
3. **Secret custody:** Choose shared KEK, references-only shared config
   (recommended), or per-replica re-encryption. Define key rotation and recovery.
4. **Bootstrap/migration:** Approve an explicit migration command, its
   authorization, first-write behavior, local backup retention, and whether
   existing inline secrets must be converted to references before enabling.
5. **Mixed-version rollout:** Define the minimum version/schema, feature gate,
   rollout order, and behavior when an older replica is still running. The
   default must be fail closed, not silent local writes.
6. **Availability policy:** Choose fail-closed startup versus last-known-good
   local cache, and the maximum tolerated staleness. Define write-conflict UX.
7. **Conflict and rollback:** Decide CAS-conflict handling (reject/re-read/
   replay), whether whole-document rollback is allowed, and how restore from a
   bundle interacts with a newer shared revision.
8. **Backup/restore ownership:** Decide whether a bundle includes the shared
   config snapshot, who owns external PostgreSQL/Redis backups, and how restore
   verifies revision compatibility.
9. **Security/audit:** Define access control to the config namespace, audit
   records for config writes, payload size/schema validation, and redaction
   requirements for logs, events and `/api/hello`.

Until decisions 1–9 are resolved and an implementation issue is approved, the
correct behavior is the current one: local config, atomic local save, explicit
per-instance reporting, and no shared-config writes or migrations.

## References

- `internal/config/config.go`
- `internal/secrets/secrets.go`, `internal/secrets/key_provider.go`
- `internal/controlplane/types.go`, `internal/controlplane/manager.go`
- `cmd/gateway/controlplane_runtime.go`, `cmd/gateway/backup.go`, `cmd/gateway/main.go`
- `internal/httpapi/server.go`, `internal/httpapi/settings.go`, `internal/httpapi/deployment.go`
- `docs/HA_STATE_INVENTORY.md`, `docs/KNOWN_GAPS.md`, `docs/CONFIGURATION.md`,
  `docs/DEPLOYMENT.md`, `docs/OPERATIONS.md`
- `docs/reports/shared-state-design.md` (breaker/affinity design; not config)
- HA regression: `internal/httpapi/deployment_test.go` →
  `TestConfigSnapshotReportMatchesLocalStore`
