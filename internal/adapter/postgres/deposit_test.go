//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

func TestDepositWritesALedgerEntryWithoutTransfer(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	ctx := context.Background()

	if _, err := newService(db).Submit(ctx, request("k1", 150_000)); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	balance, err := transfer.Deposit(ctx, postgres.NewStore(db), 1, 500_000)
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}

	if balance != 1_350_000 {
		t.Errorf("Deposit returned balance %d, want 1350000", balance)
	}
	var entry struct {
		TransferID sql.NullString `db:"transfer_id"`
		Amount     int64          `db:"amount"`
	}
	const q = `
		SELECT transfer_id, amount
		FROM balance_entries
		WHERE type = 'DEPOSIT'
	`
	if err := db.GetContext(ctx, &entry, q); err != nil {
		t.Fatalf("select deposit entry: %v", err)
	}
	if entry.TransferID.Valid || entry.Amount != 500_000 {
		t.Errorf("deposit entry has transfer %v and amount %d, want no transfer and 500000", entry.TransferID, entry.Amount)
	}
	assertBalance(t, db, 1_350_000, 3)
	assertLedgerConsistent(t, db)
}
