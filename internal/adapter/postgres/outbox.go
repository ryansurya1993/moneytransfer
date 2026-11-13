package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

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
