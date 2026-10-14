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
