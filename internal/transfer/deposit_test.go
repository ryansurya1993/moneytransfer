package transfer_test

import (
	"errors"
	"testing"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

func TestDeposit(t *testing.T) {
	db := newFakeDB(1000)
	db.committed.version = 4

	balance, err := transfer.Deposit(t.Context(), db, 1, 500)
	if err != nil {
		t.Fatal(err)
	}

	if balance != 1500 || db.committed.balance != 1500 {
		t.Errorf("returned balance %d, stored %d; want 1500", balance, db.committed.balance)
	}
	want := transfer.LedgerEntry{MerchantID: 1, Type: transfer.EntryDeposit, Amount: 500, BalanceAfter: 1500, MerchantVersion: 5}
	if len(db.committed.ledger) != 1 || db.committed.ledger[0] != want {
		t.Errorf("ledger = %+v, want [%+v]", db.committed.ledger, want)
	}
}

func TestDepositRejectsNonPositiveAmount(t *testing.T) {
	for _, amount := range []transfer.Amount{0, -1} {
		db := newFakeDB(1000)

		if _, err := transfer.Deposit(t.Context(), db, 1, amount); err == nil {
			t.Errorf("Deposit(%d) succeeded, want an error", amount)
		}
		if len(db.calls) != 0 {
			t.Errorf("Deposit(%d) called %v, want no database calls", amount, db.calls)
		}
	}
}

func TestDepositRollsBackWhenTheLedgerFails(t *testing.T) {
	db := newFakeDB(1000)
	injected := errors.New("ledger down")
	db.failAt, db.failErr = "ledger.Append", injected

	_, err := transfer.Deposit(t.Context(), db, 1, 500)

	if !errors.Is(err, injected) {
		t.Errorf("Deposit = %v, want the ledger error", err)
	}
	if db.committed.balance != 1000 || len(db.committed.ledger) != 0 {
		t.Errorf("balance %d with %d ledger entries, want 1000 and none", db.committed.balance, len(db.committed.ledger))
	}
}
