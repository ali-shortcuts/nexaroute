package securitystore

// Wave C-1 model-based differential suite: a small reference model of
// the audit-chain semantics (append / prune-to-prefix / verify /
// anchor) drives every backend — bbolt (real), PostgreSQL and Redis
// (via the in-test fakes; the gated integration suites cover the real
// services) — through identical seeded operation sequences. After
// every operation each backend must match the model: the chain
// verifies, head hash/seq equal the model's, pruning removes only a
// sequence prefix, and the anchor never moves backwards. Cross-backend
// record-hash parity is asserted throughout: identical operation
// sequences must produce byte-identical audit records on all three
// backends.

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"
)

// waveCModel mirrors the observable chain state: every surviving
// record's sequence in order, the head/anchor refs and the allocator.
// It is updated only from successful appends, exactly as the shared
// planAuditAppend semantics dictate (verified pre-state, prefix
// pruning, anchor = newest pruned record).
type waveCModel struct {
	seqs      []uint64
	head      chainRef
	hasHead   bool
	anchor    chainRef
	hasAnchor bool
	allocator uint64
}

// waveCOp is one step of a seeded operation sequence.
type waveCOp struct {
	actor     string
	action    string
	backdated bool // timestamp older than the previous record
}

func waveCOps(rng *rand.Rand, n int, base time.Time) []waveCOp {
	actors := []string{"alice", "bob", "carol"}
	actions := []string{"auth.login", "auth.logout", "admin.write", "data.read"}
	ops := make([]waveCOp, 0, n)
	for i := 0; i < n; i++ {
		ops = append(ops, waveCOp{
			actor:     actors[rng.Intn(len(actors))],
			action:    actions[rng.Intn(len(actions))],
			backdated: rng.Intn(4) == 0,
		})
	}
	return ops
}

// waveCBackend bundles one Backend under test with its display name.
type waveCBackend struct {
	name    string
	backend Backend
}

// waveCOpenBackends opens the three backends under test with generous
// retention/count bounds (no pruning) unless overridden. PG/Redis run
// on the in-test fakes; the build-tagged integration suites prove the
// same contract against the real services.
func waveCOpenBackends(t *testing.T, maxItems int, retention time.Duration) []waveCBackend {
	t.Helper()
	days := int(retention.Hours() / 24)
	if days < 1 {
		days = 1
	}
	if days > 3650 {
		days = 3650
	}
	bb, err := Open(contractStorePath(t, "model.db"), maxItems, days)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bb.Close() })
	pg := openFakePostgres(t, maxItems, retention)
	rd := newRedisStoreWithClient(newFakeRedisClient(), maxItems, retention)
	return []waveCBackend{
		{name: "bbolt", backend: bb},
		{name: "postgres-fake", backend: pg},
		{name: "redis-fake", backend: rd},
	}
}

// checkWaveCModelInvariant verifies one backend against the model
// after an operation: chain verifies, head/anchor/allocator match,
// pruning removed only a prefix, anchor never regressed.
func checkWaveCModelInvariant(t *testing.T, ctx context.Context, wb waveCBackend, m *waveCModel) {
	t.Helper()
	report, err := wb.backend.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("%s: VerifyAuditChain error: %v", wb.name, err)
	}
	if !report.Verified {
		t.Fatalf("%s: chain must verify: %+v", wb.name, report)
	}
	if m.hasHead {
		if report.HeadHash != m.head.Hash || report.LastSeq != m.head.Seq {
			t.Fatalf("%s: head = (%d %q), model = (%d %q); report=%+v",
				wb.name, report.LastSeq, report.HeadHash, m.head.Seq, m.head.Hash, report)
		}
	}
	if m.hasAnchor != report.HasAnchor {
		t.Fatalf("%s: HasAnchor = %v, model = %v; report=%+v", wb.name, report.HasAnchor, m.hasAnchor, report)
	}
	if m.hasAnchor && report.AnchorSeq != m.anchor.Seq {
		t.Fatalf("%s: AnchorSeq = %d, model = %d", wb.name, report.AnchorSeq, m.anchor.Seq)
	}
	records, err := wb.backend.AuditRecords(ctx, 1_000_000)
	if err != nil {
		t.Fatalf("%s: AuditRecords: %v", wb.name, err)
	}
	if len(records) != len(m.seqs) {
		t.Fatalf("%s: %d records, model has %d", wb.name, len(records), len(m.seqs))
	}
	// AuditRecords returns newest first; model seqs are ascending.
	for i, r := range records {
		want := m.seqs[len(m.seqs)-1-i]
		if r.Seq != want {
			t.Fatalf("%s: records[%d].Seq = %d, want %d", wb.name, i, r.Seq, want)
		}
		if i > 0 && records[i-1].Seq <= r.Seq {
			t.Fatalf("%s: records not strictly newest-first: %+v", wb.name, records)
		}
	}
}

// TestWaveCModelDifferential drives all three backends through the
// same seeded operation sequences and checks the model invariant
// after every append, plus byte-identical records across backends.
func TestWaveCModelDifferential(t *testing.T) {
	for _, seed := range []int64{1, 7, 20261016} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			ctx := context.Background()
			backends := waveCOpenBackends(t, 1000, 365*24*time.Hour)
			models := map[string]*waveCModel{}
			for _, wb := range backends {
				models[wb.name] = &waveCModel{}
			}
			rng := rand.New(rand.NewSource(seed))
			base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
			ops := waveCOps(rng, 40, base)
			var prevTS time.Time
			for i, op := range ops {
				ts := base.Add(time.Duration(i) * time.Second)
				if op.backdated && !prevTS.IsZero() {
					ts = prevTS.Add(-time.Hour)
				}
				prevTS = ts
				event := AuditEvent{Timestamp: ts, Actor: op.actor, Action: op.action, Outcome: "success", Status: 200}
				var firstRecords []AuditEvent
				for bi, wb := range backends {
					if err := wb.backend.AppendAudit(ctx, event); err != nil {
						t.Fatalf("%s: append %d: %v", wb.name, i, err)
					}
					records, err := wb.backend.AuditRecords(ctx, 1_000_000)
					if err != nil {
						t.Fatal(err)
					}
					// Head of the newest-first list is the just-appended record.
					m := models[wb.name]
					m.allocator++
					m.seqs = append(m.seqs, m.allocator)
					m.head = chainRef{Seq: m.allocator, Hash: records[0].Hash}
					m.hasHead = true
					if bi == 0 {
						firstRecords = records
					} else if len(records) != len(firstRecords) {
						t.Fatalf("op %d: %s has %d records, bbolt has %d", i, wb.name, len(records), len(firstRecords))
					}
					checkWaveCModelInvariant(t, ctx, wb, m)
				}
				// Cross-backend parity: identical ops must hash identically.
				for bi, wb := range backends[1:] {
					records, err := wb.backend.AuditRecords(ctx, 1_000_000)
					if err != nil {
						t.Fatal(err)
					}
					for j := range records {
						if records[j] != firstRecords[j] {
							t.Fatalf("op %d: %s record %d differs from bbolt:\n%+v\n%+v",
								i, backends[bi+1].name, j, records[j], firstRecords[j])
						}
					}
				}
			}
		})
	}
}

// TestWaveCAuditRecordsOrderParity is the regression test for the
// ordering divergence the differential suite caught: bbolt
// AuditRecords iterated the timestamp-major storage key, so
// caller-supplied non-monotonic timestamps came back newest-timestamp
// first while PostgreSQL/Redis return newest-sequence first. "Newest
// first" is the append order (highest sequence): every backend must
// agree byte-for-byte on identical histories.
func TestWaveCAuditRecordsOrderParity(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	events := []AuditEvent{
		{Timestamp: base.Add(30 * time.Second), Actor: "p", Action: "first-appended", Outcome: "success", Status: 200},
		{Timestamp: base.Add(20 * time.Second), Actor: "p", Action: "second-appended", Outcome: "success", Status: 200},
		{Timestamp: base.Add(10 * time.Second), Actor: "p", Action: "third-appended", Outcome: "success", Status: 200},
	}
	backends := waveCOpenBackends(t, 1000, 365*24*time.Hour)
	var reference []AuditEvent
	for bi, wb := range backends {
		for _, ev := range events {
			if err := wb.backend.AppendAudit(ctx, ev); err != nil {
				t.Fatalf("%s: append: %v", wb.name, err)
			}
		}
		got, err := wb.backend.AuditRecords(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[0].Seq != 3 || got[1].Seq != 2 || got[2].Seq != 1 {
			t.Fatalf("%s: AuditRecords seq order = [%d %d %d], want newest-appended first [3 2 1]",
				wb.name, got[0].Seq, got[1].Seq, got[2].Seq)
		}
		if bi == 0 {
			reference = got
		} else {
			for j := range got {
				if got[j] != reference[j] {
					t.Fatalf("%s record %d differs from %s:\n%+v\n%+v",
						wb.name, j, backends[0].name, got[j], reference[j])
				}
			}
		}
	}
}

// TestWaveCPruningPrefixInvariant drives each backend past its count
// bound with a seeded interleaving and asserts the pruning model:
// survivors are exactly the highest-N sequences (a prefix was
// removed), the anchor is the newest pruned record, the first
// survivor links to the anchor, and the anchor never moves backwards.
func TestWaveCPruningPrefixInvariant(t *testing.T) {
	ctx := context.Background()
	const maxItems = 100
	// Retention of a century disables age pruning; the count bound
	// drives pruning deterministically.
	backends := waveCOpenBackends(t, maxItems, 36500*24*time.Hour)
	rng := rand.New(rand.NewSource(99))
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var lastAnchor uint64
	for i := 0; i < 160; i++ {
		event := AuditEvent{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Actor:     "prune", Action: fmt.Sprintf("op-%d", rng.Intn(5)),
			Outcome: "success", Status: 200,
		}
		for _, wb := range backends {
			if err := wb.backend.AppendAudit(ctx, event); err != nil {
				t.Fatalf("%s: append %d: %v", wb.name, i, err)
			}
		}
	}
	for _, wb := range backends {
		report, err := wb.backend.VerifyAuditChain(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Verified {
			t.Fatalf("%s: pruned chain must verify: %+v", wb.name, report)
		}
		if !report.HasAnchor {
			t.Fatalf("%s: expected a pruning anchor after 160 appends capped at %d", wb.name, maxItems)
		}
		if report.AnchorSeq < lastAnchor && lastAnchor != 0 {
			t.Fatalf("%s: anchor moved backwards", wb.name)
		}
		lastAnchor = report.AnchorSeq
		records, err := wb.backend.AuditRecords(ctx, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != maxItems {
			t.Fatalf("%s: %d survivors, want %d", wb.name, len(records), maxItems)
		}
		// Survivors must be exactly the highest-maxItems sequences:
		// pruning removed a prefix and nothing else.
		seqs := make([]uint64, 0, len(records))
		for _, r := range records {
			seqs = append(seqs, r.Seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for j := 1; j < len(seqs); j++ {
			if seqs[j] != seqs[j-1]+1 {
				t.Fatalf("%s: survivors are not contiguous: %v", wb.name, seqs)
			}
		}
		if seqs[0] != report.AnchorSeq+1 {
			t.Fatalf("%s: first survivor seq %d must follow anchor %d", wb.name, seqs[0], report.AnchorSeq)
		}
		if seqs[len(seqs)-1] != report.LastSeq {
			t.Fatalf("%s: last survivor %d must equal head %d", wb.name, seqs[len(seqs)-1], report.LastSeq)
		}
	}
}

// TestWaveCConcurrentAppendsStayLinear hammers each backend with
// parallel writers: the chain must remain a single linear history —
// no forks, no lost appends, no duplicate sequences.
//
// Writer count is deliberately 4, not 8: the Redis adapter's CAS loop
// has a fixed redisCASMaxAttempts = 16 retry budget (pinned by the
// conflict-injection test in redis_fake_test.go), and 8 writers with
// zero think time exhausted it on a loaded CI runner (run 38838554066:
// "redis concurrent modification; retry budget exhausted"). Four
// writers still prove linearity under genuine parallel contention
// without racing the documented budget.
func TestWaveCConcurrentAppendsStayLinear(t *testing.T) {
	const writers = 4
	const perWriter = 10
	for _, wb := range waveCOpenBackends(t, 1000, 365*24*time.Hour) {
		wb := wb
		t.Run(wb.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			var wg sync.WaitGroup
			errs := make(chan error, writers*perWriter)
			for g := 0; g < writers; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < perWriter; i++ {
						if err := wb.backend.AppendAudit(ctx, AuditEvent{
							Timestamp: time.Now().UTC(),
							Actor:     fmt.Sprintf("worker-%d", g),
							Action:    "wave.concurrent", Outcome: "success", Status: 200,
						}); err != nil {
							errs <- err
						}
					}
				}(g)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("%s: concurrent append: %v", wb.name, err)
			}
			report, err := wb.backend.VerifyAuditChain(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Verified || report.ChainedRecords != writers*perWriter {
				t.Fatalf("%s: chain after concurrent appends: %+v", wb.name, report)
			}
			records, err := wb.backend.AuditRecords(ctx, 1_000_000)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[uint64]bool{}
			for _, r := range records {
				if seen[r.Seq] {
					t.Fatalf("%s: duplicate sequence %d", wb.name, r.Seq)
				}
				seen[r.Seq] = true
			}
			if len(seen) != writers*perWriter {
				t.Fatalf("%s: %d unique seqs, want %d (lost appends)", wb.name, len(seen), writers*perWriter)
			}
		})
	}
}
