# NexaRoute v0.16.3 Operations Manual

Version: v0.16.3

Target: Gateway operators and administrators

## 1. Start and configure

NexaRoute is a self-contained gateway binary. The default config path is selected by the CLI resolver; use `-config` to select an explicit file:

```bash
./nexaroute
./nexaroute -config /etc/nexaroute/config.json
./nexaroute -config /etc/nexaroute/config.json -no-browser
```

The data-plane listener defaults to `127.0.0.1:8080`. Admin is local-only by default and shares the gateway listener; it is not a separately configured listener. The runtime overrides are:

| Variable | Purpose |
|---|---|
| `NEXAROUTE_CONFIG` | Default config path used by the CLI resolver |
| `NEXAROUTE_LISTEN` | Listener address |
| `NEXAROUTE_ADMIN_KEY` | Process-scoped Admin key override |
| `NEXAROUTE_ADMIN_BIND_LOCAL_ONLY` | Boolean override for local-only Admin mode |
| `NEXAROUTE_LOG_FILE` | Log path override; `off` disables the app-owned file sink |
| `NEXAROUTE_STRICT_CONFIG` | Refuse startup when implicit defaults/legacy migration are required |
| `NEXAROUTE_MASTER_KEY` | Base64-encoded 32-byte master-key override |
| `NEXAROUTE_MASTER_KEY_FILE` | Raw 32-byte master-key file override, mode `0600` |

Master-key source precedence, encryption coverage, and failure behavior are described in the canonical [security model](SECURITY.md). Do not use legacy environment-variable names copied from older operations guides.

## 2. Secret storage, backup, and restore

Secret-bearing config values are encrypted at rest. The default auto-managed master key lives at `<config-path>.key`; the config and key are separate files and both are needed to restore encrypted values. Protect the key independently from the config backup. Loss of the only matching key is unrecoverable.

### Back up before an upgrade or key rotation

1. Restrict the backup directory (`0700`) and the backup files (`0600`). Use an encrypted, access-controlled backup target separate from the gateway host where practical.
2. Copy the active encrypted config and its exact matching master key as a pair. If an explicit key source is configured, back up that source through its secret-management system instead of copying a different key.
3. Record the pairing securely and test restoration in an isolated environment. Never assume that a config backup can be decrypted by the current key after later rotations.
4. Preserve any `.enc.bak` migration files together with the matching master key. These backups are authenticated encrypted blobs, not standalone plaintext JSON configs.

Example for an auto-managed key (adapt paths and secure storage to the deployment):

```bash
umask 077
install -d -m 0700 "$BACKUP_DIR"
install -m 0600 "$CONFIG" "$BACKUP_DIR/config.json"
install -m 0600 "${CONFIG}.key" "$BACKUP_DIR/config.json.key"
```

To restore, stop the gateway, restore the selected config and its matching key, set the key mode to `0600`, verify directory permissions, then run `nexaroute secrets verify --config "$CONFIG"` before starting the listener. Do not create a replacement key to troubleshoot authentication failures.

### Backup and restore commands (preferred)

`nexaroute backup` bundles everything an installation needs into one hash-verified archive (format `nexaroute-backup/1`): the config exactly as stored, a consistent online snapshot of the audit/security store (safe to run against a live gateway), and the file-backend control-plane state when present. The bundle is written atomically with mode `0600`; its manifest records a SHA-256 fingerprint of the master key so pairing can be checked later.

```bash
nexaroute backup --config "$CONFIG" --out "$BACKUP_DIR/nexaroute-backup.tar"
# Only when the key must travel with the backup (store the bundle like a secret):
nexaroute backup --config "$CONFIG" --out "$BACKUP_DIR/nexaroute-backup-with-key.tar" --include-master-key
```

The master key is excluded by default: keep it as an independent secret (above) unless the deployment deliberately accepts a combined bundle.

`nexaroute restore` verifies the whole bundle — format, manifest, and every file's SHA-256 — *before* writing anything, then installs files atomically with mode `0600`. Existing different files are refused without `--force`, and a bundled key that would overwrite a different key warns loudly for the same reason.

Restore failure behavior: files install in a fixed order (config, security store, control-plane state, master key). Before the first write, restore journals the previous bytes of every file it will replace — including its permission bits, and, for a target that is a symlink, the link target (installing replaces the link with a regular file; rollback recreates the link). Targets that are neither regular files nor symlinks (directories, devices) are refused before anything is written. If any install step fails, restore rolls back the files already written (journaled bytes and modes restored, newly created files removed, symlinks recreated) and the error says so.

Crash recovery: the journal is a durable, versioned record (`nexaroute-restore-journal/1`). Each saved pre-restore copy is written through an open handle and **fsynced individually** (a directory sync alone does not make a file's bytes durable), the journal directory entry is synced after creation, and its manifest — destination paths, prior existence, modes, symlink targets, saved-copy names *and their SHA-256 checksums*, per-entry progress and a transaction state (`prepared` → `installing` → `completed`) — is written atomically and fsynced (file and directory) *before* the first install, after every install, and at completion; journal creation and deletion also sync the parent directory (best effort) so the journal's own existence is durable. If preparing the journal fails at any step, restore aborts with every destination still untouched. If the restore process is killed or crashes mid-install, the journal survives; the next `nexaroute restore` run and `nexaroute doctor` both report it, and

```bash
nexaroute restore --recover <journal-dir>
```

first *strictly validates* the journal, then rolls every destination back to its journaled pre-restore state (recovery restores *all* entries, because a crash can land between an install and its progress update) and removes the journal only when every destination was recovered — otherwise the journal is preserved and the error names the destinations that still need recovery. Validation treats the journal as untrusted input: the directory must be a real, private (`0700`) directory owned by the invoking user — not a symlink, not group/other-accessible — with a regular, private manifest; the state must be a known one, the ID well-formed, the entry list small and limited to the known backup entries without duplicates; every destination must be absolute, and no two may canonicalize to the same file; saved-state metadata must be consistent (an absent destination carries none, a symlink entry carries only its link target), and every saved copy must be a private regular file named exactly `entry-<n>` whose content **verifies against its recorded SHA-256**. Any failure refuses recovery before anything changes, and the journal is preserved. A journal whose transaction reached `completed` is cleaned up by `--recover`, never rolled back, and an unknown journal format is refused, never replayed. Validation cannot prove a journal was written by NexaRoute (journals are unsigned), so treat the journal directory as operator-trusted input inside the same trust boundary as the config directory: a fully self-consistent forged journal in a private directory could direct recovery writes to its recorded destinations.

Destination durability: installing is more than a rename — after every destination mutation (install, in-process rollback, and `--recover` recovery alike), restore syncs the destination's parent directory (and its grandparent, which holds the parent's own entry when the install created it) before the transaction may advance, complete, or discard the journal. A failed directory sync is never read as "the mutation did not happen": a rename that landed counts as installed even when its sync fails (the destination joins the rollback scope and the transaction aborts), and a rollback or recovery mutation whose sync fails counts as *unconfirmed* — the destination is named, the journal is preserved, and the operation can simply be retried, because rollback and recovery are idempotent. The journal is removed only after every destination change has been made and synced.

One edge is deliberately loud: if every install lands but the final `completed` state cannot be persisted, restore exits nonzero, keeps the journal (still `installing` on disk), and says so — the files on disk reflect the restored bundle, but the transaction is not recorded as finished. Verify with `nexaroute doctor`, then either roll back with `--recover` or remove the journal manually; restore never reports success while silently discarding the only recovery record.

What is *not* guaranteed: rollback/recovery is best-effort, not a filesystem transaction — atomic rename cannot be made atomic *across* files, a rollback write can itself fail (disk full, permissions changed mid-restore), and power-loss durability extends only as far as the filesystem and hardware honor `fsync`. Run `nexaroute doctor` before starting the gateway after any failed or interrupted restore.

TOCTOU scope: destination validation canonicalizes paths component-wise (symlinked ancestors and not-yet-existing deeper directories resolve correctly), the journal re-inspects every destination immediately before installing, and each install is an atomic rename that never writes *through* a final-component symlink. A concurrent process with write access to the destination directories could still swap a path between validation and install; closing that class completely would require file-descriptor-relative, no-follow I/O that portable Go does not expose, so restore's documented assumption is the same trust boundary as the installation itself: **the config directory and its subdirectories must not be writable by untrusted users** (the operational guidance elsewhere in this document keeps them at `0700`).

Restore also refuses, before writing, any bundle whose payloads would install to aliased destinations (two entries resolving to the same path — including through symlinked ancestors with missing descendants, and hard links between existing targets) and any payload inconsistent with the bundled configuration's backends (for example a security-store payload when the bundled config selects a non-bbolt security store).

Two related guarantees: `nexaroute backup` refuses an `--out` path that aliases any of its sources (the config, the master key, the security store, or the control-plane state — including symlink and hard-link aliases), because the bundle write atomically replaces the output path. And a backup never silently omits an unreadable source: only a genuinely absent security store, control-plane state file, or key file is skipped (and noted in the manifest); permission or I/O errors fail the backup.

```bash
nexaroute restore --bundle "$BACKUP_DIR/nexaroute-backup.tar" --config "$CONFIG"
nexaroute doctor --config "$CONFIG"   # read-only health + chain verification
```

`nexaroute doctor` is the read-only diagnostics entry point: config load/validation, file permissions, master-key resolvability and its file permissions, pending/interrupted key rotation, security-store audit-chain verification (counts only), control-plane backend readiness (bounded ping for Postgres/Redis, failure classes only — never DSNs or URLs), and the listen address. With `--bundle PATH` it additionally preflights a backup bundle: integrity, freshness (`--max-age`), master-key/config pairing, and control-plane schema/migration compatibility. Exit codes are `0` (no failures), `1` (a check failed), `2` (usage).

### Isolated restore verification and drills

Restore is verified before it is trusted. `nexaroute restore --verify` performs the whole restore *read* path — bundle structure and per-file SHA-256, config decryption/validation (including route-profile/pool/provider referential integrity), audit-chain integrity, control-plane schema, master-key pairing, and external-store ownership — **without writing to any live target**. With `--into DIR` it also materializes the bundle into an empty disposable directory for inspection.

```bash
# Verify only; write a machine-readable report as auditable drill evidence.
nexaroute restore --verify --bundle "$BACKUP_DIR/nexaroute-backup.tar" \
  --config "$CONFIG" --report "$EVIDENCE/restore-verify.json" --max-age 24h

# Verify and materialize the restored bytes into a disposable directory.
nexaroute restore --verify --bundle "$BACKUP_DIR/nexaroute-backup.tar" \
  --config "$CONFIG" --into "$DRILL_DIR" --report "$EVIDENCE/restore-verify.json" --max-age 24h
```

Each named check reports `PASS`, `FAIL`, `BLOCKED`, or `UNVERIFIED`; the command exits `0` only when nothing failed and nothing is blocked, and `1` when any check is `FAIL` or `BLOCKED`. The report (format `nexaroute-restore-verify/1`, mode `0600`) carries bundle/version references, per-file hashes, statuses and timings only — never secret values or ciphertext.

Fail-closed semantics: an encrypted config with no key material available is `BLOCKED` (never `PASS`); a config that does not decrypt with a key that *is* available is `FAIL`; a tampered audit chain or an unsupported control-plane schema is `FAIL`. `--into` refuses a directory that is not a real, empty, private directory, so a drill can never overwrite production. `--max-age 0` (the default) leaves freshness `UNVERIFIED` rather than implying it was checked.

### Recovery objectives (examples, not guarantees)

NexaRoute does not promise RPO/RTO; the numbers below are placeholders an operator replaces with values **measured by a drill** on the actual deployment. Decide and record both a single-node and, when used, a multi-instance objective.

- **Single-node, file backends.** The application bundle covers the encrypted config, a consistent bbolt security/audit snapshot, and file-backend control-plane state. Example objective: RPO = the backup interval (for example `24h`, and never more than the age recorded in the last `restore --verify` report); RTO = time to a `restore --verify` `PASS`, install, `nexaroute doctor` `PASS`, and service restart. Measure both by running the drill below and recording the timings in the report.
- **Multi-instance or external state.** The application bundle does **not** snapshot external PostgreSQL, Redis, or video assets/jobs. RPO/RTO for those are owned by the database/object-store tooling (replication, PITR, snapshots) and must be measured separately. `restore --verify` reports `external_store_ownership` as `BLOCKED` when the config depends on them, so a bundle is never mistaken for full recovery.

### Retention and backup rotation

- Keep the backup directory `0700` and each bundle `0600`; store bundles on an encrypted, access-controlled target separate from the gateway host where practical.
- Retain a small rotation set (for example 14 daily + 8 weekly) plus one known-good pre-upgrade bundle. Never delete the last verified bundle; a replacement only supersedes it after its own `restore --verify` returns `PASS`.
- Bundles created with `--include-master-key` are secret material: keep them under the same custody as the key, or prefer independent key custody and exclude the key.
- **An application bundle is not a database backup.** It captures only what `nexaroute backup` read: the config, the bbolt security/audit snapshot, and file-backend control-plane state. It never snapshots an external database, Redis, or video assets/jobs unless the operator separately included a configured database backup. Enumerate each external store and confirm its backup/replication independently.
- Safe deletion: remove a superseded bundle only after its replacement verifies `PASS`; securely delete any bundle that ever carried a master key according to the key-custody policy.

### Restore-drill runbook (repeatable)

1. **Pre-upgrade backup.** `nexaroute backup --config "$CONFIG" --out "$B/pre-upgrade.tar"` (add `--include-master-key` only when key custody requires a combined bundle).
2. **Isolated verification.** `nexaroute restore --verify --bundle "$B/pre-upgrade.tar" --config "$CONFIG" --into "$DRILL" --report "$EVIDENCE/drill.json" --max-age 24h`. Require status `PASS`; investigate any `BLOCKED`/`FAIL` before proceeding.
3. **Prepared rollback.** Keep the current config, `.key`, and security store; if a real restore is interrupted, `nexaroute restore --recover <journal-dir>` rolls the transaction back.
4. **Key recovery.** If decryption is `BLOCKED` or `FAIL`, restore the exact paired `<config>.key` (or the matching pre-rotation `.key.previous`); never generate a replacement key for an encrypted config.
5. **Service restart.** Stop the gateway, install the verified bundle (`nexaroute restore --bundle ... --config "$CONFIG"`), then `nexaroute doctor --config "$CONFIG" --bundle "$B/pre-upgrade.tar" --max-age 24h` before starting.
6. **Post-restore smoke.** Start against a local fake/loopback provider (no paid calls): confirm config loads, `nexaroute audit verify` passes, `nexaroute secrets verify` reports the expected fields, and a synthetic request succeeds.


### Rotate the auto-managed key

`nexaroute secrets rotate` currently supports the automatically managed sibling key only; it refuses while `NEXAROUTE_MASTER_KEY` or `NEXAROUTE_MASTER_KEY_FILE` is set. For environment/KMS-managed keys, use the deployment's approved key-management workflow; this CLI does not rotate those sources.

```bash
nexaroute secrets status --config "$CONFIG"
nexaroute secrets verify --config "$CONFIG"
# Back up the current encrypted config and matching key before proceeding.
nexaroute secrets rotate --config "$CONFIG"
nexaroute secrets verify --config "$CONFIG"
```

Rotation retains the previous key as `<config-path>.key.previous`, stages the replacement as `.key.next`, replaces the encrypted config atomically, and promotes the staged key. At startup, NexaRoute checks which key authenticates the config before promoting a staged key. A staged key that does not match the current config is not activated. The fixed `.key.previous` is replaced on the next rotation, so preserve a securely paired copy before rotating again.

### Recover an interrupted rotation

If the process stops during rotation, preserve the config, `.key`, `.key.next`, and `.key.previous`; do not delete or overwrite any of them. Restart or run `nexaroute secrets verify --config "$CONFIG"` with the same config path: when the config authenticates with the staged key, NexaRoute promotes `.key.next` to `.key`. If verification does not succeed, restore a known-good encrypted config and its matching key from backup. Do not start an older gateway with an encrypted config unless it supports the same `nxs1` envelope format.

## 3. Secrets CLI and plaintext handling

- `nexaroute secrets status --config PATH` reports encrypted-field counts only.
- `nexaroute secrets verify --config PATH` authenticates/decrypts in memory and reports field identifiers and status, never secret values.
- `nexaroute secrets decrypt --config PATH --to-stdout --allow-plaintext` is a deliberate plaintext export. Both flags are mandatory. Restrict terminal capture and shell redirection; if a plaintext file is unavoidable, create it in a private directory with mode `0600`, use it only for the specific migration/rollback, and securely remove it afterward.
- The first load of a legacy plaintext config migrates it atomically and leaves a timestamped encrypted `.enc.bak`; normal runtime/Admin config saves do not create a backup copy.

## 3a. Audit chain verification

Admin audit records in the security store form a tamper-evident hash
chain (M3-6): every record carries a monotonic sequence, the previous
record's hash, and its own SHA-256 hash over a canonical encoding of its
fields. Retention/count pruning deletes only a prefix of the chain and
records a pruning anchor (the newest pruned record's sequence and hash),
so the surviving window stays verifiable.

### Decision policy lifecycle (`nexaroute policy`, M6-2)

Policies are code: they live in `decision_policies[]` and the
active one is the `decision.policy` pointer. The lifecycle CLI
never serves traffic:

- `nexaroute policy simulate --config PATH --policy ID --fixtures FILE`
  evaluates a policy against recorded-shape decision requests
  (candidates + signals only) and prints the winner, the
  effective order, the policy trace breakdown, and a SHA-256
  digest of the canonical report — identical inputs always yield
  the identical digest.
- `nexaroute policy replay --config PATH --policy ID --traces FILE.jsonl`
  re-evaluates recorded production decisions (`{request,
  recorded_winner}` per line) under a candidate policy and
  reports agreement plus every divergence. Review this before
  promoting a policy change.
- `nexaroute policy promote --config PATH --policy ID` validates
  the whole config, then atomically points `decision.policy` at
  ID and appends a record (timestamp, from/to, config SHA-256
  before/after) to `<config>.policy-audit.jsonl`.
- `nexaroute policy rollback --config PATH` restores the previous
  pointer from the audit log. It refuses when the config file's
  digest no longer matches the last audited action (out-of-band
  change) unless `--force` is given.

Whole-config rollback remains the admin API's config history;
this CLI is the policy-pointer lifecycle on the config file.

**Policy versions and safe change (Phase 12).** A policy's effective version is
its explicit `decision_policies[].version` label or, when unset, the derived
content fingerprint `auto:<sha256[:8]>`. The applied version is recorded with
every served decision (event, trace breakdown) and printed by `simulate` /
`replay`; `replay` reports `version_mismatches` so a divergence explained only by
a policy revision is distinguishable from a genuine behavior change. Set
`decision_policies[].min_observations` to require a minimum success+failure
sample count before a deployment's reliability evidence is trusted (below it the
reliability score decays to neutral, preventing single-sample flapping; `0`
disables the gate). Set `decision.shadow: true` (with `decision.mode` not `off`)
to evaluate the configured policy on live traffic for observation only: the
gateway emits `decision_shadow` events recording the proposed selection and
whether it differs from the production primary, but never reorders served
traffic and never issues extra inference. Use offline `policy replay` for
recorded-traffic comparison and live shadow mode for a bounded window before
promoting a policy pointer.

- `nexaroute audit verify --config PATH` opens the store **read-only**
  and revalidates every record: hash recomputation, predecessor
  linkage, sequence order, anchor linkage, and chain-head consistency
  (which detects tail truncation). Exit codes: `0` verified, `1`
  integrity problems (each printed), `2` usage/operational error.
  `--format json` emits the structured report. Output is metadata only
  (counts, sequences, head hash) — never event payloads.
- The store is a single-writer bbolt file: while the gateway is
  running it holds the lock and verification fails with exit `2`.
  Stop the gateway for offline verification.
- Records written before the chain existed are reported as a *legacy
  pre-chain* segment. They are counted, never claimed as verified.
- **Limits:** the chain is local and unanchored. It detects edits,
  deletions, insertions, reordering, and truncation of the stored
  records, but an attacker who controls the whole database file —
  records *and* chain metadata — can rewrite history consistently.
  Record the reported head hash in an external system (ticket, log
  archive, second host) after each verification if you need evidence
  that survives full-store compromise. Investigate any `FAILED`
  result before trusting or rotating the store: preserve the file,
  compare against your last externally recorded head hash, and treat
  unexplained gaps as a security incident.

## 3b. Privacy audit CLI

`nexaroute privacy audit --config PATH` inspects the configuration and the
runtime policy surfaces derived from it (providers, route profiles,
candidate pools, virtual keys, egress policy, cache, logging) and reports
privacy problems that can be determined locally. It is **read-only**: it
never decrypts, migrates, or rewrites the config file, and it sends no
provider requests. Findings name configuration objects and fields only —
the audit never prints API keys, credential values, authorization headers,
URL userinfo, or request bodies.

```bash
nexaroute privacy audit --config "$CONFIG"              # human-readable findings
nexaroute privacy audit --config "$CONFIG" --format json  # machine-readable report
```

Exit codes are deterministic:

| Exit | Meaning |
| --- | --- |
| `0` | Audit completed; no error-severity findings |
| `1` | Audit completed; at least one error-severity finding |
| `2` | Usage error, or the config could not be read or parsed |

Error findings include plaintext credentials stored in the config file
(`PLAINTEXT_SECRET`), credentials embedded in provider URLs
(`URL_CREDENTIALS`), cleartext `http://` endpoints on non-loopback hosts
(`CLEARTEXT_ENDPOINT`), and `no_training` requirements (route profile or
virtual key) that no reachable deployment can satisfy
(`NO_TRAINING_UNSATISFIABLE`) — those requests fail closed at runtime, so
the audit mirrors the runtime gate exactly. Warnings and infos cover
partial `no_training` pools, `logging.access_mode=all`, an enabled
response cache, undeclared provider data-handling classes, and an empty
operator egress deny policy.

## 4. Health and status

Failure behavior under upstream and platform faults — what clients see, how recovery works per routing strategy, and the tests that pin each behavior — is specified in [`docs/FAILURE_SEMANTICS.md`](FAILURE_SEMANTICS.md).


- `GET /healthz` is a liveness endpoint.
- `GET /readyz` reports whether an eligible deployment is ready.
- `GET /admin/api/health` reports deployment-level health details and requires the configured Admin access boundary.

A deployment must satisfy the configured health and capability requirements before it is eligible for traffic. See [configuration](CONFIGURATION.md) for probe cadence, leases, and recovery behavior.

Separately from live health, the gateway computes **static eligibility warnings** (M3-3) from the configuration alone: no enabled model deployments, a referenced candidate pool that matches no enabled deployment, a route profile whose whole fallback chain is unusable, and `no_training` profiles or virtual keys with no deployment satisfying the fail-closed privacy rule. These warnings are printed at startup with the other config warnings, returned by `PUT /admin/api/settings`, and exposed in `GET /admin/api/snapshot` as `eligibility_warnings`. They are advisory only — they never fail config validation (including strict mode). The snapshot also carries `route_eligibility`: per-profile candidate counts with current health-based exclusion reasons (privacy-filtered, provider unavailable, retired, cooldown, health not yet established, not healthy). Those counts reflect pool membership and health only; per-request filters (model, capabilities, context window) are not applied.

### Shared control-plane state

The optional `control_plane` section defaults to the atomic local `file`
backend. Its state path is relative to the config directory unless absolute;
the file and parent directory are created with restrictive permissions. The
gateway writes versioned snapshots with optimistic revisions and never logs
the contents of environment-provided DSNs or URLs. `memory` is test-only.

Before changing a backend, copy the current state file to a protected backup:

```bash
install -m 0600 "$CONFIG.controlplane.json" "$BACKUP_DIR/controlplane.json"
```

Set `control_plane.migration_dry_run=true` for startup validation without a
state write. For Redis this means only a bounded connectivity `PING`: dry-run
does not read, initialize, or mutate a snapshot and does not validate the
deployment's persistence or recovery configuration.

Redis stores each namespace in a hash under
`nexaroute:control:snapshot:<namespace>` with no expiration. Lua scripts apply
revision checks atomically on the Redis server executing the script; they do
not promise multi-primary consistency or cross-replica safety. A successful
connection is not a durability guarantee. Configure, monitor, and test AOF
and/or RDB persistence, durable storage, replication/failover, and backup/
restore according to the deployment's recovery objectives. An acknowledged
write may still be lost during a Redis failure or promotion depending on that
configuration.

The PostgreSQL and Redis control-plane clients are wired and have disposable-
service integration coverage. These tests verify adapter behavior, not a
deployment's recovery guarantees. Store switching does not automatically copy
or convert state: preserve the previous backend and its protected backup until
the destination state is verified. To roll back to the local file backend,
stop the gateway, restore the validated file backup to the configured state
path with mode `0600`, set `control_plane.backend` to `file`, run
`config validate`, and restart. A partial temp file is not a committed state;
never delete the last valid backup until the replacement has been verified. The
Redis-backed admin security-store adapter is a separate subsystem with its
own configuration, and cross-replica breaker synchronization is an opt-in
layer that runs over this control-plane store (`control_plane.breaker_sync`,
ADR 0003); neither changes the store's own state format.

## 4a. Diagnostic bundles (secret-safe, operator-invoked)

When an incident needs investigation, an operator can produce a single
shareable file instead of copying configuration or log excerpts by hand:

```bash
nexaroute diagnostics bundle --config /path/to/config.json --output /path/to/incident-bundle.json
```

The command is explicit and offline: it never starts the gateway, never
sends anything to any external service, and never prints the bundle to
stdout. It reuses the `doctor` checks and the `config validate` path
(including the TLS preflight), and it builds a new JSON document
(`nexaroute-diagnostics/v1`) from an allowlist of fields only:

- Gateway version, VCS commit where the build recorded one, Go version,
  OS/architecture, and CPU count. No hostname is included.
- Config validation summary: valid/invalid, provider/model/route-profile
  counts, sanitized config warnings, TLS, control-plane backend, video
  enabled, and logging mode (`auto`/`off`/`custom` — never a path).
- Provider readiness summaries: id, name, type, auth mode, enabled,
  base URL reduced to scheme://host, model counts, credential presence
  as a boolean, credential source as an env-var name or `configured`,
  and custom header names only. These are configuration-derived
  readiness facts, not live health: live deployment health and live
  counters come from `/admin/api/health` and `/metrics` on a running
  gateway, which this offline bundle deliberately does not contain.
- Doctor checks (config, master-key permissions, security store, control
  plane, restore journals, and optionally a backup-bundle preflight with
  `--bundle`/`--max-age`), each as status/name/sanitized detail.
- Sanitized recent errors: at most 50 lines from a bounded tail (256 KiB,
  last 200 lines) of the gateway log file, each truncated, with lines
  that still look secret or payload-shaped dropped and counted.
  Pass `--no-log-errors` to exclude this section.
- Bounded counts: check pass/warn/fail totals, audit-chain record count
  when the doctor check established it, and provider/model/error totals.

Never included: credentials, master keys, tokens, cookies, authorization
headers, prompt text, model responses, raw configuration, URL user info,
URL query strings or fragments, and full filesystem paths (doctor
details are reduced to file basenames). Sanitization runs before
serialization, and the serialized bytes are scanned a second time; any
scan failure aborts the write and leaves no file behind. A field that
cannot be proven safe is dropped or withheld, never copied verbatim —
including validation errors, which are reduced to a sanitized class
string, and malformed configuration, which still yields a bundle with
`config.valid=false` rather than a crash or a raw dump.

The file is written atomically (temp file in the destination directory,
mode `0600`, fsync, rename) and an existing file is never overwritten
unless `--overwrite` is given. Exit codes: 0 = bundle written, 1 =
bundle could not be produced safely or written, 2 = usage error. A
failing doctor check is data inside the bundle, not a command failure.

Before sharing, read the file yourself: the command prints what it
contains and what it excluded, but it cannot judge context. The bundle
still contains operational metadata that may be sensitive in context —
provider hostnames, model names, file basenames, check statuses, and
counts — so share it only with a party you trust with operational
detail, over a channel with equivalent protection, and never commit it
to a repository or store it alongside backups.

Retention and deletion: a bundle is an incident artifact, not a backup
and not evidence to keep indefinitely. Delete it when the incident is
closed (for example `shred -u` or your host's accepted sanitization
policy for the storage medium), and delete any copies forwarded to a
support channel once the case is resolved. If a bundle is ever suspected
of containing secret material, treat it as a credential exposure: rotate
the affected credentials, then delete every copy.

## 5. Admin surface, events, and logs

Admin API access is privileged. Keep it loopback-only unless OIDC, TLS, and external network controls are configured. Provider keys, pooled credentials, custom header values, and proxy URLs are write-only and redacted on Admin reads and exports. The event feed uses Server-Sent Events; clients can reconnect with `Last-Event-ID`, and should refresh the snapshot after an epoch change or sequence gap.

The default app-owned file sink is bounded and mode `0600`; console mirroring is rate-limited. Request/response bodies are not written as normal access-log fields. Preserve log rotation limits and restrict access to the log directory.

### OIDC and session operations

When `admin.oidc.enabled` is true, human users sign in through the configured Authorization Code + PKCE flow. Before rollout:

1. Register the exact HTTPS redirect URI `/admin/auth/oidc/callback` with the issuer and provision the named client-secret environment variable through the service manager/secret injector.
2. Configure `issuer_url`, `client_id`, `audience`, and a reviewed `role_mappings` allowlist. The discovered issuer must match exactly. Do not map unreviewed provider groups to `admin`.
3. Set `emergency_access_enabled` deliberately. It is not a provider-outage fallback; with OIDC enabled, the API key is not accepted if discovery/login is unavailable. Old configs missing this explicit flag load with emergency access disabled.
4. Place `security_store_path` on durable local storage. Relative paths are resolved against the config directory; the default is `security/nexaroute-security.db`, whose missing `security/` child is created mode `0700`. For an explicit absolute path, create a private parent when needed (for example, `install -d -m 0700 /var/lib/nexaroute`). Startup creates a 0600 bbolt file and fails if an existing parent is not private or the file is a symlink/group/world-readable. Back it up using access controls appropriate for identity and audit records. Do not share this DB over NFS or across gateway instances.
5. Use one gateway instance or sticky routing for the full OIDC login round-trip: pending state/nonce/PKCE/browser-binding data is process-local. Server-side sessions and audit records survive restarts on the same host, but distributed session/audit coordination is not implemented.

The UI session cookie is opaque, `HttpOnly`, `SameSite=Lax`, scoped to `/admin/`, and `Secure` on HTTPS. The server stores only its hash and the verified identity/roles. `session_ttl_seconds` sets the absolute lifetime; `idle_timeout_seconds` revokes idle sessions. Browser mutations and logout require same-origin validation and the strict CSRF cookie/header pair. Login rotates any prior session; logout revokes the server-side record before clearing cookies. A changed OIDC policy fingerprint invalidates old sessions.

Structured audit records contain request ID, actor, action, target, method/path, outcome, status, permission, and authentication method; no body, code, token, cookie, or session ID is recorded. Retention is limited by both `audit_retention_days` and `audit_max_events`. The security-store HTTP API is admin-only. A failed required audit write returns 503 and stops the protected action; investigate disk permissions, free space, filesystem health, and DB lock state rather than disabling audit. A recovery from provider outage requires a trusted operator to change configuration explicitly, restart, and restore OIDC after break-glass use; do not rely on silent fallback.

## 6. Graceful shutdown and rollback

NexaRoute handles `SIGINT` and `SIGTERM` by stopping new accepts, canceling background probes, and draining in-flight work. The drain window is based on the request timeout and the longest configured stream idle timeout, with a minimum safety window; it is not a fixed 15-second deadline.

A binary rollback must account for encrypted config compatibility. Do not point a pre-encryption binary at an `nxs1` config. Prefer restoring a pre-upgrade config/key pair. If the only usable source is encrypted, use the current binary's explicitly gated decrypt command to create a temporary private plaintext config, protect it as mode `0600`, and follow the older binary's documented config path; after the rollback window, remove the plaintext file according to the host's accepted data-sanitization policy (unlinking alone may not securely erase flash-backed storage). Test this process before relying on it in production.

For unresolved network exposure, keyring, CSRF/session, RBAC, or provider-egress limits, consult [docs/KNOWN_GAPS.md](KNOWN_GAPS.md) and [docs/SECURITY.md](SECURITY.md).

## 7. TLS, mTLS, and config preflight
The shared listener can be configured for HTTPS with `tls.enabled`, certificate/key paths, and optional `client_ca_file`. TLS 1.2 is the minimum; certificate files are re-read for new handshakes. `require_client_cert_admin` and `require_client_cert_data_plane` independently protect the Admin and `/v1/*` routes. Keep the Admin API key enabled: a client certificate does not replace authorization.

Before rollout, use the read-only checks with the exact config path:
```bash
nexaroute config validate --config /etc/nexaroute/config.json
nexaroute config diff --config /etc/nexaroute/config.json --against /etc/nexaroute/previous.json
nexaroute config dry-run --config /etc/nexaroute/config.json
```
`diff` emits paths/status only and never secret values. `dry-run` opens no listener and sends no probes. Loading a legacy plaintext config may perform the documented encrypted migration side effect; this is the only expected on-disk mutation.

## 8. Phase status and deliberate boundaries

The Phase 1 hardening work is complete on `main`: encrypted secret storage,
TLS/mTLS, browser CSRF checks, and the read-only `config validate|diff|dry-run`
preflight commands are implemented and covered by repository gates. Current
coverage and WP4 acceptance status are recorded in PR #224 and
[`docs/reports/WP4B_REVIEW.md`](reports/WP4B_REVIEW.md); do not use older
baseline percentages in this manual as a current measurement.

Phase 2 is **in progress**. The opt-in video runtime is partial: its local fake
provider is for development/tests, and no real external provider is claimed
verified. PR #224 implements OIDC-backed `viewer`/`operator`/`admin` sessions,
route/method RBAC, and a local durable security/audit store; live
external-IdP interoperability is unverified. The provider egress policy and
local control-plane file backend are merged, while distributed sessions/audit,
live Postgres/Redis runtime interoperability, and real external KMS/keyring
interoperability remain unverified. Do not treat transport security or client
keys as an egress-policy or distributed-state substitute.

Virtual-key tenant/project/team scopes are evaluated as parent-to-child
intersections. A wildcard on a child key cannot widen a configured parent
allow-list; unknown or inconsistent references deny the request. Keep these
scopes explicit in configuration and test the deny path before exposing the
data plane.

The video runtime is opt-in through `video.enabled` and fails closed unless
`video.auth_token_env` names a non-empty bearer token in the process environment.
It uses a bounded worker queue and an atomic local JSON job store; that store is
single-process and is not a multi-node coordination mechanism. On restart,
queued jobs are recovered, known provider job IDs are resumed, and ambiguous
submissions without a provider job ID require manual action rather than risking
a duplicate billable submission. Shutdown cancels and joins the worker pool
before returning. Use the fake provider only for local development/tests; no
real external provider integration is claimed verified. See
[`docs/VIDEO_GATEWAY.md`](VIDEO_GATEWAY.md) and [`docs/KNOWN_GAPS.md`](KNOWN_GAPS.md)
for the supported surface and remaining limits.
