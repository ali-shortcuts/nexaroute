# Tasks

- [x] T1 Chain fields, canonical hash, meta head/anchor in appendAuditTx
- [x] T2 Pruning anchor maintenance (retention + count paths)
- [x] T3 VerifyAuditChain + report type
- [x] T4 OpenReadOnly
- [x] T5 Store tests: build/restart/concurrency, mutation, deletion,
      reorder, tail truncation, head truncation beyond anchor, pruning
      with anchor, legacy segment, empty store
- [x] T6 CLI `audit verify` + exit-code/output tests
- [x] T7 OPERATIONS.md verification + limitations section
- [x] T8 Checklist M3-6 IN PROGRESS with evidence; full verify.sh; PR
- [x] T9 Fail closed on missing audit/meta buckets (+ tail-truncation-with-missing-meta regression)
- [x] T10 Bootstrap predecessor by highest sequence (+ out-of-order legacy timestamp regression)
- [x] T11 Storage-key consistency checks (+ key seq / key timestamp tamper regressions)
- [x] T12 Presence-aware ref loading (absent vs malformed vs valid)
- [x] T13 Append-time chain-state validation with fail-closed rollback (chainAppendPrev)
- [x] T14 Chained-segment sequence continuity in VerifyAuditChain
- [x] T15 Corrupt-head regression suite: tail-deleted (head intact / head removed), malformed head JSON, wrong head seq, wrong head hash, head removed with anchor — append error + verify failure + byte-identical store; false-verify scenario covered
- [x] T16 Extract verifyChainTx shared by VerifyAuditChain and the append path; remove chainAppendPrev (subsumed)
- [x] T17 Append gate: full verification + legacy/empty metadata distinction; prevHash derived from the verified report
- [x] T18 Beyond-the-tail regression suite: tail/middle payload edits, PrevHash edit, middle removal with head intact, forged tail hash + matching head, corrupted anchor hash, corrupted first-record anchor link — append error + verify failure + byte-identical store, all proven to fail pre-fix
- [x] T19 Confirm bbolt v1.4.3 allocator semantics from source (bucket header, commit-persisted, tx-rolled-back, SetSequence unchecked)
- [x] T20 Allocator invariants in verifyChainTx: floor = max evidenced sequence; equality with head on chained/emptied logs; legacy/genesis exemptions
- [x] T21 Allocator regression suite: below tail, reset to zero, above head, below anchor/head — verify failure + append refusal + byte-identical store and unchanged allocator; legacy-above-survivors and fully-pruned-legacy positives; rejection cases proven to fail pre-fix
- [x] T22 Anchor-without-head on an empty retained bucket is an integrity failure in verifyChainTx
- [x] T23 Regression test: intact fully pruned state verifies; deleting only the head fails verification explicitly, rejects append, leaves the store byte-identical (fails pre-fix)
