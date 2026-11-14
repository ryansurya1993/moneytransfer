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

// Claim implements transfer.TransferWriter.
// A PROCESSING transfer is claimed again, since its worker may have crashed, and so is an UNKNOWN one.
// The provider's idempotency key makes the second call safe.
func (w transferWriter) Claim(ctx context.Context, id uuid.UUID) (transfer.Transfer, error) {
	const q = `
		UPDATE transfers
		SET
			status = 'PROCESSING',
			processing_started_at = COALESCE(processing_started_at, now()),
			updated_at = now()
		WHERE id = $1
			AND status IN ('PENDING', 'PROCESSING', 'UNKNOWN')
		RETURNING ` + transferColumns

	var row transferRow
	err := w.tx.GetContext(ctx, &row, q, id)
	if errors.Is(err, sql.ErrNoRows) {
		return transfer.Transfer{}, w.whyNotClaimable(ctx, id)
	}
	if err != nil {
		return transfer.Transfer{}, fmt.Errorf("update transfer status: %w", err)
	}
	return row.toDomain(), nil
}

func (w transferWriter) whyNotClaimable(ctx context.Context, id uuid.UUID) error {
	const q = `
		SELECT EXISTS (
			SELECT 1
			FROM transfers
			WHERE id = $1
		)
	`
	var exists bool
	if err := w.tx.GetContext(ctx, &exists, q, id); err != nil {
		return fmt.Errorf("select transfer: %w", err)
	}
	if exists {
		return transfer.ErrAlreadyFinal
	}
	return transfer.ErrNotFound
}

// Finish implements transfer.TransferWriter.
func (w transferWriter) Finish(ctx context.Context, id uuid.UUID, o transfer.Outcome) (bool, error) {
	const q = `
		UPDATE transfers
		SET
			status = $2,
			failure_code = NULLIF($3, ''),
			provider_reference = NULLIF($4, ''),
			attempts = attempts + $5,
			last_error = NULLIF($6, ''),
			completed_at = CASE WHEN $7::boolean THEN now() END,
			updated_at = now()
		WHERE id = $1
			AND status = 'PROCESSING'
	`
	completed := o.Status == transfer.StatusSuccess || o.Status == transfer.StatusFailed
	res, err := w.tx.ExecContext(ctx, q, id, o.Status, o.FailureCode, o.ProviderReference, o.Attempts, o.LastError, completed)
	if err != nil {
		return false, fmt.Errorf("update transfer outcome: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update transfer outcome: %w", err)
	}
	return n == 1, nil
}

// MarkForReconcile implements transfer.TransferWriter.
func (w transferWriter) MarkForReconcile(ctx context.Context, gap transfer.ReconcileGap, maxAttempts, limit int) ([]transfer.ReconcileTarget, error) {
	// The database clock decides what is due, so relays on hosts with different clocks agree.
	// The first updated_at condition is the shortest gap; it lets the index skip recent rows.
	const q = `
		WITH due AS (
			SELECT id
			FROM transfers t
			WHERE status IN ('PENDING', 'PROCESSING', 'UNKNOWN')
				AND updated_at < now() - make_interval(secs => $1)
				AND updated_at < now() - make_interval(secs => least($1 * power(2, reconcile_attempts), $4))
				AND reconcile_attempts < $2
				AND NOT EXISTS (
					SELECT 1
					FROM outbox o
					WHERE o.transfer_id = t.id
						AND o.published_at IS NULL
				)
			ORDER BY updated_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE transfers t
		SET
			reconcile_attempts = t.reconcile_attempts + 1,
			updated_at = now()
		FROM due
		WHERE t.id = due.id
		RETURNING t.id, t.correlation_id, t.status, t.reconcile_attempts
	`
	var rows []struct {
		ID                uuid.UUID `db:"id"`
		CorrelationID     string    `db:"correlation_id"`
		Status            string    `db:"status"`
		ReconcileAttempts int       `db:"reconcile_attempts"`
	}
	if err := w.tx.SelectContext(ctx, &rows, q, gap.First.Seconds(), maxAttempts, limit, gap.Max.Seconds()); err != nil {
		return nil, fmt.Errorf("mark transfers for reconciliation: %w", err)
	}
	targets := make([]transfer.ReconcileTarget, len(rows))
	for i, r := range rows {
		targets[i] = transfer.ReconcileTarget{
			ID:            r.ID,
			CorrelationID: r.CorrelationID,
			Status:        transfer.Status(r.Status),
			Attempt:       r.ReconcileAttempts,
		}
	}
	return targets, nil
}

const transferColumns = `
	id, merchant_id, idempotency_key, request_hash, amount,
	currency, bank_code, account_number, account_holder_name, reference_id,
	description, status, failure_code, correlation_id, created_at,
	updated_at
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
	FailureCode       sql.NullString `db:"failure_code"`
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
		FailureCode:       transfer.FailureCode(r.FailureCode.String),
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

var _ transfer.ExhaustedFinder = (*Store)(nil)

// Exhausted implements transfer.ExhaustedFinder.
func (s *Store) Exhausted(ctx context.Context, olderThan time.Duration, maxAttempts, limit int) (transfer.Exhausted, error) {
	const q = `
		SELECT id, updated_at, count(*) OVER () AS total
		FROM transfers t
		WHERE status IN ('PENDING', 'PROCESSING', 'UNKNOWN')
			AND updated_at < now() - make_interval(secs => $1)
			AND reconcile_attempts >= $2
			AND NOT EXISTS (
				SELECT 1
				FROM outbox o
				WHERE o.transfer_id = t.id
					AND o.published_at IS NULL
			)
		ORDER BY updated_at
		LIMIT $3
	`
	var rows []struct {
		ID        uuid.UUID `db:"id"`
		UpdatedAt time.Time `db:"updated_at"`
		Total     int       `db:"total"`
	}
	if err := s.db.SelectContext(ctx, &rows, q, olderThan.Seconds(), maxAttempts, limit); err != nil {
		return transfer.Exhausted{}, fmt.Errorf("select transfers out of reconcile tries: %w", wrapUnavailable(err))
	}
	if len(rows) == 0 {
		return transfer.Exhausted{}, nil
	}
	ex := transfer.Exhausted{Count: rows[0].Total, WaitingSince: rows[0].UpdatedAt}
	for _, r := range rows {
		ex.IDs = append(ex.IDs, r.ID)
	}
	return ex, nil
}
