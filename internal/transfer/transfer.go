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

const (
	// StatusPending means the balance is deducted and the payout hasn't started.
	StatusPending Status = "PENDING"
	// StatusProcessing means a worker has claimed the transfer and is talking to the provider.
	StatusProcessing Status = "PROCESSING"
	// StatusSuccess means the provider paid out.
	StatusSuccess Status = "SUCCESS"
	// StatusFailed means the provider refused the payout and the amount was refunded.
	StatusFailed Status = "FAILED"
	// StatusUnknown means the provider never gave a definite answer.
	// The money may have left, so nothing is refunded until someone finds out.
	StatusUnknown Status = "UNKNOWN"
)

// FailureCode says why the provider refused a payout.
// A provider's own codes are mapped to these, so changing providers doesn't change the API.
type FailureCode string

// Failure codes a merchant can see.
const (
	FailureInvalidAccount  FailureCode = "INVALID_ACCOUNT"
	FailureAccountClosed   FailureCode = "ACCOUNT_CLOSED"
	FailureInvalidBankCode FailureCode = "INVALID_BANK_CODE"
	FailureRejectedByBank  FailureCode = "REJECTED_BY_BANK"
)

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
	FailureCode       FailureCode // set only when Status is StatusFailed
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

const (
	// EntryDebit records money taken from the balance for a transfer.
	EntryDebit EntryType = "DEBIT"
	// EntryRefund returns the amount of a failed transfer to the balance.
	EntryRefund EntryType = "REFUND"
	// EntryDeposit adds money from outside, such as a merchant top-up.
	EntryDeposit EntryType = "DEPOSIT"
)

// LedgerEntry records one balance change. Entries are never updated.
type LedgerEntry struct {
	MerchantID      MerchantID
	TransferID      uuid.UUID // uuid.Nil for a deposit
	Type            EntryType
	Amount          Amount // signed: negative for a debit, positive otherwise
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
