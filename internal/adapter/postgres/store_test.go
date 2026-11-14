//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/pgtest"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const maxAmount transfer.Amount = 250_000_000

func newService(db *sqlx.DB) *transfer.Service {
	store := postgres.NewStore(db)
	return transfer.NewService(store, store, maxAmount)
}

func request(key string, amount transfer.Amount) transfer.SubmitRequest {
	return transfer.SubmitRequest{
		MerchantID:        1,
		IdempotencyKey:    key,
		CorrelationID:     "corr-" + key,
		Amount:            amount,
		Currency:          transfer.IDR,
		BankCode:          "BCA",
		AccountNumber:     "0123456789",
		AccountHolderName: "Budi Santoso",
		ReferenceID:       "INV-" + key,
		Description:       "invoice payout",
	}
}

func TestSubmitStoresTransferOutboxDebitAndLedger(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	ctx := context.Background()

	res, err := newService(db).Submit(ctx, request("k1", 150_000))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got, err := postgres.NewStore(db).ByID(ctx, res.Transfer.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got != res.Transfer {
		t.Errorf("stored transfer differs from returned one:\n got  %+v\n want %+v", got, res.Transfer)
	}

	var payload []byte
	if err := db.GetContext(ctx, &payload, `SELECT payload FROM outbox WHERE transfer_id = $1`, res.Transfer.ID); err != nil {
		t.Fatalf("select outbox: %v", err)
	}
	var event map[string]string
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("outbox payload is not JSON: %v", err)
	}
	wantEvent := map[string]string{
		"transfer_id":    res.Transfer.ID.String(),
		"correlation_id": "corr-k1",
		"event_type":     "transfer.created",
	}
	if !maps.Equal(event, wantEvent) {
		t.Errorf("outbox payload = %v, want %v", event, wantEvent)
	}

	assertBalance(t, db, 850_000, 2)
	assertLedgerConsistent(t, db)
}

func TestSubmitWithoutOptionalFieldsRoundTrips(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	ctx := context.Background()

	req := request("k1", 1_000)
	req.ReferenceID, req.Description = "", ""
	res, err := newService(db).Submit(ctx, req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got, err := postgres.NewStore(db).ByID(ctx, res.Transfer.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.ReferenceID != "" || got.Description != "" {
		t.Errorf("optional fields = %q, %q, want empty", got.ReferenceID, got.Description)
	}
}

func TestSubmitInsufficientBalanceRollsBackEarlierInserts(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 100_000)

	_, err := newService(db).Submit(context.Background(), request("k1", 150_000))
	if !errors.Is(err, transfer.ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}

	// The transfer and outbox rows were inserted before the debit failed.
	assertCount(t, db, "transfers", 0)
	assertCount(t, db, "outbox", 0)
	assertBalance(t, db, 100_000, 1)
}

func TestSubmitReplayAndKeyReuse(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	svc := newService(db)
	ctx := context.Background()

	first, err := svc.Submit(ctx, request("k1", 150_000))
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	replay, err := svc.Submit(ctx, request("k1", 150_000))
	if err != nil {
		t.Fatalf("replay Submit: %v", err)
	}
	if !replay.Replayed || replay.Transfer.ID != first.Transfer.ID {
		t.Errorf("replay = (replayed %v, id %v), want (true, %v)", replay.Replayed, replay.Transfer.ID, first.Transfer.ID)
	}

	if _, err := svc.Submit(ctx, request("k1", 160_000)); !errors.Is(err, transfer.ErrIdempotencyKeyReused) {
		t.Errorf("different body: err = %v, want ErrIdempotencyKeyReused", err)
	}

	assertCount(t, db, "transfers", 1)
	assertBalance(t, db, 850_000, 2)
	assertLedgerConsistent(t, db)
}

func TestGetUnknownTransfer(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)

	_, err := postgres.NewStore(db).ByID(context.Background(), uuid.New())
	if !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDoRollsBackAndRepanics(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	store := postgres.NewStore(db)

	recovered := func() (p any) {
		defer func() { p = recover() }()
		_ = store.Do(context.Background(), func(r transfer.TxRepos) error {
			tr := newTransfer()
			if err := r.Transfers().Create(context.Background(), &tr); err != nil {
				t.Errorf("Create: %v", err)
			}
			panic("boom")
		})
		return nil
	}()

	if recovered != "boom" {
		t.Errorf("recovered %v, want the original panic", recovered)
	}
	assertCount(t, db, "transfers", 0)
}

func TestDoReturnsCommitError(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	ctx := context.Background()

	// A deferred constraint is only checked at COMMIT, so this makes the commit itself fail.
	_, err := db.ExecContext(ctx, `
		ALTER TABLE outbox
		ADD CONSTRAINT outbox_one_event_per_transfer
			UNIQUE (transfer_id) DEFERRABLE INITIALLY DEFERRED
	`)
	if err != nil {
		t.Fatal(err)
	}

	err = postgres.NewStore(db).Do(ctx, func(r transfer.TxRepos) error {
		tr := newTransfer()
		if err := r.Transfers().Create(ctx, &tr); err != nil {
			return err
		}
		event := transfer.OutboxEvent{TransferID: tr.ID, Type: transfer.EventTransferCreated, CorrelationID: "c"}
		if err := r.Outbox().Add(ctx, event); err != nil {
			return err
		}
		return r.Outbox().Add(ctx, event)
	})

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "outbox_one_event_per_transfer" {
		t.Fatalf("err = %v, want the deferred constraint violation from COMMIT", err)
	}
	assertCount(t, db, "transfers", 0)
}

func TestDoAfterCancelReturnsOnlyTheCallbackError(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	stop := errors.New("stop")

	err := postgres.NewStore(db).Do(ctx, func(r transfer.TxRepos) error {
		cancel()
		// database/sql rolls the transaction back in the background once ctx is done.
		// Wait until it has, so Do's own Rollback finds it already closed.
		deadline := time.Now().Add(5 * time.Second)
		for {
			tr := newTransfer()
			if err := r.Transfers().Create(context.Background(), &tr); errors.Is(err, sql.ErrTxDone) {
				return stop
			}
			if time.Now().After(deadline) {
				t.Fatal("transaction was not rolled back after cancel")
			}
			time.Sleep(time.Millisecond)
		}
	})

	if err == nil || err.Error() != stop.Error() {
		t.Errorf("err = %v, want only %q", err, stop)
	}
	assertCount(t, db, "transfers", 0)
}

func TestConcurrentSubmitsNeverOverdraw(t *testing.T) {
	t.Parallel()
	const (
		initial  = 1_000_000
		amount   = 30_000
		requests = 100
		fits     = initial / amount // 33
	)
	db := pg.NewDB(t, initial)
	svc := newService(db)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		declined int
		other    []error
	)
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Submit(context.Background(), request(fmt.Sprintf("k%d", i), amount))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, transfer.ErrInsufficientBalance):
				declined++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if accepted != fits || declined != requests-fits {
		t.Errorf("accepted %d, declined %d; want %d, %d", accepted, declined, fits, requests-fits)
	}
	assertBalance(t, db, initial-fits*amount, 1+fits)
	assertCount(t, db, "transfers", fits)
	assertCount(t, db, "outbox", fits)
	assertLedgerConsistent(t, db)
}

func TestConcurrentDuplicatesCreateOneTransfer(t *testing.T) {
	t.Parallel()
	const requests = 20
	db := pg.NewDB(t, 1_000_000)
	svc := newService(db)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     []uuid.UUID
		created int
	)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Submit(context.Background(), request("same-key", 50_000))
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids = append(ids, res.Transfer.ID)
			if !res.Replayed {
				created++
			}
		}()
	}
	wg.Wait()

	if created != 1 {
		t.Errorf("%d submits created a transfer, want exactly 1", created)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Errorf("submits returned different transfer IDs: %v", ids)
			break
		}
	}
	assertCount(t, db, "transfers", 1)
	assertBalance(t, db, 950_000, 2)
	assertLedgerConsistent(t, db)
}

func newTransfer() transfer.Transfer {
	return transfer.Transfer{
		ID:                uuid.Must(uuid.NewV7()),
		MerchantID:        1,
		IdempotencyKey:    uuid.NewString(),
		RequestHash:       "hash",
		Amount:            1_000,
		Currency:          transfer.IDR,
		BankCode:          "BCA",
		AccountNumber:     "0123456789",
		AccountHolderName: "Budi Santoso",
		Status:            transfer.StatusPending,
		CorrelationID:     "corr",
	}
}

func assertCount(t *testing.T, db *sqlx.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.GetContext(context.Background(), &got, "SELECT count(*) FROM "+table); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("%s has %d rows, want %d", table, got, want)
	}
}

func assertBalance(t *testing.T, db *sqlx.DB, wantBalance, wantVersion int64) {
	t.Helper()
	var balance, version int64
	if err := db.QueryRowContext(context.Background(), "SELECT balance, version FROM merchants WHERE id = 1").Scan(&balance, &version); err != nil {
		t.Fatal(err)
	}
	if balance != wantBalance || version != wantVersion {
		t.Errorf("merchant balance, version = %d, %d, want %d, %d", balance, version, wantBalance, wantVersion)
	}
}

// assertLedgerConsistent checks that the ledger explains the balance: entries sum to it, the
// latest entry matches it, and versions run 1..N without gaps.
func assertLedgerConsistent(t *testing.T, db *sqlx.DB) {
	t.Helper()
	var balance, version int64
	if err := db.QueryRowContext(context.Background(), "SELECT balance, version FROM merchants WHERE id = 1").Scan(&balance, &version); err != nil {
		t.Fatal(err)
	}

	var sum, lastBalance int64
	const q = `
		SELECT
			sum(amount),
			(SELECT balance_after FROM balance_entries ORDER BY merchant_version DESC LIMIT 1)
		FROM balance_entries
		WHERE merchant_id = 1
	`
	err := db.QueryRowContext(context.Background(), q).Scan(&sum, &lastBalance)
	if err != nil {
		t.Fatal(err)
	}
	if sum != balance || lastBalance != balance {
		t.Errorf("ledger sum %d, latest balance_after %d, merchant balance %d; want all equal", sum, lastBalance, balance)
	}

	var versions []int64
	if err := db.SelectContext(context.Background(), &versions, "SELECT merchant_version FROM balance_entries WHERE merchant_id = 1 ORDER BY merchant_version"); err != nil {
		t.Fatal(err)
	}
	for i, v := range versions {
		if v != int64(i+1) {
			t.Fatalf("ledger versions %v are not 1..%d without gaps", versions, version)
		}
	}
	if int64(len(versions)) != version {
		t.Errorf("%d ledger entries, merchant version %d; want equal", len(versions), version)
	}
}

// A transaction left open on a live connection, as after a failed rollback, holds its row locks.
// The idle-in-transaction timeout makes Postgres end that session and release them.
func TestIdleTransactionLosesItsLocks(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	var name string
	if err := db.GetContext(t.Context(), &name, "SELECT current_database()"); err != nil {
		t.Fatal(err)
	}
	pool := pgtest.PoolConfig()
	pool.IdleInTransactionTimeout = 200 * time.Millisecond
	stuckDB, err := postgres.Open(t.Context(), pg.URL(name), pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stuckDB.Close() })

	stuck, err := stuckDB.BeginTxx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stuck.Rollback() })
	if _, err := stuck.ExecContext(t.Context(), "SELECT 1 FROM merchants WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	// Without the timeout this waits for the stuck transaction forever.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := db.ExecContext(ctx, "UPDATE merchants SET name = name WHERE id = 1"); err != nil {
		t.Fatalf("update after the stuck transaction: %v", err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Errorf("update waited only %v; the row should have been locked until the timeout", waited)
	}
}
