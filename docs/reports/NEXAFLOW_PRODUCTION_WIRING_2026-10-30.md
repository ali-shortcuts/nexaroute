# NexaFlow production wiring — evidence report (issue #765)

Date: 2026-10-30. Base: main `fc89961`. Head: `4995948`
(`supervisor/issue765-prod-wiring`). No live, paid, or production
calls; all evidence is loopback/local.

## What landed in this branch

- `cmd/gateway` calls `AttachNexaflow` only when
  `nexaflow.enabled` is true, before `api.Handler()` is captured.
  Flag off: no routes, no store, no behaviour change.
- Config section `nexaflow`: `enabled` (default false),
  `store_backend` (`file` default, or explicit `memory`),
  `store_path` (required for the file backend). Unknown backends
  and missing paths fail config validation, hence gateway startup;
  a corrupt or locked file store fails at open time. There is no
  silent fallback to memory. Memory is explicit-only and documented
  as volatile.
- Construction: file store (single-process lock, atomic persist)
  or memory store → `DefaultRegistry()` → engine with `Backend:
  nil` (backend nodes fail closed as `backend_unavailable`) and
  default (random per process) worker id → service with nil
  budget guard → one `Recover()` pass at startup (reconciles only;
  never auto-executes) → attach. Shutdown releases the store lock
  after the HTTP graceful drain.
- No scheduler daemon, no webhook listener, no loopback gateway
  backend, no PostgreSQL store, no `workflow.*` permission, no
  tenant dimension. Authz keeps the interim `config.read` /
  `config.write` / `audit.read` mapping; runs are filtered by
  principal subject (ownership ceiling, not tenant isolation).
  Approval separation of duties applies: a run created with the
  single emergency admin key cannot be approved with that same key.

## Evidence (all on head `4995948`)

| Check | Result |
|---|---|
| `TestNexaflowWiring*` (`cmd/gateway`, plain) | PASS |
| `TestNexaflowProductionWiring*` (`internal/httpapi`, plain) | PASS |
| Same two suites with `-race` | PASS |
| `internal/config` full package | PASS |
| `gofmt`, `py_compile`, `git diff --check` | clean |
| Browser acceptance, production class (`scripts/test-browser-nexaflow-studio.py`, boots the real gateway with the flag on) | PASS, all 15 cases, no SKIP line |
| Binary E2E (below) | PASS |

Binary E2E: binary built from head `4995948` with
`go build -o /tmp/nexaroute-765 ./cmd/gateway`, sha256
`c8f7c6d93b04f4b6b48c4a691703d0abfa3eb86a342ed4cae3e8e2ba0e530674`.
Booted with `nexaflow.enabled: true`, file store in a temp dir:
`/healthz` 200, `/admin/api/nexaflow/nodes` 200, `/studio.html`
200, draft create 201, publish 200 (revision 1), run
`run_cc27b166d346581129f0658f44c8eef0` succeeded, SIGTERM
shutdown, restart: draft and run still present and succeeded.

Restart/lease semantics pinned by test (canonical rule): after a
restart with a new worker id, the first resume stalls (run stays
running, approval node stays pending) while the old worker's live
30 s lease holds; after lease expiry the same store resumes to
completion. Immediate resume after restart is therefore NOT
claimed to succeed.

## Explicitly unsupported (unchanged by this wiring)

Scheduler daemon, webhook listener, loopback gateway backend
(LLM/MCP/video/gateway-HTTP nodes), PostgreSQL store,
`workflow.*` permissions, tenant isolation, subworkflows,
any-join, durable timer waits, hard-budget enforcement,
live evaluation, video cancellation.

## Relation to the parallel candidates (no merge by this branch)

PR #779 (supervisor core) and PR #771 (opencode) cover the same
scope. A supervisor review note on #779 recommends landing
#779-core after its memory-default fix plus the script boot fix,
and keeping PostgreSQL and startup recovery as follow-up
decisions. This branch differs from #779-core on: config key
`store_backend` (vs `store`); file as the default backend with
`store_path` required (the review's fix, already satisfied here);
startup `Recover()` run at boot (a follow-up in the #779 line);
worker id left at the engine default (random per process).
Contrary to one line in that review note, this branch does NOT
offer PostgreSQL and does NOT use a hostname-pid worker id.
Only one `attachNexaflow` and one config key can land; this
branch is published as a ready alternative and is NOT merged
unilaterally while the owner choice between candidates is
pending. Issue #765 stays open until merge AND a merged-main
recheck, per its acceptance criteria.
