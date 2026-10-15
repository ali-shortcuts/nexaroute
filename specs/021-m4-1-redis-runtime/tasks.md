# M4-1 Redis runtime tasks

- [x] T1 go-redis pinned (v9.17.0; graph stays on Go 1.25 floor)
- [x] T2 RedisStore with Lua CAS put/delete + corrupt-data semantics
- [x] T3 Unit tests with a script-emulating fake client
- [x] T4 Runtime wiring: env URL, bounded client, ping, closer lifecycle, redacted errors
- [x] T5 Real-Redis integration tests (gated) + CI service job
- [ ] T6 Full gates + PR checks incl. the integration job on the PR head
- [ ] T7 Checklist: M4-1 DONE (both backends, real-service evidence), M4-2 remainder recorded
