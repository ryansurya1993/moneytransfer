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
	Merchants() BalanceDebiter
	Ledger() LedgerWriter
}

// TransferWriter stores new transfers.
type TransferWriter interface {
	// Create inserts t and fills in its timestamps.
	// It returns ErrDuplicateIdempotencyKey if the merchant already used the key.
	Create(ctx context.Context, t *Transfer) error
}

// OutboxWriter stores events to publish after the transaction commits.
type OutboxWriter interface {
	Add(ctx context.Context, e OutboxEvent) error
}

// BalanceDebiter takes money from a merchant balance.
type BalanceDebiter interface {
	// Debit atomically subtracts amount if the balance covers it and returns the new balance and version.
	// Otherwise it returns ErrInsufficientBalance.
	Debit(ctx context.Context, merchantID MerchantID, amount Amount) (BalanceChange, error)
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
