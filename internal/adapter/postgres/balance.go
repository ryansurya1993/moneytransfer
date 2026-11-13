package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type merchantRepo txRepos

// Debit checks and deducts in one statement. Its row lock serializes concurrent submits for the
// same merchant, which a separate SELECT followed by an UPDATE would not.
func (m merchantRepo) Debit(ctx context.Context, merchantID transfer.MerchantID, amount transfer.Amount) (transfer.BalanceChange, error) {
	const q = `
		UPDATE merchants
		SET
			balance = balance - $1,
			version = version + 1,
			updated_at = now()
		WHERE id = $2
			AND balance >= $1
		RETURNING balance, version
	`

	var balance, version int64
	err := m.tx.QueryRowxContext(ctx, q, amount, merchantID).Scan(&balance, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return transfer.BalanceChange{}, transfer.ErrInsufficientBalance
	}
	if err != nil {
		return transfer.BalanceChange{}, fmt.Errorf("debit merchant balance: %w", err)
	}
	return transfer.BalanceChange{Balance: transfer.Amount(balance), Version: version}, nil
}

type ledgerWriter txRepos

func (l ledgerWriter) Append(ctx context.Context, e transfer.LedgerEntry) error {
	const q = `
		INSERT INTO balance_entries (merchant_id, transfer_id, type, amount, balance_after, merchant_version)
		VALUES ($1, $2, $3, $4, $5, $6)
	`

	_, err := l.tx.ExecContext(ctx, q, e.MerchantID, e.TransferID, e.Type, e.Amount, e.BalanceAfter, e.MerchantVersion)
	if err != nil {
		return fmt.Errorf("insert ledger entry: %w", err)
	}
	return nil
}
