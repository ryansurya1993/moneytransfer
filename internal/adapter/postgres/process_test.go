//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type scriptedProvider struct {
	result transfer.PayoutResult
	err    error
	wait   func() // runs before answering
}

func (p scriptedProvider) Payout(context.Context, transfer.Transfer) (transfer.PayoutResult, error) {
	if p.wait != nil {
		p.wait()
	}
	return p.result, p.err
}

var (
	providerPaid    = scriptedProvider{result: transfer.PayoutResult{Succeeded: true, ProviderReference: "p-1"}}
	providerRefused = scriptedProvider{result: transfer.PayoutResult{ProviderReference: "p-1", FailureCode: transfer.FailureAccountClosed}}
	providerSilent  = scriptedProvider{err: errors.New("provider timed out")}
)

func newProcessor(db *sqlx.DB, p transfer.Provider) *transfer.Processor {
	return transfer.NewProcessor(postgres.NewStore(db), p, slog.New(slog.NewJSONHandler(io.Discard, nil)), transfer.ProcessorOptions{
		DBTimeout:        5 * time.Second,
		MaxAttempts:      3,
		BackoffBase:      time.Millisecond,
		BackoffMax:       time.Millisecond,
		StoreBackoffBase: time.Millisecond,
		StoreBackoffMax:  time.Millisecond,
	})
}

// submitOne creates a 30,000 transfer against a balance of 1,000,000.
func submitOne(t *testing.T) (*sqlx.DB, uuid.UUID) {
	t.Helper()
	db := pg.NewDB(t, 1_000_000)
	res, err := newService(db).Submit(t.Context(), request("k1", 30_000))
	if err != nil {
		t.Fatal(err)
	}
	return db, res.Transfer.ID
}

type transferState struct {
	Status            string         `db:"status"`
	FailureCode       sql.NullString `db:"failure_code"`
	ProviderReference sql.NullString `db:"provider_reference"`
	Attempts          int            `db:"attempts"`
	LastError         sql.NullString `db:"last_error"`
	Started           bool           `db:"started"`
	Completed         bool           `db:"completed"`
}

func loadState(t *testing.T, db *sqlx.DB, id uuid.UUID) transferState {
	t.Helper()
	const q = `
		SELECT status, failure_code, provider_reference, attempts, last_error,
			processing_started_at IS NOT NULL AS started,
			completed_at IS NOT NULL AS completed
		FROM transfers
		WHERE id = $1
	`
	var s transferState
	if err := db.GetContext(t.Context(), &s, q, id); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProcessRecordsTheOutcome(t *testing.T) {
	tests := []struct {
		name          string
		provider      scriptedProvider
		wantStatus    string
		wantCode      string
		wantAttempts  int
		wantCompleted bool
		wantBalance   int64
		wantVersion   int64
	}{
		{name: "paid", provider: providerPaid, wantStatus: "SUCCESS", wantAttempts: 1, wantCompleted: true, wantBalance: 970_000, wantVersion: 2},
		{name: "refused", provider: providerRefused, wantStatus: "FAILED", wantCode: "ACCOUNT_CLOSED", wantAttempts: 1, wantCompleted: true, wantBalance: 1_000_000, wantVersion: 3},
		{name: "no answer", provider: providerSilent, wantStatus: "UNKNOWN", wantAttempts: 3, wantCompleted: false, wantBalance: 970_000, wantVersion: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, id := submitOne(t)

			if err := newProcessor(db, tt.provider).Process(t.Context(), id); err != nil {
				t.Fatal(err)
			}

			s := loadState(t, db, id)
			if s.Status != tt.wantStatus || s.FailureCode.String != tt.wantCode || s.Attempts != tt.wantAttempts {
				t.Errorf("got %s %q after %d attempts, want %s %q after %d", s.Status, s.FailureCode.String, s.Attempts, tt.wantStatus, tt.wantCode, tt.wantAttempts)
			}
			if !s.Started || s.Completed != tt.wantCompleted {
				t.Errorf("started %v, completed %v; want started, completed %v", s.Started, s.Completed, tt.wantCompleted)
			}
			if tt.wantStatus == "UNKNOWN" && s.LastError.String != "provider timed out" {
				t.Errorf("last_error = %q, want the provider error", s.LastError.String)
			}
			if tt.wantStatus != "UNKNOWN" && s.ProviderReference.String != "p-1" {
				t.Errorf("provider_reference = %q, want p-1", s.ProviderReference.String)
			}
			assertBalance(t, db, tt.wantBalance, tt.wantVersion)
			assertLedgerConsistent(t, db)

			got, err := postgres.NewStore(db).ByID(t.Context(), id)
			if err != nil || string(got.FailureCode) != tt.wantCode {
				t.Errorf("ByID failure code = %q, %v; want %q", got.FailureCode, err, tt.wantCode)
			}
		})
	}
}

func TestProcessSkipsAFinishedTransfer(t *testing.T) {
	t.Parallel()
	db, id := submitOne(t)
	if err := newProcessor(db, providerRefused).Process(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	// A duplicate message for the same transfer must not pay or refund again.
	called := false
	again := scriptedProvider{result: providerPaid.result, wait: func() { called = true }}
	if err := newProcessor(db, again).Process(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	if called {
		t.Error("provider called for a transfer that already failed")
	}
	assertBalance(t, db, 1_000_000, 3)
	assertCount(t, db, "balance_entries WHERE type = 'REFUND'", 1)
}

func TestProcessUnknownTransferIsNotFound(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)

	err := newProcessor(db, providerPaid).Process(t.Context(), uuid.New())

	if !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("Process = %v, want ErrNotFound", err)
	}
}

// Two workers handle the same transfer at once, as with a duplicate message.
// Both get FAILED from the provider; only one may refund.
func TestConcurrentWorkersRefundOnce(t *testing.T) {
	t.Parallel()
	db, id := submitOne(t)

	var bothCalled sync.WaitGroup
	bothCalled.Add(2)
	p := scriptedProvider{result: providerRefused.result, wait: func() {
		bothCalled.Done()
		bothCalled.Wait() // both have claimed the transfer before either records the outcome
	}}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := newProcessor(db, p).Process(context.Background(), id); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	assertCount(t, db, "balance_entries WHERE type = 'REFUND'", 1)
	assertBalance(t, db, 1_000_000, 3)
	assertLedgerConsistent(t, db)
}

// A code path that forgets its guard still can't debit or refund a transfer twice.
func TestLedgerHoldsOneDebitAndOneRefundPerTransfer(t *testing.T) {
	for _, entry := range []transfer.EntryType{transfer.EntryDebit, transfer.EntryRefund} {
		t.Run(string(entry), func(t *testing.T) {
			t.Parallel()
			db, id := submitOne(t)
			if err := newProcessor(db, providerRefused).Process(t.Context(), id); err != nil {
				t.Fatal(err)
			}

			err := postgres.NewStore(db).Do(t.Context(), func(r transfer.TxRepos) error {
				change, err := r.Merchants().Credit(t.Context(), 1, 30_000)
				if err != nil {
					return err
				}
				amount := transfer.Amount(30_000)
				if entry == transfer.EntryDebit {
					amount = -amount
				}
				return r.Ledger().Append(t.Context(), transfer.LedgerEntry{
					MerchantID:      1,
					TransferID:      id,
					Type:            entry,
					Amount:          amount,
					BalanceAfter:    change.Balance,
					MerchantVersion: change.Version,
				})
			})

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.ConstraintName != "balance_entries_transfer_type_uniq" {
				t.Fatalf("err = %v, want the one-entry-per-type constraint", err)
			}
			assertBalance(t, db, 1_000_000, 3)
		})
	}
}
