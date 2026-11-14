package transfer

import (
	"context"

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
}

// TransferWriter stores transfers and their status changes.
type TransferWriter interface {
	// Create inserts t and fills in its timestamps.
	// It returns ErrDuplicateIdempotencyKey if the merchant already used the key.
	Create(ctx context.Context, t *Transfer) error
	// Claim moves a PENDING or PROCESSING transfer to PROCESSING and returns it.
	// It returns ErrAlreadyFinal if the transfer has an outcome, and ErrNotFound if it doesn't exist.
	Claim(ctx context.Context, id uuid.UUID) (Transfer, error)
	// Finish records the outcome of a PROCESSING transfer.
	// It returns false, and changes nothing, if the transfer is no longer PROCESSING.
	Finish(ctx context.Context, id uuid.UUID, o Outcome) (bool, error)
}

// OutboxWriter stores events to publish after the transaction commits.
type OutboxWriter interface {
	Add(ctx context.Context, e OutboxEvent) error
}

// BalanceUpdater changes merchant balances.
type BalanceUpdater interface {
	// Debit atomically subtracts amount if the balance covers it and returns the new balance and version.
	// Otherwise it returns ErrInsufficientBalance.
	Debit(ctx context.Context, merchantID MerchantID, amount Amount) (BalanceChange, error)
	// Credit adds amount and returns the new balance and version.
	Credit(ctx context.Context, merchantID MerchantID, amount Amount) (BalanceChange, error)
}

// LedgerWriter appends ledger entries.
type LedgerWriter interface {
	Append(ctx context.Context, e LedgerEntry) error
}

// TransferReader reads committed transfers.
type TransferReader interface {
	// ByID returns ErrNotFound if no transfer has the ID.
	ByID(ctx context.Context, id uuid.UUID) (Transfer, error)
	// ByIdempotencyKey returns ErrNotFound if the merchant never used the key.
	ByIdempotencyKey(ctx context.Context, merchantID MerchantID, key string) (Transfer, error)
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
