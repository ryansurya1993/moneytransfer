package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Service implements the transfer use cases.
type Service struct {
	uow       UnitOfWork
	transfers TransferReader
	maxAmount Amount
}

// NewService returns a Service that rejects transfers above maxAmount.
func NewService(uow UnitOfWork, transfers TransferReader, maxAmount Amount) *Service {
	return &Service{uow: uow, transfers: transfers, maxAmount: maxAmount}
}

// SubmitResult is the result of an accepted submit.
type SubmitResult struct {
	Transfer Transfer
	// Replayed is true if the transfer already existed for this idempotency key.
	Replayed bool
}

// Submit validates req and stores the transfer, outbox event, debit and ledger entry in one transaction.
// A repeated request with the same idempotency key returns the original transfer with Replayed set.
// Rejected requests return a *ValidationError, ErrInsufficientBalance or ErrIdempotencyKeyReused.
func (s *Service) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if err := req.validate(s.maxAmount); err != nil {
		return SubmitResult{}, err
	}
	hash := req.hash()

	id, err := uuid.NewV7()
	if err != nil {
		return SubmitResult{}, fmt.Errorf("generate transfer id: %w", err)
	}
	t := Transfer{
		ID:                id,
		MerchantID:        req.MerchantID,
		IdempotencyKey:    req.IdempotencyKey,
		RequestHash:       hash,
		Amount:            req.Amount,
		Currency:          req.Currency,
		BankCode:          req.BankCode,
		AccountNumber:     req.AccountNumber,
		AccountHolderName: req.AccountHolderName,
		ReferenceID:       req.ReferenceID,
		Description:       req.Description,
		Status:            StatusPending,
		CorrelationID:     req.CorrelationID,
	}

	// Insert the transfer first: it claims the idempotency key.
	// Debit last: the merchant row stays locked until commit and every submit needs it.
	err = s.uow.Do(ctx, func(r TxRepos) error {
		if err := r.Transfers().Create(ctx, &t); err != nil {
			return err
		}
		event := OutboxEvent{TransferID: t.ID, Type: EventTransferCreated, CorrelationID: t.CorrelationID}
		if err := r.Outbox().Add(ctx, event); err != nil {
			return err
		}
		change, err := r.Merchants().Debit(ctx, t.MerchantID, t.Amount)
		if err != nil {
			return err
		}
		entry := LedgerEntry{
			MerchantID:      t.MerchantID,
			TransferID:      t.ID,
			Type:            EntryDebit,
			Amount:          -t.Amount,
			BalanceAfter:    change.Balance,
			MerchantVersion: change.Version,
		}
		return r.Ledger().Append(ctx, entry)
	})
	switch {
	case err == nil:
		return SubmitResult{Transfer: t}, nil
	case errors.Is(err, ErrDuplicateIdempotencyKey):
		return s.replay(ctx, req.MerchantID, req.IdempotencyKey, hash)
	default:
		return SubmitResult{}, err
	}
}

// replay returns the transfer that owns key, or ErrIdempotencyKeyReused if its request differs.
// A duplicate insert only fails once the original has committed, so the original is always found.
func (s *Service) replay(ctx context.Context, merchantID MerchantID, key, hash string) (SubmitResult, error) {
	t, err := s.transfers.ByIdempotencyKey(ctx, merchantID, key)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("load transfer for idempotency key: %w", err)
	}
	if t.RequestHash != hash {
		return SubmitResult{}, ErrIdempotencyKeyReused
	}
	return SubmitResult{Transfer: t, Replayed: true}, nil
}

// Get returns the transfer with the given ID, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Transfer, error) {
	return s.transfers.ByID(ctx, id)
}
