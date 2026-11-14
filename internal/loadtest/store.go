package loadtest

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// snapshot is the merchant's balance and version, read together.
type snapshot struct {
	Balance int64 `db:"balance"`
	Version int64 `db:"version"`
}

// ledgerWindow sums the merchant's ledger entries between two snapshots.
type ledgerWindow struct {
	Entries int64 `db:"entries"`
	Sum     int64 `db:"sum"`
	// LastBalanceAfter is the balance_after of the newest entry, nil when there are none.
	LastBalanceAfter *int64 `db:"last_balance_after"`
	// Other counts entries that don't belong to the run's transfers.
	Other int64 `db:"other"`
}

// storedTransfer is one of the run's transfers with its ledger entries and transfer.created events.
type storedTransfer struct {
	ID              string `db:"id"`
	Key             string `db:"idempotency_key"`
	Status          string `db:"status"`
	Amount          int64  `db:"amount"`
	Debits          int    `db:"debits"`
	Debited         int64  `db:"debited"`
	Refunds         int    `db:"refunds"`
	Refunded        int64  `db:"refunded"`
	Events          int    `db:"events"`
	PublishedEvents int    `db:"published_events"`
}

// store reads what the run left in the database. It never writes.
type store struct {
	db         *sqlx.DB
	merchantID int64
}

func (s store) snapshot(ctx context.Context) (snapshot, error) {
	const q = `
		SELECT balance, version
		FROM merchants
		WHERE id = $1
	`
	var snap snapshot
	if err := s.db.GetContext(ctx, &snap, q, s.merchantID); err != nil {
		return snapshot{}, fmt.Errorf("read merchant %d: %w", s.merchantID, err)
	}
	return snap, nil
}

// unsettled counts the run's transfers still waiting for an outcome, and its transfer.created
// events not yet marked published.
func (s store) unsettled(ctx context.Context, ref string) (inFlight, unpublished int, err error) {
	const q = `
		SELECT
			(
				SELECT count(*)
				FROM transfers
				WHERE merchant_id = $1
					AND reference_id = $2
					AND status IN ('PENDING', 'PROCESSING')
			) AS in_flight,
			(
				SELECT count(*)
				FROM outbox o
				JOIN transfers t ON t.id = o.transfer_id
				WHERE t.merchant_id = $1
					AND t.reference_id = $2
					AND o.event_type = 'transfer.created'
					AND o.published_at IS NULL
			) AS unpublished
	`
	var row struct {
		InFlight    int `db:"in_flight"`
		Unpublished int `db:"unpublished"`
	}
	if err := s.db.GetContext(ctx, &row, q, s.merchantID, ref); err != nil {
		return 0, 0, fmt.Errorf("count the run's unsettled transfers: %w", err)
	}
	return row.InFlight, row.Unpublished, nil
}

// ledger reads the entries with a merchant version after from and up to to.
func (s store) ledger(ctx context.Context, from, to int64, ref string) (ledgerWindow, error) {
	const q = `
		SELECT
			count(*) AS entries,
			COALESCE(sum(e.amount), 0) AS sum,
			(array_agg(e.balance_after ORDER BY e.merchant_version DESC))[1] AS last_balance_after,
			count(*) FILTER (WHERE t.reference_id IS DISTINCT FROM $4) AS other
		FROM balance_entries e
		LEFT JOIN transfers t ON t.id = e.transfer_id
		WHERE e.merchant_id = $1
			AND e.merchant_version > $2
			AND e.merchant_version <= $3
	`
	var w ledgerWindow
	if err := s.db.GetContext(ctx, &w, q, s.merchantID, from, to, ref); err != nil {
		return ledgerWindow{}, fmt.Errorf("read ledger entries %d-%d: %w", from+1, to, err)
	}
	return w, nil
}

// runTransfers reads the run's transfers in one statement, so a status and the entries written
// with it (FAILED and its refund) are always seen together.
func (s store) runTransfers(ctx context.Context, ref string) ([]storedTransfer, error) {
	const q = `
		WITH run AS (
			SELECT id, idempotency_key, status, amount
			FROM transfers
			WHERE merchant_id = $1
				AND reference_id = $2
		),
		entries AS (
			SELECT
				transfer_id,
				count(*) FILTER (WHERE type = 'DEBIT') AS debits,
				COALESCE(sum(amount) FILTER (WHERE type = 'DEBIT'), 0) AS debited,
				count(*) FILTER (WHERE type = 'REFUND') AS refunds,
				COALESCE(sum(amount) FILTER (WHERE type = 'REFUND'), 0) AS refunded
			FROM balance_entries
			WHERE transfer_id IN (SELECT id FROM run)
			GROUP BY transfer_id
		),
		events AS (
			SELECT
				transfer_id,
				count(*) AS events,
				count(published_at) AS published_events
			FROM outbox
			WHERE transfer_id IN (SELECT id FROM run)
				AND event_type = 'transfer.created'
			GROUP BY transfer_id
		)
		SELECT
			r.id::text AS id, r.idempotency_key, r.status, r.amount,
			COALESCE(e.debits, 0) AS debits, COALESCE(e.debited, 0) AS debited,
			COALESCE(e.refunds, 0) AS refunds, COALESCE(e.refunded, 0) AS refunded,
			COALESCE(v.events, 0) AS events, COALESCE(v.published_events, 0) AS published_events
		FROM run r
		LEFT JOIN entries e ON e.transfer_id = r.id
		LEFT JOIN events v ON v.transfer_id = r.id
	`
	var ts []storedTransfer
	if err := s.db.SelectContext(ctx, &ts, q, s.merchantID, ref); err != nil {
		return nil, fmt.Errorf("read the run's transfers: %w", err)
	}
	return ts, nil
}
