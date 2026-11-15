//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

func newLedgerChecker(db *sqlx.DB) *transfer.LedgerChecker {
	return transfer.NewLedgerChecker(postgres.NewStore(db), slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// paidAndRefunded builds a history through the real code: version 1 opening, 2 debit of the paid
// transfer, 3 debit of the refused one, 4 its refund. Balance 970,000.
func paidAndRefunded(t *testing.T) (db *sqlx.DB, paid, refused uuid.UUID) {
	t.Helper()
	db = pg.NewDB(t, 1_000_000)
	svc := newService(db)
	for i, p := range []scriptedProvider{providerPaid, providerRefused} {
		res, err := svc.Submit(t.Context(), request(fmt.Sprintf("k%d", i), 30_000))
		if err != nil {
			t.Fatal(err)
		}
		if err := newProcessor(db, p).Process(t.Context(), res.Transfer.ID); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			paid = res.Transfer.ID
		} else {
			refused = res.Transfer.ID
		}
	}
	return db, paid, refused
}

func check(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := newLedgerChecker(db).Check(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, db *sqlx.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), q, args...); err != nil {
		t.Fatal(err)
	}
}

// findings lists the stored findings as "RULE v<version> <transfer>", sorted.
func findings(t *testing.T, db *sqlx.DB) []string {
	t.Helper()
	var rows []struct {
		Rule       string        `db:"rule"`
		Version    sql.NullInt64 `db:"merchant_version"`
		TransferID uuid.NullUUID `db:"transfer_id"`
	}
	if err := db.SelectContext(t.Context(), &rows, `SELECT rule, merchant_version, transfer_id FROM ledger_findings`); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = finding(r.Rule, r.Version.Int64, r.TransferID.UUID)
	}
	slices.Sort(out)
	return out
}

func finding(rule string, version int64, id uuid.UUID) string {
	s := rule + fmt.Sprintf(" v%d", version)
	if id != uuid.Nil {
		s += " " + id.String()
	}
	return s
}

func held(t *testing.T, db *sqlx.DB) bool {
	t.Helper()
	var h bool
	if err := db.GetContext(t.Context(), &h, `SELECT payouts_held_reason IS NOT NULL FROM merchants WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLedgerCheckPassesARealHistory(t *testing.T) {
	db, _, _ := paidAndRefunded(t)

	check(t, db)

	if got := findings(t, db); len(got) != 0 {
		t.Errorf("findings = %v, want none", got)
	}
	var cp struct {
		Version int64 `db:"merchant_version"`
		Balance int64 `db:"balance_after"`
	}
	if err := db.GetContext(t.Context(), &cp, `SELECT merchant_version, balance_after FROM ledger_checkpoints WHERE merchant_id = 1`); err != nil {
		t.Fatal(err)
	}
	if cp.Version != 4 || cp.Balance != 970_000 {
		t.Errorf("checkpoint = v%d %d, want v4 970000", cp.Version, cp.Balance)
	}
	var unchecked int
	if err := db.GetContext(t.Context(), &unchecked, `SELECT count(*) FROM transfers WHERE ledger_checked_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if unchecked != 0 {
		t.Errorf("%d finished transfers left unchecked", unchecked)
	}
	if held(t, db) {
		t.Error("clean merchant was put on hold")
	}
}

func TestLedgerCheckFindsCorruption(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, db *sqlx.DB, paid, refused uuid.UUID)
		want    func(paid, refused uuid.UUID) []string
	}{
		{
			name: "balance changed without an entry",
			corrupt: func(t *testing.T, db *sqlx.DB, _, _ uuid.UUID) {
				exec(t, db, `UPDATE merchants SET balance = balance + 1 WHERE id = 1`)
			},
			want: func(_, _ uuid.UUID) []string { return []string{finding("MERCHANT_BALANCE", 4, uuid.Nil)} },
		},
		{
			name: "debit entry deleted",
			corrupt: func(t *testing.T, db *sqlx.DB, _, _ uuid.UUID) {
				exec(t, db, `DELETE FROM balance_entries WHERE merchant_version = 2`)
			},
			want: func(paid, _ uuid.UUID) []string {
				return []string{finding("DEBIT_MISSING", 0, paid), finding("VERSION_GAP", 3, uuid.Nil)}
			},
		},
		{
			name: "entry balance edited",
			corrupt: func(t *testing.T, db *sqlx.DB, _, _ uuid.UUID) {
				exec(t, db, `UPDATE balance_entries SET balance_after = balance_after + 5 WHERE merchant_version = 3`)
			},
			want: func(_, _ uuid.UUID) []string {
				return []string{finding("BALANCE_CHAIN", 3, uuid.Nil), finding("BALANCE_CHAIN", 4, uuid.Nil)}
			},
		},
		{
			name: "refund never written",
			corrupt: func(t *testing.T, db *sqlx.DB, _, _ uuid.UUID) {
				exec(t, db, `DELETE FROM balance_entries WHERE merchant_version = 4`)
				exec(t, db, `UPDATE merchants SET balance = 940000, version = 3 WHERE id = 1`)
			},
			want: func(_, refused uuid.UUID) []string { return []string{finding("REFUND_MISSING", 0, refused)} },
		},
		{
			name: "refund of a transfer that isn't failed",
			corrupt: func(t *testing.T, db *sqlx.DB, _, refused uuid.UUID) {
				exec(t, db, `UPDATE transfers SET status = 'SUCCESS' WHERE id = $1`, refused)
			},
			want: func(_, refused uuid.UUID) []string { return []string{finding("REFUND_NOT_FAILED", 4, refused)} },
		},
		{
			name: "debit of the wrong amount",
			corrupt: func(t *testing.T, db *sqlx.DB, paid, _ uuid.UUID) {
				exec(t, db, `UPDATE transfers SET amount = 31000 WHERE id = $1`, paid)
			},
			want: func(paid, _ uuid.UUID) []string { return []string{finding("DEBIT_AMOUNT", 2, paid)} },
		},
		{
			name: "refund of the wrong amount",
			corrupt: func(t *testing.T, db *sqlx.DB, _, refused uuid.UUID) {
				exec(t, db, `UPDATE balance_entries SET amount = 31000, balance_after = 971000 WHERE merchant_version = 4`)
				exec(t, db, `UPDATE merchants SET balance = 971000 WHERE id = 1`)
			},
			want: func(_, refused uuid.UUID) []string { return []string{finding("REFUND_AMOUNT", 4, refused)} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, paid, refused := paidAndRefunded(t)
			tt.corrupt(t, db, paid, refused)

			check(t, db)

			if got, want := findings(t, db), tt.want(paid, refused); !slices.Equal(got, want) {
				t.Errorf("findings = %v, want %v", got, want)
			}
			if !held(t, db) {
				t.Error("merchant not put on hold")
			}
		})
	}
}

// The version doesn't move here, so only comparing the balance brings the merchant back into the check.
func TestLedgerCheckSeesABalanceEditAfterACleanCheck(t *testing.T) {
	db, _, _ := paidAndRefunded(t)
	check(t, db)
	exec(t, db, `UPDATE merchants SET balance = balance + 1 WHERE id = 1`)

	check(t, db)

	if got, want := findings(t, db), []string{finding("MERCHANT_BALANCE", 4, uuid.Nil)}; !slices.Equal(got, want) {
		t.Errorf("findings = %v, want %v", got, want)
	}
}

func TestLedgerCheckReadsOnlyNewEntries(t *testing.T) {
	db, _, _ := paidAndRefunded(t)
	check(t, db)
	// Old entries are trusted once checked: only the service writes to its database.
	exec(t, db, `UPDATE balance_entries SET balance_after = 1 WHERE merchant_version = 2`)
	if _, err := newService(db).Submit(t.Context(), request("k9", 10_000)); err != nil {
		t.Fatal(err)
	}

	check(t, db)

	if got := findings(t, db); len(got) != 0 {
		t.Errorf("findings = %v, want none", got)
	}
}

func TestLedgerCheckRecordsAProblemOnce(t *testing.T) {
	db, _, _ := paidAndRefunded(t)
	exec(t, db, `UPDATE merchants SET balance = balance + 1 WHERE id = 1`)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := newLedgerChecker(db).Check(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	check(t, db)

	if got := findings(t, db); len(got) != 1 {
		t.Errorf("findings = %v, want exactly one", got)
	}
}

// The same break at the same version, after its first finding was resolved, must hold the merchant again.
func TestLedgerCheckFlagsAProblemAgainAfterAResolve(t *testing.T) {
	db, _, _ := paidAndRefunded(t)
	first := holdMerchant(t, db)
	if _, stillHeld, err := transfer.ResolveLedgerFinding(t.Context(), postgres.NewStore(db), first, "ops", "reverted"); err != nil || stillHeld {
		t.Fatalf("resolve = held %t, %v; want released", stillHeld, err)
	}

	exec(t, db, `UPDATE merchants SET balance = balance + 1 WHERE id = 1`)
	check(t, db)

	var open int
	if err := db.GetContext(t.Context(), &open, `SELECT count(*) FROM ledger_findings WHERE resolved_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if open != 1 || !held(t, db) {
		t.Errorf("open findings = %d, held = %t; want a new open finding and the merchant held", open, held(t, db))
	}
}

// holdMerchant gives merchant 1 an open finding and a hold, as the check does, and returns the finding ID.
func holdMerchant(t *testing.T, db *sqlx.DB) int64 {
	t.Helper()
	exec(t, db, `UPDATE merchants SET balance = balance + 1 WHERE id = 1`)
	check(t, db)
	exec(t, db, `UPDATE merchants SET balance = balance - 1 WHERE id = 1`)
	var id int64
	if err := db.GetContext(t.Context(), &id, `SELECT id FROM ledger_findings WHERE resolved_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if !held(t, db) {
		t.Fatal("merchant not held")
	}
	return id
}

func TestHeldMerchantPayoutsWaitUntilResolved(t *testing.T) {
	db := pg.NewDB(t, 1_000_000)
	svc := newService(db)
	waiting, err := svc.Submit(t.Context(), request("before-hold", 10_000))
	if err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Submit(t.Context(), request("at-provider", 10_000))
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `UPDATE transfers SET status = 'PROCESSING' WHERE id = $1`, sent.Transfer.ID)
	findingID := holdMerchant(t, db)

	_, err = svc.Submit(t.Context(), request("during-hold", 10_000))
	if !errors.Is(err, transfer.ErrPayoutsHeld) {
		t.Fatalf("submit during hold = %v, want ErrPayoutsHeld", err)
	}
	assertCount(t, db, "transfers", 2)
	assertCount(t, db, "outbox", 2)

	calls := 0
	counting := countingProvider{scriptedProvider: providerPaid, calls: &calls}
	if err := newProcessor(db, counting).Process(t.Context(), waiting.Transfer.ID); err != nil {
		t.Fatal(err)
	}
	if got := loadState(t, db, waiting.Transfer.ID).Status; got != "PENDING" || calls != 0 {
		t.Errorf("held PENDING transfer: status %s after %d provider calls, want PENDING after 0", got, calls)
	}
	if err := newProcessor(db, counting).Process(t.Context(), sent.Transfer.ID); err != nil {
		t.Fatal(err)
	}
	if got := loadState(t, db, sent.Transfer.ID).Status; got != "SUCCESS" {
		t.Errorf("transfer already at the provider: status %s, want SUCCESS", got)
	}

	age(t, db, waiting.Transfer.ID, transfer.StatusPending, time.Hour, 0)
	markAllPublished(t, db)
	if n, err := newReconciler(db).Sweep(t.Context()); err != nil || n != 0 {
		t.Errorf("sweep during hold = %d, %v; want 0 queued", n, err)
	}

	if _, stillHeld, err := transfer.ResolveLedgerFinding(t.Context(), postgres.NewStore(db), findingID, "ops", "test"); err != nil || stillHeld {
		t.Fatalf("resolve = held %t, %v; want released", stillHeld, err)
	}
	if n, err := newReconciler(db).Sweep(t.Context()); err != nil || n != 1 {
		t.Errorf("sweep after the hold = %d, %v; want the waiting transfer queued", n, err)
	}
	if _, err := svc.Submit(t.Context(), request("during-hold", 10_000)); err != nil {
		t.Errorf("same key after the hold = %v, want accepted", err)
	}
}

type countingProvider struct {
	scriptedProvider
	calls *int
}

func (p countingProvider) Payout(ctx context.Context, t transfer.Transfer) (transfer.PayoutResult, error) {
	*p.calls++
	return p.scriptedProvider.Payout(ctx, t)
}

func TestResolveKeepsTheHoldWhileFindingsRemain(t *testing.T) {
	db, _, refused := paidAndRefunded(t)
	exec(t, db, `UPDATE transfers SET status = 'SUCCESS' WHERE id = $1`, refused)
	exec(t, db, `UPDATE merchants SET balance = balance + 1 WHERE id = 1`)
	check(t, db)
	var ids []int64
	if err := db.SelectContext(t.Context(), &ids, `SELECT id FROM ledger_findings ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("%d findings, want 2", len(ids))
	}
	store := postgres.NewStore(db)

	if _, stillHeld, err := transfer.ResolveLedgerFinding(t.Context(), store, ids[0], "ops", "first"); err != nil || !stillHeld {
		t.Errorf("first resolve = held %t, %v; want still held", stillHeld, err)
	}
	if _, stillHeld, err := transfer.ResolveLedgerFinding(t.Context(), store, ids[1], "ops", "second"); err != nil || stillHeld {
		t.Errorf("last resolve = held %t, %v; want released", stillHeld, err)
	}
	if _, _, err := transfer.ResolveLedgerFinding(t.Context(), store, ids[1], "ops", "again"); !errors.Is(err, transfer.ErrFindingResolved) {
		t.Errorf("resolving twice = %v, want ErrFindingResolved", err)
	}
	if _, _, err := transfer.ResolveLedgerFinding(t.Context(), store, 999, "ops", "x"); !errors.Is(err, transfer.ErrFindingNotFound) {
		t.Errorf("resolving a missing finding = %v, want ErrFindingNotFound", err)
	}
	var by, note string
	if err := db.QueryRowContext(t.Context(), `SELECT resolved_by, resolution_note FROM ledger_findings WHERE id = $1`, ids[0]).Scan(&by, &note); err != nil {
		t.Fatal(err)
	}
	if by != "ops" || note != "first" {
		t.Errorf("stored resolution = %q, %q", by, note)
	}
}

// A check recording a new finding while a resolve lifts the hold must leave the merchant held.
func TestNewFindingDuringResolveKeepsTheHold(t *testing.T) {
	db := pg.NewDB(t, 1_000_000)
	findingID := holdMerchant(t, db)
	store := postgres.NewStore(db)

	lifted := make(chan struct{})
	commit := make(chan struct{})
	resolveDone := make(chan error)
	go func() {
		resolveDone <- store.Do(t.Context(), func(r transfer.TxRepos) error {
			m, err := r.LedgerCheck().Resolve(t.Context(), findingID, "ops", "test")
			if err != nil {
				return err
			}
			if _, err := r.LedgerCheck().ReleaseHold(t.Context(), m); err != nil {
				return err
			}
			close(lifted)
			<-commit
			return nil
		})
	}()
	<-lifted

	recordDone := make(chan error)
	go func() {
		recordDone <- store.Do(t.Context(), func(r transfer.TxRepos) error {
			added, err := r.LedgerCheck().AddFindings(t.Context(), []transfer.LedgerFinding{{MerchantID: 1, Rule: transfer.RuleVersionGap, MerchantVersion: 7}})
			if err != nil {
				return err
			}
			if len(added) != 1 {
				return fmt.Errorf("%d findings added, want 1", len(added))
			}
			_, err = r.LedgerCheck().HoldPayouts(t.Context(), []transfer.MerchantID{1})
			return err
		})
	}()
	waitUntilBlocked(t, db)
	close(commit)

	if err := <-resolveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-recordDone; err != nil {
		t.Fatal(err)
	}
	if !held(t, db) {
		t.Error("merchant released although a new finding is open")
	}
}

// waitUntilBlocked waits until some session of db's database waits for a row lock.
func waitUntilBlocked(t *testing.T, db *sqlx.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		const q = `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
				AND wait_event_type = 'Lock'
		`
		if err := db.GetContext(t.Context(), &n, q); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session blocked on a lock within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
