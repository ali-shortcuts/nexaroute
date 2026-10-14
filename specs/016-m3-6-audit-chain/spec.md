# M3-6 — Tamper-evident hash-chain audit log and verification CLI

## Requirement
Audit records in the admin security store must be tamper-evident: modification,
deletion, insertion, reordering, or truncation of stored audit events must be
detectable after the fact, and an operator must be able to verify chain
integrity offline with a CLI command.

## Current state (baseline ef187fd)
`internal/securitystore` persists sanitized `AuditEvent` JSON records in a
bbolt bucket (`audit-v1`), keyed by (timestamp, monotonic sequence). Records
are pruned inside the append transaction by retention age and by
`max_events`. There is no integrity linkage between records: a party with
write access to the database file can alter or drop records undetectably.

## Design (see plan.md)
Each new record gains `seq`, `prev_hash`, and `hash` fields. The hash is
SHA-256 over a versioned canonical encoding of the event's semantic fields
plus its sequence and previous hash. Chain head and pruning anchor are kept
in the meta bucket and updated in the same transaction as every append and
prune, so the chain survives retention pruning: the anchor records the
newest pruned record's sequence and hash, and the first surviving record
must link to it.

Pre-existing records (written before this change) have no chain fields.
They form a *legacy segment*: counted and reported by verification, never
claimed as verified, and usable as the hash anchor predecessor for the
first chained record while they survive.

## Acceptance criteria
1. Appends (all producers, via `appendAuditTx`) form a single linear chain
   under concurrency; chain state persists across restarts.
2. `Store.VerifyAuditChain` detects: modified record, deleted middle
   record, reordered/duplicated sequence, tail truncation, head-truncated
   history beyond the pruning anchor.
3. Retention/count pruning keeps the surviving chain verifiable via the
   anchor; pruning itself is not reported as tampering.
4. `nexaroute audit verify --config PATH` opens the store read-only and
   exits 0 (verified), 1 (integrity problems, each printed), or 2
   (operational error). Output is metadata only (counts, sequences, head
   hash); no event payloads or secrets.
5. Documentation states the limits honestly: a purely local, unanchored
   chain cannot defeat an attacker who controls both the records and the
   chain metadata; operators are advised to record the head hash externally.

## Non-goals
External anchoring/signing of checkpoints, distributed audit backends, and
changes to event content or producers.

## Review hardening (PR #301, round 2)
- Verification fails closed when required storage structures are
  absent: a missing `audit-v1` or `meta-v1` bucket is an integrity
  failure, never VERIFIED. Legitimate empty history (both buckets
  present, zero records) still verifies; pre-chain databases always
  have both buckets because `Open` creates them.
- The bootstrap predecessor for a pre-chain database is selected by
  highest stored sequence, not by bbolt key order (keys sort by
  timestamp first, so key order can disagree with chain order).
- Verification validates storage-key consistency per record: key
  length 16, key timestamp equal to the record timestamp at
  nanosecond precision, and (chained records only) key sequence equal
  to the record sequence. Key corruption is an integrity failure.

## Review hardening round 3 — fail-closed chain-head recovery
Appends validate the persisted chain state before writing and fail
closed (whole transaction rolls back, store byte-identical) unless
the state is demonstrably legitimate: genesis/empty; emptied-with-
head-equal-to-anchor; pure legacy with no chain metadata ever
written; or a chained log whose head exactly matches the retained
chain tail. Absent and malformed metadata are distinct states
(`refAbsent`/`refMalformed`/`refValid`); a malformed or missing head
on a chained log, a head that disagrees with the tail, or chain
metadata on a legacy-only log are corruption — integrity metadata is
never reconstructed from stored records. Within the chained segment,
surviving sequences must be contiguous (the counter is shared and
monotonic); legacy-segment gaps stay legitimate because the pre-chain
store pruned by timestamp.

## Review hardening round 4 — full-chain validation before append
The append path runs the complete chain verification (the shared
`verifyChainTx`, identical to `nexaroute audit verify`) on the
pre-append state inside the write transaction and refuses to append
unless it passes: every chained record's canonical hash is
recomputed, every predecessor link and the anchor connection are
validated, sequence continuity and storage-key consistency are
checked, and the head is compared against the verified tail. A
tail-only agreement check is not sufficient evidence of integrity.
Two VERIFIED states keep an extra append-time distinction: a pure
legacy log must carry no chain metadata, and an emptied log must be
genesis or head==anchor. There is exactly one verification
implementation; there is no second scan and no metadata
reconstruction path.

## Review hardening round 5 — sequence allocator validation
bbolt v1.4.3 keeps the bucket sequence in the persisted bucket
header (`Bucket.Sequence`/`NextSequence`/`SetSequence`, bucket.go):
it is saved on commit and restored on rollback, and `SetSequence`
accepts any value with no floor check — so a lowered allocator is a
durable corruption vector that record-level checks cannot see. The
shared verification therefore also validates the meta bucket's
allocator: it must never be below the highest sequence evidenced by
retained records, the chain head, or the pruning anchor; and
wherever a valid head exists on a chained or emptied log it must
equal the head exactly (the allocation contract: one sequence per
chained append, recorded as the head). Legacy-only logs are exempt
from equality (pre-chain timestamp pruning legitimately leaves the
allocator above the survivors), as are genesis and fully pruned
legacy stores, where no evidence exists to judge the counter. The
allocator is never reset, advanced, or reconstructed by the store.

## Review hardening round 6 — fully pruned chained log requires its head
An empty retained bucket with a valid pruning anchor but no chain
head previously verified: the missing-head check only fired when
chained records survived, and the allocator can legitimately still
equal the anchor. Verification now treats anchor-without-head on an
empty bucket as an integrity failure; a fully pruned chained log
must retain head == anchor, with the allocator validated against
that sequence. Genesis and fully pruned legacy-only stores (no chain
metadata at all) remain valid. The append path already rejected
this state; verification now agrees with it.
