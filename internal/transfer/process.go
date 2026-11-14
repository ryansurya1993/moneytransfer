package transfer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
)

// Outcome is what a processing round found out about a transfer.
type Outcome struct {
	Status            Status // StatusSuccess, StatusFailed or StatusUnknown
	FailureCode       FailureCode
	ProviderReference string
	Attempts          int    // provider calls made in this round
	LastError         string // why the last call gave no definite answer
}

// ProcessorOptions sets the retry policy.
type ProcessorOptions struct {
	// DBTimeout bounds each database step.
	DBTimeout time.Duration
	// MaxAttempts is how many times the provider is asked before the transfer becomes UNKNOWN.
	MaxAttempts int
	// BackoffBase and BackoffMax shape the wait between provider attempts.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// StoreBackoffBase and StoreBackoffMax shape the wait while the database can't be reached.
	StoreBackoffBase time.Duration
	StoreBackoffMax  time.Duration
}

const maxLastErrorLen = 500

// Processor pays out claimed transfers through the provider.
type Processor struct {
	uow      UnitOfWork
	provider Provider
	logger   *slog.Logger
	opts     ProcessorOptions
}

// NewProcessor returns a Processor.
func NewProcessor(uow UnitOfWork, provider Provider, logger *slog.Logger, opts ProcessorOptions) *Processor {
	return &Processor{uow: uow, provider: provider, logger: logger, opts: opts}
}

// Process claims the transfer, asks the provider to pay it, and records the outcome.
// A failed payout is refunded in the same transaction that records it.
// It returns nil if the transfer already had an outcome, and ErrNotFound if it doesn't exist.
// If ctx ends first it returns the context's error; the transfer stays PROCESSING for a later claim.
func (p *Processor) Process(ctx context.Context, id uuid.UUID) error {
	var t Transfer
	err := p.retryUnavailable(ctx, func(ctx context.Context) error {
		return p.uow.Do(ctx, func(r TxRepos) error {
			var err error
			t, err = r.Transfers().Claim(ctx, id)
			return err
		})
	})
	if errors.Is(err, ErrAlreadyFinal) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim transfer: %w", err)
	}

	// No transaction is open here: a provider call can take seconds.
	o, err := p.payout(ctx, t)
	if err != nil {
		return err
	}

	var finished bool
	err = p.retryUnavailable(ctx, func(ctx context.Context) error {
		return p.uow.Do(ctx, func(r TxRepos) error {
			var err error
			if finished, err = r.Transfers().Finish(ctx, t.ID, o); err != nil || !finished {
				return err
			}
			if o.Status != StatusFailed {
				return nil
			}
			// The transfer row is locked before the merchant row, as in Submit, so they can't deadlock.
			refund, err := r.Merchants().Credit(ctx, t.MerchantID, t.Amount)
			if err != nil {
				return err
			}
			return r.Ledger().Append(ctx, LedgerEntry{
				MerchantID:      t.MerchantID,
				TransferID:      t.ID,
				Type:            EntryRefund,
				Amount:          t.Amount,
				BalanceAfter:    refund.Balance,
				MerchantVersion: refund.Version,
			})
		})
	})
	if err != nil {
		return fmt.Errorf("record outcome: %w", err)
	}
	if !finished {
		// Another worker recorded the outcome first; its refund, if any, is the only one.
		return nil
	}
	p.logOutcome(ctx, t, o)
	return nil
}

// payout asks the provider until it gives a definite answer or the attempts run out.
func (p *Processor) payout(ctx context.Context, t Transfer) (Outcome, error) {
	var lastErr error
	for attempt := 1; attempt <= p.opts.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, fullJitter(p.opts.BackoffBase, p.opts.BackoffMax, attempt-1)); err != nil {
				return Outcome{}, err
			}
		}
		res, err := p.provider.Payout(ctx, t)
		if err == nil && res.Succeeded {
			return Outcome{Status: StatusSuccess, ProviderReference: res.ProviderReference, Attempts: attempt}, nil
		}
		if err == nil {
			return Outcome{Status: StatusFailed, FailureCode: res.FailureCode, ProviderReference: res.ProviderReference, Attempts: attempt}, nil
		}
		if ctx.Err() != nil {
			return Outcome{}, ctx.Err()
		}
		lastErr = err
		p.logger.InfoContext(ctx, "payout attempt without a definite answer",
			"transfer_id", t.ID,
			"attempt", attempt,
			"error", err,
		)
	}
	msg := lastErr.Error()
	if len(msg) > maxLastErrorLen {
		msg = msg[:maxLastErrorLen]
	}
	return Outcome{Status: StatusUnknown, Attempts: p.opts.MaxAttempts, LastError: msg}, nil
}

func (p *Processor) logOutcome(ctx context.Context, t Transfer, o Outcome) {
	switch o.Status {
	case StatusSuccess:
		p.logger.InfoContext(ctx, "transfer paid out", "transfer_id", t.ID, "attempts", o.Attempts)
	case StatusFailed:
		p.logger.InfoContext(ctx, "transfer failed and refunded",
			"transfer_id", t.ID,
			"merchant_id", t.MerchantID,
			"failure_code", o.FailureCode,
			"amount", t.Amount,
		)
	case StatusUnknown:
		// One of these is normal; a spike means the provider is struggling.
		p.logger.WarnContext(ctx, "transfer outcome unknown, not refunded",
			"event", "transfer_unknown",
			"transfer_id", t.ID,
			"attempts", o.Attempts,
			"last_error", o.LastError,
		)
	}
}

// retryUnavailable runs a database step with its own timeout.
// While the database can't be reached it waits and tries again.
// An outage delays transfers but never fails them.
func (p *Processor) retryUnavailable(ctx context.Context, step func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		stepCtx, cancel := context.WithTimeout(ctx, p.opts.DBTimeout)
		err := step(stepCtx)
		cancel()
		transient := errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
		if !transient {
			return err
		}
		p.logger.WarnContext(ctx, "database unavailable, retrying", "attempt", attempt, "error", err)
		if err := sleep(ctx, fullJitter(p.opts.StoreBackoffBase, p.opts.StoreBackoffMax, attempt)); err != nil {
			return err
		}
	}
}

// fullJitter returns a random wait between 0 and min(maxWait, base * 2^(retry-1)).
func fullJitter(base, maxWait time.Duration, retry int) time.Duration {
	ceiling := maxWait
	if shift := retry - 1; shift < 30 && base<<shift < maxWait {
		ceiling = base << shift
	}
	if ceiling <= 0 {
		return 0
	}
	return rand.N(ceiling)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
