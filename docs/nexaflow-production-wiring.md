# NexaFlow production wiring (issue #765)

The production gateway mounts the Task 8 NexaFlow service behind an
explicit opt-in flag. Default is OFF: with the flag off, gateway
behaviour is byte-identical to a gateway without this wiring — no
NexaFlow route is mounted, no store is opened, no engine is built.

## Flag and store options

Config section `nexaflow` (see `internal/config`):

| Key | Default | Meaning |
| --- | --- | --- |
| `nexaflow.enabled` | `false` | Opt-in master switch. Only when `true` does `cmd/gateway` call `AttachNexaflow`. |
| `nexaflow.store` | `"memory"` | Store backend: `"memory"` or `"file"`. The PostgreSQL store is deliberately NOT offered here. |
| `nexaflow.store_path` | (empty) | Required when `store` is `"file"`: path of the durable single-host JSON store file. |

Example (opt-in with the durable single-host store):

```json
{
  "nexaflow": {
    "enabled": true,
    "store": "file",
    "store_path": "nexaflow-store.json"
  }
}
```

Validation (fail-closed at config load, before any route mounts):

- `store` must be `memory` or `file` when enabled; anything else
  (including `postgres`) is rejected.
- `store=file` without `store_path` is rejected.
- When disabled, the whole section is inert: a stray `nexaflow`
  block never fails validation and never opens a store.

Startup semantics: a construction error (unopenable file store,
engine/service build failure) fails gateway startup instead of
serving a half-wired service. The file store takes an exclusive,
non-blocking flock on `<path>.lock`, so a second live gateway on the
same path fails to start rather than corrupting state. The memory
store loses all drafts, revisions, runs and approvals on restart.

## What the wiring mounts

With the flag on, the gateway mounts exactly the service the Task 8
HTTP tests exercise, behind the existing admin authentication
(`x-admin-key`, loopback binding unchanged):

- `GET /admin/api/nexaflow/nodes` and `/templates` (+ detail),
- `POST /admin/api/nexaflow/templates/{id}/instantiate` (saves a
  draft),
- `POST /admin/api/nexaflow/definitions/validate` (draft-body
  validation; invalid definitions answer `valid:false` with HTTP
  200, never a stubbed success),
- draft CRUD, run listing, run detail, and the audit route,
- the standalone Studio page (`/studio.html`), which degrades
  gracefully when the service is absent.

The engine runs with `Backend: nil`, exactly as in the Task 8 probe
precedent: pure nodes execute; any node that needs a provider backend
fails closed with `backend_unavailable` from the registry path. No
provider backend is wired, so no live model call can happen through
this wiring. The `BudgetGuard` is nil, matching the same precedent:
`budget_check` stays report-only and admission reserve is not claimed.

## Preserved-unsupported list (never claimed by this wiring)

The following stay unsupported, and no stub reports success for them:

- live eval
- hard-budget enforcement (admission reserve + settle accounting)
- scheduler daemon (no tick, no schedule state, no startup recovery,
  no daily schedules, no trigger discovery)
- webhook listener (`AttachNexaflowWebhook` is never called by this
  wiring; `secret_ref` resolution and published-trigger validation
  are not implemented)
- video cancellation
- live-PostgreSQL history (the PostgreSQL store is not offered)
- any-join
- `workflow.*` HTTP permissions (admin routes ride the interim
  `config.read`/`config.write`/`audit.read` mapping)
- tenant isolation (ownership ceiling + scope label only)
- subworkflows
- resume support
- studio UI beyond the merged Studio page

Run-level policy approval is likewise not claimed: approvals keep
the Task 8 separation-of-duties rule (approver different from
requester) and nothing more.

## Evidence and limits

- Unit/acceptance tests through the real gateway server object live
  in `cmd/gateway/nexaflow_wiring_test.go` (default-off, opt-in
  memory, instantiate + draft validation, file-store reopen,
  invalid-store fail-closed) and
  `internal/config/nexaflow_config_test.go` (section validation).
- The browser acceptance script
  (`scripts/test-browser-nexaflow-studio.py`) runs its 15 cases
  against a pre-wired server when `NEXAFLOW_STUDIO_BASE_URL` points
  at a running gateway with the flag on. Its self-boot path still
  boots the gateway without the flag (it only touches keys the
  example config carries), so it still SKIPs on the boot path until
  that script is taught to enable the flag; that is a follow-up to
  the script, not a change to this wiring.
- Production-gateway browser acceptance is therefore: real binary +
  flag on + `NEXAFLOW_STUDIO_BASE_URL` run (recorded in the PR
  body), not the script's self-boot path.
