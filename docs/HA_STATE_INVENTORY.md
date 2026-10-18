# Runtime state inventory and multi-instance guarantees (#434)

This inventory states, for every piece of runtime state in
NexaRoute, who owns it, how durable it is, and what is actually
guaranteed when more than one gateway process runs. It is the
reference behind the machine-readable deployment report the
gateway serves at `/api/hello` (`deployment` object, computed by
`deploymentReportForConfig` from the real startup configuration).
**No HA property is claimed here without a test or a store
contract behind it.** Where a guarantee does not exist, the entry
says so; the UI and health surfaces must repeat the per-instance
truth, never imply a global one.

Scopes: `shared` (coherent across replicas via a shared store),
`shared-host` (coherent across processes on one host only),
`per-instance` (each process holds its own state).

| State | Owner / store | Durability | Scope today | Restart | Rolling deploy / replica loss | Network partition | Ambiguous submission |
|---|---|---|---|---|---|---|---|
| Config snapshot (control plane) | `internal/controlplane` Store: file / PostgreSQL / Redis / memory | Durable except memory backend | shared (PG/Redis), shared-host (file+flock), per-instance (control plane disabled) | Reloaded and reconciled from the store at startup; `last_known_good` fallback per failure policy | File: writers on one host coordinate via advisory flock + revision CAS — a stale writer gets `ErrConflict`, never a silent lost update (proven by `TestTwoInstanceFileStoreCoherence`); PG/Redis: revision-aware atomic writes, real-service integration coverage | Store unreachable ⇒ backend fails closed or serves last-known-good per the configured failure mode; no split-brain writes because every write is revision-checked | N/A (writes are CAS, not submissions) |
| Admin security store: sessions, audit chain, client keys | `internal/securitystore`: bbolt (default) / PostgreSQL / Redis | Durable except memory test backend | shared (PG/Redis), shared-host (bbolt) | Reloaded from store | PG serializes audit-chain writes transactionally; Redis uses version-checked Lua CAS; concurrent appends from two instances have real-service coverage | Backend down ⇒ fail closed | Audit append is append-only with chain verification; a conflicted append errors instead of forking the chain |
| Provider breaker / cooldown / health | `internal/health`, `internal/probe` (in-process) | None (rebuilt by probing) | **per-instance — cross-replica synchronization is NOT implemented** | Cooldowns reset; probes re-establish state | Replicas may disagree about a provider's cooldown during the probe window; routing stays safe (each replica probes independently) but not identical | Each replica sees only its own probe results | N/A |
| Quota / RPM / TPM reservations and budget counters | In-process limiters + optional control-plane budget store | In-memory (reservations); budget snapshots durable only with the control plane | **per-instance for reservations** | Reservations reset | **N replicas can collectively oversubscribe a provider quota by up to N× the per-instance limit.** The deployment report and UI must label these limits per-instance, never global | Same as replica loss, per partition side | Provider-reported usage settles reservations per instance; no cross-instance settlement exists |
| Session affinity / routing stickiness | Router in-process state | None | per-instance | Lost | A load balancer may land a session on a replica without its affinity state; routing remains correct, affinity is best-effort | Per partition side | N/A |
| Request idempotency (client-facing) | Per-instance request handling | None | per-instance | Lost | A retried submission landing on another replica is a new request to that replica | Same | **Ambiguous provider submissions are never auto-duplicated by the gateway**; retries follow the configured policy on the owning replica only |
| Video jobs, checkpoints, cost ledger | In-process video runtime + local store path | Local files only; **not durable across hosts, not distributed** | per-instance | Jobs on the lost replica are not resumed elsewhere | A second replica does not see the first replica's jobs or ledger | Per partition side | Jobs are leased per instance (`lease_owner`/`lease_until` on the job, atomic `TryClaim` on the store, renewed while polling): two orchestrators over one store execute a job exactly once (tested). The guarantee is cross-process only when the store's claim is atomic across processes — the in-repo stores prove the semantics in-process; sharing one store FILE across processes remains unsupported (whole-file overwrite), so multi-replica video still requires a store with an atomic conditional claim |
| OIDC pending transactions | In-process / security store session state | Session-lifetime | per-instance (login flows complete on the replica that started them) | Pending logins invalidated | Load balancers should keep auth callbacks sticky to the starting replica | Per partition side | Callbacks with unknown state are rejected, never guessed |
| Metrics / activity / audit views in the UI | Aggregated from the serving replica + shared stores where configured | n/a | Mixed: audit/config views are as shared as their stores; live activity and health views are **per-instance observations** | n/a | The UI of one replica does not show another replica's in-flight activity; health shown is the serving replica's own | Per partition side | n/a |

## Rules that follow from this inventory

1. Running multiple replicas is supported for stateless request
   serving with the PostgreSQL or Redis control-plane and security
   stores. Breaker, quota-reservation, affinity and video state
   remain per-instance, with the oversubscription consequence
   documented above — size per-instance limits accordingly.
2. The file control-plane backend coordinates processes on **one
   host** (flock + CAS). It is not a cross-host store, and a
   network filesystem is never a coordination substitute.
3. Deployment mode and these scopes are exposed at `/api/hello`
   (`deployment` object) and must be surfaced by operator tooling
   and the UI instead of any hand-written HA claim. Documentation
   that contradicts this inventory is a defect.
4. Backup/restore (`nexaroute backup`, `restore --verify`,
   `doctor`) covers config, security-store snapshots and
   file-backed control-plane state. It does not snapshot external
   PostgreSQL/Redis or video assets/jobs; see
   `docs/KNOWN_GAPS.md` → “State and HA”.

## Test evidence behind this inventory

- `internal/controlplane` — `TestTwoInstanceFileStoreCoherence`
  (two real FileStore instances over one store file: visibility,
  typed `ErrConflict` on stale writes and deletes), plus the
  existing cross-process flock tests and the real-service
  PostgreSQL/Redis integration suites (opt-in, service-gated).
- `internal/httpapi` — deployment report tests: the default
  topology reports per-instance scopes and warnings; shared
  backends upgrade only the subsystems they actually back.
- Live multi-host behavior (real PG/Redis clusters, rolling
  deploys) remains **UNVERIFIED** in this tree; the integration
  suites are the strongest available evidence and are gated on
  operator-provided services.
