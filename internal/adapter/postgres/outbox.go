package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/outbox"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type outboxPayload struct {
	TransferID    uuid.UUID `json:"transfer_id"`
	CorrelationID string    `json:"correlation_id"`
	EventType     string    `json:"event_type"`
}

type outboxWriter txRepos

func (o outboxWriter) Add(ctx context.Context, e transfer.OutboxEvent) error {
	payload, err := json.Marshal(outboxPayload{
		TransferID:    e.TransferID,
		CorrelationID: e.CorrelationID,
		EventType:     string(e.Type),
	})
	if err != nil {
		return fmt.Errorf("encode outbox payload: %w", err)
	}

	const q = `
		INSERT INTO outbox (transfer_id, event_type, payload, correlation_id)
		VALUES ($1, $2, $3, $4)
	`
	if _, err := o.tx.ExecContext(ctx, q, e.TransferID, e.Type, payload, e.CorrelationID); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

var _ outbox.Store = (*Store)(nil)

type outboxRow struct {
	ID            int64     `db:"id"`
	TransferID    uuid.UUID `db:"transfer_id"`
	EventType     string    `db:"event_type"`
	Payload       []byte    `db:"payload"`
	CorrelationID string    `db:"correlation_id"`
	CreatedAt     time.Time `db:"created_at"`
}

// PublishBatch implements outbox.Store.
// The rows stay locked until commit, so a second relay can't publish them too.
// SKIP LOCKED makes that relay take the next rows instead of waiting.
func (s *Store) PublishBatch(ctx context.Context, limit int, publish func(context.Context, outbox.Event) error) (_ int, err error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", wrapUnavailable(err))
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// A canceled context has already rolled the transaction back.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	const selectQ = `
		SELECT id, transfer_id, event_type, payload, correlation_id,
			created_at
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`
	var rows []outboxRow
	if err := tx.SelectContext(ctx, &rows, selectQ, limit); err != nil {
		return 0, fmt.Errorf("select unsent outbox events: %w", err)
	}

	var sent []int64
	var publishErr error
	for _, row := range rows {
		e := outbox.Event{
			ID:            row.ID,
			TransferID:    row.TransferID,
			Type:          row.EventType,
			Payload:       row.Payload,
			CorrelationID: row.CorrelationID,
			CreatedAt:     row.CreatedAt,
		}
		if publishErr = publish(ctx, e); publishErr != nil {
			break
		}
		sent = append(sent, row.ID)
	}

	// Keep the events the broker already confirmed, even when a later one failed.
	if len(sent) > 0 {
		const markQ = `
			UPDATE outbox
			SET published_at = now()
			WHERE id = ANY($1)
		`
		if _, err := tx.ExecContext(ctx, markQ, sent); err != nil {
			return 0, fmt.Errorf("mark outbox events published: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return len(sent), publishErr
}

// Backlog implements outbox.Store.
func (s *Store) Backlog(ctx context.Context) (int, time.Time, error) {
	const q = `
		SELECT count(*), min(created_at)
		FROM outbox
		WHERE published_at IS NULL
	`
	var count int
	var oldest sql.NullTime
	if err := s.db.QueryRowContext(ctx, q).Scan(&count, &oldest); err != nil {
		return 0, time.Time{}, fmt.Errorf("select outbox backlog: %w", wrapUnavailable(err))
	}
	return count, oldest.Time, nil
}
