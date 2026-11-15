package transfer_test

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

// state is the data the fake database holds.
type state struct {
	balance   transfer.Amount
	version   int64
	transfers map[uuid.UUID]transfer.Transfer
	outbox    []transfer.OutboxEvent
	ledger    []transfer.LedgerEntry
	outcomes  []transfer.Outcome
}

func (s state) clone() state {
	s.transfers = maps.Clone(s.transfers)
	s.outbox = slices.Clone(s.outbox)
	s.ledger = slices.Clone(s.ledger)
	s.outcomes = slices.Clone(s.outcomes)
	return s
}

// fakeDB is an in-memory database. Do keeps changes only if the callback and the commit succeed.
type fakeDB struct {
	committed state

	calls     []string // repository calls, in order, including rolled-back ones
	failAt    string   // repository call that returns failErr
	failErr   error
	failLeft  int   // when > 0, failAt fails only this many times
	commitErr error // returned instead of committing
	now       time.Time

	due        []transfer.ReconcileTarget // what MarkForReconcile returns
	markedWith []markArgs
	onMark     func(call int) // runs after each MarkForReconcile call

	ledgerCheck *fakeLedgerCheck // what LedgerCheck returns; it doesn't roll back
}

type markArgs struct {
	gap         transfer.ReconcileGap
	maxAttempts int
	limit       int
}

func newFakeDB(balance transfer.Amount) *fakeDB {
	return &fakeDB{
		committed: state{balance: balance, transfers: map[uuid.UUID]transfer.Transfer{}},
		now:       time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
	}
}

func (db *fakeDB) Do(_ context.Context, fn func(r transfer.TxRepos) error) error {
	tx := &fakeTx{db: db, s: db.committed.clone()}
	if err := fn(tx); err != nil {
		return err
	}
	if db.commitErr != nil {
		return db.commitErr
	}
	db.committed = tx.s
	return nil
}

func (db *fakeDB) ByID(_ context.Context, id uuid.UUID) (transfer.Transfer, error) {
	t, ok := db.committed.transfers[id]
	if !ok {
		return transfer.Transfer{}, transfer.ErrNotFound
	}
	return t, nil
}

func (db *fakeDB) ByIdempotencyKey(_ context.Context, m transfer.MerchantID, key string) (transfer.Transfer, error) {
	for _, t := range db.committed.transfers {
		if t.MerchantID == m && t.IdempotencyKey == key {
			return t, nil
		}
	}
	return transfer.Transfer{}, transfer.ErrNotFound
}

// fakeTx implements TxRepos and all transaction-bound repositories.
type fakeTx struct {
	db *fakeDB
	s  state
}

func (tx *fakeTx) Transfers() transfer.TransferWriter { return tx }
func (tx *fakeTx) Outbox() transfer.OutboxWriter      { return tx }
func (tx *fakeTx) Merchants() transfer.BalanceUpdater { return tx }
func (tx *fakeTx) Ledger() transfer.LedgerWriter      { return tx }
func (tx *fakeTx) LedgerCheck() transfer.LedgerCheckRepo {
	return tx.db.ledgerCheck
}

// call records the call and reports the injected failure, if any.
func (tx *fakeTx) call(name string) error {
	tx.db.calls = append(tx.db.calls, name)
	if tx.db.failAt == name {
		if tx.db.failLeft > 0 {
			tx.db.failLeft--
			if tx.db.failLeft == 0 {
				tx.db.failAt = ""
			}
		}
		return tx.db.failErr
	}
	return nil
}

func (tx *fakeTx) Create(_ context.Context, t *transfer.Transfer) error {
	if err := tx.call("transfers.Create"); err != nil {
		return err
	}
	for _, existing := range tx.s.transfers {
		if existing.MerchantID == t.MerchantID && existing.IdempotencyKey == t.IdempotencyKey {
			return transfer.ErrDuplicateIdempotencyKey
		}
	}
	t.CreatedAt, t.UpdatedAt = tx.db.now, tx.db.now
	tx.s.transfers[t.ID] = *t
	return nil
}

func (tx *fakeTx) Debit(_ context.Context, _ transfer.MerchantID, amount transfer.Amount) (transfer.BalanceChange, error) {
	if err := tx.call("merchants.Debit"); err != nil {
		return transfer.BalanceChange{}, err
	}
	if tx.s.balance < amount {
		return transfer.BalanceChange{}, transfer.ErrInsufficientBalance
	}
	tx.s.balance -= amount
	tx.s.version++
	return transfer.BalanceChange{Balance: tx.s.balance, Version: tx.s.version}, nil
}

func (tx *fakeTx) Append(_ context.Context, e transfer.LedgerEntry) error {
	if err := tx.call("ledger.Append"); err != nil {
		return err
	}
	tx.s.ledger = append(tx.s.ledger, e)
	return nil
}

func (tx *fakeTx) Add(_ context.Context, e transfer.OutboxEvent) error {
	if err := tx.call("outbox.Add"); err != nil {
		return err
	}
	tx.s.outbox = append(tx.s.outbox, e)
	return nil
}

func (tx *fakeTx) Claim(_ context.Context, id uuid.UUID) (transfer.Transfer, error) {
	if err := tx.call("transfers.Claim"); err != nil {
		return transfer.Transfer{}, err
	}
	t, ok := tx.s.transfers[id]
	switch {
	case !ok:
		return transfer.Transfer{}, transfer.ErrNotFound
	case t.Status != transfer.StatusPending && t.Status != transfer.StatusProcessing && t.Status != transfer.StatusUnknown:
		return transfer.Transfer{}, transfer.ErrAlreadyFinal
	}
	t.Status = transfer.StatusProcessing
	tx.s.transfers[id] = t
	return t, nil
}

func (tx *fakeTx) Finish(_ context.Context, id uuid.UUID, o transfer.Outcome) (bool, error) {
	if err := tx.call("transfers.Finish"); err != nil {
		return false, err
	}
	t := tx.s.transfers[id]
	if t.Status != transfer.StatusProcessing {
		return false, nil
	}
	t.Status, t.FailureCode = o.Status, o.FailureCode
	tx.s.transfers[id] = t
	tx.s.outcomes = append(tx.s.outcomes, o)
	return true, nil
}

func (tx *fakeTx) Credit(_ context.Context, _ transfer.MerchantID, amount transfer.Amount) (transfer.BalanceChange, error) {
	if err := tx.call("merchants.Credit"); err != nil {
		return transfer.BalanceChange{}, err
	}
	tx.s.balance += amount
	tx.s.version++
	return transfer.BalanceChange{Balance: tx.s.balance, Version: tx.s.version}, nil
}

// MarkForReconcile returns db.due; which transfers are due is the database adapter's query.
func (tx *fakeTx) MarkForReconcile(_ context.Context, gap transfer.ReconcileGap, maxAttempts, limit int) ([]transfer.ReconcileTarget, error) {
	if err := tx.call("transfers.MarkForReconcile"); err != nil {
		return nil, err
	}
	tx.db.markedWith = append(tx.db.markedWith, markArgs{gap, maxAttempts, limit})
	due := tx.db.due
	if tx.db.onMark != nil {
		tx.db.onMark(len(tx.db.markedWith))
	}
	return due, nil
}
