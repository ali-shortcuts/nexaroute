# OpenCode upstream (Console + Go) — implementation notes

Status: fixture-verified only. **Live `opencode.ai` behavior is NOT_RUN**:
no live call, no paid call, and no real credential was used for this
work. Every claim below that is not marked fixture-verified is a
documented limitation, not a capability.

## Profiles

| Profile | Type | Base | Discovery | Session header |
|---|---|---|---|---|
| Console (pay-as-you-go) | `opencode-console` | `https://opencode.ai/inference` | Documented `GET /v1/models` | None gateway-derived |
| Go (subscription) | `opencode-go` | `https://opencode.ai/inference/go` | Undocumented — unsupported, models saved manually | Derived `x-opencode-session` (Go only) |

There is **no Zen profile**: no Zen URL appears in the documented
Console/Go material, so none is shipped and none is claimed dead.
The 2026-09-05 rejection date is not in those docs and is not
asserted here.

## Routing

The upstream protocol is selected by the **client ingress kind**,
never by model id and never from the generic `/v1/...` defaults:

- OpenAI chat ingress → `/openai/v1/chat/completions`
- Responses ingress → `/openai/v1/responses`
- Anthropic ingress → `/anthropic/v1/messages`

All opencode requests run through the canonical-IR path (never legacy
passthrough/translate), and the bundle path is carried through every
attempt including hedged and repair attempts, so a retry can never
fall back to the generic chat path. The adapter's `endpoint` helper
needed no change for these bases/paths. Count-tokens stays gated to
`anthropic_compatible` (documented edge, not silently opened).
Media/images paths for opencode are untested and unclaimed.

## Credential selection (one discipline, inside the adapter)

Order: (1) eligibility — enabled, not cooling; (2) minimum
`priority` tier among available credentials; (3) credential-level
session-affinity pin (session SHA-256 key, bounded, 1h TTL) while the
pinned credential stays healthy and eligible, else fallback;
(4) the least-loaded tournament with effective load `Active/weight`
(weight 0 → 1), then fewer failures, then never-failed. With all
priorities 0 and weights 1 this is exactly the legacy tournament.
Cooldowns are per credential and independent; expiry restores a
credential with no operator action and never flips `enabled` or
deletes an entry. Capacity comes only from configured
`max_concurrency` and in-flight counts. The redacted state surface
(index, pool index, name, tier, weight, last status, failures,
active, cooling) and the last-selection explanation (tier / affinity
/ least-loaded / only-available) carry no keys and no emails.
Pool entries may carry `email` (metadata only, masked everywhere).

## Go session header

`ses_` + first 32 hex chars of SHA-256(session key + `|` + salt),
salt = `opencode_session_salt` or the documented default
`nexaroute-opencode-session-v1` (a domain separator, never a
credential, never logged). A client-supplied value (via
`forward_headers`) wins unchanged. With no session key, the fallback
is deterministic over provider id + model + salt. Derived values are
cached in a bounded LRU keyed by the SHA-256 of the derivation input,
never by the raw session key. Never derived from an API key, email,
prompt text, or user id; never from the per-request id; never placed
in static headers; never trusted for Console (Console gets no
gateway-derived header).

## 403 model-entitlement handling (opencode only)

A 403 is a model-entitlement denial, not a credential failure: no
cooldown, no rotation to another credential for the same restricted
model in the same request, no deployment failover or quarantine, and
a diagnostic naming the restriction and the documented next step
(use a paid model or a Go subscription, or pick a model the plan
includes). Classification is status-based (no distinguishing error
body is documented), so an invalid opencode key returning 403
surfaces the same diagnostic on that request; the next request
re-evaluates independently and other models on the same key are not
poisoned. Generic providers still cool on 403 exactly as before.
Error classification otherwise follows the mission taxonomy
(401/403/429/quota/unsupported/network/timeout-ambiguous/5xx) with
per-credential cooldowns and bounded retries respecting Retry-After
(both delta-seconds and HTTP-date forms, fallback, cap — already
parsed; no gap was found to extend). Transport errors return
immediately with no failover and no cooldown; 5xx returns as-is;
there is never failover after streamed output begins and never a
blind resubmit of a possibly-billed outcome.

## ADOPTED vs DEFERRED (algorithm input)

| Proposal | Disposition | One-line reason |
|---|---|---|
| Fair-Cycle + Least-Used + priority + weight as one selection discipline | ADOPTED | Implemented inside `reserveCredential` in Stage C order, deterministic and explainable. |
| Credential-level session-affinity pin | ADOPTED | Bounded TTL'd pin inside the adapter keyed by session hash; falls back when the pinned credential is cooling/ineligible/absent. |
| Independent per-credential cooldown + auto-recovery after expiry | ADOPTED | Cooldown expiry restores availability with no operator action; distinct from operator `Enabled=false`; expiry recovery is tested. |
| Retry-After in delta-seconds + HTTP-date forms | ADOPTED (already present) | Parser already handled both forms, fallback, and cap; extended only by tests, no gap found. |
| Capacity-aware spreading via tournament + MaxConcurrency + Active | ADOPTED (honest sense) | Spreading delays/distributes limits; no invented capacity constant. |
| Mission error classification with per-credential cooldowns + bounded retries | ADOPTED | Implemented at adapter + compat error layers; generic behavior unchanged. |
| "Never hit a rate limit" / "infinite speed" | REJECTED as false | Quotas are finite — demand above the sum of authorized quotas WILL hit limits; latency is bounded below by upstream + network + tokens. |
| Zero-copy stream pass-through wholesale | REJECTED/DEFERRED | Would break usage accounting, protocol translation, tool-call handling, and redaction on required paths. |
| In-memory catalog with Postgres NOTIFY/LISTEN cross-pod sync | DEFERRED (out of scope) | A new subsystem; the registry/catalog is already in-memory in-process and multi-instance state is a separate program concern. |
| Rolling-p95 latency routing + token-aware balancing | DEFERRED (future only) | Not deterministic/explainable in the mission's sense; in-flight token counts are not known during streaming; no external P99 figure is cited as NexaRoute evidence. |
| Soft capacity of 1 concurrent request per key by default | REJECTED | Would throttle paid keys whose configured MaxConcurrency is far higher; capacity comes from configured controls only. |
| 401/403 → permanently disabled for all models | NUANCE REQUIRED, not adopted as stated | 403 is scoped to the denied model/plan (no whole-credential poisoning, no bypass rotation); 401 cools and is re-testable/re-enableable, never silently deleted or auto-disabled. |
| 5xx/timeout → retry with the same key with backoff (blanket rule) | REJECTED where it conflicts | Retry only when failure semantics allow another attempt; never blindly resubmit a possibly-billed outcome; never fail over after streaming begins. |
| "Compatibility with all models/providers" | REJECTED as a claim | Compatibility is per provider/model/protocol/feature in the conformance manifest, fixture-verified, live = NOT_RUN. |

## Remaining gaps (not bundled, not hidden)

- **G1** per-protocol capability separation in `compat.Store`
  (keyed by deployment + kind): NOT implemented — the contract
  remains per-deployment, shared across opencode protocols. It was
  optional and would have changed the Store API for all deployments.
- **G8** router staleness: NOT implemented — deliberately not bundled
  as a global change; follow-up.
- **G6** audit: implemented best-effort (one event per provider
  mutation, provider id + pool indices, never values); when no audit
  store is configured the mutation still succeeds and no event is
  recorded.
- Per-credential live state on the provider detail response appears
  only when a live adapter exists; config-only paths never fake it
  (the field is absent, reported as a gap, not filled with invented
  data).
- Live acceptance is NOT_RUN by design (no tenant, no credentials,
  no paid calls). Browser acceptance was not run (no browser step in
  this lane); UI changes are syntax-checked (`node --check`) only.

## Compliance

Authorized multi-account management only. Free-tier gates are
reported as BLOCKED/limitations, never bypassed by rotation,
impersonation, undocumented headers, IP rotation, or fabricated
tools. No push, PR, merge, tag, or release was made from this work.
