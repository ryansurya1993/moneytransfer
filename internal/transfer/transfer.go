package transfer

import (
	"time"

	"github.com/google/uuid"
)

// Amount is money in the currency's smallest unit (whole rupiah for IDR).
type Amount int64

// Currency is an ISO 4217 currency code.
type Currency string

// IDR is the only supported currency.
const IDR Currency = "IDR"

// MerchantID identifies a merchant.
type MerchantID int64

// Status is the lifecycle state of a transfer.
type Status string

// StatusPending means the balance is deducted and the payout hasn't started.
const StatusPending Status = "PENDING"

// Transfer is a payout from a merchant's balance to a bank account.
type Transfer struct {
	ID                uuid.UUID
	MerchantID        MerchantID
	IdempotencyKey    string
	RequestHash       string
	Amount            Amount
	Currency          Currency
	BankCode          string
	AccountNumber     string
	AccountHolderName string
	ReferenceID       string // merchant's own reference, optional, not unique
	Description       string // optional
	Status            Status
	CorrelationID     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// BalanceChange is the merchant balance and version after a change.
// The version goes up by one per change, so gaps in the ledger are detectable.
type BalanceChange struct {
	Balance Amount
	Version int64
}

// EntryType is the kind of ledger entry.
type EntryType string

// EntryDebit records money taken from the balance for a transfer.
const EntryDebit EntryType = "DEBIT"

// LedgerEntry records one balance change. Entries are never updated.
type LedgerEntry struct {
	MerchantID      MerchantID
	TransferID      uuid.UUID
	Type            EntryType
	Amount          Amount // signed: negative for a debit
	BalanceAfter    Amount
	MerchantVersion int64
}

// EventType names an outbox event.
type EventType string

// EventTransferCreated announces a new transfer to be processed.
const EventTransferCreated EventType = "transfer.created"

// OutboxEvent is a message published after its transaction commits.
// It holds IDs only; consumers load the details.
type OutboxEvent struct {
	TransferID    uuid.UUID
	Type          EventType
	CorrelationID string
}
