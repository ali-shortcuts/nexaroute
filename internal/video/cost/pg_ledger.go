package cost

// Durable PostgreSQL video cost ledger: the same reserve/settle/release
// semantics as the in-memory Ledger, persisted in one table so cost
// accounting survives restarts and is shared by every process connected
// to the database (architecture mission section B).
//
// Exactly-once properties come from the table shape, not from
// application vigilance:
//
//   - one row per job (job_id PRIMARY KEY): a duplicate reserve resolves
//     to the existing row and is a no-op while it is still reserved;
//   - every mutation runs in a transaction that locks the job row
//     (SELECT ... FOR UPDATE, after an INSERT ... ON CONFLICT DO NOTHING
//     placeholder), so concurrent duplicates serialize at the row and
//     the state machine below is applied exactly once;
//   - settlement is a state transition (reserved -> actual_settled /
//     estimate_settled): repeating the same settlement is a no-op, and
//     a conflicting actual cost is an error, never a second charge;
//   - aggregates (Spent / EstimatedSpent / Reserved) are computed from
//     the rows on every read, so a restarted process reports the durable
//     truth instead of rebuilding state from job payloads.
//
// Crash consistency between a job row and its settlement is therefore
// reconciliation-based: both stores are durable in the same database,
// every settlement is idempotent, and Reconciliation(jobID) exposes the
// per-job estimate-vs-actual state for operators. There is deliberately
// no silent fallback: an unreachable or misconfigured database is a
// hard startup error (see OpenPostgresLedger), never volatile
// accounting.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for this adapter

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

const videoCostTable = "nexaroute_video_cost_reservations"

// Advisory-lock key for first-open migration ("NEXACOST").
const videoCostMigrateLock = int64(0x4e455841434f5354)

// PostgresLedger is a durable cost Ledger over PostgreSQL. The zero
// value is not usable; construct it with OpenPostgresLedger.
type PostgresLedger struct {
	db *sql.DB
}

// OpenPostgresLedger connects, pings and migrates the schema. The DSN
// is never logged or embedded in returned errors. A background context
// with bounded timeouts is used for each operation because the
// orchestrator's ledger contract is context-free.
func OpenPostgresLedger(ctx context.Context, dsn string) (*PostgresLedger, error) {
	if dsn == "" {
		return nil, errors.New("video cost ledger: postgres DSN is required for the postgres backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("video cost ledger: invalid postgres configuration")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, errors.New("video cost ledger: postgres is unreachable")
	}
	l := &PostgresLedger{db: db}
	migrateCtx, migrateCancel := context.WithTimeout(ctx, 60*time.Second)
	defer migrateCancel()
	if err := l.migrate(migrateCtx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return l, nil
}

// Close releases the database connections.
func (l *PostgresLedger) Close() error { return l.db.Close() }

func (l *PostgresLedger) migrate(ctx context.Context) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("video cost ledger: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, videoCostMigrateLock); err != nil {
		return fmt.Errorf("video cost ledger: migration lock: %w", err)
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+videoCostTable+` (`+
		`job_id text PRIMARY KEY,`+
		`provider text NOT NULL DEFAULT '',`+
		`model text NOT NULL DEFAULT '',`+
		`currency text NOT NULL DEFAULT '',`+
		`estimated_usd double precision NOT NULL DEFAULT 0,`+
		`upper_bound_usd double precision NOT NULL DEFAULT 0,`+
		`billable_seconds double precision NOT NULL DEFAULT 0,`+
		`price_known boolean NOT NULL DEFAULT false,`+
		`state text NOT NULL,`+
		`actual_usd double precision NOT NULL DEFAULT 0,`+
		`entry_listed boolean NOT NULL DEFAULT false,`+
		`created_at timestamptz NOT NULL DEFAULT now(),`+
		`updated_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		return fmt.Errorf("video cost ledger: migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("video cost ledger: commit migration: %w", err)
	}
	return nil
}

func (l *PostgresLedger) opContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

type costRow struct {
	estimate    video.CostEstimate
	state       reservationState
	actual      float64
	entryListed bool
}

func scanCostRow(row *sql.Row) (costRow, bool, error) {
	var r costRow
	var state string
	err := row.Scan(&r.estimate.Provider, &r.estimate.Model, &r.estimate.Currency,
		&r.estimate.EstimatedUSD, &r.estimate.UpperBoundUSD, &r.estimate.BillableSeconds,
		&r.estimate.PriceKnown, &state, &r.actual, &r.entryListed)
	if errors.Is(err, sql.ErrNoRows) {
		return costRow{}, false, nil
	}
	if err != nil {
		return costRow{}, false, err
	}
	r.state = reservationState(state)
	return r, true, nil
}

const costRowColumns = `provider, model, currency, estimated_usd, upper_bound_usd, billable_seconds, price_known, state, actual_usd, entry_listed`

// ReserveForJob persists a reservation for the job. It mirrors the
// in-memory contract exactly: idempotent while reserved, an error once
// the job's cost has been finalized.
func (l *PostgresLedger) ReserveForJob(jobID string, e video.CostEstimate, max float64) error {
	if jobID == "" {
		return errors.New("job ID is required for cost reservation")
	}
	if !validEstimate(e) {
		if !e.PriceKnown {
			return ErrUnknownPrice
		}
		return ErrInvalidEstimate
	}
	if max < 0 || math.IsNaN(max) || math.IsInf(max, 0) {
		return ErrInvalidEstimate
	}
	if max > 0 && e.UpperBoundUSD > max {
		return ErrBudgetExceeded
	}
	ctx, cancel := l.opContext()
	defer cancel()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("video cost ledger: begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+videoCostTable+
		` (job_id, provider, model, currency, estimated_usd, upper_bound_usd, billable_seconds, price_known, state, entry_listed)`+
		` VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'reserved', true) ON CONFLICT (job_id) DO NOTHING`,
		jobID, e.Provider, e.Model, e.Currency, e.EstimatedUSD, e.UpperBoundUSD, e.BillableSeconds, e.PriceKnown); err != nil {
		return fmt.Errorf("video cost ledger: reserve job %s: %w", jobID, err)
	}
	// Whether we inserted or a concurrent duplicate did, the row now
	// exists; lock it and apply the state rules.
	existing, found, err := scanCostRow(tx.QueryRowContext(ctx,
		`SELECT `+costRowColumns+` FROM `+videoCostTable+` WHERE job_id = $1 FOR UPDATE`, jobID))
	if err != nil {
		return fmt.Errorf("video cost ledger: read reservation for job %s: %w", jobID, err)
	}
	if !found {
		return fmt.Errorf("video cost ledger: reservation for job %s vanished during reserve", jobID)
	}
	if existing.state != reservedState {
		return fmt.Errorf("cost reservation for job %s has already been finalized", jobID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("video cost ledger: commit reserve for job %s: %w", jobID, err)
	}
	return nil
}

// ReleaseForJob releases a still-reserved reservation. It is a single
// conditional update; a job in any other state is untouched. The
// orchestrator contract has no error return here, and a failed release
// leaves the row visibly reserved (Reserved keeps reporting it) rather
// than silently dropping accounting.
func (l *PostgresLedger) ReleaseForJob(jobID string) {
	ctx, cancel := l.opContext()
	defer cancel()
	_, _ = l.db.ExecContext(ctx, `UPDATE `+videoCostTable+
		` SET state = 'released', updated_at = now() WHERE job_id = $1 AND state = 'reserved'`, jobID)
}

// settle runs one settlement transition under the job row lock. The
// placeholder insert gives concurrent first-time settlements a row to
// serialize on without exposing a phantom reservation: the placeholder
// is only ever visible inside this transaction and is updated to the
// settled state before commit.
func (l *PostgresLedger) settle(jobID string, placeholder video.CostEstimate, apply func(r costRow) (state reservationState, estimated, actual float64, err error)) error {
	ctx, cancel := l.opContext()
	defer cancel()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("video cost ledger: begin settlement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+videoCostTable+
		` (job_id, provider, model, currency, estimated_usd, upper_bound_usd, billable_seconds, price_known, state, entry_listed)`+
		` VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'reserved', false) ON CONFLICT (job_id) DO NOTHING`,
		jobID, placeholder.Provider, placeholder.Model, placeholder.Currency,
		placeholder.EstimatedUSD, placeholder.UpperBoundUSD, placeholder.BillableSeconds, placeholder.PriceKnown); err != nil {
		return fmt.Errorf("video cost ledger: settlement placeholder for job %s: %w", jobID, err)
	}
	r, found, err := scanCostRow(tx.QueryRowContext(ctx,
		`SELECT `+costRowColumns+` FROM `+videoCostTable+` WHERE job_id = $1 FOR UPDATE`, jobID))
	if err != nil {
		return fmt.Errorf("video cost ledger: read settlement for job %s: %w", jobID, err)
	}
	if !found {
		return fmt.Errorf("video cost ledger: settlement row for job %s vanished", jobID)
	}
	state, estimated, actual, err := apply(r)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+videoCostTable+
		` SET state = $2, estimated_usd = $3, actual_usd = $4, updated_at = now() WHERE job_id = $1`,
		jobID, string(state), estimated, actual); err != nil {
		return fmt.Errorf("video cost ledger: write settlement for job %s: %w", jobID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("video cost ledger: commit settlement for job %s: %w", jobID, err)
	}
	return nil
}

// SettleActual records the provider's actual cost for the job. It
// mirrors the in-memory contract: the same actual twice is a no-op, a
// different actual after settlement is a conflict error, an
// estimate-settled job upgrades to actual, and a job with no prior
// reservation settles against the estimate fallback.
func (l *PostgresLedger) SettleActual(jobID string, actual, estimateFallback float64) error {
	if jobID == "" || actual < 0 || math.IsNaN(actual) || math.IsInf(actual, 0) ||
		estimateFallback < 0 || math.IsNaN(estimateFallback) || math.IsInf(estimateFallback, 0) {
		return ErrInvalidEstimate
	}
	placeholder := video.CostEstimate{EstimatedUSD: estimateFallback, UpperBoundUSD: estimateFallback, PriceKnown: true}
	return l.settle(jobID, placeholder, func(r costRow) (reservationState, float64, float64, error) {
		if r.state == actualSettledState {
			if r.actual == actual {
				return r.state, r.estimate.EstimatedUSD, r.actual, nil
			}
			return r.state, 0, 0, fmt.Errorf("conflicting actual-cost settlement for job %s", jobID)
		}
		estimated := r.estimate.EstimatedUSD
		if estimated == 0 {
			estimated = estimateFallback
		}
		return actualSettledState, estimated, actual, nil
	})
}

// SettleEstimate settles the job at its estimate when no actual cost is
// available. Settled jobs (either kind) ignore repeat calls.
func (l *PostgresLedger) SettleEstimate(jobID string, estimateUSD float64) error {
	if jobID == "" || estimateUSD < 0 || math.IsNaN(estimateUSD) || math.IsInf(estimateUSD, 0) {
		return ErrInvalidEstimate
	}
	placeholder := video.CostEstimate{EstimatedUSD: estimateUSD, UpperBoundUSD: estimateUSD, PriceKnown: true}
	return l.settle(jobID, placeholder, func(r costRow) (reservationState, float64, float64, error) {
		if r.state == estimateSettledState || r.state == actualSettledState {
			return r.state, r.estimate.EstimatedUSD, r.actual, nil
		}
		return estimateSettledState, estimateUSD, r.actual, nil
	})
}

func (l *PostgresLedger) sumWhere(state reservationState, column string) float64 {
	ctx, cancel := l.opContext()
	defer cancel()
	var total float64
	err := l.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+column+`), 0) FROM `+videoCostTable+` WHERE state = $1`, string(state)).Scan(&total)
	if err != nil {
		// Reporting degrades to zero on a store error; mutations remain
		// fail-closed and the durable rows are unaffected. The failure
		// is logged, never silent: a clean zero during an outage would
		// otherwise be indistinguishable from "nothing spent".
		log.Printf("video cost ledger: aggregate read (state=%s) failed, reporting 0: %v", state, err)
		return 0
	}
	return total
}

// Spent returns the durable sum of settled actual costs.
func (l *PostgresLedger) Spent() float64 { return l.sumWhere(actualSettledState, "actual_usd") }

// EstimatedSpent returns the durable sum of estimate-settled costs.
func (l *PostgresLedger) EstimatedSpent() float64 {
	return l.sumWhere(estimateSettledState, "estimated_usd")
}

// Reserved returns the durable sum of outstanding reservation bounds.
func (l *PostgresLedger) Reserved() float64 { return l.sumWhere(reservedState, "upper_bound_usd") }

// Reconciliation mirrors the in-memory ledger's per-job
// estimate-vs-actual report over the durable row.
func (l *PostgresLedger) Reconciliation(jobID string) (estimate, actual, delta float64, reconciled bool, ok bool) {
	ctx, cancel := l.opContext()
	defer cancel()
	r, found, err := scanCostRow(l.db.QueryRowContext(ctx,
		`SELECT `+costRowColumns+` FROM `+videoCostTable+` WHERE job_id = $1`, jobID))
	if err != nil || !found {
		return 0, 0, 0, false, false
	}
	est := r.estimate.EstimatedUSD
	estimateValid := !math.IsNaN(est) && !math.IsInf(est, 0) && est >= 0
	if !estimateValid {
		est = 0
	}
	act := r.actual
	actualValid := !math.IsNaN(act) && !math.IsInf(act, 0) && act >= 0
	if !actualValid {
		act = 0
	}
	switch r.state {
	case actualSettledState:
		if !estimateValid || !actualValid {
			return est, act, 0, false, true
		}
		d := act - est
		if math.IsNaN(d) || math.IsInf(d, 0) {
			return est, act, 0, false, true
		}
		return est, act, d, true, true
	case estimateSettledState:
		return est, 0, 0, false, true
	default:
		return est, 0, 0, false, true
	}
}

// Entries lists the estimates of reservations created through
// ReserveForJob, oldest first — the durable counterpart of the
// in-memory ledger's entries.
func (l *PostgresLedger) Entries() []video.CostEstimate {
	ctx, cancel := l.opContext()
	defer cancel()
	rows, err := l.db.QueryContext(ctx, `SELECT `+costRowColumns+` FROM `+videoCostTable+
		` WHERE entry_listed ORDER BY created_at, job_id`)
	if err != nil {
		log.Printf("video cost ledger: entries read failed, reporting none: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []video.CostEstimate
	for rows.Next() {
		var r costRow
		var state string
		if err := rows.Scan(&r.estimate.Provider, &r.estimate.Model, &r.estimate.Currency,
			&r.estimate.EstimatedUSD, &r.estimate.UpperBoundUSD, &r.estimate.BillableSeconds,
			&r.estimate.PriceKnown, &state, &r.actual, &r.entryListed); err != nil {
			return out
		}
		out = append(out, r.estimate)
	}
	return out
}
