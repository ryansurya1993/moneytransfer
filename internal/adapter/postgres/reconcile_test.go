//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const (
	reconcileAfter = time.Minute
	reconcileMax   = 8
)

var reconcileGap = transfer.ReconcileGap{First: reconcileAfter, Max: time.Hour}

func newReconciler(db *sqlx.DB) *transfer.Reconciler {
	store := postgres.NewStore(db)
	return transfer.NewReconciler(store, store, slog.New(slog.NewJSONHandler(io.Discard, nil)), transfer.ReconcilerOptions{
		After:       reconcileAfter,
		MaxAttempts: reconcileMax,
	})
}

// submitKeys creates one transfer per key against a balance of 1,000,000 and returns their IDs in order.
func submitKeys(t *testing.T, keys ...string) (*sqlx.DB, []uuid.UUID) {
	t.Helper()
	db := pg.NewDB(t, 1_000_000)
	svc := newService(db)
	ids := make([]uuid.UUID, len(keys))
	for i, k := range keys {
		res, err := svc.Submit(t.Context(), request(k, 1000))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = res.Transfer.ID
	}
	return db, ids
}

// age sets the transfer's status and reconcile tries, and makes it look unchanged for the given time.
func age(t *testing.T, db *sqlx.DB, id uuid.UUID, status transfer.Status, unchanged time.Duration, tries int) {
	t.Helper()
	const q = `
		UPDATE transfers
		SET
			status = $2,
			updated_at = now() - make_interval(secs => $3),
			reconcile_attempts = $4
		WHERE id = $1
	`
	if _, err := db.ExecContext(t.Context(), q, id, status, unchanged.Seconds(), tries); err != nil {
		t.Fatal(err)
	}
}

func markAllPublished(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `UPDATE outbox SET published_at = now()`); err != nil {
		t.Fatal(err)
	}
}

func TestSweepQueuesOnlyDueTransfers(t *testing.T) {
	tests := []struct {
		name      string
		status    transfer.Status
		unchanged time.Duration
		tries     int
		unsent    bool // the transfer still has an outbox event waiting
		wantDue   bool
	}{
		{name: "unknown", status: transfer.StatusUnknown, unchanged: 2 * time.Minute, wantDue: true},
		{name: "pending whose message was lost", status: transfer.StatusPending, unchanged: 2 * time.Minute, wantDue: true},
		{name: "processing whose message was lost", status: transfer.StatusProcessing, unchanged: 5 * time.Minute, tries: 2, wantDue: true},
		{name: "changed recently", status: transfer.StatusUnknown, unchanged: 30 * time.Second},
		{name: "fourth try waits 8 minutes", status: transfer.StatusUnknown, unchanged: 7 * time.Minute, tries: 3},
		{name: "fourth try after 8 minutes", status: transfer.StatusUnknown, unchanged: 9 * time.Minute, tries: 3, wantDue: true},
		{name: "gap stops growing at an hour", status: transfer.StatusUnknown, unchanged: 61 * time.Minute, tries: 7, wantDue: true},
		{name: "out of tries", status: transfer.StatusUnknown, unchanged: 2 * time.Hour, tries: reconcileMax},
		{name: "event not sent yet", status: transfer.StatusPending, unchanged: 2 * time.Hour, unsent: true},
		{name: "success", status: transfer.StatusSuccess, unchanged: time.Hour},
		{name: "failed", status: transfer.StatusFailed, unchanged: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, ids := submitKeys(t, "k1")
			if !tt.unsent {
				markAllPublished(t, db)
			}
			age(t, db, ids[0], tt.status, tt.unchanged, tt.tries)
			r := newReconciler(db)

			n, err := r.Sweep(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			if got := n == 1; got != tt.wantDue {
				t.Fatalf("queued %d, want due = %v", n, tt.wantDue)
			}
			wantTries, wantEvents := tt.tries, 0
			if tt.wantDue {
				wantTries, wantEvents = tt.tries+1, 1
			}
			var got struct {
				Tries  int  `db:"reconcile_attempts"`
				Recent bool `db:"recent"`
			}
			const q = `
				SELECT reconcile_attempts, updated_at > now() - interval '1 minute' AS recent
				FROM transfers
				WHERE id = $1
			`
			if err := db.GetContext(t.Context(), &got, q, ids[0]); err != nil {
				t.Fatal(err)
			}
			if got.Tries != wantTries || (tt.wantDue && !got.Recent) {
				t.Errorf("reconcile tries %d, updated recently %v; want %d tries and a fresh updated_at", got.Tries, got.Recent, wantTries)
			}
			reconcileEvents := fmt.Sprintf(
				"outbox WHERE event_type = 'transfer.reconcile' AND correlation_id = 'corr-k1' AND payload->>'event_type' = 'transfer.reconcile' AND transfer_id = '%s'",
				ids[0])
			assertCount(t, db, reconcileEvents, wantEvents)

			// The event just queued, and the fresh updated_at, keep the next sweep away.
			if n, err := r.Sweep(t.Context()); n != 0 || err != nil {
				t.Errorf("second Sweep = %d, %v; want 0", n, err)
			}
		})
	}
}

// Two relays sweeping at once must each take different transfers, and neither may wait for the other.
func TestConcurrentSweepsTakeDifferentTransfers(t *testing.T) {
	t.Parallel()
	db, ids := submitKeys(t, "k1", "k2")
	markAllPublished(t, db)
	age(t, db, ids[0], transfer.StatusUnknown, 10*time.Minute, 0)
	age(t, db, ids[1], transfer.StatusUnknown, 8*time.Minute, 0)
	store := postgres.NewStore(db)

	mark := func(ctx context.Context, r transfer.TxRepos) (uuid.UUID, error) {
		got, err := r.Transfers().MarkForReconcile(ctx, reconcileGap, reconcileMax, 1)
		if err != nil || len(got) != 1 {
			return uuid.Nil, errors.Join(err, fmt.Errorf("marked %d transfers, want 1", len(got)))
		}
		return got[0].ID, nil
	}
	var first, second uuid.UUID
	err := store.Do(t.Context(), func(r transfer.TxRepos) error {
		var err error
		if first, err = mark(t.Context(), r); err != nil {
			return err
		}
		// The first sweep still holds its row; the second must skip it instead of waiting.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		return store.Do(ctx, func(r transfer.TxRepos) error {
			second, err = mark(ctx, r)
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	if first != ids[0] || second != ids[1] {
		t.Errorf("sweeps took %v and %v, want the oldest %v first and %v second", first, second, ids[0], ids[1])
	}
}

func TestExhaustedListsTransfersOutOfTries(t *testing.T) {
	t.Parallel()
	db, ids := submitKeys(t, "k1", "k2", "k3", "k4", "k5")
	markAllPublished(t, db)
	age(t, db, ids[0], transfer.StatusUnknown, 20*time.Minute, reconcileMax)
	age(t, db, ids[1], transfer.StatusPending, 30*time.Minute, reconcileMax)
	age(t, db, ids[2], transfer.StatusUnknown, 30*time.Second, reconcileMax) // last try still running
	age(t, db, ids[3], transfer.StatusUnknown, 20*time.Minute, reconcileMax-1)
	age(t, db, ids[4], transfer.StatusSuccess, 20*time.Minute, reconcileMax)

	ex, err := postgres.NewStore(db).Exhausted(t.Context(), reconcileAfter, reconcileMax, 1)
	if err != nil {
		t.Fatal(err)
	}

	if ex.Count != 2 || len(ex.IDs) != 1 || ex.IDs[0] != ids[1] {
		t.Errorf("got %d exhausted, ids %v; want 2, listing only the oldest %v", ex.Count, ex.IDs, ids[1])
	}
	if waited := time.Since(ex.WaitingSince); waited < 29*time.Minute || waited > 31*time.Minute {
		t.Errorf("oldest unchanged for %v, want about 30m", waited)
	}

	none, err := postgres.NewStore(pg.NewDB(t, 1)).Exhausted(t.Context(), reconcileAfter, reconcileMax, 10)
	if err != nil || none.Count != 0 || none.IDs != nil {
		t.Errorf("empty database: got %+v, %v; want nothing", none, err)
	}
}

func TestProcessAsksAgainAboutUnknownTransfers(t *testing.T) {
	tests := []struct {
		name        string
		later       scriptedProvider
		wantStatus  string
		wantBalance int64
		wantVersion int64
	}{
		{name: "paid after all", later: providerPaid, wantStatus: "SUCCESS", wantBalance: 970_000, wantVersion: 2},
		{name: "refused after all", later: providerRefused, wantStatus: "FAILED", wantBalance: 1_000_000, wantVersion: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, id := submitOne(t)
			if err := newProcessor(db, providerSilent).Process(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			if s := loadState(t, db, id); s.Status != "UNKNOWN" {
				t.Fatalf("status %s after no answer, want UNKNOWN", s.Status)
			}

			if err := newProcessor(db, tt.later).Process(t.Context(), id); err != nil {
				t.Fatal(err)
			}

			s := loadState(t, db, id)
			if s.Status != tt.wantStatus || s.Attempts != 4 || !s.Completed {
				t.Errorf("got %s after %d attempts, completed %v; want %s after 4, completed", s.Status, s.Attempts, s.Completed, tt.wantStatus)
			}
			assertBalance(t, db, tt.wantBalance, tt.wantVersion)
			assertLedgerConsistent(t, db)
		})
	}
}
