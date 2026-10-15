# M4-1 PostgreSQL runtime tasks

- [x] T1 pgx driver pinned (v5.11.0 + x/text v0.41.0 govulncheck-fixed; Go floor raised 1.24 → 1.25, see spec)
- [x] T2 DefaultMigrationRegistry (v1 snapshots table)
- [x] T3 SQLStore.Put advisory-lock race fix + scripted-driver regression
- [x] T4 Runtime wiring: env DSN, bounded pool, ping, migrations, Close lifecycle
- [x] T5 Runtime fail-closed/redaction tests (canary DSN)
- [x] T6 Real-PostgreSQL integration tests (gated) + CI service job
- [ ] T7 Full gates + PR checks incl. the integration job on the PR head
