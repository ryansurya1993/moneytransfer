package loadtest

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// snapshot is the merchant's balance and the newest ledger entry id, read in one statement.
type snapshot struct {
	Balance     int64 `db:"balance"`
	LastEntryID int64 `db:"last_entry_id"`
}

type storedTransfer struct {
	Key    string `db:"idempotency_key"`
	Status string `db:"status"`
	Amount int64  `db:"amount"`
}

// store reads what the run left in the database. It never writes.
type store struct {
	db         *sqlx.DB
	merchantID int64
}

func (s store) snapshot(ctx context.Context) (snapshot, error) {
	const q = `
		SELECT
			balance,
			(SELECT COALESCE(max(id), 0) FROM balance_entries) AS last_entry_id
		FROM merchants
		WHERE id = $1
	`
	var snap snapshot
	if err := s.db.GetContext(ctx, &snap, q, s.merchantID); err != nil {
		return snapshot{}, fmt.Errorf("read merchant %d: %w", s.merchantID, err)
	}
	return snap, nil
}

// inFlight counts all of the merchant's transfers still waiting for an outcome.
func (s store) inFlight(ctx context.Context) (int, error) {
	const q = `
		SELECT count(*)
		FROM transfers
		WHERE merchant_id = $1
			AND status IN ('PENDING', 'PROCESSING')
	`
	var n int
	if err := s.db.GetContext(ctx, &n, q, s.merchantID); err != nil {
		return 0, fmt.Errorf("count transfers in flight: %w", err)
	}
	return n, nil
}

// runInFlight counts the run's transfers still waiting for an outcome.
func (s store) runInFlight(ctx context.Context, prefix string) (int, error) {
	const q = `
		SELECT count(*)
		FROM transfers
		WHERE merchant_id = $1
			AND status IN ('PENDING', 'PROCESSING')
			AND reference_id LIKE $2 || '%'
	`
	var n int
	if err := s.db.GetContext(ctx, &n, q, s.merchantID, prefix); err != nil {
		return 0, fmt.Errorf("count the run's transfers in flight: %w", err)
	}
	return n, nil
}

func (s store) runTransfers(ctx context.Context, prefix string) ([]storedTransfer, error) {
	const q = `
		SELECT idempotency_key, status, amount
		FROM transfers
		WHERE merchant_id = $1
			AND reference_id LIKE $2 || '%'
	`
	var ts []storedTransfer
	if err := s.db.SelectContext(ctx, &ts, q, s.merchantID, prefix); err != nil {
		return nil, fmt.Errorf("read the run's transfers: %w", err)
	}
	return ts, nil
}

// foreignEntries counts ledger entries after afterID that don't belong to the run's transfers.
// Each one changed the balance behind the run's back.
func (s store) foreignEntries(ctx context.Context, afterID int64, prefix string) (int, error) {
	const q = `
		SELECT count(*)
		FROM balance_entries e
		LEFT JOIN transfers t ON t.id = e.transfer_id
		WHERE e.merchant_id = $1
			AND e.id > $2
			AND (t.reference_id IS NULL OR t.reference_id NOT LIKE $3 || '%')
	`
	var n int
	if err := s.db.GetContext(ctx, &n, q, s.merchantID, afterID, prefix); err != nil {
		return 0, fmt.Errorf("count ledger entries outside the run: %w", err)
	}
	return n, nil
}
