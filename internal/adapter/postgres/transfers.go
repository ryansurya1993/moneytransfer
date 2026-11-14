package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const (
	uniqueViolation       = "23505"
	idempotencyConstraint = "transfers_merchant_idempotency_key_uniq"
)

type transferWriter txRepos

func (w transferWriter) Create(ctx context.Context, t *transfer.Transfer) error {
	const q = `
		INSERT INTO transfers (
			id, merchant_id, idempotency_key, request_hash, amount,
			currency, bank_code, account_number, account_holder_name, reference_id,
			description, status, correlation_id
		)
		VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, NULLIF($10, ''),
			NULLIF($11, ''), $12, $13
		)
		RETURNING created_at, updated_at
	`

	err := w.tx.QueryRowxContext(ctx, q,
		t.ID, t.MerchantID, t.IdempotencyKey, t.RequestHash, t.Amount, t.Currency, t.BankCode,
		t.AccountNumber, t.AccountHolderName, t.ReferenceID, t.Description, t.Status, t.CorrelationID,
	).Scan(&t.CreatedAt, &t.UpdatedAt)

	// Only this constraint means "already submitted". Any other unique violation is a real error.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == idempotencyConstraint {
		return transfer.ErrDuplicateIdempotencyKey
	}
	if err != nil {
		return fmt.Errorf("insert transfer: %w", err)
	}
	return nil
}

const transferColumns = `
	id, merchant_id, idempotency_key, request_hash, amount,
	currency, bank_code, account_number, account_holder_name, reference_id,
	description, status, correlation_id, created_at, updated_at
`

type transferRow struct {
	ID                uuid.UUID      `db:"id"`
	MerchantID        int64          `db:"merchant_id"`
	IdempotencyKey    string         `db:"idempotency_key"`
	RequestHash       string         `db:"request_hash"`
	Amount            int64          `db:"amount"`
	Currency          string         `db:"currency"`
	BankCode          string         `db:"bank_code"`
	AccountNumber     string         `db:"account_number"`
	AccountHolderName string         `db:"account_holder_name"`
	ReferenceID       sql.NullString `db:"reference_id"`
	Description       sql.NullString `db:"description"`
	Status            string         `db:"status"`
	CorrelationID     string         `db:"correlation_id"`
	CreatedAt         time.Time      `db:"created_at"`
	UpdatedAt         time.Time      `db:"updated_at"`
}

func (r transferRow) toDomain() transfer.Transfer {
	return transfer.Transfer{
		ID:                r.ID,
		MerchantID:        transfer.MerchantID(r.MerchantID),
		IdempotencyKey:    r.IdempotencyKey,
		RequestHash:       r.RequestHash,
		Amount:            transfer.Amount(r.Amount),
		Currency:          transfer.Currency(r.Currency),
		BankCode:          r.BankCode,
		AccountNumber:     r.AccountNumber,
		AccountHolderName: r.AccountHolderName,
		ReferenceID:       r.ReferenceID.String,
		Description:       r.Description.String,
		Status:            transfer.Status(r.Status),
		CorrelationID:     r.CorrelationID,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

// ByID implements transfer.TransferReader.
func (s *Store) ByID(ctx context.Context, id uuid.UUID) (transfer.Transfer, error) {
	const q = `
		SELECT ` + transferColumns + `
		FROM transfers
		WHERE id = $1
	`
	return s.getTransfer(ctx, q, id)
}

// ByIdempotencyKey implements transfer.TransferReader.
func (s *Store) ByIdempotencyKey(ctx context.Context, merchantID transfer.MerchantID, key string) (transfer.Transfer, error) {
	const q = `
		SELECT ` + transferColumns + `
		FROM transfers
		WHERE merchant_id = $1
			AND idempotency_key = $2
	`
	return s.getTransfer(ctx, q, merchantID, key)
}

func (s *Store) getTransfer(ctx context.Context, q string, args ...any) (transfer.Transfer, error) {
	var row transferRow
	err := s.db.GetContext(ctx, &row, q, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return transfer.Transfer{}, transfer.ErrNotFound
	}
	if err != nil {
		return transfer.Transfer{}, fmt.Errorf("select transfer: %w", wrapUnavailable(err))
	}
	return row.toDomain(), nil
}
