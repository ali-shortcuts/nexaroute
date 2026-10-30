# Known gaps

These are explicit boundaries of the current code, not hidden assumptions.

## Protocol scope

Implemented runtime protocol classes are:

- OpenAI-compatible Chat Completions
- Anthropic-compatible Messages
- OpenAI Responses (explicit `/v1/responses` path)
- Gemini GenerateContent ingress (explicit `/v1beta/models` paths, for
  Gemini CLI) through the canonical adapter
- Gemini upstreams through the canonical adapter
- AWS Bedrock, Google Vertex AI and Azure OpenAI provider types through
  the canonical adapter (SigV4 / ADC / api-key custody respectively)
- media passthrough endpoints: embeddings, images, audio, rerank,
  files/batches and video, proxied per provider with canonical byte
  preservation

Not implemented as native protocol classes: none in the current
scope. Provider-specific extensions that exceed the canonical IR are
bounded by the cross-protocol fidelity notes below.

## Cross-protocol fidelity

Common Claude Code text/tool/stream flows are implemented and regression-tested, including a mocked two-turn tool round trip and parallel streamed tool calls. That does not mean every future or provider-specific Anthropic/OpenAI extension has a lossless equivalent.

In particular:

- reasoning/thinking formats differ across providers. NexaRoute does not silently translate those controls: when a request explicitly asks for reasoning/thinking, routing is constrained to the matching native ingress protocol;
- prompt-cache metadata does not always have an OpenAI-compatible equivalent;
- provider-specific beta fields are safest on native Anthropic passthrough;
- a committed broken stream is not transparently resumed on another provider.

## Token counting

`POST /v1/messages/count_tokens` first tries the native token-count endpoint of an eligible Anthropic-compatible upstream. If no native count can be obtained, NexaRoute returns a local estimate and explicitly includes `"estimated": true`.

The estimate is not a substitute for model-specific tokenization when exact billing/context calculations matter.

## Secrets

Implemented:

- literal secret
- environment-variable reference
- multiple credentials per provider
- credential rotation/failover/cooldown
- rewritten config mode `0600`
- AES-256-GCM encrypted-at-rest provider, credential-pool, decision-provider, admin, client-auth, header and proxy secrets; random per-secret data keys, master-key wrapping and field-bound AAD (`internal/secrets`, `internal/config`)
- atomic plaintext-config migration with an authenticated encrypted timestamped backup; `nexaroute secrets status|verify|rotate|decrypt` operator commands
- fail-closed missing/wrong-key and tamper handling; durable staged-key promotion recovers an interrupted auto-managed rotation when the staged key authenticates the config
- optional Vault Transit `nxs2` envelope-key path (`NEXAROUTE_KEY_PROVIDER=transit` with `NEXAROUTE_TRANSIT_ENABLED=true`): the Transit KEK stays in Vault, each secret still uses a random local DEK, the canonical AAD binds owner/field/key reference/nonce at the local AES-GCM layer only (no associated data is sent to Transit), strict schema/key-version checks, 5 s per-request timeouts inside 30 s/120 s overall budgets, 64 KiB response cap, redirect rejection, and classified errors apply, and there is no local fallback; explicit `secrets migrate-to-transit` and `secrets rewrap-transit` commands write verbatim backups and atomically replace only after decrypt-verify, with rollback on post-write failure
- secret-preserving provider edit
- plaintext credentials are not returned by Admin read surfaces, snapshots, event/log surfaces, or metrics; see the secret-canary regression coverage and [`docs/SECURITY.md`](SECURITY.md)

Boundaries:

- OS keyring and external command/KMS custody adapters are implemented behind `NEXAROUTE_KEY_PROVIDER`; service interoperability and deployment-specific KMS semantics remain unverified
- Vault Transit `nxs2` is implemented and covered by deterministic fake-API tests, but **no real Vault Transit acceptance run has been executed**. Do not claim production HSM/KMS isolation, HA failover behavior, token renewal, or real Vault compatibility until that run is recorded in `docs/EXTERNAL_VERIFICATION.md`
- Transit mode requires Vault availability for decrypt; an outage or denied policy fails closed and there is no local-key fallback. Disabling a historical Transit key version before `secrets rewrap` and verification succeeds can make affected secrets unrecoverable
- externally managed rotation is intentionally performed by the custody system; `nexaroute secrets rotate` refuses to rewrite an external provider and retains only the most recent `.key.previous` file for the local provider

Saved provider keys are write-only, including literal, pool, and environment keys. Headers and proxy URLs are also write-only. Editing a credential field replaces the whole credential set; individual saved pool keys cannot be revealed. Do not store credentials in base URLs or other public metadata. Do not expose the admin surface to untrusted networks.

## Admin security

Implemented:

- loopback-only admin mode by default
- explicitly controlled emergency Admin API key/keyless-loopback compatibility identity
- constant-time admin-key comparison
- OIDC Authorization Code + PKCE S256, verified claim-to-role allowlist, opaque server-side sessions, CSRF-protected browser mutations, logout/revocation, and durable local structured audit on PR #224's configured single-issuer implementation
- optional data-plane client API keys (`client_auth`) with constant-time digest comparison and an optional per-key RPM ceiling

WP4 implementation boundaries and acceptance status:

- Every registered Admin API route/method maps server-side to a permission;
  unknown paths and unsupported methods deny by default. OIDC claim mapping
  accepts only explicit viewer/operator/admin values; emergency `owner` is not
  an OIDC role.
- OIDC discovery issuer, client ID, configured audience, signature/JWKS,
  expiry, nonce, state, PKCE, and browser-binding are verified. Login/logout,
  denied access, and privileged action intent/completion are structured audit
  records; required audit failures stop the protected action.
- bbolt sessions/audit survive process restart on the same host. The store is
  mode 0600 and fail-closed on unsafe file permissions. It is not a distributed
  backend: do not share it between hosts or through a network filesystem;
  OIDC pending transactions are process-local and the callback must return to
  the initiating instance.
- **WP4b security review evidence is in `docs/reports/WP4B_REVIEW.md`**. The
  in-process OIDC provider and negative security tests pass locally; all four
  inspected GitHub checks (`CI/verify`, vulnerability scan, CodeQL Go, and
  CodeQL) were green on the reviewed PR head. Any subsequent push requires
  fresh checks before merge. Live external-IdP interoperability is unverified;
  the exact operator-owned tenant checklist remains **BLOCKED** in
  [`docs/reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md`](reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md).

- per-client quotas beyond the RPM and TPM ceilings (hashed-at-rest client keys landed with M3-8: legacy plaintext keys migrate to SHA-256 digests on load/save, and TPM reservations settle to provider-reported usage);
- SAML and enterprise identity protocols other than the configured single OIDC issuer;
- cross-instance pending OIDC transactions (the transactions themselves
  stay process-local; shared session/audit storage IS available via the
  PostgreSQL/Redis security-store backends — see State and HA below);
- enterprise provisioning/deprovisioning and automated role/group lifecycle beyond the explicit claim mapping.

Virtual-key tenant/project/team policy intersection is implemented fail-closed:
child key wildcards cannot widen a configured parent allow-list, unresolved
scope references deny access, and duplicate model/token fields are inspected
conservatively. This is separate from Admin RBAC and OIDC sessions. PR #224 is
merged; its single-host storage and unverified live external-IdP interoperability
remain the documented boundaries.

## Model discovery

Discovery parses several common result shapes, but model-list APIs are not standardized. Providers that omit or customize discovery may require manual model IDs.

## Video gateway boundaries

The video gateway is opt-in. When enabled, runtime startup fails closed if the
configured bearer-token environment variable is missing or empty. The local
JSON job store and queue remain single-process, and the cost ledger follows the
job-store backend: in-memory with the file backend, or — with
`video.job_store_backend=postgres` — a durable PostgreSQL ledger shared by
every process on the same database, with exactly-once settlement (per-job
row, row-locked transitions; see the section-B change). Assets remain on
per-instance local storage in both backends, so multi-node video as a
product topology is not declared; that positioning decision is recorded in
ADR 0004 (Accepted — Option B). Restart recovery
re-enqueues queued jobs and resumes known provider job IDs; ambiguous
submissions without a provider job ID move to `needs_manual_action` to avoid
an unsafe duplicate billable submission. Output assets are written through the
configured local asset sink and must include a URI, non-zero size, and SHA-256
metadata before jobs are marked complete. Cost-estimation failures or malformed
estimates are rejected before admission.

The opt-in `code-render` provider (Universal AI-to-Video, Tier A) lets any
configured text/coding model author a validated declarative plan that a
deterministic local FFmpeg renderer executes into a real MP4 (animated text,
charts, diagrams, motion graphics; optional local Piper narration and
burned-in SRT captions). Its boundaries are explicit: it is programmatic
rendering, not native generative video — photoreal footage still requires a
specialist video model; reference/first/last-frame assets, dialogue, music
generation, and image elements are rejected rather than silently ignored;
the planning model's token cost is metered by the gateway data plane, not
the video ledger; QC is deterministic (ffprobe + blank-frame checks) with no
vision-model verdict and no automatic plan repair after a QC failure yet;
finished jobs and their work directories are reclaimed after a configurable
retention window (`code_render_retention_hours`, default 24, hourly sweep +
opportunistic sweep on submission; non-terminal jobs are never pruned);
and live planning through a real external model is UNVERIFIED until run
with an operator's configured model (CI uses a scripted planner).

No commercial external video provider adapter is bundled. The opt-in local
ComfyUI adapter supports text-to-video through a user-supplied API-format
workflow; its HTTP contract is covered by fixtures, but actual model inference
on a real ComfyUI installation is not part of CI. The opt-in
`nexaroute comfyui-accept --confirm-live` command automates the real check
(server stats, one minimal job, read-back SHA-256/size/metadata, `ffprobe`
playability, bounded cleanup), but its current status is NOT_RUN/BLOCKED until
an operator supplies a provisioned installation and records the sanitized
report; loopback fixtures are not real inference. The fake provider writes a
text placeholder, not a playable video. The asset-size cap is enforced with streaming-memory protection: the ComfyUI adapter reads downloads through `io.LimitReader` bounded at `maxOutputBytes+1`, so an oversized response cannot OOM the gateway before the cap applies. Multi-shot jobs persist per-shot checkpoints and automatically compose generated shot assets into a final silent MP4; Gateway-side Piper narration and estimated same-language SRT captions are supported for configured multi-shot jobs. With the default file backend the video jobs, local JSON job store, queue and cost ledger remain single-process; with `video.job_store_backend=postgres` the job store and cost ledger are durable and shared with exactly-once settlement, while assets stay per-instance local — so end-to-end multi-node video is still not a declared topology (ADR 0004, Accepted — Option B): provider-native idempotent recovery after an ambiguous submission, and verified production workloads for ten-minute episodes remain unimplemented or UNVERIFIED. Provider-generated
dialogue/sound effects/music, translated or word-aligned subtitles, and distributed
asset/job storage remain unimplemented; the local CLI also supports existing
narration/music mixing and SRT subtitle burning.
Actual-vs-estimated cost reconciliation is implemented for completed jobs:
provider-reported actuals (explicit `actual_cost_usd` on the provider job
status, with the legacy `Usage["actual_cost_usd"]` entry honored for
backwards compatibility) reconcile the ledger entry from estimate to actual,
and completed jobs expose additive `actual_cost_usd`, `cost_reconciled`,
`cost_delta_usd` (actual − estimate), and `cost_basis` (`"actual"` vs
`"estimate"`) through the video job API and Video Studio. Jobs whose
providers report no (or invalid) actual keep the estimate with
`cost_reconciled: false` and `cost_basis: "estimate"`, never silently
presented as actual. Episode completion reconciles the summed per-shot
actuals only when every shot reports a valid actual. Completion is
idempotent by job ID and terminal state (replayed completions and restart
recovery never double-settle), and the fake provider reports a deterministic
actual when its `ActualCostUSD` hook is set (nil by default). Live
external-provider billing behavior stays UNVERIFIED. With the default file
backend the in-process ledger remains non-durable/non-distributed; with
`video.job_store_backend=postgres` the ledger is the durable, shared
PostgreSQL ledger described above. The Gateway now persists per-shot checkpoints and
automatically composes generated shot assets into a final silent MP4; the local
`videogen compose` command can additionally mix existing narration/music and
burn in SRT subtitles. Video Studio now accepts multi-shot plans, shows progress, and downloads persisted outputs. Manual audio/music/subtitle composition remains CLI-only, and — with the default file backend — the in-process ledger is not durable or distributed (the PostgreSQL-backend ledger is both).

## State and HA

The optional control-plane runtime selects the atomic local file store (on
Linux, cross-process safe via an advisory `.controlplane.lock` file in the
state directory; file-backend writes fail closed on platforms without flock
support), an
explicit test-only memory store, PostgreSQL, or Redis and reconciles a versioned
runtime-metadata startup snapshot. Config snapshots are never part of that
store: config is loaded from the local config file and admin saves atomically
rewrite that same local file, so config edits do not propagate across replicas.
[ADR 0001](adr/0001-shared-config-snapshot.md) records the shared-config
options and secret-custody constraints; it was accepted on 2026-10-22 as
Option 1 — local configuration stays authoritative indefinitely — so none
of the propagation options is implemented or enabled, by decision rather
than by omission.
Both database backends fail closed when unavailable; Redis
uses atomic revision-aware Lua scripts and has real-service integration
coverage. Adapter tests do not guarantee deployment-level persistence,
replication, failover, or recovery; those remain operator-owned and must be
separately configured and tested. The file store's advisory locking assumes a
local filesystem honored by all writers; network filesystems with unreliable
lock coordination are not covered — point every gateway process at the same
local state directory.

The admin security/session store supports bbolt (default), PostgreSQL (M4-2a),
and Redis (M4-2b). PostgreSQL serializes audit-chain writes and verifies the
chain in transactions; Redis uses version-checked Lua compare-and-set and has
real Redis integration coverage, including concurrent appends from two
instances. Both external backends fail closed if unavailable.

The M4-3 `nexaroute backup` / `restore` CLI is implemented and tested. Backup
captures the config, audit/security-store snapshot, and file-backed control-plane
state; restore verifies payloads before writing and uses a durable journal for
rollback/recovery. `nexaroute restore --verify` (optionally `--into` a disposable
directory, `--report` a machine-readable `nexaroute-restore-verify/1` artifact)
runs the whole restore read path — integrity, decryption, audit chain,
control-plane schema, key pairing, freshness — without writing to a live target,
and fails closed (`FAIL`/`BLOCKED`, nonzero exit) when secrets cannot be
decrypted. `nexaroute doctor --bundle` is the matching preflight. This remains a
documented application-level recovery mechanism, not a claim that databases
survive arbitrary infrastructure loss: the bundle never snapshots an external
PostgreSQL, Redis, or video assets/jobs, and `restore --verify` reports those as
`BLOCKED` when the config depends on them. Deployment persistence, replication,
failover, retention and measured RPO/RTO still require operator configuration and
recovery drilling. Cross-replica breaker synchronization exists as opt-in
breaker sync (ADR 0003): it is disabled by default, synchronizes only
cooldown/retired markers over the control-plane store, and applies a remote
retired marker only as a bounded cooldown.
The full per-subsystem state inventory, scopes and failure-mode
behavior live in `docs/HA_STATE_INVENTORY.md` (#434), and the same
scopes are served machine-readably in the `/api/hello` deployment
report.
## RBAC and SSO

PR #224 wires the permission matrix in `internal/authz` to every registered
Admin API route/method and denies unknown combinations. OIDC session roles are
derived only from verified issuer claims using the explicit config allowlist.
The web UI's visibility is a convenience only; the server middleware is
authoritative. The local bbolt store is single-host. The **95% repository-wide Go statement-coverage target is MET on current `main` as a measurement**: the Coverage report workflow on commit `2c5f5832de4b5408f64d97cad8b3c06bfa9ec801` (PR #749 merge, Go 1.27.3; GitHub Actions Coverage report run [40926334325](https://github.com/ali-shortcuts/nexaroute/actions/runs/40926334325), retained artifact `coverage-2c5f5832de4b5408f64d97cad8b3c06bfa9ec801-40926334325`) measured **95.63% (41,704 / 43,612 statements)**, margin +272 above the 95% bar (needs 41,432). CI run [40926334806](https://github.com/ali-shortcuts/nexaroute/actions/runs/40926334806) and Security run [40926334476](https://github.com/ali-shortcuts/nexaroute/actions/runs/40926334476) on the same SHA are all-success. The previous step was 95.6% on `0ea90a0` (PR #746 merge, run 40914837082); the 95.96% local Go 1.27.0 figure quoted for the #749 branch head is a branch measurement and is superseded by the CI figure for the merged head — denominators differ between toolchains and must not be read as a trend. The explicitly selected comparison baseline for issue #663 remains **91.8% (41,782 / 45,515 statements)** on commit `ba5ff2f5f0ace8953e7c4f0e4913de5105e7a13f` (Go 1.27.0; see the pre-change measurement documented in [PR #724](https://github.com/ali-shortcuts/nexaroute/pull/724)). The former 84.9%/85%/91.4%/93.0% figures are historical and superseded. Measurement is not enforcement: the coverage workflow on main only measures, summarizes and uploads the artifact (no threshold step; `go-version: '1.27.x'` is unpinned), and the `main` branch has no branch protection, so issue #663's enforcement criterion is still OPEN even though the measurement is above 95%. The `TestWave3ResponsesDeadline504` flake that failed verify on the old `c2ca13c` head is fixed on main via PR #746 (the 502→504 deadline mapping); coverage success must still not be presented as green gates — the verify gate is a separate gate with its own current state, including the known recurring `TestIssue663ProvidersScheduleRecoveryQueueFullCallback` probe flake (failed the `806ad64` post-merge verify, passed the `2c5f583` verify).
PR #224 is merged; live external-IdP interoperability remains unverified.


## Response cache boundaries

The exact-match response cache is opt-in and deliberately narrow: non-streaming, deterministic-sampling requests only, bounded by entries/bytes/TTL, invalidated on every config swap. Two requests that differ by one byte are different keys. A separate opt-in semantic cache (`cache.semantic`) matches by embedding similarity inside a per-client isolated scope and serves only entries whose training-data provenance satisfies the request's privacy requirement; it is off by default. Streaming responses are never cached by either cache, and cached entries are replayed with the deployment label that produced them so the dashboard stays honest about provenance.

## Probe cadence and model quality

Health probing is selective and event-driven. Startup establishes readiness, new/unverified deployments are probed, and failed deployments move into dedicated recovery loops. Successful real Claude traffic refreshes a deployment's ready-health lease, so actively used models are not needlessly synthetic-probed. A healthy deployment that remains idle past `probe.ready_lease_seconds` is micro-probed before its health proof is trusted indefinitely. The sweep interval remains configurable (minimum 1 second) without turning health checks into a quota/rate-limit attack.

Micro-probes measure availability and latency. They do not measure model intelligence/answer quality. Model strength is expressed through configured deployment `priority` and `weight`; automatic quality benchmarking is outside current scope.


## Routing boundaries

Implemented routing intelligence is deterministic and observable: session affinity, capability filtering, priority/weight policy, live concurrency pressure, latency/failure evidence, provider-level P2C selection, credential-level P2C selection, scoped capability circuits, and supervised recovery.

Not implemented yet:

- proactive quota headroom pressure and in-flight local reservations are implemented when provider quota evidence exists, but reservations are deliberately advisory: NexaRoute does not yet persist rolling-window quota debt after a completed response without fresh headers, hard-throttle traffic from inferred quota, or coordinate reservations across multiple gateway processes. Quota evidence is scoped to the window it was observed in — a window rollover reported without a fresh remaining value, or a reset that has since passed, degrades that resource's evidence to unknown instead of letting a closed window's value drive routing pressure or stats;
- `cost_aware` routing uses configured base input/output prices and a bounded request estimate, but it is not invoice-perfect billing optimization (cache discounts, credits, batch pricing, taxes, and provider-specific billing rules remain outside the router);
- shared/distributed session affinity across multiple NexaRoute processes (breaker state is covered separately by opt-in breaker sync, ADR 0003);
- an online learned semantic router that sends every prompt through another model/encoder.

The last item is deliberate for the current data plane: a learned router would add latency, cost and a new failure mode. Model/task specialization should currently be expressed with aliases plus explicit capability metadata until a separately evaluated routing model can prove a measurable benefit.

## Retry safety boundaries

Retry decisions are classified by sent state, execution state,
stream commitment, and verified idempotency support, with the
full policy, bounds, and evidence in `docs/RETRY_SAFETY.md`.
Boundaries that remain, by design: no dialect has a verified
provider idempotency mechanism (manifest `idempotency` =
`UNTESTED` for all dialects), so exactly-once provider execution
is never claimed; a pre-commit failover after an ambiguous
transport/timeout failure may duplicate provider-side work or
cost, and is recorded as such rather than hidden; cancellation
ends the gateway's wait and prevents failover but cannot recall
provider-side execution, and provider-side cancellation of
in-flight work is unverified; the gateway's sent-state reading
for transport errors is a conservative heuristic (dial/DNS/
credential-exhaustion failures are proven not-sent, everything
else dispatched without a response is unknown), because the Go
HTTP client does not expose whether request bytes left the
process.

## Compatibility engine boundaries

Implemented but bounded by design:

- Level B probing sends OpenAI-shaped, Anthropic-shaped, or Gemini-shaped
  (`GenerateContent` with `contents`/`systemInstruction`/`tools`/`toolConfig`
  and `generationConfig`) probe payloads per provider family. Gemini and
  Vertex upstreams are probed via `RunCapabilitySuiteGemini` (wired into the
  probe engine and the admin test surface alongside the OpenAI/Anthropic
  suites) and learn from both probes and real traffic with error
  classification. Verdict vocabulary and conservative learning match the
  other families: UNKNOWN upgrades to SUPPORTED only on positive evidence,
  verified UNSUPPORTED never flips except via contract-reset paths.
- The repair engine mutates OpenAI-style, Anthropic-style, and Gemini-native
  payload keys. Gemini-native payloads are repaired inline inside
  `generationConfig` (`temperature`, `topP`/`topK`, `maxOutputTokens`,
  `stopSequences`, `responseMimeType`/`responseSchema`, `candidateCount`)
  and the Gemini equivalents of repaired top-level fields (`toolConfig` for
  `tool_choice`, OpenAI-style token keys aliased into
  `generationConfig.maxOutputTokens`). Every Gemini repair uses the same
  audit/event surfacing as existing repairs (`last_repair`, `compat_repair`
  / `compat_sanitize` events, capability learning). Semantics-critical
  Gemini fields (`contents`, `tools`, `systemInstruction`, required
  `toolConfig`, vision parts) are never stripped; unrepairable capability
  failures still fail over without marking health.
- Runtime learning upgrades UNKNOWN to SUPPORTED conservatively and never
  flips verified UNSUPPORTED back; only a contract reset (identity change or
  `/admin/api/compat/reset`) re-opens the question.
- The capability cache is in-memory, matching the single-process state model
  described above; multi-process deployments re-probe after restart.

## Model intelligence and evaluation boundaries

The evaluation plane is an admin-only observation surface and is
explicitly bounded:

- evaluation has two modes. The default is **offline replay**: the admin
  endpoint scores recorded artifacts and never prompts a model, calls an
  upstream or spends provider quota. **Live evaluation is implemented**
  as an opt-in: with `evaluation.live_enabled` set (which requires
  `evaluation.enabled`; the combination is rejected fail-closed at config
  validation), an admin run sends the suite prompts to a physically
  selected deployment through an upstream adapter and scores the real
  responses. Live runs spend real provider quota, and live behavior
  against real external providers stays UNVERIFIED — tests and CI make no
  paid calls;
- no judge implementation ships. Deterministic evaluators decide every case; the
  judge path exists and is proven never to override a deterministic verdict, but
  the HTTP surface cannot enable it;
- the suite catalog is built-in and versioned, plus operator-defined suites
  via `evaluation.suites_path` (a JSON file or a directory of `*.json` case
  packs in the existing suite format, max 32 suites, 1 MiB per file). Packs
  load atomically — a malformed pack refuses the whole load with a
  `suites_error` and never partially registers — ids colliding with built-ins
  are rejected, and operator suites list alongside built-ins in
  `GET /admin/api/evaluation/suites` (`source: "operator"`) and run through
  the same deterministic runner;
- `production_telemetry` provenance is produced by the validated
  (`FromTelemetry`) constructor from two sources: completed evaluation runs
  (availability, failure rate, and latency/TTFT when a target is configured)
  and opt-in automatic ingestion of completed gateway traffic
  (`evaluation.telemetry_ingest_enabled`, off by default, minimum
  `evaluation.telemetry_min_samples` aggregated samples). Ingestion carries
  aggregated operational values only — success/failure counts and numeric
  latency/TTFT samples; no prompts, responses, error text or user data — and
  never changes routing, health, or candidate order;
- scorecards are single-process state. `evaluation.state_path` gives one process
  durable runs/scorecards, but there is no shared/distributed scorecard store,
  no multi-node coordination and no history beyond the bounded version ring;
- scorecard values are evidence records, not routing inputs. The evaluation plane does not
  order candidates, change weights/priorities, gate failover or alter health by
  scorecard content, and a structural guard test keeps the dependency direction
  that way.

The MCP gateway enforces an operator-configurable inbound JSON-RPC request-size
cap (`mcp.max_request_bytes`) before parsing or forwarding a request. This is a
payload bound, not a content-safety or schema-validation policy; tool-specific
argument validation remains the responsibility of the upstream MCP server and
the configured server/tool authorization lists.

## NexaFlow boundaries (canonical line; PR #691 reconciliation)

PR #691's executor/store lineage is retired; its unique capabilities
were reconciled onto the canonical Task 8 line in
`docs/nexaflow-691-reconciliation.md`, which is the authoritative
mapping. In brief:

Implemented on the canonical interfaces: durable per-definition
run-limit admission at the store boundary (PostgreSQL atomic across
processes/hosts, file atomic on one host, memory test-only);
idempotency with input-conflict detection and concurrent-duplicate
resolution; engine-level empty-approver rejection; approval
pause/resume across restarts (including the stall-then-resume rule
for a new worker id under a live foreign lease); cancellation
(idempotent, ownership-checked); crash recovery that re-queues only
provably-safe attempts and parks dispatched effectful attempts as
ambiguous, never re-executing them; signed webhooks with a durable
delivery ledger; durable interval scheduling with slot claims; and
the resume, validate, and per-definition runs-listing admin routes.

Storage guarantees: PostgreSQL is the multi-instance authority;
file mode is single-host only (one live store per path, ledger via
`O_EXCL` files); memory mode is test-only and loses state on
restart. The delivery ledger gives at-most-once execution of an
identical delivery, not exactly-once external side effects: a
claimed delivery whose start fails is consumed, and a parked
ambiguous attempt is precisely an unproven external outcome.

Not implemented (deferred, not done): production gateway wiring
(no caller of `AttachNexaflow` in `cmd/gateway`, so no gateway
process ticks schedules, serves webhooks, or runs startup recovery
yet); scheduler daily schedules, trigger discovery, persisted
schedule state, and the scheduler tick endpoint; webhook
`secret_ref` resolution and published-trigger validation in the
(wiring-owned) Start function; async dispatch (202 executions);
subworkflows, run-level fallback execution, any-join, durable timer
waits; `workflow.*` permissions (admin routes ride
`config.read`/`config.write`/`audit.read` as an interim mapping);
tenant isolation (ownership ceiling + scope label only);
hard-budget settlement (`budget_check` is report-only; admission
reserve only); the JSON-editor definition page and Studio canvas
(those belong to the Studio line). #691's ambiguous-step resume
modes (including re-executing an ambiguous node) were deliberately
rejected, not deferred: re-execution is the double-execution hazard
the parking rule exists to prevent.

## Audit follow-up record (top-15 audit #57, recorded via #178)

Status labels used below: `open` means an unresolved defect/boundary with no
fix on `main`; `intentional boundary` means the behavior is deliberate and is
not a bug; `verified fixed` would require a merged fix plus test evidence and
is not claimed for any item here. Nothing below is marked fixed merely because
it is documented. Evidence snapshot date: 2026-09-30. Parent audit: #57.
Audit follow-up set: #169, #170, #171, #172, #173, #174, #175, #176, #177.
Full re-verification report: `docs/reports/audit-top15-issue57.md`.

### Panic boundary contract — `intentional boundary`

`internal/httpapi/server.go` recovers handler panics, emits an `internal_panic`
bus event plus a `request_id=... handler_panic` log line, and returns HTTP 500
(`"internal gateway error"`) — except for `http.ErrAbortHandler`, which is
re-panicked as required by the `net/http` contract. Converting the abort path
to a 500 would break connection handling, so the re-panic is deliberate policy,
not a bug.

- Impact: ordinary handler panics are fail-closed 500s with an audit event; the
  abort path never produces a response by design.
- Mitigation/workaround: none required; operators monitor `internal_panic`
  events and `handler_panic` log lines.
- Owner: audit finding F3; follow-up set #169–#177.

### Provider/proxy URL credential and egress/SSRF policy — `partially fixed`

Finding F4, tracked by #125. `ValidateProviderConfig` now rejects embedded
`user:pass@` URL credentials in provider `base_url`, plus fragments and
malformed/hostless endpoint URLs. Proxy URL userinfo remains accepted for
backwards compatibility with existing authenticated proxy configurations and
is write-only on admin read surfaces. Private-IP and cloud-metadata targets
remain intentionally permitted for localhost/self-hosted providers and are
still an operator trust-boundary concern.

- Impact: a proxy can still intentionally point at an internal target when an
  authorized operator configures it.
- Mitigation/workaround: store credentials only in the dedicated credential
  fields or environment references (write-only, never revealed on admin read
  surfaces); do not embed `user:pass@` in URLs; do not expose the admin surface
  to untrusted networks.
- Owner: #125 (finding F4); follow-up set #169–#177.

### Stress/soak tests opt-in — `intentional boundary` (CI budget)

The bounded stress checks (`NEXAROUTE_STRESS=1`) and long-form soak checks
(`NEXAROUTE_SOAK=1`) in `internal/router`, `internal/events`,
`internal/probe`, `internal/logging`, `internal/eval`, and `internal/httpapi`
skip by default so routine `go test ./...` stays within budget. The
bounded subset is now in CI: `scripts/ci-soak-smoke.sh` (stress selectors
identical to `scripts/stress.sh`, fixed-iteration soak tests, loopback
outage/cancellation tests, one short race soak stage) runs as the
`ci-soak-smoke` job in `.github/workflows/ci.yml` and records every stage
as PASS/FAIL/SKIPPED/BLOCKED/CANCELLED in a sanitized report. The
long-form multi-hour soak stays deliberately out of normal PR CI: it is
opt-in via the Soak workflow (`workflow_dispatch`) on a suitable runner
(see `docs/reports/ISSUE664_SOAK_PROCEDURE.md`).

- Evidence (actual run, 2026-10-29): `docs/reports/ISSUE664_CI_SMOKE_2026-10-29.md`
  — CI smoke PASS, 16/16 stages, code commit `1facf65`, Go 1.27.0, 1035s
  wall time on a shared 2-vCPU VM. Long-form multi-hour soak: NOT RUN.
  Live items (#646–#652) and any RPO/RTO value: NOT RUN / not reported.
- Impact: default test runs do not exercise the stress/soak paths, and
  no short CI run proves long-duration stability.
- Mitigation/workaround: run them explicitly, e.g.
  `NEXAROUTE_STRESS=1 go test ./...`, `NEXAROUTE_SOAK=1 go test ./...`, or via
  `scripts/ci-soak-smoke.sh` (bounded) / `scripts/soak.sh` (long-form,
  operator-dispatched).
- Owner: audit finding F11; follow-up set #169–#177; issue #664.

### Dashboard missing-DOM resilience and CLI-snippet injection — `verified fixed`

Findings F1/F2, in `internal/httpapi/web/app.js`, are covered by the dashboard
section-isolation helpers and `dom_isolation.test.mjs`. CLI snippet values are
HTML-escaped before insertion into the code block by `cliSnippet`, and missing
non-critical nodes no longer abort unrelated sections.

- Evidence: `go test ./...` plus `node --test internal/httpapi/web/dom_isolation.test.mjs`.

### Package coverage gaps and targets — 95% target MET

Issue #663 is authorized to start from current main after checking and re-scoping
around the focused work already merged. Its explicit comparison baseline is
commit `ba5ff2f5f0ace8953e7c4f0e4913de5105e7a13f` (Go 1.27.0): **91.8%
repository statement coverage (41,782 / 45,515 statements)**, measured with
the canonical uncached command `go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...`.
The measurement and its run-to-run caveat are recorded in [PR #724](https://github.com/ali-shortcuts/nexaroute/pull/724).
This comparison baseline predates PR #724 and later changes to main; it must
not be presented as the current-main aggregate.

The current measurement is the Coverage report workflow on main `2c5f583` (Go 1.27.3, run 40926334325, artifact 12920986741):
**95.63% repository statement coverage (41,704 / 43,612 statements)**, above the 95% target by 272 covered statements. Coverage success is a measurement, not a gate pass and not an enforcement: the workflow has no threshold step and `main` has no branch protection (see the RBAC section above), so a future sub-95% result would not fail the workflow or block a merge today. It must still be distinguished from the verify gate, whose current state includes the known recurring `TestIssue663ProvidersScheduleRecoveryQueueFullCallback` probe flake (failed the `806ad64` post-merge verify, passed the `2c5f583` verify).

Package-level figures from the successful `2c5f583` CI artifact (22 of 62
statement-bearing packages below 95%; `internal/doccheck` carries no
statements and is excluded from that count):

- `internal/nexaflow`: 81.8%
- `internal/desktop`: 85.7%
- `internal/budget`: 85.9%
- `internal/evallive`: 87.0%
- `internal/video/storage`: 87.7%
- `internal/video/audio`: 90.3%
- `internal/video/agent`: 90.4%
- `internal/transport`: 90.6%
- `internal/policysim`: 90.9%
- `internal/video/tts`: 91.4%
- `internal/sharedstate`: 92.0%
- `internal/video/render`: 92.1%
- `internal/telemetry`: 92.8%
- `cmd/gateway`: 93.0%
- `internal/authz`: 93.8%
- `internal/retrysafety`: 93.9%
- `internal/events`: 94.0%
- `internal/logging`: 94.2%
- `internal/privacyaudit`: 94.4%
- `internal/route`: 94.8%
- `cmd/videogen`: 94.9%
- `internal/video/composer`: 94.9%

Packages at or above 95% on the same measurement include `internal/httpapi`
(97.4%), `internal/providers` (96.8%), `internal/video/cost` (96.6%),
`internal/video/queue` (98.1%), `internal/video/providers` (95.4%), `internal/video/runtime` (96.2%),
`internal/secrets` (95.4%), `internal/providers/livecanary` (99.6%), and `internal/protocol/canonical` (99.8%).
`internal/nexaflow` remains the lowest package by design of this branch:
its behavior is pending the Phase 6 owner decision, and no tests were
added there to lock disputed behavior. Three wave-2 files were dropped
in this branch as duplicative of the Continue95 set already on main
(zero statements Continue95 does not already cover): the route-preview,
evaluation-state, and quality-gate-CLI waves; the kept waves add 108
statements Continue95 does not reach (OIDC 47, compat 13, simple 13,
server 12, hedging 10, autopilot 8, Gemini 5).
The earlier failed run on commit
`39a7498f5dd8e9f7ad4e06633b1c42d1a7365200` (GitHub Actions run
[40848476783](https://github.com/ali-shortcuts/nexaroute/actions/runs/40848476783))
is now historical: it failed on the `TestWave3ResponsesDeadline504`
flake (fixed on main via PR #746), and its package diagnostics (notably
`cmd/gateway` 84.8% there versus 90.1% on the `c2ca13c` successful run)
must not be quoted as current-`main` figures.

This measurement covers the test suite only. It is not evidence for
external live acceptance: live provider acceptance (issue #646) was
`BLOCKED_EXTERNAL` on 2026-10-29 for lack of operator authorization, and
other external live items keep their own `BLOCKED`/`NOT_RUN` status in
their records. A coverage percentage never verifies a live provider,
tenant, or device.

Focused results from merged PR #724 are meaningful but do not satisfy the
repository-wide goal: `internal/video/cost` measured **96.6% (288/298)** and
`internal/video/queue` **98.1% (370/377)** on that PR's verification head.
Audit the new baseline/current-main artifact package by package, add meaningful
behavior assertions, and do not pad coverage, exclude production packages, or
lower the 95% target.

Historical audit-record note (2026-10-15, superseded — not current
measurements): the earlier thin-package list recorded `internal/compat`
86.6% and `internal/core` 100.0%, among others. These figures are retained only so the F15 audit record stays complete.
The package-level numbers above are the current-`main` CI measurement from the
successful `2c5f583` artifact (95.63%). They are valid as a measurement; meeting
the percentage does not by itself satisfy issue #663's enforcement criterion
(no threshold step, no branch protection) and must not be presented as release
sign-off while enforcement and the external acceptance items (#646–#652) are open.

Historically, the low coverage in `internal/video/cost` and `internal/video/queue`
was chiefly concentrated in the PostgreSQL adapter code, whose real
database integration tests are build-tagged and are not included in the
canonical plain `go test -count=1 -coverprofile=... ./...` measurement.
The durable adapter behavior has separate PostgreSQL integration
coverage, but that does not make the canonical coverage number higher.
Coverage must be raised with meaningful behavior tests or a reviewed,
clearly defined measurement-policy change; assertion-free padding or
excluding production code to make the percentage pass is not acceptable.

The current-`main` artifact is [coverage-2c5f5832de4b5408f64d97cad8b3c06bfa9ec801-40926334325](https://github.com/ali-shortcuts/nexaroute/actions/runs/40926334325/artifacts/12920986741) (successful run, 2026-10-29). The previous current-`main` artifact was the `0ea90a0` (PR #746) run 40914837082 (95.6%). The 2026-10-25 artifact is historical and retained by GitHub Actions:
`coverage-c7867355c6d5f9773dd185e19c3ab5e689aa6577-40196054365`. The failed
2026-10-29 diagnostic artifact is [coverage-39a7498f5dd8e9f7ad4e06633b1c42d1a7365200-40848476783](https://github.com/ali-shortcuts/nexaroute/actions/runs/40848476783/artifacts/12885399833); it is historical and is not a passing aggregate measurement.

The canonical measurement command is:
`go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...`.

Remaining acceptance work is tracked in [issue #653](https://github.com/ali-shortcuts/nexaroute/issues/653),
including meaningful behavior coverage and a fresh long-form stress/soak
run. The older package measurements later in this historical audit
record are not current and must not be used for release sign-off.

- Impact: insufficient regression evidence in the listed low-coverage packages.
- Mitigation/workaround: inspect the retained per-function artifact and add behavior assertions for meaningful untested outcomes.
- Owner: Phase 1 coverage follow-up; audit finding **F7** and follow-up set
  #169–#177 remain the historical tracking reference.
