package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
)

// Event is an outbox row waiting to be published.
type Event struct {
	ID            int64
	TransferID    uuid.UUID
	Type          string
	Payload       []byte
	CorrelationID string
	CreatedAt     time.Time
}

// Store hands out unsent events.
type Store interface {
	// PublishBatch locks up to limit unsent events, oldest first, and calls publish for each in order.
	// It marks the events publish accepted as sent and stops at the first error.
	// It returns how many were marked, and the publish error if there was one.
	PublishBatch(ctx context.Context, limit int, publish func(context.Context, Event) error) (int, error)
	// Backlog returns the number of unsent events and when the oldest was created.
	Backlog(ctx context.Context) (count int, oldest time.Time, err error)
}

// Publisher sends an event to the broker.
type Publisher interface {
	// Publish returns nil only once the broker has confirmed it stored the event.
	Publish(ctx context.Context, e Event) error
}

// Options tunes the relay.
type Options struct {
	BatchSize      int
	PollInterval   time.Duration
	PublishTimeout time.Duration
	// AlertAge is how old the oldest unsent event may get before the relay raises an alert.
	AlertAge time.Duration
}

const backlogCheckInterval = 30 * time.Second

// Relay moves events from the store to the broker.
type Relay struct {
	store  Store
	pub    Publisher
	logger *slog.Logger
	opts   Options

	lastRound   atomic.Int64 // unix nanoseconds
	lastBacklog time.Time
}

// NewRelay returns a Relay that publishes from store through pub.
func NewRelay(store Store, pub Publisher, logger *slog.Logger, opts Options) *Relay {
	r := &Relay{store: store, pub: pub, logger: logger, opts: opts}
	r.lastRound.Store(time.Now().UnixNano())
	return r
}

// Run publishes events until ctx is canceled. A failed round is logged and retried on the next poll.
func (r *Relay) Run(ctx context.Context) error {
	for {
		// A round runs to its end even during shutdown.
		// Stopping halfway would roll back the marks for confirmed events, and they would be sent again.
		n, err := r.store.PublishBatch(context.WithoutCancel(ctx), r.opts.BatchSize, r.publish)
		r.lastRound.Store(time.Now().UnixNano())
		switch {
		case err != nil:
			r.logger.ErrorContext(ctx, "publish outbox events", "error", err, "published", n)
		case n > 0:
			r.logger.InfoContext(ctx, "published outbox events", "count", n)
		}
		r.checkBacklog(ctx)

		// A full batch means more are waiting: keep going without the pause.
		if err == nil && n == r.opts.BatchSize {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.opts.PollInterval):
		}
	}
}

// LastRound returns when the relay last finished a round, for a liveness check.
func (r *Relay) LastRound() time.Time {
	return time.Unix(0, r.lastRound.Load())
}

func (r *Relay) publish(ctx context.Context, e Event) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.PublishTimeout)
	defer cancel()
	if err := r.pub.Publish(ctx, e); err != nil {
		return fmt.Errorf("publish outbox event %d: %w", e.ID, err)
	}
	r.logger.DebugContext(logging.WithCorrelationID(ctx, e.CorrelationID), "published outbox event",
		"outbox_id", e.ID,
		"transfer_id", e.TransferID,
		"event_type", e.Type,
	)
	return nil
}

func (r *Relay) checkBacklog(ctx context.Context) {
	if time.Since(r.lastBacklog) < backlogCheckInterval {
		return
	}
	r.lastBacklog = time.Now()

	count, oldest, err := r.store.Backlog(ctx)
	if err != nil {
		r.logger.ErrorContext(ctx, "check outbox backlog", "error", err)
		return
	}
	if count > 0 && time.Since(oldest) > r.opts.AlertAge {
		r.logger.ErrorContext(ctx, "outbox events are not being published",
			"alert", "outbox_backlog",
			"severity", "high",
			"unsent", count,
			"oldest_age_seconds", int(time.Since(oldest).Seconds()),
		)
	}
}
