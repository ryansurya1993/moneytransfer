package transfer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// ReconcileTarget is a transfer the reconciler queued for another look.
type ReconcileTarget struct {
	ID            uuid.UUID
	CorrelationID string
	Status        Status // status when it was picked
	Attempt       int    // reconcile tries used, including this one
}

// Exhausted describes open transfers the reconciler has given up on.
type Exhausted struct {
	Count  int
	Waited time.Duration // since the last change of the transfer that has waited longest
	IDs    []uuid.UUID   // oldest first, at most the limit asked for
}

// ReconcileGap is how long an open transfer must go without a change before its next reconcile try.
// The gap starts at First and doubles after each try, up to Max.
type ReconcileGap struct {
	First time.Duration
	Max   time.Duration
}

// ReconcilerOptions sets when transfers are looked at again.
type ReconcilerOptions struct {
	// After is the gap before the first try.
	After time.Duration
	// MaxAttempts is how many times one transfer is queued before a person has to resolve it.
	MaxAttempts int
}

const (
	sweepInterval          = 30 * time.Second
	sweepBatch             = 100
	maxGap                 = time.Hour
	exhaustedCheckInterval = 5 * time.Minute
	exhaustedListMax       = 10
)

// Reconciler re-queues transfers whose processing stopped without an outcome:
// UNKNOWN ones, and PENDING or PROCESSING ones whose message was lost.
// The worker then asks the provider again with the same idempotency key.
type Reconciler struct {
	uow    UnitOfWork
	finder ExhaustedFinder
	logger *slog.Logger
	opts   ReconcilerOptions
}

// NewReconciler returns a Reconciler.
func NewReconciler(uow UnitOfWork, finder ExhaustedFinder, logger *slog.Logger, opts ReconcilerOptions) *Reconciler {
	return &Reconciler{uow: uow, finder: finder, logger: logger, opts: opts}
}

// Run sweeps every 30 seconds and checks for transfers out of tries every 5 minutes, until ctx is canceled.
// A sweep that fills its batch is followed by another right away, so a backlog after an outage drains
// at full speed. Failures are logged and retried on the next tick.
func (r *Reconciler) Run(ctx context.Context) error {
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	check := time.NewTicker(exhaustedCheckInterval)
	defer check.Stop()

	r.sweepAll(ctx)
	r.checkExhausted(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sweep.C:
			r.sweepAll(ctx)
		case <-check.C:
			r.checkExhausted(ctx)
		}
	}
}

func (r *Reconciler) sweepAll(ctx context.Context) {
	for ctx.Err() == nil {
		if r.sweep(ctx) < sweepBatch {
			return
		}
	}
}

func (r *Reconciler) sweep(ctx context.Context) int {
	// Shutdown may cancel a sweep halfway: it is one transaction, so none of it stays.
	ctx, cancel := context.WithTimeout(ctx, sweepInterval)
	defer cancel()
	n, err := r.Sweep(ctx)
	switch {
	case err != nil:
		r.logger.ErrorContext(ctx, "reconcile sweep", "error", err)
	case n > 0:
		r.logger.InfoContext(ctx, "queued transfers for reconciliation", "count", n)
	}
	return n
}

func (r *Reconciler) checkExhausted(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, sweepInterval)
	defer cancel()
	if err := r.CheckExhausted(ctx); err != nil {
		r.logger.ErrorContext(ctx, "check transfers out of reconcile tries", "error", err)
	}
}

// Sweep queues a transfer.reconcile event for each transfer that is due, oldest first.
// It counts a try for each.
// A transfer is due when it is PENDING, PROCESSING or UNKNOWN, has tries left, has no outbox event
// still waiting to be sent, and hasn't changed for its gap: opts.After before the first try,
// doubling after each try, up to an hour.
// It returns how many transfers it queued.
func (r *Reconciler) Sweep(ctx context.Context) (int, error) {
	var targets []ReconcileTarget
	err := r.uow.Do(ctx, func(tx TxRepos) error {
		var err error
		gap := ReconcileGap{First: r.opts.After, Max: maxGap}
		targets, err = tx.Transfers().MarkForReconcile(ctx, gap, r.opts.MaxAttempts, sweepBatch)
		if err != nil {
			return err
		}
		for _, t := range targets {
			e := OutboxEvent{TransferID: t.ID, Type: EventTransferReconcile, CorrelationID: t.CorrelationID}
			if err := tx.Outbox().Add(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("queue transfers for reconciliation: %w", err)
	}
	for _, t := range targets {
		r.logger.DebugContext(ctx, "queued transfer for reconciliation",
			"transfer_id", t.ID,
			"status", t.Status,
			"attempt", t.Attempt,
		)
	}
	return len(targets), nil
}

// CheckExhausted raises an alert while any open transfer has used all its reconcile tries
// and its last try has had opts.After to finish.
// Such a transfer holds the merchant's money with no outcome, so a person has to find out what happened.
func (r *Reconciler) CheckExhausted(ctx context.Context) error {
	ex, err := r.finder.Exhausted(ctx, r.opts.After, r.opts.MaxAttempts, exhaustedListMax)
	if err != nil {
		return fmt.Errorf("find transfers out of reconcile tries: %w", err)
	}
	if ex.Count == 0 {
		return nil
	}
	r.logger.ErrorContext(ctx, "transfers need manual resolution",
		"alert", "reconcile_exhausted",
		"severity", "high",
		"count", ex.Count,
		"oldest_age_seconds", int(ex.Waited.Seconds()),
		"transfer_ids", ex.IDs,
	)
	return nil
}
