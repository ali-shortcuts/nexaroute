# OpenCode reconciliation: PR #694 and PR #695

Status: reconciled implementation branch. This document is the design
record for the reconciled branch; it does not merge anything. PR #694
and PR #695 both stay open (no auto-close keywords are used anywhere
in the reconciled PR) for the owner to close, and nothing here is
merged until all required checks pass.

Live state re-verified at the start of this work (2026-10-28):

- PR #694 — DRAFT, OPEN, MERGEABLE, CLEAN, head `2d225f5`
  ("feat: synthesize x-opencode-session for opted-in OpenCode
  providers"), +403/−2, 9 files, all CI checks pass (verify 8m31s).
  No formal reviews.
- PR #695 — DRAFT, OPEN, MERGEABLE, CLEAN, head `30c706a8`
  ("nexaroute: OpenCode Console/Go compatibility + intelligent
  multi-credential routing (review only, do not merge)"),
  +3091/−58, 27 files, all CI checks pass (verify 7m12s). No formal
  reviews; one supervisor guidance comment (the G7 browser-credential
  regression) which head `30c706a8` already resolves.
- Base of the reconciled branch: #695 head `30c706a8`
  (`supervisor/opencode-reconciled`). #694 was never merged, and
  #695 was never merged, so there is no shipped behaviour of either
  PR to preserve: backward compatibility is owed to main `e6c3cdf3`
  behaviour for non-OpenCode providers, and that is what the tests
  below pin (legacy tournament reduction, generic 403 cooling,
  unchanged protocol selection for generic types).

## Canonical decisions (verified against the diffs, not trusted from memory)

1. **Type is the sole opt-in.** `opencode-console` / `opencode-go`
   (config validation accepts exactly these two new types). #694's
   `ProviderConfig.OpencodeSession` flag is NOT shipped and does not
   exist on this branch.
2. **Derived session values are hashed, never plaintext.** Go
   derivation is `ses_` + 32 hex chars of SHA-256 over
   scope/session-key/salt (or scope/provider/model/salt fallback).
   #694's per-request UUID fallback and its plaintext
   `<virtual-key-id>:` prefix are NOT shipped: a fresh UUID per
   request fragments every session-less conversation and defeats
   retries on one session, and a plaintext prefix leaks the caller
   id and breaks the `ses_` shape.
3. **Scope is hashed in, never printed.** The caller scope
   (virtual-key ID, else tenant ID) enters the SHA-256 input only.
   An empty scope keeps the pre-scope derivation (operator tests,
   legacy local callers).
4. **Client values win only when allowlisted, and only when valid.**
   A client `x-opencode-session` reaches the adapter only when the
   operator listed it in `ForwardHeaders` (the adapter's
   `applyHeaders` drops everything else). A valid allowlisted value
   (1–256 chars, no control bytes) is forwarded unchanged for both
   profiles; an invalid value is deleted and Go falls through to a
   derived value — invalid input never travels upstream and never
   fails the request. Console never receives a gateway-derived value.
5. **Gating is enforced in the adapter, after header forwarding.**
   The header is sent only on POST inference calls whose upstream
   path is a chat/responses/messages path, only for the two opencode
   types. It is deleted on discovery, embeddings, count_tokens,
   media/images, speech, transcriptions, and rerank calls, and
   deleted for every non-opencode provider even when forwarded.
6. **Unknown ingress fails closed at candidate level.** The compat
   engine returns candidate-unbuildable; another eligible candidate
   may still serve. There is no silent fallback to chat.
7. **G7 guard (narrowed) + primary-source contract.** `replace_key`
   with an explicitly empty `credentials` array AND a new primary
   supplied preserves the pool (the UI placeholder); a nil list
   clears it; a non-empty list replaces it; `replace_key` with no
   primary at all clears primary and pool. `secret_source` reports
   the PRIMARY source in order set env > literal > unset env >
   pool, mirroring `ResolvedAPIKey` order. Both are kept intact here
   with their regression tests.
8. **Opencode 403 is model-scoped entitlement, not credential
   failure.** No cooldown, no rotation to another credential for
   the same restricted model in the same request, no deployment
   failover or quarantine. Generic providers still cool on 403
   exactly as before.

## Area-by-area analysis and decisions

### Session-header behaviour

| Topic | #694 | #695 | Relationship | Decision |
|---|---|---|---|---|
| Opt-in | Explicit `opencode_session` bool on any provider | Provider `Type` (`opencode-console`/`opencode-go`) | Conflict | **RETAIN #695, DISCARD #694 flag.** A flag duplicates the Type opt-in and would let any provider type claim the header. |
| Value derivation | Client header, then session headers/body id, then per-request UUID; virtual-key id prefixed in plaintext | Client header (allowlisted forward) wins; else `ses_`+SHA-256(scope\|session\|salt), deterministic provider\|model fallback | Conflict on fallback and scope | **RETAIN #695, DISCARD #694 UUID + plaintext prefix.** Hashed derivation is stable per conversation; UUID fragments sessions; plaintext scope leaks identity. |
| Session-key source | Own resolver over client headers + body `session_id` | Router-derived `SessionKey` (same header order + body key, bounded 256) carried on context with `ClientScope` | Complement (same sources, different plumbing) | **RETAIN #695 plumbing.** #694's fallback chain is already covered by `sessionKeyFromRequestParts` + `BodySessionKey`; no second resolver is ported. |
| Context-carried value | Synthesized value on context so a client cannot inject it | Session key + scope on context; header value derived in the adapter | Complement in intent, different mechanism | **REVISE: port the principle, not the mechanism.** #695 already prevents injection: a client value wins only when the operator allowlisted `x-opencode-session` in `ForwardHeaders`, and the derived value is computed gateway-side. What #694 adds and this branch ports is the ordering rule below. |
| Static provider header vs synthesized value | Synthesized value wins over a statically configured provider header or an allowlisted client header | For Go, derived `Set` overwrites static; for Console, a valid value already on the request (including a static provider header) was kept | Gap in #695, fixed here | **REVISE (defect fix R1).** `applyOpencodeSession` now takes the forwarded header set and treats ONLY an allowlisted forwarded client value as a client value. A statically configured `x-opencode-session` in provider `Headers` is never sent for either profile: Console deletes it, Go overwrites it with the derived value. Static session headers are not a conversation mechanism. See `TestReconciledSessionStaticHeaderNeverSent`. |
| Path gating | Configured paths by suffix + generic suffixes; POST only; models/embeddings/count_tokens/media excluded | Configured paths by exact equality + generic suffixes; POST only (caller-checked); discovery/embeddings/count_tokens/media/speech/transcriptions/rerank excluded | Complement, #694 stricter on custom paths | **REVISE (defect fix R2).** Configured paths now match by suffix too (trimmed, non-empty, not `/`), so a provider whose base URL carries a prefix, or whose operator configured a custom chat path, is gated the same way #694 gated it. Generic suffixes unchanged. See `TestReconciledSessionConfiguredPathSuffix`. |
| Value bound | 512 chars | 256 chars (router bound) | Conflict | **RETAIN #695 (256).** The tighter bound is the router's, and the adapter must not accept what the router would bound. |
| Handler attachment | 3 inference handlers attach the context value; models/count_tokens/media/embeddings never do | Same 3 handlers attach `SessionKey` + `ClientScope`; other handlers do not | Complement (same shape) | **RETAIN #695.** Adapter-side deletion is the second layer and covers direct `DoPath` callers. |
| Failover carrying | Header never sent to non-opted-in failover candidates (flag gate) | Header deleted for every non-opencode provider even when forwarded; failover test asserts generic candidate gets none | Complement, #695 stronger | **RETAIN #695**, plus the committed-bytes test below. |

### Provider types

| Topic | #694 | #695 | Decision |
|---|---|---|---|
| New types | None (flag on any type) | `opencode-console`, `opencode-go`, validated in config, presets, catalog, conformance | **RETAIN #695.** Type-scoped capabilities: Console has discovery (`GET /v1/models`), Go does not (models saved manually, `ModelsPath` left empty by `ApplyDefaults`). |
| Zen profile | None | Explicitly not shipped (no Zen URL in documented Console/Go material) | **RETAIN #695.** No Zen claims. |

### Protocol paths and routing

| Topic | #694 | #695 | Decision |
|---|---|---|---|
| Upstream path selection | Adapter-side only (header gating); no path selection | Ingress-determined: chat → `/openai/v1/chat/completions`, responses → `/openai/v1/responses`, Anthropic → `/anthropic/v1/messages`; canonical-IR path only; bundle path carried through hedged/repair attempts | **RETAIN #695.** Protocol follows the incoming request type, never the model id or generic defaults. Verified against `docs/OPENCODE_UPSTREAM.md` and `finishCanonicalAttempt`: the doc and code agree on all three paths and both bases (`https://opencode.ai/inference`, `https://opencode.ai/inference/go`). |
| Unknown ingress | N/A (no path selection) | Candidate-unbuildable, fail closed | **RETAIN #695** with its test. |
| Gemini ingress to opencode | N/A | Fails closed (documented; Gemini path out of scope) | **RETAIN #695.** |
| Hedging / failover after commit | N/A | `responseCommitted` (statusWriter) gates every failover `continue` in openai/anthropic/responses handlers; role chunk deferred until the first valid upstream event, so a 200-then-die upstream still fails over pre-commit; after commit, only a terminal error chunk is emitted | **RETAIN #695, proven by new test R3.** Adapter credential retry happens before the response is returned, i.e. before any client byte flows, so it cannot violate the invariant either. Mid-stream adapter errors do not trigger a second credential attempt (see stream test). |
| Count-tokens | Excluded from header | Stays gated to `anthropic_compatible` (documented edge, not silently opened for opencode) | **RETAIN #695.** |

### Credentials

| Topic | #694 | #695 | Decision |
|---|---|---|---|
| Pool model | None | `ResolvedCredentialEntries` (primary pool-index −1, pool entries with name/priority/weight, exact-duplicate collapse, disabled skipped) | **RETAIN #695.** |
| Selection | None | Stage C order: eligibility (not cooling) → minimum priority tier → session-affinity pin (SHA-256 key, 1h TTL, bounded 1024, falls back when pinned credential is cooling/ineligible/absent) → least-loaded tournament on `Active/Weight`, then fewer failures, then never-failed. All-priority-0/weight-1 reduces exactly to the legacy tournament | **RETAIN #695.** |
| Session affinity expiry | N/A | Pin TTL 1h; expiry restores tournament selection | **RETAIN #695, proven by new white-box expiry test R4** (expired pin is ignored; live pin still pins). |
| Cooldowns | None | Per-credential, `CooldownUntil` never flips `Enabled`, expiry auto-restores; `Retry-After` delta-seconds + HTTP-date forms with fallback and cap | **RETAIN #695.** |
| Per-credential testing | None | `singleCredentialProvider` (nil = primary; out-of-range 400; disabled 400, never silently enabled) used by provider-test, connection-test, discovery; results name the credential index + name | **RETAIN #695** with its tests. |
| Empty pool | None | Adapter with zero credentials: no `Authorization` header is sent (auth falls back to an empty resolved key and is skipped); upstream 401 is returned, not a panic and not a fake success | **Documented + tested here (R5).** This is the keyless-provider behaviour of the base adapter, not an opencode special case; an opencode provider with no credential is therefore a 401 at the upstream, classified by the handler, never a synthesized success. |
| Concurrency | None | `credMu` guards selection; `sessionMu` with fixed lock order credMu → sessionMu; bounded pin/cache maps | **RETAIN #695, proven by new parallel-affinity test R6** (same session key pins one credential across concurrent requests; run under `-race`). |
| Retries | None | Bounded by credential count; retryable statuses only; 429 no-retry-storm; 401 cools/re-testable, never deletes or auto-disables | **RETAIN #695.** |
| Stream handling | None | Credential retry only on pre-body statuses; streaming bodies are returned wrapped (quota/credential release on body close); mid-stream upstream death surfaces as a terminal error chunk, never a failover | **RETAIN #695, proven by R3 (httpapi) and the adapter stream test.** |
| Entitlement errors | None | Opencode 403: no cooldown, no rotation, no failover/quarantine, diagnostic message; invalid-key 403 surfaces the same status-based diagnostic (documented limitation) | **RETAIN #695.** |
| Email metadata | None | Pool `email` is metadata only, masked everywhere (`MaskEmail`: first char + domain, unparseable → placeholder); never used for auth | **RETAIN #695, with defect fix R7 below.** |
| Credential states surface | None | `CredentialStates` / `LastCredentialSelection`: index, pool index, name, tier, weight, last status, failures, active, cooling — no keys, no emails; present only when a live adapter exists, never faked for config-only paths | **RETAIN #695.** |

### Admin create / edit / replace / clear / reveal

| Topic | #694 | #695 | Decision |
|---|---|---|---|
| Create / edit / delete | None | Existing admin routes + audit event per mutation (provider id + pool indices, never values; best-effort when no audit store) | **RETAIN #695.** |
| Replace primary (G7) | None | Narrowed guard + primary-source contract (see canonical decision 7) | **RETAIN #695 intact**, including `TestProviderReplaceKeyWithPoolPreserved` and the browser-e2e line-877 assertion it protects. |
| Explicit clear | None | `replace_key` with no primary clears primary AND pool; nil list clears pool; `credential_pool {replace:true, entries:[]}` clears pool | **RETAIN #695** with its tests. |
| Masked email round-trip | None | GET detail masks pool emails — but the UI pool editor sends the masked value back, and the pool update treated any non-empty email as a new value, so saving an unrelated pool edit would persist the masked string as the real email | **REVISE (defect fix R7).** `applyCredentialPoolUpdate` now ignores email values containing the mask character (`•`) as unchanged. A genuinely new (unmasked) email still updates. See `TestReconciledPoolEmailMaskRoundTrip`. |
| Reveal | None | Owner/admin-only, rate-limited, env-sourced keys never revealed, pool entries by index, preview never exposes most of a short key | **RETAIN #695** with its tests. |
| Discovery without models endpoint | None | `adminProviderCheck` returns an explicit "no models endpoint; use provider-test" result for opencode-go instead of a misleading `auth_ok` | **RETAIN #695.** |
| Connection test dialects | None | Path selected by requested dialect (opencode path for that protocol, never the adapter chat default); payload shape follows the dialect (Anthropic shape was previously always OpenAI-shaped) | **RETAIN #695.** |

### UI

| Topic | #694 | #695 | Decision |
|---|---|---|---|
| Provider types in form | None | `opencode-console` / `opencode-go` options (en/fa/ps labels) | **RETAIN #695.** |
| Per-credential test | None | "Test this credential" button → `credential_index` | **RETAIN #695.** |
| Pool editor | None | Name/enabled/email/priority/weight/env/replacement/remove rows; live state text from `credential_states`; replacement secret is a password field with "Leave blank to preserve"; `readProvider` sends `credentials: []` exactly on the G7 placeholder path (new primary or clear) | **RETAIN #695**, with the R7 server-side guard as backstop for the masked-email input. |
| Secret rendering | None | Keys are password inputs / masked placeholders; saved secrets are never rendered (server returns `api_key: ""` and masked pool metadata) | **RETAIN #695**, verified by the existing secret-persistence + canary tests and the new leak assertions in the reconciled tests (planted fake secret appears in no read/save/diagnostic surface). |

### Discovery, configuration, documentation, tests

| Topic | #694 | #695 | Decision |
|---|---|---|---|
| Discovery | None | Console: documented models listing; Go: unsupported (no endpoint assumed); `SupportsDiscovery=false` for Go in catalog | **RETAIN #695.** |
| Config defaults | Flag field | Opencode path defaults (never generic `/v1/...` fall-through; preset-less providers repaired; Go `ModelsPath` left empty), weight normalisation, `ResolvedCredentialEntries` | **RETAIN #695.** |
| Docs | Flag comment only | `docs/OPENCODE_UPSTREAM.md` (profiles, routing, session contract, 403 handling, adopted/deferred table, remaining gaps G1/G6/G7/G8), CONFIGURATION/COMPATIBILITY/QUICKSTART updates, conformance manifest rows (PARTIAL, fixture-only, live NOT_RUN) | **RETAIN #695**, with two doc corrections in this branch: (1) the gating paragraph now states the static-header rule (R1) and the configured-path suffix rule (R2); (2) the client-value source is precisely "allowlisted `ForwardHeaders` entry", not any client header. |
| #694 tests | Adapter gating, value validation, resolver chain, virtual-key-id validation | Superseded: gating is covered more strongly by #695's session tests (eligible/ineligible paths, invalid fall-through, scope isolation, non-opencode deletion, Console forwarding); resolver-chain and virtual-key-id tests test discarded behaviour and are NOT ported | **DISCARD #694 tests verbatim; RETAIN their intent** via #695's tests + R1/R2/R3 tests here. |
| #695 tests | N/A | ~30 opencode tests (conformance cells S1–S5 × 3 ingresses × 2 profiles, pool/priority/weight/affinity/cooldown/403/connection-test/unknown-ingress/failover/G7) | **RETAIN #695 all green**, plus the reconciled tests below. |

## Defects found and fixed in this reconciliation

- **R1 — Static provider header could supply a Console session
  value.** `applyOpencodeSession` kept any valid value already on
  the request, including one from statically configured provider
  `Headers`. Fixed: the function now takes the forwarded header set
  and only an allowlisted forwarded client value counts as a client
  value; static headers never supply a session value (Console
  deletes, Go derives). Test:
  `TestReconciledSessionStaticHeaderNeverSent`.
- **R2 — Configured-path exact match missed prefixed/custom
  paths.** `isOpencodeInferencePath` required exact equality with a
  configured path, so a provider whose base URL carries a prefix
  (or a custom chat path) lost the header even though #694's suffix
  rule covered it. Fixed: configured paths match by suffix
  (trimmed, non-empty, not `/`). Test:
  `TestReconciledSessionConfiguredPathSuffix`.
- **R7 — Masked pool email could be persisted as the real email.**
  GET masks pool emails; the pool editor round-trips the masked
  value; the update treated it as a new email. Fixed: masked
  (`•`-containing) values are ignored as unchanged. Test:
  `TestReconciledPoolEmailMaskRoundTrip`.
- **Verified, not defects.** (a) Failover-after-commit gating:
  every failover `continue` in the chat/responses/anthropic
  handlers sits inside `if !responseCommitted(w)`, and the role
  chunk is deferred until the first valid upstream event; proven
  by `TestReconciledNoFailoverAfterStreamCommit`. (b) Secret
  surfaces: admin GET/reveal/diagnostics/connection-test were
  audited; existing canary + persistence tests already pin them,
  and the reconciled tests re-assert a planted fake secret appears
  in none of them. (c) Session values are never logged: no log or
  event call in the provider/httpapi code references a session
  value, session key, or session hash; the only session-key uses
  are context plumbing and SHA-256 hashing.

## Reconciled tests added (all actually run; see the final report)

- `TestReconciledSessionStaticHeaderNeverSent` (providers):
  static header deleted for Console, overwritten by derived for Go,
  allowlisted client value still wins over static.
- `TestReconciledSessionConfiguredPathSuffix` (providers):
  custom chat path under a prefixed base still gets the header;
  ineligible paths still get none.
- `TestReconciledSessionPinExpiredFallsBack` (providers,
  white-box): expired pin is ignored (tournament picks), live pin
  still pins.
- `TestReconciledConcurrentSessionAffinity` (providers):
  parallel requests with one session key pin one credential;
  run under `-race`.
- `TestReconciledEmptyPoolOpencode` (providers): zero-credential
  adapter sends no `Authorization` header and returns the
  upstream 401 (no panic, no fake success).
- `TestReconciledStreamMidErrorNoRetry` (providers): streaming
  upstream death after the first chunk triggers no second
  credential attempt (upstream hit count 1).
- `TestReconciledNoFailoverAfterStreamCommit` (httpapi): first
  candidate streams one valid chunk then dies; second candidate
  is never hit; the client keeps the first chunk plus a terminal
  error, never the second candidate's answer.
- `TestReconciledPoolEmailMaskRoundTrip` (httpapi): masked email
  round-trip leaves the stored email unchanged; a genuine new
  email still updates.

## NOT_RUN (honest, never substituted by fixtures)

- Live inference against `opencode.ai`: no suitable credential is
  configured securely in this environment and no paid-call
  authorisation exists. Live acceptance is NOT_RUN; every PASS
  above is fixture PASS.
- Full `scripts/verify.sh`: run only if feasible without
  disturbing other sessions' baselines; otherwise the narrow
  suites + race are the reported evidence and verify.sh remains
  the pre-merge gate.
- Browser e2e (including the G7 `secret_source` assertion):
  reported with its real result if run, else NOT_RUN (this
  subagent cannot operate the live browser; a script run is not
  a browser visit claim).
- CodeQL / vulnerability scan: CI-side only; the reconciled PR's
  CI must be awaited and reported, not assumed.

## Remaining gaps (carried, not hidden)

- #695's deferred items: G1 per-protocol capability separation in
  `compat.Store`, G8 router staleness, G6 audit best-effort
  (mutation succeeds when no audit store is configured).
- Per-credential live state appears only when a live adapter
  exists; config-only paths report it absent, never faked.
- 403 entitlement classification is status-based: an invalid
  opencode key returning 403 surfaces the same diagnostic as a
  genuine entitlement denial on that request (documented in
  #695 and in the conformance manifest).
- Live OpenCode acceptance is NOT_RUN (see above).
