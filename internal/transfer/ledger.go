package transfer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// LedgerRule names a ledger rule the check found broken.
type LedgerRule string

// Rules the ledger check verifies.
const (
	// RuleVersionGap: an entry's merchant version doesn't follow the previous entry's.
	RuleVersionGap LedgerRule = "VERSION_GAP"
	// RuleBalanceChain: an entry's balance_after isn't the previous balance plus its amount.
	RuleBalanceChain LedgerRule = "BALANCE_CHAIN"
	// RuleMerchantBalance: the merchant's balance or version differs from its newest entry.
	RuleMerchantBalance LedgerRule = "MERCHANT_BALANCE"
	// RuleDebitAmount: a debit entry isn't exactly minus its transfer's amount.
	RuleDebitAmount LedgerRule = "DEBIT_AMOUNT"
	// RuleDebitMissing: a finished transfer has no debit entry.
	RuleDebitMissing LedgerRule = "DEBIT_MISSING"
	// RuleRefundAmount: a refund entry isn't exactly its transfer's amount.
	RuleRefundAmount LedgerRule = "REFUND_AMOUNT"
	// RuleRefundNotFailed: a refund entry belongs to a transfer that isn't FAILED.
	RuleRefundNotFailed LedgerRule = "REFUND_NOT_FAILED"
	// RuleRefundMissing: a FAILED transfer has no refund entry.
	RuleRefundMissing LedgerRule = "REFUND_MISSING"
)

// LedgerFinding is a broken ledger rule, kept until a person resolves it.
type LedgerFinding struct {
	ID              int64 // set once stored
	MerchantID      MerchantID
	Rule            LedgerRule
	MerchantVersion int64     // the entry or merchant version involved, 0 if none
	TransferID      uuid.UUID // uuid.Nil if no transfer is involved
}

// LedgerCheckpoint is the last ledger entry the check verified for a merchant.
type LedgerCheckpoint struct {
	MerchantID   MerchantID
	Version      int64
	BalanceAfter Amount
}

// FinishedTransfer is a SUCCESS or FAILED transfer and the ledger entries it has.
type FinishedTransfer struct {
	ID         uuid.UUID
	MerchantID MerchantID
	Status     Status
	HasDebit   bool
	HasRefund  bool
}

// OpenFindings summarizes the unresolved findings.
type OpenFindings struct {
	Count       int
	MerchantIDs []MerchantID
	Rules       []LedgerRule
}

const (
	ledgerCheckInterval = 30 * time.Second
	ledgerRoundTimeout  = 2 * time.Minute
	ledgerMerchantBatch = 1000
	ledgerEntryBatch    = 10000
	ledgerTransferBatch = 10000
	openFindingsListMax = 10
)

// LedgerChecker verifies new ledger entries and finished transfers against the ledger rules.
// A broken rule is stored as a finding and holds the merchant's payouts until a person resolves it.
// It reads only what changed since its last round, so its cost follows the traffic, not the table size.
type LedgerChecker struct {
	uow       UnitOfWork
	logger    *slog.Logger
	lastRound atomic.Int64 // unix nanoseconds
	lastAlert time.Time
}

// NewLedgerChecker returns a LedgerChecker.
func NewLedgerChecker(uow UnitOfWork, logger *slog.Logger) *LedgerChecker {
	c := &LedgerChecker{uow: uow, logger: logger}
	c.lastRound.Store(time.Now().UnixNano())
	return c
}

// LastRound returns when the last round finished, with or without an error.
func (c *LedgerChecker) LastRound() time.Time {
	return time.Unix(0, c.lastRound.Load())
}

// Run checks the ledger until ctx is canceled.
// While a round hits one of its limits the next starts at once; once caught up it waits 30 seconds.
// A failed round is logged and retried after the wait.
func (c *LedgerChecker) Run(ctx context.Context) error {
	for {
		busy := c.round(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if busy {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(ledgerCheckInterval):
		}
	}
}

func (c *LedgerChecker) round(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, ledgerRoundTimeout)
	defer cancel()

	busy, err := c.Check(ctx)
	if err != nil {
		c.logger.ErrorContext(ctx, "ledger check", "error", err)
	}
	if time.Since(c.lastAlert) >= ledgerCheckInterval {
		c.lastAlert = time.Now()
		if err := c.alertOpenFindings(ctx); err != nil {
			c.logger.ErrorContext(ctx, "read open ledger findings", "error", err)
		}
	}
	c.lastRound.Store(time.Now().UnixNano())
	return busy && err == nil
}

// Check runs one round: the merchants whose ledger changed, then the transfers that finished since the last round.
// It reports whether a limit cut the round short, so more is waiting.
// One merchant's error doesn't stop the others; the errors are returned together.
func (c *LedgerChecker) Check(ctx context.Context) (bool, error) {
	var due []MerchantID
	err := c.uow.Do(ctx, func(r TxRepos) error {
		var err error
		due, err = r.LedgerCheck().DueMerchants(ctx, ledgerMerchantBatch)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("find merchants to check: %w", err)
	}

	var errs []error
	var progressed, more bool
	for _, id := range due {
		if ctx.Err() != nil {
			return false, errors.Join(append(errs, ctx.Err())...)
		}
		read, err := c.checkMerchant(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("check ledger of merchant %d: %w", id, err))
		}
		progressed = progressed || read > 0
		more = more || read == ledgerEntryBatch
	}
	// A full list of merchants only means more work if it moved: a mismatch nobody has fixed yet keeps
	// its merchant due on every round and would otherwise keep the check spinning.
	busy := more || (len(due) == ledgerMerchantBatch && progressed)

	moreFinished, err := c.checkFinished(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("check finished transfers: %w", err))
	}
	return busy || moreFinished, errors.Join(errs...)
}

// checkMerchant checks the merchant's entries after its checkpoint and returns how many it read.
func (c *LedgerChecker) checkMerchant(ctx context.Context, id MerchantID) (int, error) {
	var read int
	var added []LedgerFinding
	var held []MerchantID
	err := c.uow.Do(ctx, func(r TxRepos) error {
		repo := r.LedgerCheck()
		cp, ok, err := repo.LockCheckpoint(ctx, id)
		if err != nil || !ok {
			return err
		}
		// Read the merchant first: its version is committed, so every entry up to it is committed too.
		balance, err := repo.Balance(ctx, id)
		if err != nil {
			return err
		}
		entries, err := repo.EntriesAfter(ctx, id, cp.Version, balance.Version, ledgerEntryBatch)
		if err != nil {
			return err
		}
		read = len(entries)
		ts, err := repo.Transfers(ctx, transferIDs(entries))
		if err != nil {
			return err
		}
		transfers := make(map[uuid.UUID]Transfer, len(ts))
		for _, t := range ts {
			transfers[t.ID] = t
		}

		findings, last := checkEntries(cp, entries, transfers)
		if read < ledgerEntryBatch && (last.Version != balance.Version || last.BalanceAfter != balance.Balance) {
			findings = append(findings, LedgerFinding{MerchantID: id, Rule: RuleMerchantBalance, MerchantVersion: balance.Version})
		}
		if added, held, err = record(ctx, repo, findings); err != nil {
			return err
		}
		// Saved even without new entries: the check time moves this merchant to the back of the queue.
		return repo.SaveCheckpoint(ctx, last)
	})
	if err != nil {
		return 0, err
	}
	c.logRecorded(ctx, added, held)
	return read, nil
}

// checkEntries checks entries that follow cp and returns the broken rules and the last entry as a checkpoint.
// After a break it carries on from the stored values, so one bad entry doesn't make every later one look wrong.
func checkEntries(cp LedgerCheckpoint, entries []LedgerEntry, transfers map[uuid.UUID]Transfer) ([]LedgerFinding, LedgerCheckpoint) {
	var findings []LedgerFinding
	found := func(rule LedgerRule, e LedgerEntry, transferID uuid.UUID) {
		findings = append(findings, LedgerFinding{
			MerchantID:      e.MerchantID,
			Rule:            rule,
			MerchantVersion: e.MerchantVersion,
			TransferID:      transferID,
		})
	}

	last := cp
	for _, e := range entries {
		switch {
		case e.MerchantVersion != last.Version+1:
			// The balance chain can't be checked across missing entries.
			found(RuleVersionGap, e, uuid.Nil)
		case e.BalanceAfter != last.BalanceAfter+e.Amount:
			found(RuleBalanceChain, e, uuid.Nil)
		}

		t := transfers[e.TransferID]
		switch e.Type {
		case EntryDebit:
			if e.Amount != -t.Amount {
				found(RuleDebitAmount, e, e.TransferID)
			}
		case EntryRefund:
			if t.Status != StatusFailed {
				found(RuleRefundNotFailed, e, e.TransferID)
			}
			if e.Amount != t.Amount {
				found(RuleRefundAmount, e, e.TransferID)
			}
		}
		last = LedgerCheckpoint{MerchantID: e.MerchantID, Version: e.MerchantVersion, BalanceAfter: e.BalanceAfter}
	}
	return findings, last
}

func (c *LedgerChecker) checkFinished(ctx context.Context) (bool, error) {
	var more bool
	var added []LedgerFinding
	var held []MerchantID
	err := c.uow.Do(ctx, func(r TxRepos) error {
		repo := r.LedgerCheck()
		ts, err := repo.UncheckedFinished(ctx, ledgerTransferBatch)
		if err != nil || len(ts) == 0 {
			return err
		}
		more = len(ts) == ledgerTransferBatch

		var findings []LedgerFinding
		ids := make([]uuid.UUID, len(ts))
		for i, t := range ts {
			ids[i] = t.ID
			if !t.HasDebit {
				findings = append(findings, LedgerFinding{MerchantID: t.MerchantID, Rule: RuleDebitMissing, TransferID: t.ID})
			}
			if t.Status == StatusFailed && !t.HasRefund {
				findings = append(findings, LedgerFinding{MerchantID: t.MerchantID, Rule: RuleRefundMissing, TransferID: t.ID})
			}
		}
		if added, held, err = record(ctx, repo, findings); err != nil {
			return err
		}
		return repo.MarkChecked(ctx, ids)
	})
	if err != nil {
		return false, err
	}
	c.logRecorded(ctx, added, held)
	return more, nil
}

// record stores new findings and holds the payouts of their merchants, in the caller's transaction.
func record(ctx context.Context, repo LedgerCheckRepo, findings []LedgerFinding) ([]LedgerFinding, []MerchantID, error) {
	if len(findings) == 0 {
		return nil, nil, nil
	}
	added, err := repo.AddFindings(ctx, findings)
	if err != nil || len(added) == 0 {
		return nil, nil, err
	}
	var merchants []MerchantID
	for _, f := range added {
		if !slices.Contains(merchants, f.MerchantID) {
			merchants = append(merchants, f.MerchantID)
		}
	}
	held, err := repo.HoldPayouts(ctx, merchants)
	if err != nil {
		return nil, nil, err
	}
	return added, held, nil
}

func (c *LedgerChecker) logRecorded(ctx context.Context, added []LedgerFinding, held []MerchantID) {
	for _, f := range added {
		attrs := []any{"finding_id", f.ID, "merchant_id", f.MerchantID, "rule", f.Rule}
		if f.MerchantVersion != 0 {
			attrs = append(attrs, "merchant_version", f.MerchantVersion)
		}
		if f.TransferID != uuid.Nil {
			attrs = append(attrs, "transfer_id", f.TransferID)
		}
		c.logger.ErrorContext(ctx, "ledger finding recorded", attrs...)
	}
	for _, m := range held {
		c.logger.InfoContext(ctx, "merchant payouts held until its ledger findings are resolved", "merchant_id", m)
	}
}

// alertOpenFindings logs one alert if findings are open.
// The checker calls it every 30 seconds, so a lost log line isn't the end of it.
// It carries no amounts: those stay in the database.
func (c *LedgerChecker) alertOpenFindings(ctx context.Context) error {
	var open OpenFindings
	err := c.uow.Do(ctx, func(r TxRepos) error {
		var err error
		open, err = r.LedgerCheck().OpenFindings(ctx, openFindingsListMax)
		return err
	})
	if err != nil || open.Count == 0 {
		return err
	}
	c.logger.ErrorContext(ctx, "ledger findings need a person",
		"alert", "ledger_mismatch",
		"severity", "critical",
		"open_findings", open.Count,
		"merchant_ids", open.MerchantIDs,
		"rules", open.Rules,
	)
	return nil
}

// ResolveLedgerFinding closes an open finding after a person has looked into it, recording who and why.
// The merchant's payouts resume once none of its findings is open; it reports whether they are still held.
// It returns ErrFindingNotFound or ErrFindingResolved if there is nothing to close.
func ResolveLedgerFinding(ctx context.Context, uow UnitOfWork, id int64, by, note string) (MerchantID, bool, error) {
	by, note = strings.TrimSpace(by), strings.TrimSpace(note)
	if by == "" || note == "" {
		return 0, false, errors.New("who resolves the finding and why are both required")
	}
	var merchantID MerchantID
	var held bool
	err := uow.Do(ctx, func(r TxRepos) error {
		var err error
		if merchantID, err = r.LedgerCheck().Resolve(ctx, id, by, note); err != nil {
			return err
		}
		held, err = r.LedgerCheck().ReleaseHold(ctx, merchantID)
		return err
	})
	if err != nil {
		return 0, false, fmt.Errorf("resolve ledger finding %d: %w", id, err)
	}
	return merchantID, held, nil
}

func transferIDs(entries []LedgerEntry) []uuid.UUID {
	var ids []uuid.UUID
	for _, e := range entries {
		if e.TransferID != uuid.Nil {
			ids = append(ids, e.TransferID)
		}
	}
	return ids
}
