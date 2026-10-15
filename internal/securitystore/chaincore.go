package securitystore

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// This file holds the audit hash chain's decision logic as pure
// functions over an in-memory chain state, for the network-backed
// security store adapters (PostgreSQL, Redis). The logic is a faithful
// port of the bbolt store's verifyChainTx/appendAuditTx semantics,
// minus the bbolt storage-key consistency checks: network stores keep
// an explicit sequence per record, so a record's Seq field is its
// storage sequence by construction. The bbolt implementation is
// unchanged; cross-backend parity is enforced by tests that require
// identical record hashes for identical operation sequences.

// Chain metadata states reuse the refState classification of the
// bbolt store (absent / valid / malformed): adapters report malformed
// when their stored meta value fails to decode.

// chainState is a full snapshot of one audit chain: every surviving
// record in ascending sequence order, the chain head and pruning
// anchor metadata, and the sequence allocator (the highest sequence
// ever allocated; bbolt persists it as the meta bucket sequence).
type chainState struct {
	Records     []AuditEvent
	Head        chainRef
	HeadState   refState
	Anchor      chainRef
	AnchorState refState
	Allocator   uint64
}

// verifyChainState verifies a chain snapshot with the same rules —
// and the same problem wording where the rule is shared — as
// verifyChainTx. Storage-key checks are the only omissions.
func verifyChainState(st chainState) ChainReport {
	report := ChainReport{Verified: true}
	problem := func(format string, args ...any) {
		report.Verified = false
		report.Problems = append(report.Problems, fmt.Sprintf(format, args...))
	}
	hasAnchor := st.AnchorState == refValid
	if st.AnchorState == refMalformed {
		problem("chain anchor metadata is malformed; pruning history cannot be trusted")
	}
	report.HasAnchor = hasAnchor
	if hasAnchor {
		report.AnchorSeq = st.Anchor.Seq
	}
	records := append([]AuditEvent(nil), st.Records...)
	sort.SliceStable(records, func(i, j int) bool { return records[i].Seq < records[j].Seq })
	for _, event := range records {
		report.TotalRecords++
		if event.Hash == "" {
			report.LegacyRecords++
		} else {
			report.ChainedRecords++
		}
	}
	var prev *AuditEvent
	var prevSeq uint64
	for i := range records {
		r := records[i]
		seq := r.Seq
		if prev != nil && seq <= prevSeq {
			problem("audit sequence %d is duplicated or out of order", seq)
		}
		if r.Hash == "" {
			prev = &records[i]
			prevSeq = seq
			continue
		}
		if prev != nil && prev.Hash != "" && seq != prevSeq+1 {
			problem("sequence gap in chained audit records: seq %d follows seq %d; chained sequences must be contiguous", seq, prevSeq)
		}
		if got := canonicalAuditHash(r); got != r.Hash {
			problem("audit record seq %d was modified after it was written (hash mismatch)", seq)
		}
		switch {
		case prev != nil:
			if want := recordHash(*prev, prev.Seq); r.PrevHash != want {
				problem("audit record seq %d does not link to its predecessor (history was deleted, inserted, or reordered)", seq)
			}
		case hasAnchor:
			if r.PrevHash != st.Anchor.Hash {
				problem("first surviving audit record seq %d does not link to the pruning anchor seq %d (history before the retained window was altered)", seq, st.Anchor.Seq)
			}
			if seq != st.Anchor.Seq+1 && report.LegacyRecords == 0 {
				problem("gap between pruning anchor seq %d and first surviving audit record seq %d", st.Anchor.Seq, seq)
			}
		default:
			if r.PrevHash != "" {
				problem("first chained audit record seq %d links to a predecessor that does not exist and no pruning anchor is recorded", seq)
			}
		}
		prev = &records[i]
		prevSeq = seq
	}
	if len(records) > 0 {
		report.FirstSeq = records[0].Seq
		last := records[len(records)-1]
		report.LastSeq = last.Seq
		report.HeadHash = recordHash(last, last.Seq)
	}
	head, headState := st.Head, st.HeadState
	switch headState {
	case refMalformed:
		problem("chain head metadata is malformed; the chain tail cannot be trusted")
	case refValid:
		switch {
		case len(records) == 0:
			if !hasAnchor || head != st.Anchor {
				problem("chain head metadata does not match the pruning anchor for an empty audit log")
			}
			report.HeadHash = head.Hash
		case head.Seq != report.LastSeq || head.Hash != report.HeadHash:
			problem("chain head metadata (seq %d) does not match the newest surviving audit record (seq %d); the newest records may have been truncated", head.Seq, report.LastSeq)
		}
	default: // refAbsent
		if report.ChainedRecords > 0 {
			problem("audit records are chained but chain head metadata is missing")
		}
		if len(records) == 0 && hasAnchor {
			problem("audit log was fully pruned to the pruning anchor seq %d but chain head metadata is missing; a fully pruned chained log must retain its head at the anchor", st.Anchor.Seq)
		}
	}
	allocator := st.Allocator
	evidence := report.LastSeq
	if hasAnchor && st.Anchor.Seq > evidence {
		evidence = st.Anchor.Seq
	}
	if headState == refValid && head.Seq > evidence {
		evidence = head.Seq
	}
	if allocator < evidence {
		problem("audit sequence allocator is at %d, below the highest allocated sequence %d evidenced by retained records and chain metadata; the allocator must never be reconstructed or lowered", allocator, evidence)
	}
	if headState == refValid && (report.ChainedRecords > 0 || report.TotalRecords == 0) && allocator != head.Seq {
		problem("audit sequence allocator is at %d but the chain head is at sequence %d; chained appends allocate exactly one sequence per record, so the allocator must equal the head", allocator, head.Seq)
	}
	return report
}

// sanitizeAuditEvent applies the storage-boundary sanitization and
// validation of appendAuditTx (timestamp stamping included).
func sanitizeAuditEvent(event AuditEvent) (AuditEvent, error) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	event.RequestID = Sanitize(event.RequestID, 128)
	event.Actor = Sanitize(event.Actor, 200)
	event.Action = Sanitize(event.Action, 128)
	event.Target = Sanitize(event.Target, 200)
	event.Method = Sanitize(event.Method, 16)
	event.Path = Sanitize(event.Path, 256)
	event.Outcome = Sanitize(event.Outcome, 64)
	event.Permission = Sanitize(event.Permission, 64)
	event.Authn = Sanitize(event.Authn, 64)
	if event.Actor == "" || event.Action == "" || event.Outcome == "" || event.Status < 0 || event.Status > 599 {
		return AuditEvent{}, ErrInvalidAudit
	}
	return event, nil
}

// appendPlan is the result of planning one audit append against a
// verified chain state: the record to insert, the sequences pruning
// removes, and the metadata to persist — all applied atomically by
// the adapter.
type appendPlan struct {
	Event      AuditEvent
	PrunedSeqs []uint64
	Head       chainRef
	Anchor     chainRef
	HasAnchor  bool
	Allocator  uint64
}

// planAuditAppend mirrors appendAuditTx's decision logic: the
// pre-append chain must verify, genesis/emptied metadata states are
// distinguished exactly as the bbolt gate distinguishes them, the new
// record links to the verified head hash, and pruning deletes a
// sequence prefix (retention age or count bound), recording the
// newest pruned record as the chain anchor.
func planAuditAppend(st chainState, event AuditEvent, maxAuditItems int, retention time.Duration, now time.Time) (appendPlan, error) {
	event, err := sanitizeAuditEvent(event)
	if err != nil {
		return appendPlan{}, err
	}
	report := verifyChainState(st)
	if !report.Verified {
		return appendPlan{}, fmt.Errorf("audit chain failed verification (%s); refusing to append audit records", report.Problems[0])
	}
	switch {
	case report.TotalRecords == 0:
		genesis := st.HeadState == refAbsent && st.AnchorState == refAbsent
		emptied := st.HeadState == refValid && st.AnchorState == refValid
		if !genesis && !emptied {
			return appendPlan{}, errors.New("security store chain metadata is inconsistent with an empty audit log; refusing to append audit records")
		}
	case report.ChainedRecords == 0:
		if st.HeadState != refAbsent || st.AnchorState != refAbsent {
			return appendPlan{}, errors.New("security store has chain metadata but no chained audit records; refusing to append audit records")
		}
	}
	event.Seq = st.Allocator + 1
	event.PrevHash = report.HeadHash
	event.Hash = canonicalAuditHash(event)

	// Pruning, over the post-append record list in sequence order —
	// the same prefix rule as the bbolt store: a record leaves when it
	// is over the retention age or the count bound still overflows.
	type seqTS struct {
		seq uint64
		ts  int64
	}
	infos := make([]seqTS, 0, len(st.Records)+1)
	for _, r := range st.Records {
		infos = append(infos, seqTS{seq: r.Seq, ts: r.Timestamp.UnixNano()})
	}
	infos = append(infos, seqTS{seq: event.Seq, ts: event.Timestamp.UnixNano()})
	sort.Slice(infos, func(i, j int) bool { return infos[i].seq < infos[j].seq })
	cutoff := now.Add(-retention).UnixNano()
	toDelete := 0
	for toDelete < len(infos) {
		overAge := infos[toDelete].ts < cutoff
		overCount := len(infos)-toDelete > maxAuditItems
		if !overAge && !overCount {
			break
		}
		toDelete++
	}
	bySeq := make(map[uint64]AuditEvent, len(st.Records))
	for _, r := range st.Records {
		bySeq[r.Seq] = r
	}
	plan := appendPlan{Event: event, Allocator: event.Seq}
	var pruned *chainRef
	for _, info := range infos[:toDelete] {
		old, ok := bySeq[info.seq]
		if !ok {
			old = event // the just-appended record pruned immediately
		}
		ref := chainRef{Seq: info.seq, Hash: recordHash(old, info.seq)}
		pruned = &ref
		plan.PrunedSeqs = append(plan.PrunedSeqs, info.seq)
	}
	plan.Head = chainRef{Seq: event.Seq, Hash: event.Hash}
	if toDelete == len(infos) && pruned != nil {
		plan.Head = *pruned
	}
	if pruned != nil {
		plan.Anchor = *pruned
		plan.HasAnchor = true
	} else if st.AnchorState == refValid {
		plan.Anchor = st.Anchor
		plan.HasAnchor = true
	}
	return plan, nil
}
