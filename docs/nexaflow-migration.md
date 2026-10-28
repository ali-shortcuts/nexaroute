# NexaFlow Migration and Upgrade Notes (Phase 8)

## Store format versions

The workflow store is versioned. The current format version is 2.

- Format 1 records: version records without `content_hash`,
  runs without the optimistic-concurrency `version` field, and no
  `meta.json` marker.
- Format 2 records: content hashes on version records, run
  `version` counters, and a `meta.json` carrying
  `{"format_version": 2}`.

## Upgrade procedure (format 1 → 2)

1. Stop the gateway (or stop dispatching: the store is
   single-host, and migration must not race execution).
2. Back up the store directory.
3. Start the gateway with NexaFlow enabled. Startup runs
   `Migrate` before serving:
   - Missing version-record hashes are recomputed from the stored
     definition content. Content is never rewritten; only the
     derived hash is filled in.
   - Runs with a zero `version` are stamped `1`.
   - `meta.json` is written at format 2.
   - Migration is a no-op when the store is already current.
4. Verify: definitions still list, the latest published revision
   is unchanged, and a sample run's step history is intact
   (migration never drops, reorders, or rewrites steps).
5. Rollback of the migration itself is restoring the backed-up
   store directory and restarting the previous build; there is
   no down-migration that rewrites format 2 records into format 1
   (the fields format 2 adds are additive, and older builds
   ignore unknown fields only if they tolerate them — do not rely
   on that; restore the backup instead).

Migration is covered by `TestPhase8FileStoreMigration`, which
hand-downgrades on-disk records (hash removed, run version zeroed,
`meta.json` deleted) and asserts hashes recompute, histories
survive, and the format marker advances.

## Definition and revision upgrades

- Definitions are immutable snapshots. Changing a definition
  means saving a new draft and publishing a new revision; running
  runs keep the revision they started on.
- Republishing an older snapshot as a new revision is the
  rollback path. There is no in-place revision edit, no
  "unpublish", and no revision deletion in this phase.
- Schedule identity (`definition:trigger`) survives republication,
  so upgrading a scheduled definition does not reset its fire
  clock or double-fire its next slot.
- Renaming a trigger instance creates a new schedule. If you
  rename a schedule trigger, expect the new schedule to fire on
  its next tick and the old schedule's state to stop advancing.

## Gateway upgrades with in-flight runs

- `running` runs at shutdown are interrupted. On the next startup,
  `Recover` parks them with their ambiguous step named. They do
  not auto-resume. The operator resumes each parked run with an
  explicit mode (see the user guide's crash-recovery section).
- `awaiting_approval` runs survive restarts unchanged: the pause,
  the decision (once given), and the step history all live in the
  store.
- `parked` runs survive restarts unchanged.
- Never delete the store directory to "clear" parked runs: that
  destroys definitions, versions, history, claims, and audit
  together. Cancel or resolve parked runs through the API.

## What is NOT migrated

- There is no PostgreSQL workflow store in this phase, so there is
  no file-to-PostgreSQL migration to run. When that store lands,
  it will need its own migration path; file-store data will not
  silently move.
- In-memory stores (`MemoryStore`) are test-only: everything in
  them is lost on exit by design. Never enable NexaFlow in
  production with anything but the FileStore in this phase.
