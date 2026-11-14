//go:build integration

package loadtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/outbox"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const runRef = "lt-run1"

// The provider's answer is chosen by amount.
const (
	paidAmount    transfer.Amount = 1_000
	refusedAmount transfer.Amount = 2_000
	silentAmount  transfer.Amount = 3_000
)

// payoutRecorder answers like a provider and keeps each outcome by key, as the mock's /stats does.
type payoutRecorder struct {
	payouts map[string]string
}

func (p *payoutRecorder) Payout(_ context.Context, t transfer.Transfer) (transfer.PayoutResult, error) {
	switch t.Amount {
	case paidAmount:
		p.payouts[t.ID.String()] = "SUCCEEDED"
		return transfer.PayoutResult{Succeeded: true, ProviderReference: "p-" + t.ID.String()}, nil
	case refusedAmount:
		p.payouts[t.ID.String()] = "FAILED"
		return transfer.PayoutResult{ProviderReference: "p-" + t.ID.String(), FailureCode: transfer.FailureAccountClosed}, nil
	}
	return transfer.PayoutResult{}, errors.New("provider timed out")
}

func newService(db *sqlx.DB) (*transfer.Service, *postgres.Store) {
	pgStore := postgres.NewStore(db)
	return transfer.NewService(pgStore, pgStore, 1_000_000), pgStore
}

func submitRequest(key, ref string, amount transfer.Amount) transfer.SubmitRequest {
	return transfer.SubmitRequest{
		MerchantID:        1,
		IdempotencyKey:    key,
		CorrelationID:     "corr-" + key,
		Amount:            amount,
		Currency:          transfer.IDR,
		BankCode:          "BCA",
		AccountNumber:     "0123456789",
		AccountHolderName: "Budi Santoso",
		ReferenceID:       ref,
	}
}

// scenario is a run of three transfers (paid, refused, no answer) written by the service's own code
// between two snapshots, with a transfer and a deposit from outside the run in the same window.
type scenario struct {
	db         *sqlx.DB
	st         store
	start, end snapshot
	payouts    map[string]string
	// ids are the run's transfers in the order paid, refused, no answer.
	ids []uuid.UUID
}

// newScenario builds the scenario; corrupt, when not nil, damages it before the end snapshot.
func newScenario(t *testing.T, corrupt func(t *testing.T, s *scenario)) *scenario {
	t.Helper()
	ctx := t.Context()
	db := pg.NewDB(t, 1_000_000)
	svc, pgStore := newService(db)
	rec := &payoutRecorder{payouts: map[string]string{}}
	proc := transfer.NewProcessor(pgStore, rec, slog.New(slog.NewTextHandler(io.Discard, nil)), transfer.ProcessorOptions{
		DBTimeout:        5 * time.Second,
		MaxAttempts:      1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       time.Millisecond,
		StoreBackoffBase: time.Millisecond,
		StoreBackoffMax:  time.Millisecond,
	})
	deposit := func(amount transfer.Amount) {
		if _, err := transfer.Deposit(ctx, pgStore, 1, amount); err != nil {
			t.Fatal(err)
		}
	}
	submitAndProcess := func(key, ref string, amount transfer.Amount) uuid.UUID {
		res, err := svc.Submit(ctx, submitRequest(key, ref, amount))
		if err != nil {
			t.Fatal(err)
		}
		if err := proc.Process(ctx, res.Transfer.ID); err != nil {
			t.Fatal(err)
		}
		return res.Transfer.ID
	}

	s := &scenario{db: db, st: store{db: db, merchantID: 1}, payouts: rec.payouts}
	deposit(500) // before the run, so outside the window
	var err error
	if s.start, err = s.st.snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	s.ids = []uuid.UUID{
		submitAndProcess("run-paid", runRef, paidAmount),
		submitAndProcess("run-refused", runRef, refusedAmount),
		submitAndProcess("run-silent", runRef, silentAmount),
	}
	submitAndProcess("outside", "INV-9", paidAmount)
	deposit(700)
	publishAll(t, pgStore)
	if corrupt != nil {
		corrupt(t, s)
	}
	if s.end, err = s.st.snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	deposit(900) // after the run, so outside the window
	return s
}

// publishAll marks every outbox event published through the relay's own code.
func publishAll(t *testing.T, pgStore *postgres.Store) {
	t.Helper()
	for {
		n, err := pgStore.PublishBatch(t.Context(), 100, func(context.Context, outbox.Event) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// failingChecks reads the run back with the load test's queries and returns the names of the checks that fail.
func (s *scenario) failingChecks(t *testing.T) []string {
	t.Helper()
	w, err := s.st.ledger(t.Context(), s.start.Version, s.end.Version, runRef)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := s.st.runTransfers(t.Context(), runRef)
	if err != nil {
		t.Fatal(err)
	}
	var failing []string
	for _, c := range []check{
		checkLedger(s.start, s.end, w),
		checkTransferEntries(ts),
		checkProvider(ts, s.payouts),
		checkEventsPublished(ts),
	} {
		if !c.passed {
			failing = append(failing, c.name)
		}
	}
	return failing
}

func TestChecksPassOnServiceData(t *testing.T) {
	t.Parallel()
	s := newScenario(t, nil)
	ctx := t.Context()

	if failing := s.failingChecks(t); len(failing) > 0 {
		t.Errorf("failing checks %v, want none", failing)
	}

	w, err := s.st.ledger(ctx, s.start.Version, s.end.Version, runRef)
	if err != nil {
		t.Fatal(err)
	}
	// The run: 3 debits and 1 refund. Outside it: 1 debit and the deposit of 700.
	// The deposits of 500 and 900 were made before and after the snapshots.
	wantSum := int64(-paidAmount - refusedAmount - silentAmount + refusedAmount - paidAmount + 700)
	if w.Entries != 6 || w.Other != 2 || w.Sum != wantSum {
		t.Errorf("window: %d entries, %d other, sum %d; want 6, 2, %d", w.Entries, w.Other, w.Sum, wantSum)
	}

	ts, err := s.st.runTransfers(ctx, runRef)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tr := range ts {
		got[tr.ID] = tr.Status
	}
	want := map[string]string{s.ids[0].String(): "SUCCESS", s.ids[1].String(): "FAILED", s.ids[2].String(): "UNKNOWN"}
	if len(got) != len(want) {
		t.Fatalf("run transfers %v, want %v", got, want)
	}
	for id, status := range want {
		if got[id] != status {
			t.Errorf("transfer %s is %q, want %q", id, got[id], status)
		}
	}

	inFlight, unpublished, err := s.st.unsettled(ctx, runRef)
	if err != nil {
		t.Fatal(err)
	}
	if inFlight != 0 || unpublished != 0 {
		t.Errorf("unsettled: %d in flight, %d unpublished; want 0, 0", inFlight, unpublished)
	}
}

// Each kind of damage must fail exactly the checks meant to see it, and no others.
func TestChecksCatchBrokenData(t *testing.T) {
	const (
		ledger   = "ledger explains the balance"
		entries  = "each transfer debited once, refunded only if FAILED"
		provider = "provider agrees with every outcome"
		events   = "every transfer's event published"
	)
	exec := func(t *testing.T, s *scenario, q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name    string
		corrupt func(t *testing.T, s *scenario)
		want    []string
	}{
		{
			name: "balance changed without a ledger entry",
			corrupt: func(t *testing.T, s *scenario) {
				const q = `
					UPDATE merchants
					SET
						balance = balance - 5,
						version = version + 1
					WHERE id = 1
				`
				exec(t, s, q)
			},
			want: []string{ledger},
		},
		{
			name: "refund paid twice",
			corrupt: func(t *testing.T, s *scenario) {
				const q = `
					WITH m AS (
						UPDATE merchants
						SET
							balance = balance + $2,
							version = version + 1
						WHERE id = 1
						RETURNING balance, version
					)
					INSERT INTO balance_entries (merchant_id, transfer_id, type, amount, balance_after, merchant_version)
					SELECT 1, $1, 'REFUND', $2, balance, version
					FROM m
				`
				exec(t, s, q, s.ids[1], refusedAmount)
			},
			want: []string{entries},
		},
		{
			name: "refund entry missing",
			corrupt: func(t *testing.T, s *scenario) {
				const q = `
					DELETE FROM balance_entries
					WHERE transfer_id = $1
						AND type = 'REFUND'
				`
				exec(t, s, q, s.ids[1])
			},
			want: []string{ledger, entries},
		},
		{
			name: "debit of another amount",
			corrupt: func(t *testing.T, s *scenario) {
				const q = `
					UPDATE balance_entries
					SET amount = amount - 1
					WHERE transfer_id = $1
						AND type = 'DEBIT'
				`
				exec(t, s, q, s.ids[0])
			},
			want: []string{ledger, entries},
		},
		{
			name: "provider paid a refunded transfer",
			corrupt: func(_ *testing.T, s *scenario) {
				s.payouts[s.ids[1].String()] = "SUCCEEDED"
			},
			want: []string{provider},
		},
		{
			name: "second transfer.created event",
			corrupt: func(t *testing.T, s *scenario) {
				const q = `
					INSERT INTO outbox (transfer_id, event_type, payload, correlation_id, published_at)
					VALUES ($1, 'transfer.created', '{}', 'corr', now())
				`
				exec(t, s, q, s.ids[0])
			},
			want: []string{events},
		},
		{
			name: "event never published",
			corrupt: func(t *testing.T, s *scenario) {
				const q = `
					UPDATE outbox
					SET published_at = NULL
					WHERE transfer_id = $1
				`
				exec(t, s, q, s.ids[2])
			},
			want: []string{events},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScenario(t, tt.corrupt)
			if failing := s.failingChecks(t); !slices.Equal(failing, tt.want) {
				t.Errorf("failing checks %v, want %v", failing, tt.want)
			}
		})
	}
}

// The run is found by its exact reference: look-alike references belong to other transfers.
func TestRunTransfersMatchTheReferenceExactly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := pg.NewDB(t, 1_000_000)
	svc, _ := newService(db)
	st := store{db: db, merchantID: 1}

	start, err := st.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var runID uuid.UUID
	for i, ref := range []string{runRef, "lt-run10", "lt-run_", ""} {
		res, err := svc.Submit(ctx, submitRequest(string(rune('a'+i)), ref, paidAmount))
		if err != nil {
			t.Fatal(err)
		}
		if ref == runRef {
			runID = res.Transfer.ID
		}
	}
	end, err := st.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ts, err := st.runTransfers(ctx, runRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].ID != runID.String() {
		t.Errorf("run transfers %+v, want only %s", ts, runID)
	}
	w, err := st.ledger(ctx, start.Version, end.Version, runRef)
	if err != nil {
		t.Fatal(err)
	}
	if w.Other != 3 {
		t.Errorf("other entries %d, want 3", w.Other)
	}
	inFlight, unpublished, err := st.unsettled(ctx, runRef)
	if err != nil {
		t.Fatal(err)
	}
	if inFlight != 1 || unpublished != 1 {
		t.Errorf("unsettled: %d in flight, %d unpublished; want 1, 1 (the run's PENDING transfer)", inFlight, unpublished)
	}
}
