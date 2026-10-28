# NexaFlow Security Guidance (Phase 8)

## Threat model in one paragraph

Workflows execute operator-authored definitions with the gateway's
own credentials: they can call models (billable), MCP tools
(external side effects), video jobs, and HTTP endpoints. The
controls below exist so a workflow cannot silently spend, exfiltrate,
or repeat a side effect, and so a compromised or mistaken definition
fails closed instead of running anyway.

## Authorization

- API boundary: `workflow.read` (GET) / `workflow.write`
  (state changes). Owner, admin, and operator hold both;
  developer and viewer hold read only.
- Node boundary: every node execution checks the run actor's
  permission for that node (`routing.read` for LLM, `providers.read`
  for MCP, `video.manage` for video, `usage.read` for cost,
  `evaluation.run` for eval). A run whose actor lacks a node's
  permission fails at that node with `permission_denied`; earlier
  nodes may already have executed.
- Operator policy: nodes that spend money or touch external
  systems beyond reads (`llm.chat`, `video.submit`, non-loopback
  `http.request`, policy templates) additionally require
  `settings.policy_approved: true`, recorded at publish time.
  Runs inherit it; without it those nodes fail with
  `policy_required`. Policy approval is a publish-time operator
  decision, not a per-run checkbox.
- Approval separation: an approver must differ from the run
  requester, enforced by the engine and re-checked by the approval
  node itself. Self-approval is refused.
- Webhook authentication is HMAC, not the admin session (see the
  user guide). The webhook endpoint is outside `/admin/api` on
  purpose; do not "protect" it with admin auth instead of HMAC.

## Secrets

- Definitions, runs, step history, and audit events must never
  contain secret values. Webhook and HTTP credentials travel as
  `secret_ref` values resolved at execution time by the
  operator-supplied resolver. In this phase the resolver maps a
  reference to an environment variable name and fails closed when
  unset or empty. There is no vault or keyring integration for
  general secret refs in this phase.
- The gateway's `Authorization` header is rejected in node input
  and config: credentials come only from `secret_ref`.
- Node outputs and error messages pass through redaction before
  storage: secret-shaped keys are replaced with `[REDACTED]`,
  bearer-shaped values are replaced wholesale, and strings over
  8192 characters are truncated. Step history is stored redacted;
  registry outputs are redacted before the engine ever sees them.
- Known gap, stated plainly: the run record persists the raw
  workflow input (`run.input`) so a run can be inspected and
  re-driven. Do not put secret values in workflow input; use
  `secret_ref` for anything credential-shaped. Audit events carry
  no payloads at all.
- Workflow input, webhook bodies, and node inputs/outputs are
  bounded (64 KiB workflow input; per-node bounds). Oversized
  values fail; they are never silently truncated into a different
  meaning, except the 8192-character redaction truncation of
  stored strings noted above.

## Execution safety

- No blind repetition: crashed runs park with the ambiguous node
  named. Only an explicit operator resume mode re-executes,
  skips, or fails it. Scheduler slots are claimed before dispatch.
  Webhook deliveries are ledger-deduplicated. Idempotency keys
  deduplicate manual dispatches. A suspended batch keeps the work
  its parallel nodes already did; resuming never re-executes them.
- Bounds are enforced, not advisory: loop caps (100), step caps
  (5000), merge key caps (1000), MCP allowlist caps, config size
  caps, run and node timeouts, per-definition concurrency
  (default 1), engine concurrency (16). A run that exceeds its
  step cap or run timeout fails; it is not silently extended.
- `budget_check` is a report, not enforcement. A workflow that
  must not overspend needs the (unapproved) hard-budget proposal
  or an external control; do not present `budget_check` as a
  spending guard.
- HTTP egress: `http.request` allows https plus loopback http,
  refuses cross-origin redirects, bounds responses, and rejects
  `Authorization` in input/config. Non-loopback URLs require
  operator policy approval.
- MCP: a tool call requires the tool in the node's allowlist AND
  in the gateway's listing for that server, and the gateway broker
  additionally enforces its own server/tool allowlists, tenant
  allowlists, and per-key rate limits. A forbidden tool is refused
  before any backend call (`tool_forbidden`, zero calls made).
- Video: submissions go through the video HTTP API, never
  directly to the queue store (which alone does not run a job).
  Asset downloads go through the asset path; unlisted assets are
  refused.

## Tenancy (limitation)

`TenantScope` on a definition is an admin-plane label. The store
filters drafts and runs by scope, and the API exposes that filter.
But `Identity` carries no tenant, so the engine and API do not
block a principal from starting a run of another scope's
definition. Until a tenant claim exists on identity, treat scope
as organization, not isolation: scope who may hold
`workflow.write` accordingly, and do not use scope as the sole
boundary between mutually untrusted tenants.

## Operational rules

- Keep the workflow store on a private path (mode 0700
  directories, 0600 files; the FileStore enforces this). Back it
  up with the gateway config: definitions, versions, runs, claims,
  deliveries, schedule state, and audit live there.
- Never point two gateway processes at one store path in this
  phase. The FileStore is single-host; cross-process safety is a
  PostgreSQL-store phase.
- Rotate webhook secrets by changing the referenced environment
  variable and restarting (or re-resolving, where the deployment
  supports it). Old signatures remain rejected by the replay
  ledger for their original delivery, which is intended.
- Review `run_parked` and `run_finished` with failure codes in
  the workflow audit trail as part of incident review; parked
  runs are the visible form of "something interrupted execution".
