# ADR 0004: Multi-node video — product goal or single-process positioning

- Status: **Accepted — Option B (single-process positioning is settled).**
  Decided 2026-10-22 by the supervising engineer under the owner's
  explicit delegation of the remaining decisions ("fix the remaining
  sections yourself", 2026-10-22). The exact question at the end of
  this ADR is answered **no**: multi-replica video is not a current
  product goal. The PostgreSQL job store and cost ledger are
  documented as single-active-process durability features, and
  multi-node video leaves the roadmap. Option C (shared asset backend,
  then declare) remains the only acceptable path if that answer ever
  changes; Option A stays rejected at any point.
- Date: 2026-10-22
- Parent audit: #593; related: #609/#623 (PostgreSQL job store),
  the section-B change (durable PostgreSQL cost ledger)

## Context

Video started as a single-process subsystem: jobs, checkpoints and the
cost ledger lived in one gateway process, and the documentation said so.
Two changes have since moved the ground:

- The PostgreSQL job store (#609/#623) makes job records, idempotency
  and lease claims durable and atomic across processes.
- The PostgreSQL cost ledger (section B) makes reservations and
  settlements durable, shared and exactly-once across processes
  connected to the same database.

What has **not** changed: assets. Inputs and rendered outputs live in
`internal/video/storage.Local` — one host's filesystem, integrity-checked
on read, with no shared or replicated backend and no operator-facing
contract for providing one.

That leaves the subsystem in a deliberately awkward middle state: two of
three state planes are multi-process safe, and the third is not. A job
claimed by replica B whose input assets were written by replica A cannot
read them. This is not graceful degradation; for cross-replica claims it
is a correctness failure that today is avoided only because the
documentation positions video as single-process.

## Options

### Option A — Declare multi-node video supported on PostgreSQL stores + operator-provided shared assets

Docs and the deployment report would call video "shared" when the
PostgreSQL backend is configured, with a contract that the asset root
must be shared storage (e.g. the same network filesystem mounted at the
same path on every replica).

- Pro: smallest step from today's code; honest about jobs/ledger.
- Con: the asset contract is unenforceable and unverifiable by the
  gateway — an operator who misses it gets cross-replica job failures,
  and NexaRoute cannot detect the misconfiguration. Support burden and
  silent-failure risk land on exactly the operators least able to see
  why renders fail.

### Option B — Keep single-process positioning (placeholder, current)

Documentation keeps video positioned as a single-host subsystem. The
PostgreSQL stores are described as durability/restart-safety features
for one active video process (plus standby), not as a multi-replica
video offering.

- Pro: fully honest today; zero new code; the stores still deliver
  restart durability and exactly-once settlement, which were the
  section-B requirements.
- Con: leaves real multi-process capability (jobs + ledger) deliberately
  unadvertised and untested as a fleet topology; operators who want
  multi-replica video have no supported path.

### Option C — Build a shared asset backend, then declare multi-node video

Implement an operator-configured shared `AssetSink` (S3-compatible
object storage is the natural contract: Put/Open/Read with the existing
size + SHA-256 integrity semantics), selected by configuration with the
same fail-closed posture as the job store. Only after it lands with
real-service integration tests does the documentation declare
multi-node video supported.

- Pro: the only option that makes multi-node video actually correct
  end-to-end; object storage fits the write-once/read-many asset shape.
- Con: the largest scope: a new storage backend, credential handling,
  lifecycle/retention semantics, and a real-service acceptance suite.
  Until it lands, positioning stays at Option B anyway.

## Recommendation

Option B now, Option C as the explicit path if multi-node video becomes
a product goal. Option A is not recommended at any point: it declares a
topology the build cannot keep correct.

## Exact decision required from the owner

**Is multi-replica video a product goal?**
- "Yes" → approve Option C as a scoped implementation program (asset
  backend first, declaration after), with Option B positioning until it
  lands.
- "No" → Option B becomes the settled positioning; the PostgreSQL video
  stores are documented as single-active-process durability features,
  and multi-node video leaves the roadmap.
