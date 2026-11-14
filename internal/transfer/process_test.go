package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const (
	startBalance transfer.Amount = 1_000_000
	payoutAmount transfer.Amount = 150_000
)

var transferID = uuid.MustParse("0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f")

// newProcessDB holds one transfer in status, with its amount already taken from the balance.
func newProcessDB(status transfer.Status) *fakeDB {
	db := newFakeDB(startBalance - payoutAmount)
	db.committed.version = 2
	db.committed.transfers[transferID] = transfer.Transfer{
		ID:         transferID,
		MerchantID: 1,
		Amount:     payoutAmount,
		Status:     status,
	}
	return db
}

type payoutReply struct {
	res transfer.PayoutResult
	err error
}

type fakeProvider struct {
	replies []payoutReply // one per call; the last one repeats
	calls   int
	onCall  func()
}

func (p *fakeProvider) Payout(context.Context, transfer.Transfer) (transfer.PayoutResult, error) {
	if p.onCall != nil {
		p.onCall()
	}
	r := p.replies[min(p.calls, len(p.replies)-1)]
	p.calls++
	return r.res, r.err
}

var (
	paid     = payoutReply{res: transfer.PayoutResult{Succeeded: true, ProviderReference: "ref-1"}}
	refused  = payoutReply{res: transfer.PayoutResult{ProviderReference: "ref-1", FailureCode: transfer.FailureInvalidAccount}}
	noAnswer = payoutReply{err: errors.New("provider timed out")}
)

var fastRetries = transfer.ProcessorOptions{
	DBTimeout:        time.Second,
	MaxAttempts:      3,
	BackoffBase:      time.Millisecond,
	BackoffMax:       4 * time.Millisecond,
	StoreBackoffBase: time.Millisecond,
	StoreBackoffMax:  4 * time.Millisecond,
}

func newProcessor(db *fakeDB, p transfer.Provider) *transfer.Processor {
	return transfer.NewProcessor(db, p, slog.New(slog.NewJSONHandler(io.Discard, nil)), fastRetries)
}

func TestProcessOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		start        transfer.Status
		replies      []payoutReply
		wantStatus   transfer.Status
		wantCode     transfer.FailureCode
		wantAttempts int
		wantBalance  transfer.Amount
		wantRefund   bool
	}{
		{name: "paid", start: transfer.StatusPending, replies: []payoutReply{paid}, wantStatus: transfer.StatusSuccess, wantAttempts: 1, wantBalance: startBalance - payoutAmount},
		{name: "refused and refunded", start: transfer.StatusPending, replies: []payoutReply{refused}, wantStatus: transfer.StatusFailed, wantCode: transfer.FailureInvalidAccount, wantAttempts: 1, wantBalance: startBalance, wantRefund: true},
		{name: "paid on the third try", start: transfer.StatusPending, replies: []payoutReply{noAnswer, noAnswer, paid}, wantStatus: transfer.StatusSuccess, wantAttempts: 3, wantBalance: startBalance - payoutAmount},
		{name: "no answer at all is not refunded", start: transfer.StatusPending, replies: []payoutReply{noAnswer}, wantStatus: transfer.StatusUnknown, wantAttempts: 3, wantBalance: startBalance - payoutAmount},
		{name: "processing is claimed again", start: transfer.StatusProcessing, replies: []payoutReply{paid}, wantStatus: transfer.StatusSuccess, wantAttempts: 1, wantBalance: startBalance - payoutAmount},
		{name: "unknown is asked again", start: transfer.StatusUnknown, replies: []payoutReply{paid}, wantStatus: transfer.StatusSuccess, wantAttempts: 1, wantBalance: startBalance - payoutAmount},
		{name: "unknown refused on a later look is refunded", start: transfer.StatusUnknown, replies: []payoutReply{refused}, wantStatus: transfer.StatusFailed, wantCode: transfer.FailureInvalidAccount, wantAttempts: 1, wantBalance: startBalance, wantRefund: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newProcessDB(tt.start)
			provider := &fakeProvider{replies: tt.replies}

			if err := newProcessor(db, provider).Process(t.Context(), transferID); err != nil {
				t.Fatalf("Process: %v", err)
			}

			got := db.committed.transfers[transferID]
			if got.Status != tt.wantStatus || got.FailureCode != tt.wantCode {
				t.Errorf("status %s code %q, want %s %q", got.Status, got.FailureCode, tt.wantStatus, tt.wantCode)
			}
			if provider.calls != tt.wantAttempts {
				t.Errorf("provider calls = %d, want %d", provider.calls, tt.wantAttempts)
			}
			if len(db.committed.outcomes) != 1 || db.committed.outcomes[0].Attempts != tt.wantAttempts {
				t.Errorf("recorded outcomes %+v, want one with %d attempts", db.committed.outcomes, tt.wantAttempts)
			}
			if db.committed.balance != tt.wantBalance {
				t.Errorf("balance = %d, want %d", db.committed.balance, tt.wantBalance)
			}
			refunds := 0
			for _, e := range db.committed.ledger {
				if e.Type == transfer.EntryRefund {
					refunds++
					if e.Amount != payoutAmount || e.BalanceAfter != startBalance || e.MerchantVersion != 3 || e.TransferID != transferID {
						t.Errorf("refund entry %+v, want +%d, balance after %d, version 3", e, payoutAmount, startBalance)
					}
				}
			}
			if wantRefunds := map[bool]int{true: 1, false: 0}[tt.wantRefund]; refunds != wantRefunds {
				t.Errorf("refund entries = %d, want %d", refunds, wantRefunds)
			}
		})
	}
}

func TestProcessUnknownKeepsTheLastError(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)

	if err := newProcessor(db, &fakeProvider{replies: []payoutReply{noAnswer}}).Process(t.Context(), transferID); err != nil {
		t.Fatal(err)
	}

	if got := db.committed.outcomes[0].LastError; got != "provider timed out" {
		t.Errorf("last error = %q, want the provider error", got)
	}
}

func TestProcessRefundLocksTransferBeforeMerchant(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)

	if err := newProcessor(db, &fakeProvider{replies: []payoutReply{refused}}).Process(t.Context(), transferID); err != nil {
		t.Fatal(err)
	}

	want := []string{"transfers.Claim", "transfers.Finish", "merchants.Credit", "ledger.Append"}
	if !slices.Equal(db.calls, want) {
		t.Errorf("calls = %v, want %v", db.calls, want)
	}
}

func TestProcessSkipsTransfersWithAnOutcome(t *testing.T) {
	for _, status := range []transfer.Status{transfer.StatusSuccess, transfer.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			db := newProcessDB(status)
			provider := &fakeProvider{replies: []payoutReply{paid}}

			if err := newProcessor(db, provider).Process(t.Context(), transferID); err != nil {
				t.Errorf("Process = %v, want nil so the message is acked", err)
			}
			if provider.calls != 0 {
				t.Errorf("provider called %d times for a %s transfer", provider.calls, status)
			}
		})
	}
}

func TestProcessUnknownTransfer(t *testing.T) {
	err := newProcessor(newFakeDB(0), &fakeProvider{replies: []payoutReply{paid}}).Process(t.Context(), transferID)

	if !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("Process = %v, want ErrNotFound", err)
	}
}

// Two workers on one transfer: the one that records second must not refund again.
func TestProcessLosingTheRaceDoesNotRefund(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)
	provider := &fakeProvider{replies: []payoutReply{refused}}
	provider.onCall = func() {
		other := db.committed.transfers[transferID]
		other.Status = transfer.StatusFailed
		db.committed.transfers[transferID] = other
	}

	if err := newProcessor(db, provider).Process(t.Context(), transferID); err != nil {
		t.Fatal(err)
	}

	if db.committed.balance != startBalance-payoutAmount || len(db.committed.ledger) != 0 {
		t.Errorf("balance %d, ledger %v: the losing worker refunded", db.committed.balance, db.committed.ledger)
	}
}

func TestProcessWaitsOutAnUnreachableDatabase(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)
	db.failAt, db.failErr, db.failLeft = "transfers.Finish", transfer.ErrUnavailable, 3
	provider := &fakeProvider{replies: []payoutReply{paid}}

	if err := newProcessor(db, provider).Process(t.Context(), transferID); err != nil {
		t.Fatal(err)
	}

	if got := db.committed.transfers[transferID].Status; got != transfer.StatusSuccess {
		t.Errorf("status = %s, want SUCCESS once the database is back", got)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1: only the failed database step is retried", provider.calls)
	}
}

func TestProcessReturnsOtherDatabaseErrors(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)
	broken := errors.New("constraint violated")
	db.failAt, db.failErr = "merchants.Credit", broken

	err := newProcessor(db, &fakeProvider{replies: []payoutReply{refused}}).Process(t.Context(), transferID)

	if !errors.Is(err, broken) {
		t.Errorf("Process = %v, want the database error", err)
	}
	// The failed refund rolled back the status change too: nothing half done.
	if got := db.committed.transfers[transferID].Status; got != transfer.StatusProcessing {
		t.Errorf("status = %s, want PROCESSING", got)
	}
}

func TestProcessStopsOnShutdown(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)
	ctx, cancel := context.WithCancel(t.Context())
	provider := &fakeProvider{replies: []payoutReply{noAnswer}, onCall: cancel}

	err := newProcessor(db, provider).Process(ctx, transferID)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Process = %v, want context.Canceled", err)
	}
	if got := db.committed.transfers[transferID].Status; got != transfer.StatusProcessing {
		t.Errorf("status = %s, want PROCESSING so it can be claimed again", got)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want no retry after shutdown", provider.calls)
	}
}

func TestProcessTruncatesLongErrors(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)
	long := payoutReply{err: errors.New(strings.Repeat("x", 2000))}

	if err := newProcessor(db, &fakeProvider{replies: []payoutReply{long}}).Process(t.Context(), transferID); err != nil {
		t.Fatal(err)
	}

	if got := len(db.committed.outcomes[0].LastError); got != 500 {
		t.Errorf("last error length = %d, want 500", got)
	}
}

func TestRefundLogLeavesOutTheBalance(t *testing.T) {
	db := newProcessDB(transfer.StatusPending)
	var logs bytes.Buffer
	p := transfer.NewProcessor(db, &fakeProvider{replies: []payoutReply{refused}}, slog.New(slog.NewJSONHandler(&logs, nil)), fastRetries)

	if err := p.Process(t.Context(), transferID); err != nil {
		t.Fatal(err)
	}

	line := logs.String()
	for _, want := range []string{`"msg":"transfer failed and refunded"`, `"failure_code":"INVALID_ACCOUNT"`, `"amount":150000`} {
		if !strings.Contains(line, want) {
			t.Errorf("refund log %s doesn't contain %s", line, want)
		}
	}
	if strings.Contains(line, "balance") {
		t.Errorf("refund log %s contains the merchant balance; the ledger holds it", line)
	}
}
