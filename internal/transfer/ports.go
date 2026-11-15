package transfer

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// UnitOfWork groups repository calls into one database transaction.
type UnitOfWork interface {
	// Do runs fn in a transaction. It commits if fn returns nil and rolls back otherwise, including on panic.
	// A failed commit is returned as an error. The repositories passed to fn are only valid inside it.
	Do(ctx context.Context, fn func(r TxRepos) error) error
}

// TxRepos are the repositories bound to one transaction.
type TxRepos interface {
	Transfers() TransferWriter
	Outbox() OutboxWriter
	Merchants() BalanceUpdater
	Ledger() LedgerWriter
	LedgerCheck() LedgerCheckRepo
}

// TransferWriter stores transfers and their status changes.
type TransferWriter interface {
	// Create inserts t and fills in its timestamps.
	// It returns ErrDuplicateIdempotencyKey if the merchant already used the key.
	Create(ctx context.Context, t *Transfer) error
	// Claim moves a PENDING, PROCESSING or UNKNOWN transfer to PROCESSING and returns it.
	// It returns ErrAlreadyFinal if the transfer has an outcome, ErrNotFound if it doesn't exist,
	// and ErrPayoutsHeld if it is still PENDING and its merchant's payouts are on hold.
	Claim(ctx context.Context, id uuid.UUID) (Transfer, error)
	// Finish records the outcome of a PROCESSING transfer.
	// It returns false, and changes nothing, if the transfer is no longer PROCESSING.
	Finish(ctx context.Context, id uuid.UUID, o Outcome) (bool, error)
	// MarkForReconcile picks up to limit open transfers, oldest first, that haven't changed for their gap,
	// have used fewer than maxAttempts reconcile tries and have no unsent outbox event.
	// It counts a try for each and keeps them locked until the transaction ends,
	// so a concurrent caller picks other transfers.
	MarkForReconcile(ctx context.Context, gap ReconcileGap, maxAttempts, limit int) ([]ReconcileTarget, error)
}

// OutboxWriter stores events to publish after the transaction commits.
type OutboxWriter interface {
	Add(ctx context.Context, e OutboxEvent) error
}

// BalanceUpdater changes merchant balances.
type BalanceUpdater interface {
	// Debit atomically subtracts amount if the balance covers it and returns the new balance and version.
	// Otherwise it returns ErrInsufficientBalance, or ErrPayoutsHeld if the merchant's payouts are on hold.
	Debit(ctx context.Context, merchantID MerchantID, amount Amount) (BalanceChange, error)
	// Credit adds amount and returns the new balance and version.
	Credit(ctx context.Context, merchantID MerchantID, amount Amount) (BalanceChange, error)
}

// LedgerWriter appends ledger entries.
type LedgerWriter interface {
	Append(ctx context.Context, e LedgerEntry) error
}

// LedgerCheckRepo reads what the ledger check verifies and stores what it finds.
type LedgerCheckRepo interface {
	// DueMerchants returns up to limit merchants whose balance or version differs from their checkpoint,
	// least recently checked first. A merchant without a checkpoint counts as checked up to version 0.
	DueMerchants(ctx context.Context, limit int) ([]MerchantID, error)
	// LockCheckpoint returns the merchant's checkpoint, creating an empty one first if needed,
	// and keeps it locked until the transaction ends. It returns false if another transaction holds it.
	LockCheckpoint(ctx context.Context, merchantID MerchantID) (LedgerCheckpoint, bool, error)
	// Balance returns the merchant's current balance and version.
	Balance(ctx context.Context, merchantID MerchantID) (BalanceChange, error)
	// EntriesAfter returns up to limit of the merchant's entries with a version above after and at most upTo,
	// in version order.
	EntriesAfter(ctx context.Context, merchantID MerchantID, after, upTo int64, limit int) ([]LedgerEntry, error)
	// Transfers returns the transfers with the given IDs.
	Transfers(ctx context.Context, ids []uuid.UUID) ([]Transfer, error)
	SaveCheckpoint(ctx context.Context, cp LedgerCheckpoint) error
	// UncheckedFinished returns up to limit SUCCESS or FAILED transfers the check hasn't looked at yet,
	// locked so a concurrent check picks others.
	UncheckedFinished(ctx context.Context, limit int) ([]FinishedTransfer, error)
	MarkChecked(ctx context.Context, ids []uuid.UUID) error
	// AddFindings stores the findings that weren't stored before and returns those, with their IDs.
	AddFindings(ctx context.Context, fs []LedgerFinding) ([]LedgerFinding, error)
	// HoldPayouts puts the merchants' payouts on hold and returns the ones that weren't held already.
	HoldPayouts(ctx context.Context, merchantIDs []MerchantID) ([]MerchantID, error)
	// OpenFindings summarizes the findings nobody has resolved, listing at most limit merchants.
	OpenFindings(ctx context.Context, limit int) (OpenFindings, error)
	// Resolve closes an open finding and returns its merchant.
	// It returns ErrFindingNotFound or ErrFindingResolved if there is nothing to close.
	Resolve(ctx context.Context, id int64, by, note string) (MerchantID, error)
	// ReleaseHold lifts the merchant's hold if none of its findings is open
	// and reports whether its payouts are still held.
	ReleaseHold(ctx context.Context, merchantID MerchantID) (bool, error)
}

// TransferReader reads committed transfers.
type TransferReader interface {
	// ByID returns ErrNotFound if no transfer has the ID.
	ByID(ctx context.Context, id uuid.UUID) (Transfer, error)
	// ByIdempotencyKey returns ErrNotFound if the merchant never used the key.
	ByIdempotencyKey(ctx context.Context, merchantID MerchantID, key string) (Transfer, error)
}

// ExhaustedFinder finds transfers the reconciler has given up on.
type ExhaustedFinder interface {
	// Exhausted returns the open transfers that haven't changed for olderThan, have no unsent outbox
	// event and have used maxAttempts reconcile tries, listing at most limit IDs.
	Exhausted(ctx context.Context, olderThan time.Duration, maxAttempts, limit int) (Exhausted, error)
}

// Provider pays transfers out to bank accounts.
type Provider interface {
	// Payout asks the provider to pay t, using t.ID as the idempotency key.
	// Repeating the call for the same transfer never pays twice.
	// An error means the outcome is unknown: the payout may or may not have happened.
	Payout(ctx context.Context, t Transfer) (PayoutResult, error)
}

// PayoutResult is a definite answer from the provider.
type PayoutResult struct {
	Succeeded         bool
	ProviderReference string
	FailureCode       FailureCode // set when Succeeded is false
}
