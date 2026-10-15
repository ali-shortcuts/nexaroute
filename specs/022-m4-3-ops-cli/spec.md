# M4-3 — Operations CLI: doctor + backup/restore

## Existing state (verified)

`config validate|diff|dry-run` is implemented (redacted diff, exit codes
0/1/2), graceful shutdown with drain is implemented in the gateway, and
safe reload goes through config validation — BUT the implementation was
unreachable: main() never dispatched the `config` subcommand, so the
binary ignored it and started the gateway (defect found by the
2026-10-15 Phase 0 audit and proven against a binary built from main).
This unit fixes the dispatch behind one testable routing function with
a regression test, and closes the remaining genuine gaps (also the
deferred security-audit findings): no `doctor` diagnostics command, and
backup/restore is a manual, untested file-copy procedure that does not
cover the audit/security store.

## doctor (new, read-only)

`nexaroute doctor [--config path]` prints one PASS/WARN/FAIL line per
check and exits 0 (no FAIL), 1 (any FAIL) or 2 (usage). It never writes
and never prints secret values, DSNs or URLs — only statuses, counts and
permission bits. Checks:

- config: loads via the read-only loader and validates; warnings are
  reported as a count.
- config file permissions: unreadable = FAIL; group/other-readable =
  WARN (the file can hold encrypted secrets and key references).
- master key: when the stored config contains `nxs1:` envelopes, the
  read-only load must succeed (it resolves the key) — otherwise FAIL
  with guidance; key material is never read into output.
- security store: bbolt path resolution mirrors the server rule; when
  the file exists it is opened READ-ONLY and the audit chain is
  verified (counts only); a redis/postgres security-store selection is
  a WARN in this build (adapters are M4-2 scope; admin auth fails
  closed). Missing file = WARN (created on first start).
- control plane: disabled = PASS (zero-service default); file state
  present + parseable; postgres/redis = env presence + bounded ping
  with driver errors reduced to a failure class (no DSN/URL echo).
- listen address parses; provider/model counts are sane.

## backup / restore (new)

`nexaroute backup [--config path] --out file.tar [--include-master-key]`
writes a single tar bundle, atomically (temp file + fsync + rename,
mode 0600):

- `manifest.json` (first entry): format `nexaroute-backup/1`, creation
  time, gateway version, per-file SHA-256 + size, whether the master
  key is included, and the SHA-256 fingerprint of the on-disk master
  key when one exists (a hash, never the key).
- `config.json`: the config file exactly as stored (encrypted at rest
  stays encrypted).
- `security.db`: a consistent online snapshot of the audit/security
  store via a new `securitystore.Store.BackupTo` (read-only open +
  bbolt transaction WriteTo) — omitted with a manifest note when the
  store does not exist yet.
- `controlplane.json`: the file-backend control-plane state when
  present.
- `master.key`: ONLY with `--include-master-key` (the key otherwise
  stays an independent secret, per OPERATIONS pairing guidance).

`nexaroute restore --bundle file.tar [--config target] [--force]`
validates EVERYTHING before writing ANYTHING: tar structure, manifest
format version, per-file SHA-256/size, and a parse of the bundled
config. It then installs files atomically (temp + rename, 0600; parent
dirs 0700 where created) to the paths resolved from the target config
location and the bundled config's own settings. Existing targets are
refused without `--force`; a bundled master key whose fingerprint
differs from an existing key file is refused without `--force` even
then with a loud warning, because overwriting the only key copy can
destroy access to every encrypted secret. Restore prints the follow-up
checks to run (`doctor`, `audit verify`, `secrets verify`).

## Tests

CLI integration tests for exit codes and redaction; backup→restore
round trip (byte-identical files); tampered bundle rejected with
nothing written; overwrite refusals; doctor healthy/broken fixtures;
`Store.BackupTo` produces a snapshot that opens read-only with a valid
chain.

## Out of scope

Drain/reload changes (already implemented), remote backup targets,
scheduled backups, and the security-store database backends (M4-2).
