package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

// Processor handles the transfer a message points to.
type Processor interface {
	Process(ctx context.Context, transferID uuid.UUID) error
}

// ConsumeOptions tunes the consumer.
type ConsumeOptions struct {
	// Concurrency is how many messages are handled at once; it is also the prefetch count.
	Concurrency int
	// ShutdownTimeout is how long messages in progress may keep running after ctx is canceled.
	ShutdownTimeout time.Duration
}

type message struct {
	TransferID    uuid.UUID          `json:"transfer_id"`
	CorrelationID string             `json:"correlation_id"`
	EventType     transfer.EventType `json:"event_type"`
}

// ConsumeTransfers hands messages from the transfers queue to p until ctx is canceled.
// Then it waits for the messages in progress.
// It returns an error if the broker stops delivering first.
//
// A message is acked once p is done.
// When retrying can't help, it is rejected without requeue and goes to the dead-letter queue.
// Other failures are rejected with requeue, which counts toward the queue's delivery limit,
// so a message that always fails is dead-lettered instead of looping.
// Messages interrupted by shutdown are nacked, which doesn't count.
func (c *Conn) ConsumeTransfers(ctx context.Context, p Processor, opts ConsumeOptions, logger *slog.Logger) error {
	ch, err := c.conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer func() { _ = ch.Close() }()
	c.watch(ch.NotifyClose(make(chan *amqp.Error, 1)))

	if err := ch.Qos(opts.Concurrency, 0, false); err != nil {
		return fmt.Errorf("set prefetch: %w", err)
	}
	const consumerTag = "worker"
	deliveries, err := ch.Consume(transfersQueue, consumerTag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", transfersQueue, err)
	}

	// Messages in progress get ShutdownTimeout to finish after ctx is canceled.
	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()
	stopConsuming := context.AfterFunc(ctx, func() {
		_ = ch.Cancel(consumerTag, false)
		time.AfterFunc(opts.ShutdownTimeout, stopWork)
	})
	defer stopConsuming()

	var wg sync.WaitGroup
	for range opts.Concurrency {
		wg.Go(func() {
			for d := range deliveries {
				if ctx.Err() != nil {
					// Prefetched but not started: hand it back without counting a failure.
					_ = d.Nack(false, true)
					continue
				}
				handle(workCtx, p, d, logger)
			}
		})
	}
	wg.Wait()

	if ctx.Err() == nil {
		return errors.New("rabbitmq stopped delivering messages")
	}
	return nil
}

func handle(ctx context.Context, p Processor, d amqp.Delivery, logger *slog.Logger) {
	var m message
	err := json.Unmarshal(d.Body, &m)
	known := m.EventType == transfer.EventTransferCreated || m.EventType == transfer.EventTransferReconcile
	if err != nil || m.TransferID == uuid.Nil || !known {
		logger.WarnContext(ctx, "message dead-lettered: not a transfer event",
			"message_id", d.MessageId,
			"event_type", m.EventType,
		)
		_ = d.Reject(false)
		return
	}
	ctx = logging.WithCorrelationID(ctx, m.CorrelationID)
	if m.EventType == transfer.EventTransferReconcile {
		logger.InfoContext(ctx, "asking the provider again about a transfer", "transfer_id", m.TransferID)
	}

	err = p.Process(ctx, m.TransferID)
	switch {
	case err == nil:
		_ = d.Ack(false)
	case errors.Is(err, transfer.ErrNotFound):
		logger.ErrorContext(ctx, "message dead-lettered: transfer not found", "transfer_id", m.TransferID)
		_ = d.Reject(false)
	case ctx.Err() != nil:
		logger.InfoContext(ctx, "message handed back at shutdown", "transfer_id", m.TransferID)
		_ = d.Nack(false, true)
	default:
		logger.ErrorContext(ctx, "process transfer", "transfer_id", m.TransferID, "error", err)
		_ = d.Reject(true)
	}
}
