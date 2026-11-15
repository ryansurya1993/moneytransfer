package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type ledgerCheckRepo txRepos

// DueMerchants implements transfer.LedgerCheckRepo.
// Comparing the balance as well as the version also catches a balance changed without a ledger entry.
func (l ledgerCheckRepo) DueMerchants(ctx context.Context, limit int) ([]transfer.MerchantID, error) {
	const q = `
		SELECT m.id
		FROM merchants m
		LEFT JOIN ledger_checkpoints c ON c.merchant_id = m.id
		WHERE m.version <> COALESCE(c.merchant_version, 0)
			OR m.balance <> COALESCE(c.balance_after, 0)
		ORDER BY c.checked_at NULLS FIRST, m.id
		LIMIT $1
	`
	var ids []transfer.MerchantID
	if err := l.tx.SelectContext(ctx, &ids, q, limit); err != nil {
		return nil, fmt.Errorf("select merchants to check: %w", err)
	}
	return ids, nil
}

// LockCheckpoint implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) LockCheckpoint(ctx context.Context, merchantID transfer.MerchantID) (transfer.LedgerCheckpoint, bool, error) {
	const insertQ = `
		INSERT INTO ledger_checkpoints (merchant_id, merchant_version, balance_after)
		VALUES ($1, 0, 0)
		ON CONFLICT (merchant_id) DO NOTHING
	`
	if _, err := l.tx.ExecContext(ctx, insertQ, merchantID); err != nil {
		return transfer.LedgerCheckpoint{}, false, fmt.Errorf("insert ledger checkpoint: %w", err)
	}

	const lockQ = `
		SELECT merchant_version, balance_after
		FROM ledger_checkpoints
		WHERE merchant_id = $1
		FOR UPDATE SKIP LOCKED
	`
	cp := transfer.LedgerCheckpoint{MerchantID: merchantID}
	err := l.tx.QueryRowxContext(ctx, lockQ, merchantID).Scan(&cp.Version, &cp.BalanceAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return transfer.LedgerCheckpoint{}, false, nil
	}
	if err != nil {
		return transfer.LedgerCheckpoint{}, false, fmt.Errorf("lock ledger checkpoint: %w", err)
	}
	return cp, true, nil
}

// Balance implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) Balance(ctx context.Context, merchantID transfer.MerchantID) (transfer.BalanceChange, error) {
	const q = `
		SELECT balance, version
		FROM merchants
		WHERE id = $1
	`
	var b transfer.BalanceChange
	if err := l.tx.QueryRowxContext(ctx, q, merchantID).Scan(&b.Balance, &b.Version); err != nil {
		return transfer.BalanceChange{}, fmt.Errorf("select merchant balance: %w", err)
	}
	return b, nil
}

// EntriesAfter implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) EntriesAfter(ctx context.Context, merchantID transfer.MerchantID, after, upTo int64, limit int) ([]transfer.LedgerEntry, error) {
	const q = `
		SELECT merchant_id, transfer_id, type, amount, balance_after, merchant_version
		FROM balance_entries
		WHERE merchant_id = $1
			AND merchant_version > $2
			AND merchant_version <= $3
		ORDER BY merchant_version
		LIMIT $4
	`
	var rows []struct {
		MerchantID      int64         `db:"merchant_id"`
		TransferID      uuid.NullUUID `db:"transfer_id"`
		Type            string        `db:"type"`
		Amount          int64         `db:"amount"`
		BalanceAfter    int64         `db:"balance_after"`
		MerchantVersion int64         `db:"merchant_version"`
	}
	if err := l.tx.SelectContext(ctx, &rows, q, merchantID, after, upTo, limit); err != nil {
		return nil, fmt.Errorf("select ledger entries: %w", err)
	}
	entries := make([]transfer.LedgerEntry, len(rows))
	for i, r := range rows {
		entries[i] = transfer.LedgerEntry{
			MerchantID:      transfer.MerchantID(r.MerchantID),
			TransferID:      r.TransferID.UUID,
			Type:            transfer.EntryType(r.Type),
			Amount:          transfer.Amount(r.Amount),
			BalanceAfter:    transfer.Amount(r.BalanceAfter),
			MerchantVersion: r.MerchantVersion,
		}
	}
	return entries, nil
}

// Transfers implements transfer.LedgerCheckRepo. Only the fields the rules need are filled in.
func (l ledgerCheckRepo) Transfers(ctx context.Context, ids []uuid.UUID) ([]transfer.Transfer, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const q = `
		SELECT id, merchant_id, amount, status
		FROM transfers
		WHERE id = ANY($1)
	`
	var rows []struct {
		ID         uuid.UUID `db:"id"`
		MerchantID int64     `db:"merchant_id"`
		Amount     int64     `db:"amount"`
		Status     string    `db:"status"`
	}
	if err := l.tx.SelectContext(ctx, &rows, q, ids); err != nil {
		return nil, fmt.Errorf("select transfers: %w", err)
	}
	ts := make([]transfer.Transfer, len(rows))
	for i, r := range rows {
		ts[i] = transfer.Transfer{
			ID:         r.ID,
			MerchantID: transfer.MerchantID(r.MerchantID),
			Amount:     transfer.Amount(r.Amount),
			Status:     transfer.Status(r.Status),
		}
	}
	return ts, nil
}

// SaveCheckpoint implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) SaveCheckpoint(ctx context.Context, cp transfer.LedgerCheckpoint) error {
	const q = `
		UPDATE ledger_checkpoints
		SET
			merchant_version = $2,
			balance_after = $3,
			checked_at = now()
		WHERE merchant_id = $1
	`
	if _, err := l.tx.ExecContext(ctx, q, cp.MerchantID, cp.Version, cp.BalanceAfter); err != nil {
		return fmt.Errorf("update ledger checkpoint: %w", err)
	}
	return nil
}

// UncheckedFinished implements transfer.LedgerCheckRepo.
// Finished transfers don't change any more, so locking them doesn't hold up a worker.
func (l ledgerCheckRepo) UncheckedFinished(ctx context.Context, limit int) ([]transfer.FinishedTransfer, error) {
	const q = `
		SELECT
			t.id,
			t.merchant_id,
			t.status,
			EXISTS (
				SELECT 1
				FROM balance_entries e
				WHERE e.transfer_id = t.id
					AND e.type = 'DEBIT'
			) AS has_debit,
			EXISTS (
				SELECT 1
				FROM balance_entries e
				WHERE e.transfer_id = t.id
					AND e.type = 'REFUND'
			) AS has_refund
		FROM transfers t
		WHERE t.ledger_checked_at IS NULL
			AND t.status IN ('SUCCESS', 'FAILED')
		ORDER BY t.completed_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`
	var rows []struct {
		ID         uuid.UUID `db:"id"`
		MerchantID int64     `db:"merchant_id"`
		Status     string    `db:"status"`
		HasDebit   bool      `db:"has_debit"`
		HasRefund  bool      `db:"has_refund"`
	}
	if err := l.tx.SelectContext(ctx, &rows, q, limit); err != nil {
		return nil, fmt.Errorf("select finished transfers to check: %w", err)
	}
	ts := make([]transfer.FinishedTransfer, len(rows))
	for i, r := range rows {
		ts[i] = transfer.FinishedTransfer{
			ID:         r.ID,
			MerchantID: transfer.MerchantID(r.MerchantID),
			Status:     transfer.Status(r.Status),
			HasDebit:   r.HasDebit,
			HasRefund:  r.HasRefund,
		}
	}
	return ts, nil
}

// MarkChecked implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) MarkChecked(ctx context.Context, ids []uuid.UUID) error {
	const q = `
		UPDATE transfers
		SET ledger_checked_at = now()
		WHERE id = ANY($1)
	`
	if _, err := l.tx.ExecContext(ctx, q, ids); err != nil {
		return fmt.Errorf("mark transfers checked: %w", err)
	}
	return nil
}

// AddFindings implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) AddFindings(ctx context.Context, fs []transfer.LedgerFinding) ([]transfer.LedgerFinding, error) {
	const q = `
		INSERT INTO ledger_findings (merchant_id, rule, merchant_version, transfer_id)
		SELECT merchant_id, rule, NULLIF(merchant_version, 0), NULLIF(transfer_id, '00000000-0000-0000-0000-000000000000')
		FROM unnest($1::bigint[], $2::text[], $3::bigint[], $4::uuid[]) AS f (merchant_id, rule, merchant_version, transfer_id)
		ON CONFLICT (merchant_id, rule, merchant_version, transfer_id) WHERE resolved_at IS NULL DO NOTHING
		RETURNING id, merchant_id, rule, merchant_version, transfer_id
	`
	merchants := make([]int64, len(fs))
	rules := make([]string, len(fs))
	versions := make([]int64, len(fs))
	transfers := make([]uuid.UUID, len(fs))
	for i, f := range fs {
		merchants[i], rules[i], versions[i], transfers[i] = int64(f.MerchantID), string(f.Rule), f.MerchantVersion, f.TransferID
	}
	var rows []struct {
		ID              int64         `db:"id"`
		MerchantID      int64         `db:"merchant_id"`
		Rule            string        `db:"rule"`
		MerchantVersion sql.NullInt64 `db:"merchant_version"`
		TransferID      uuid.NullUUID `db:"transfer_id"`
	}
	if err := l.tx.SelectContext(ctx, &rows, q, merchants, rules, versions, transfers); err != nil {
		return nil, fmt.Errorf("insert ledger findings: %w", err)
	}
	added := make([]transfer.LedgerFinding, len(rows))
	for i, r := range rows {
		added[i] = transfer.LedgerFinding{
			ID:              r.ID,
			MerchantID:      transfer.MerchantID(r.MerchantID),
			Rule:            transfer.LedgerRule(r.Rule),
			MerchantVersion: r.MerchantVersion.Int64,
			TransferID:      r.TransferID.UUID,
		}
	}
	return added, nil
}

// HoldPayouts implements transfer.LedgerCheckRepo.
// It locks every merchant, held or not: a resolve lifting the hold at the same moment has to finish first,
// and the hold is then set again. Rows are locked in ID order, so two checks can't deadlock.
func (l ledgerCheckRepo) HoldPayouts(ctx context.Context, merchantIDs []transfer.MerchantID) ([]transfer.MerchantID, error) {
	const q = `
		WITH locked AS (
			SELECT id, payouts_held_reason IS NULL AS newly_held
			FROM merchants
			WHERE id = ANY($1)
			ORDER BY id
			FOR UPDATE
		)
		UPDATE merchants m
		SET
			payouts_held_reason = COALESCE(m.payouts_held_reason, 'LEDGER_MISMATCH'),
			payouts_held_at = COALESCE(m.payouts_held_at, now()),
			updated_at = now()
		FROM locked
		WHERE m.id = locked.id
		RETURNING m.id, locked.newly_held
	`
	ids := make([]int64, len(merchantIDs))
	for i, id := range merchantIDs {
		ids[i] = int64(id)
	}
	var rows []struct {
		ID        transfer.MerchantID `db:"id"`
		NewlyHeld bool                `db:"newly_held"`
	}
	if err := l.tx.SelectContext(ctx, &rows, q, ids); err != nil {
		return nil, fmt.Errorf("hold merchant payouts: %w", err)
	}
	var held []transfer.MerchantID
	for _, r := range rows {
		if r.NewlyHeld {
			held = append(held, r.ID)
		}
	}
	return held, nil
}

// OpenFindings implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) OpenFindings(ctx context.Context, limit int) (transfer.OpenFindings, error) {
	const countQ = `
		SELECT count(*)
		FROM ledger_findings
		WHERE resolved_at IS NULL
	`
	var open transfer.OpenFindings
	if err := l.tx.GetContext(ctx, &open.Count, countQ); err != nil {
		return transfer.OpenFindings{}, fmt.Errorf("count open ledger findings: %w", err)
	}
	if open.Count == 0 {
		return open, nil
	}

	const merchantsQ = `
		SELECT DISTINCT merchant_id
		FROM ledger_findings
		WHERE resolved_at IS NULL
		ORDER BY merchant_id
		LIMIT $1
	`
	if err := l.tx.SelectContext(ctx, &open.MerchantIDs, merchantsQ, limit); err != nil {
		return transfer.OpenFindings{}, fmt.Errorf("select merchants with open ledger findings: %w", err)
	}
	const rulesQ = `
		SELECT DISTINCT rule
		FROM ledger_findings
		WHERE resolved_at IS NULL
		ORDER BY rule
	`
	if err := l.tx.SelectContext(ctx, &open.Rules, rulesQ); err != nil {
		return transfer.OpenFindings{}, fmt.Errorf("select rules of open ledger findings: %w", err)
	}
	return open, nil
}

// Resolve implements transfer.LedgerCheckRepo.
func (l ledgerCheckRepo) Resolve(ctx context.Context, id int64, by, note string) (transfer.MerchantID, error) {
	const q = `
		UPDATE ledger_findings
		SET
			resolved_at = now(),
			resolved_by = $2,
			resolution_note = $3
		WHERE id = $1
			AND resolved_at IS NULL
		RETURNING merchant_id
	`
	var merchantID transfer.MerchantID
	err := l.tx.QueryRowxContext(ctx, q, id, by, note).Scan(&merchantID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, l.whyNotResolvable(ctx, id)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve ledger finding: %w", err)
	}
	return merchantID, nil
}

func (l ledgerCheckRepo) whyNotResolvable(ctx context.Context, id int64) error {
	const q = `
		SELECT EXISTS (
			SELECT 1
			FROM ledger_findings
			WHERE id = $1
		)
	`
	var exists bool
	if err := l.tx.GetContext(ctx, &exists, q, id); err != nil {
		return fmt.Errorf("select ledger finding: %w", err)
	}
	if exists {
		return transfer.ErrFindingResolved
	}
	return transfer.ErrFindingNotFound
}

// ReleaseHold implements transfer.LedgerCheckRepo.
// A finding added by a check that hasn't committed yet isn't seen here, but that check's hold then waits
// for this row lock and puts the hold back.
func (l ledgerCheckRepo) ReleaseHold(ctx context.Context, merchantID transfer.MerchantID) (bool, error) {
	const releaseQ = `
		UPDATE merchants
		SET
			payouts_held_reason = NULL,
			payouts_held_at = NULL,
			updated_at = now()
		WHERE id = $1
			AND payouts_held_reason IS NOT NULL
			AND NOT EXISTS (
				SELECT 1
				FROM ledger_findings
				WHERE merchant_id = $1
					AND resolved_at IS NULL
			)
	`
	if _, err := l.tx.ExecContext(ctx, releaseQ, merchantID); err != nil {
		return false, fmt.Errorf("release merchant payouts: %w", err)
	}

	const heldQ = `
		SELECT payouts_held_reason IS NOT NULL
		FROM merchants
		WHERE id = $1
	`
	var held bool
	if err := l.tx.GetContext(ctx, &held, heldQ, merchantID); err != nil {
		return false, fmt.Errorf("select merchant hold: %w", err)
	}
	return held, nil
}
