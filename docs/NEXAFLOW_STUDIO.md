# NexaFlow Visual Workflow Studio (Phase 6)

A real visual editor for NexaFlow workflows, integrated into the NexaRoute
control plane (`Workflows` page) and backed by the workflow HTTP API. It is
not a mockup: every list, canvas, validation, publish and run action calls
the server, and runs execute against the canonical node registry
(`internal/nexaflow`, PR #675 as hardened by Phase 7).

## What it is

- **Workflow list** — search, create, rename, duplicate, publish,
  enable/disable and delete. Deleting cancels live runs first.
- **Editor** — a grouped, searchable node palette (click to add, with
  cascade placement, or drag onto the canvas), a canvas with node drag,
  port-to-port edges, background pan, cursor-anchored Ctrl+wheel zoom,
  keyboard arrows/Delete on focused nodes, and a schema-generated
  configuration panel. The panel renders the backend's node schemas
  (`/admin/api/nexaflow/nodes`); the browser invents no node schema.
- **Validation** — the panel shows structured issues from the server
  (unknown node, bad config, missing trigger, unsatisfied required
  input, cycle, …). Validate and Publish first save the canvas, because
  the server validates and publishes the *stored* draft; judging an
  unsaved canvas would report a stale graph.
- **Versions and runs** — publishing snapshots the draft as an immutable
  version (content hash = SHA-256 of the graph JSON). Runs pin a version:
  later draft edits and publishes never change a run. The run view shows
  per-node status, attempts, duration, a bounded (16 KiB) redacted output
  preview, and safe error details, plus run history, cancel, and
  approve/reject for approval gates.

## Graph and execution semantics (v1)

- Sequential topological dataflow. An edge copies one named output field
  into one named input field. Every node runs once, after its
  predecessors succeed. Condition/branch nodes are value producers and
  do **not** gate downstream execution; conditional execution is a future
  phase, not a silent reinterpretation.
- A graph declares `inputs`: parameter names a run may supply. A required
  node input is satisfied by an incoming edge or a declared input name.
  `StartRun` rejects undeclared run-input keys (allowed: declared inputs,
  `payload`, and trigger-node schema fields). Trigger nodes are exempt
  from the edge/declared rule.
- Caps: 200 nodes / 500 edges per graph, 500 workflows, 200 runs per
  workflow (oldest evicted), run input 256 KiB, output previews 16 KiB.

## Approval semantics (Phase 7)

An approval node **always** pauses the run (`awaiting_approval`, durable
across gateway restarts). Only an authenticated `ApproveRun` decision
executes it; a decision carried by an edge or a run input is
workflow-authored data and is ignored — it must never mint an approval.
At execution the runtime binds approver = the approving caller's
authenticated subject and requester = the run's authenticated starter.
The registry node rejects anonymous or self-approval, so **in a
single-identity deployment (for example keyless emergency mode) the run
starter cannot approve their own run**; that failure is surfaced
honestly in the run view. Rejecting continues with `approved=false`
and is not a run failure.

## Authorization

All `/admin/api/nexaflow/*` routes require admin authentication and map
to `routing.read` (GET) / `routing.write` (mutations) — the Phase 1
delegation decision to defer new `workflow.*` permissions. Hiding a
button is not authorization: the server re-checks on every call, and
`Registry.Execute` re-checks each node's own permission against the run
starter's preserved identity and roles.

## Secrets and previews

No secret ever reaches the browser as a value. Node config holds
references only (`env:NAME`, resolved server-side from the process
environment, fail-closed for any other form). Run inputs and node
outputs are rendered only as bounded, redacted previews; secret-shaped
keys are replaced before serialization. Never put a secret in workflow
JSON, a URL, browser storage, or console output.

## Known backend limitations (honest, not hidden)

- **Video nodes return `backend_unavailable`.** Video jobs are operated
  via the Video Studio bearer token, which the workflow service does not
  hold; inventing a token path would be a credential bypass.
- **LLM chat runs via a gateway self-call** (loopback base URL wired
  from the actual listener). It works on keyless gateways and fails
  closed with the gateway's 401 on key-gated gateways, because the
  service never mints or borrows a client credential.
- **No scheduler daemon, webhook listener, or UI/API wiring for
  triggers beyond manual runs** in this phase: schedule/webhook trigger
  nodes exist in the registry as pure computations, and runs are
  started explicitly by an operator.
- **No PostgreSQL-backed run history** in this phase: the store is a
  file-backed JSON document next to the gateway config
  (`nexaflow-workflows.json`, atomic temp + `0600` + fsync + rename
  writes, mirroring the config writer), or memory-only when the server
  has no config path (tests). Corrupt store files fail closed on load.

## Overlap to reconcile (do not run multiple engines)

This phase ships a minimal studio engine (`internal/nexaflow/runtime.go`
+ store + HTTP API) built against the Phase 7 registry. It is one of
several parallel engine lineages, none merged yet, and the owner must
decide which survives:

- PR #689 — Phase 2 durable engine with a PostgreSQL store (base
  `main`; the lineage Phase 6 was originally meant to port onto).
- PR #688 — Phase 5 integration engine, stacked on the #675 branch
  (branch gating, subworkflows, loop/depth caps 10/3, resume tokens).
- Phase 3 (local only, unpushed) — durable engine with AND-join DAG,
  durable waits/approvals, subworkflows, handler chains.
- Phase 4 (local only, unpushed) — HTTP/scheduler/webhook layer with
  its own sequential engine and file store.

When the surviving engine is chosen, the studio's HTTP layer should
be re-pointed at it and this phase's engine retired; the DTOs and UI
are engine-agnostic. Until then, this engine is the one the studio is
tested against. Two cross-engine findings apply to any survivor:
engines must project node inputs onto the node's InputSchema (the
registry rejects unknown fields fail-closed, so a raw merge breaks
multi-node chains), and condition→branch chains cannot run in v1
without a registry fix (condition outputs `result`, branch requires
`condition_result`).

## Task 6 completion (studio gaps closed on this lineage)

- **Template gallery (backend-driven).** `GET /admin/api/nexaflow/templates`
  serves `DefaultTemplates()` (routing.read via the existing
  `/admin/api/nexaflow/*` middleware; OpenAPI + contract matrix updated).
  The gallery is searchable and creates a draft with the template's
  nodes and **no connections**: templates carry node order only, not
  port mappings, so inventing edges would be a second schema. The
  operator connects ports, fills placeholders in node configuration,
  then validates. Template configs contain no secrets and no resolved
  URLs; placeholders stay placeholders until the operator fills them.
- **Connection deletion + immediate edge diagnostics.** The editor
  lists every connection with a Delete button (the SVG edge layer keeps
  `pointer-events: none` so canvas drag still works; the list is the
  accessible deletion path). Connecting a node to itself, repeating a
  connection, or targeting an unknown port is rejected immediately
  with the same verdict backend validation would give on Validate;
  feeding an already-fed input is allowed with a warning, because the
  backend permits it and the runtime applies edges in order
  (last edge wins, see `buildInput`).
- **Immutable version inspection.** The versions panel gains Inspect:
  it renders the version's full stored graph (nodes + connections +
  full content hash + publisher + timestamp) from the already-returned
  `versions[].graph`. The snapshot is read-only; editing the draft
  never changes it.
- **Execution timeline.** The run panel gains a timeline sorted by the
  server's per-node `started_at` (offset from run start, duration,
  attempts, status) plus total run time when finished. The node table
  above it stays in snapshot order to match the canvas.
- **Approve/resume honesty.** No new resume endpoint was invented:
  in this engine ApproveRun *is* the resume (it continues the run
  loop, including after a restart), and rejecting also resumes with
  `approved=false`. The buttons are now labelled "Approve and resume"
  / "Reject and resume" so the UI says what the API does. A resume
  without an approval decision would bypass Phase 7 approval
  semantics and is deliberately not offered. The positive-approval
  path (distinct approver) remains covered by runtime tests, not by
  the keyless browser suite, which can only prove the honest
  self-approval rejection.
- **Onboarding, loading, error recovery.** The empty list gains a
  five-step onboarding checklist; the template gallery degrades
  gracefully (a gallery failure shows an error but never hides the
  workflow list or canvas); list loading/error/retry behaviour is
  unchanged.
- **Responsive, keyboard, RTL.** The existing breakpoints (1100px /
  700px) already stack the studio layout and shrink the canvas; Task 6
  adds browser coverage at a narrow viewport and with `dir=rtl`. The
  canvas itself is a coordinate plane and is not mirrored in RTL
  (mirroring coordinates would break port mapping); panels, palette
  and text follow the document direction set by the app's language
  switch. Keyboard movement (arrows, Shift for larger steps) and
  Delete on a focused node already existed and are now browser-tested.
- **Deliberately NOT done here.** Canonical-API repointing, `workflow.*`
  permissions, PostgreSQL history, scheduler/webhooks, and `bt()` i18n
  conversion of studio strings all belong to the master-mission
  integration branch (Phase 6 there, per its canonical plan) and are
  not claimed here; this branch stays on the #690 lineage's
  routing.read/write API so the delta ports cleanly. Branch gating
  (condition/branch v1 limitation above) is likewise unchanged.

## Verification

- `go test ./internal/nexaflow/ ./internal/httpapi/` (incl. `-race`),
  `go vet`, `gofmt`, `node --check internal/httpapi/web/app.js`.
- Browser acceptance: `scripts/test-browser-nexaflow.py` (skips cleanly
  without Chromium/Playwright; wired into `scripts/verify.sh` beside the
  other browser suites). It covers create, add/configure nodes,
  connect, validation errors, save/reload, publish, run success, run
  failure display, cancel, and the approval gate (pause + honest
  self-approval rejection in keyless mode).
- Defects the browser suite caught and fixed: empty graphs serialized
  as JSON `null` slices (crashed node creation); Validate/Publish
  judging the stored draft instead of the canvas; palette adds stacking
  on one point (now cascaded); sidebar navigation not leaving the
  editor (now guarded like Back).
