# Upgrade, Migration and Rollback Acceptance — 2026-10-27

Scope: repeatable evidence that a supported existing NexaRoute installation
can upgrade safely and recover when an upgrade or migration fails. All work
is test-only: no production code, no configuration default, and no security
check was changed. Nothing here merges anything and nothing here releases
anything; the accompanying branch is offered for owner review only.

## Source versions

- New (under test): main `e6c3cdf3fe0a193dda0c5857e91e46b541d04729`
  (branch `supervisor/upgrade-migration-rollback-acceptance`).
- Prior release used for binary stages: v0.24.0
  `ac5d7aa36e0b0c129ff615fc9a9bfb191f016796`.
- Format compatibility checked by tag inspection before coding:
  `nxs1:` envelope (AES-256-GCM, key-id binding) is byte-identical in shape
  across v0.21.0 `2b2fa337`, v0.22.0 `2d45f0d8`, v0.23.0 `05003db9`, and
  v0.24.0 `ac5d7aa3`; backup format `nexaroute-backup/1` is identical at
  v0.24.0 and on main (same entry names: manifest.json, config.json,
  security.db, controlplane.json, master.key when opted in).

## What was added

1. `internal/config/upgrade_acceptance_test.go` — versioned fixture matrix
   (plaintext legacy, nxs1 prior-release shape built with
   `secrets.TransformConfig`, legacy `routing.public_model`, minimal older
   config), key custody across migration (`.key` bytes unchanged by
   migration, reload, and client-key persist; `.enc.bak` decrypts to exactly
   the original plaintext with the same key; config tightened 0644→0600),
   and migration-failure preservation (bad key mode 0644 and wrong key both
   fail with the original config, key, and no `.enc.bak` untouched; fixing
   the mode retries cleanly). Synthetic canaries only.
2. `cmd/gateway/upgrade_acceptance_test.go` — restore key custody: a
   keyless bundle restored with `--force` over a differing target key
   leaves that key byte-identical and the pair then fails closed on load;
   a keyed bundle with `--force` prints the overwrite warning and only
   then replaces the key; refusal without `--force` leaves config, key,
   security store, and modes untouched and the target still loads.
3. `scripts/upgrade-acceptance.sh` — disposable end-to-end binary drill
   (one mktemp directory; synthetic canaries; explicit `-config`; install
   dir and config home inside the temp dir). Stages: build new + old pairs
   with ldflags versions; old binary starts on a plaintext fixture and
   migrates it; health + API routes before upgrade; admin provider create
   (201, redacted); secrets verify + config validate/diff/dry-run; live
   backup probe (see Finding below); stop; doctor; stopped pre-upgrade
   backup (keyless + keyed); cross-version `restore --verify` drill with
   the new binary; installer swap; post-upgrade doctor; post-upgrade run;
   SIGKILL + restart; tamper and wrong-key guards on copies; rollback
   restore + old-binary restart; arm64 cross-build. The script is
   deliberately not wired into CI: the App workflow-permission failure
   precedent from the #664 lane showed CI wiring can fail for reasons
   unrelated to the code, and `verify.sh` already gates script syntax
   (`bash -n` over `scripts/*.sh`).

## Results — Go tests (local, Go 1.25.1, root, x86_64)

Commands (from the worktree root, with the local toolchain on PATH and
`TMPDIR`/`GOTMPDIR` under the workspace, per the `/tmp` tmpfs lesson):

- `go test -count=1 ./internal/config/ ./cmd/gateway/`
  → `ok internal/config 4.622s`, `ok cmd/gateway 173.087s`
- `go test -race -count=1 -run 'TestUpgrade' ./internal/config/ ./cmd/gateway/`
  → `ok internal/config 1.689s`, `ok cmd/gateway 5.999s`
- `gofmt -l` on both new test files → no output (clean);
  `go vet ./internal/config/ ./cmd/gateway/` → clean.
- `bash -n scripts/upgrade-acceptance.sh` → clean.

One earlier local race run was lost to a runtime restart (session gone,
result unknown, never claimed); the race result above is a fresh rerun.

## Results — binary drill (local, 2026-10-27 evening +0430)

Command: `./scripts/upgrade-acceptance.sh --report <path>` (report lines
below verbatim, in order):

- `PASS build new pair (e6c3cdf)`
- `PASS build old pair (v0.24.0 ac5d7aa36e0b0c129ff615fc9a9bfb191f016796)`
- `PASS pre-upgrade health + routes (old v0.24.0): healthz/hello/models/snapshot 200, readyz=503 count_tokens=200`
- `PASS pre admin provider create (201, redacted)`
- `PASS pre config validate/diff/dry-run + secrets verify (verified_fields=3)`
- `PASS pre migration: nxs1 on disk, key bytes recorded, modes 0600/0700`
- `LIVE_BACKUP_FAILED (pre-upgrade binary (old v0.24.0)): nexaroute backup failed: open security store for snapshot: open security store read-only: timeout`
- `LIVE_BACKUP_FAILED (new binary): nexaroute backup failed: open security store for snapshot: open security store read-only: timeout`
- `PASS pre doctor (gateway stopped)`
- `PASS pre-upgrade backup (stopped gateway, keyless + keyed), bundle 0600, manifest fingerprint matches live key`
- `PASS isolated restore --verify drill with new binary (no FAIL/BLOCKED)` —
  drill checks: bundle_integrity PASS, bundle_freshness PASS,
  config_decryption PASS, route_usability PASS, master_key_pairing PASS,
  audit_chain PASS, control_plane_state UNVERIFIED (no file control-plane
  state in the fixture), external_store_ownership PASS,
  isolated_materialization PASS
- `PASS installer upgrade: versions changed, config/key bytes unchanged`
- `PASS post-upgrade doctor with pre-upgrade bundle`
- `PASS post-upgrade health + routes + secrets verify (verified_fields=3, key unchanged)`
- `PASS SIGKILL restart: lock released, config + provider intact`
- `PASS tampered bundle: verify + install both fail closed, nothing written`
- `PASS wrong-key bundle: restore --verify fails closed (pairing mismatch)`
- `PASS rollback (old binary v0.24.0 (binary rollback)): health + routes + secrets verify restored`
- `PASS arm64 cross-build (gateway + videogen)`
- Final line: `UPGRADE ACCEPTANCE PASS (old-binary stages: executed)`

Two script defects were found and fixed during the drill itself (both in
the script, not in product code): a caller umask of 022 created the
security-store directory 0755, which the gateway rightly refuses (script
now sets `umask 077` and chmods 0700 explicitly), and a fixture with
`emergency_access_enabled: false` returned 401 on the loopback snapshot
route (fixture now uses the default fresh-install value `true`, matching
`scripts/test-install.py`).

## Results — installer tests (local)

- `bash scripts/test-installer-e2e.sh` → `INSTALLER E2E PASS`
- `bash scripts/test-install.sh` (builds `dist/` locally via
  `build-release.sh v0.23.0`, nothing published; `dist/` is gitignored) →
  `PASS clean install, PATH, architecture selection, checksum/download rejection`,
  `PASS UI/browser readiness while provider probes are blocked; -no-browser`,
  `PASS real startup, headless/browser launch, duplicate/port exclusion, upgrade/reinstall, restart persistence, write-only secrets, crash recovery`,
  then `INSTALLER E2E PASS`.

## Finding — live backup against a running gateway fails closed

`docs/OPERATIONS.md` describes `nexaroute backup` as "a consistent online
snapshot of the audit/security store (safe to run against a live
gateway)". Executed evidence contradicts that wording for the
file-backend store: with the gateway running, both the v0.24.0 binary and
the new binary fail with `open security store read-only: timeout`. The
mechanism is visible in code — the running gateway holds the bbolt
exclusive flock, and `internal/securitystore/store.go` `OpenReadOnly`
uses a 2-second timeout (`bolt.Options{ReadOnly: true, Timeout: 2 *
time.Second}`); the same code path exists on main and in v0.24.0. The
script therefore probes live backup, records the failure, and takes the
real pre-upgrade backup after stopping the gateway (the standard
pre-upgrade procedure). This PR does not change the backup code or the
docs wording: fixing live backup (copy-then-open, a different lock
discipline) or correcting the docs is a product decision for the owner,
and it is flagged here rather than silently worked around.

## NOT_RUN — scenarios not executed, with reasons

- arm64 execution: no aarch64 CPU or qemu in this environment. Executed
  substitutes: arm64 cross-build (this script) and arch-selection bytes
  (`scripts/test-install.py`).
- Deterministic kill mid-migration / mid-restore at binary level:
  migration is a single atomic rename; restore crash recovery is covered
  by the Go journal tests including the subprocess crash test.
- Power-loss durability: fsync-bounded only; see `docs/OPERATIONS.md`.
- Production rollback: forbidden by design; all rollback stages ran in
  disposable directories.
- Live tenants, paid providers, multi-host, real Kubernetes: out of
  scope for this task (Kubernetes acceptance lives in a separate PR).
- PostgreSQL control-plane migration across versions: only schema
  version 1 exists, so there is no v2 to migrate from; the file backend
  has no versioned schema.
- Installer pair atomicity: the installer replaces `nexaroute` then
  `videogen` sequentially; a failure between the two replacements would
  leave mixed versions. No test injects that failure; it is a documented
  limitation, not claimed as atomic-as-a-pair.
- Full `./scripts/verify.sh`: not run locally (heavy baseline; other
  sessions own the shared VM's heavy runs, and sibling PRs report the
  same local timeouts under parallel load). The affected packages,
  race, gofmt, vet, script syntax, and installer suites above all ran
  locally; CI on the PR is the wider gate.

## Already covered upstream (not duplicated here)

Plaintext migration + idempotency; legacy `routing.public_model` +
strict refusal; client-key hash persist; missing-key no-replacement;
staged-key rotation recovery; backup key exclusion/opt-in + manifest
fingerprint; restore tamper/overwrite/rollback-on-failure/journal crash
recovery; `restore --verify` isolated drill; doctor preflight; PG v1
migration; file control-plane state preservation across restart;
installer mock e2e + real-binary lifecycle; config validate/diff/dry-run
legacy behavior; smoke-local routes.
