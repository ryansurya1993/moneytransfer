package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type merchantRepo txRepos

// Debit checks and deducts in one statement. Its row lock serializes concurrent submits for the
// same merchant, which a separate SELECT followed by an UPDATE would not.
// The hold is part of the same condition, so no debit slips in after a hold commits.
func (m merchantRepo) Debit(ctx context.Context, merchantID transfer.MerchantID, amount transfer.Amount) (transfer.BalanceChange, error) {
	const q = `
		UPDATE merchants
		SET
			balance = balance - $1,
			version = version + 1,
			updated_at = now()
		WHERE id = $2
			AND balance >= $1
			AND payouts_held_reason IS NULL
		RETURNING balance, version
	`

	var balance, version int64
	err := m.tx.QueryRowxContext(ctx, q, amount, merchantID).Scan(&balance, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return transfer.BalanceChange{}, m.whyNoDebit(ctx, merchantID)
	}
	if err != nil {
		return transfer.BalanceChange{}, fmt.Errorf("debit merchant balance: %w", err)
	}
	return transfer.BalanceChange{Balance: transfer.Amount(balance), Version: version}, nil
}

func (m merchantRepo) whyNoDebit(ctx context.Context, merchantID transfer.MerchantID) error {
	const q = `
		SELECT EXISTS (
			SELECT 1
			FROM merchants
			WHERE id = $1
				AND payouts_held_reason IS NOT NULL
		)
	`
	var held bool
	if err := m.tx.GetContext(ctx, &held, q, merchantID); err != nil {
		return fmt.Errorf("select merchant hold: %w", err)
	}
	if held {
		return transfer.ErrPayoutsHeld
	}
	return transfer.ErrInsufficientBalance
}

// Credit has no balance condition: a refund only returns money this transfer took earlier.
// A hold doesn't stop it either: money coming back is often what corrects the balance.
func (m merchantRepo) Credit(ctx context.Context, merchantID transfer.MerchantID, amount transfer.Amount) (transfer.BalanceChange, error) {
	const q = `
		UPDATE merchants
		SET
			balance = balance + $1,
			version = version + 1,
			updated_at = now()
		WHERE id = $2
		RETURNING balance, version
	`

	var balance, version int64
	if err := m.tx.QueryRowxContext(ctx, q, amount, merchantID).Scan(&balance, &version); err != nil {
		return transfer.BalanceChange{}, fmt.Errorf("credit merchant balance: %w", err)
	}
	return transfer.BalanceChange{Balance: transfer.Amount(balance), Version: version}, nil
}

type ledgerWriter txRepos

func (l ledgerWriter) Append(ctx context.Context, e transfer.LedgerEntry) error {
	const q = `
		INSERT INTO balance_entries (merchant_id, transfer_id, type, amount, balance_after, merchant_version)
		VALUES ($1, $2, $3, $4, $5, $6)
	`

	transferID := uuid.NullUUID{UUID: e.TransferID, Valid: e.TransferID != uuid.Nil}
	_, err := l.tx.ExecContext(ctx, q, e.MerchantID, transferID, e.Type, e.Amount, e.BalanceAfter, e.MerchantVersion)
	if err != nil {
		return fmt.Errorf("insert ledger entry: %w", err)
	}
	return nil
}
