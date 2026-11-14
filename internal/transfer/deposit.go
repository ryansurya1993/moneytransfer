package transfer

import (
	"context"
	"errors"
	"fmt"
)

// Deposit adds amount to the merchant's balance and records a DEPOSIT ledger entry in one transaction.
// It returns the new balance.
func Deposit(ctx context.Context, uow UnitOfWork, merchantID MerchantID, amount Amount) (Amount, error) {
	if amount <= 0 {
		return 0, errors.New("deposit amount must be greater than 0")
	}
	var balance Amount
	err := uow.Do(ctx, func(r TxRepos) error {
		change, err := r.Merchants().Credit(ctx, merchantID, amount)
		if err != nil {
			return err
		}
		balance = change.Balance
		return r.Ledger().Append(ctx, LedgerEntry{
			MerchantID:      merchantID,
			Type:            EntryDeposit,
			Amount:          amount,
			BalanceAfter:    change.Balance,
			MerchantVersion: change.Version,
		})
	})
	if err != nil {
		return 0, fmt.Errorf("deposit: %w", err)
	}
	return balance, nil
}
