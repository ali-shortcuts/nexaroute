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
