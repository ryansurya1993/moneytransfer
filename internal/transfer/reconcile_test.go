package transfer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

var reconcileOpts = transfer.ReconcilerOptions{After: time.Minute, MaxAttempts: 8}

type finderArgs struct {
	olderThan   time.Duration
	maxAttempts int
	limit       int
}

type fakeFinder struct {
	result transfer.Exhausted
	err    error
	asked  []finderArgs
}

func (f *fakeFinder) Exhausted(_ context.Context, olderThan time.Duration, maxAttempts, limit int) (transfer.Exhausted, error) {
	f.asked = append(f.asked, finderArgs{olderThan, maxAttempts, limit})
	return f.result, f.err
}

func newReconciler(db *fakeDB, f *fakeFinder, logs io.Writer) *transfer.Reconciler {
	return transfer.NewReconciler(db, f, slog.New(slog.NewJSONHandler(logs, nil)), reconcileOpts)
}

func TestSweepQueuesAReconcileEventPerDueTransfer(t *testing.T) {
	db := newFakeDB(0)
	first, second := uuid.New(), uuid.New()
	db.due = []transfer.ReconcileTarget{
		{ID: first, CorrelationID: "corr-1", Status: transfer.StatusUnknown, Attempt: 1},
		{ID: second, CorrelationID: "corr-2", Status: transfer.StatusPending, Attempt: 3},
	}

	n, err := newReconciler(db, &fakeFinder{}, io.Discard).Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if n != 2 {
		t.Errorf("Sweep = %d, want 2", n)
	}
	want := []transfer.OutboxEvent{
		{TransferID: first, Type: transfer.EventTransferReconcile, CorrelationID: "corr-1"},
		{TransferID: second, Type: transfer.EventTransferReconcile, CorrelationID: "corr-2"},
	}
	if len(db.committed.outbox) != len(want) {
		t.Fatalf("outbox = %+v, want %+v", db.committed.outbox, want)
	}
	for i := range want {
		if db.committed.outbox[i] != want[i] {
			t.Errorf("outbox[%d] = %+v, want %+v", i, db.committed.outbox[i], want[i])
		}
	}
	wantMark := markArgs{gap: transfer.ReconcileGap{First: time.Minute, Max: time.Hour}, maxAttempts: 8, limit: 100}
	if len(db.markedWith) != 1 || db.markedWith[0] != wantMark {
		t.Errorf("MarkForReconcile called with %+v, want gap 1m up to 1h, 8 tries, 100 at a time", db.markedWith)
	}
}

func TestSweepWithNothingDue(t *testing.T) {
	db := newFakeDB(0)

	n, err := newReconciler(db, &fakeFinder{}, io.Discard).Sweep(t.Context())

	if n != 0 || err != nil || len(db.committed.outbox) != 0 {
		t.Errorf("Sweep = %d, %v with outbox %v; want nothing queued", n, err, db.committed.outbox)
	}
}

// A try is counted only together with its event, or the transfer would lose a try for nothing.
func TestSweepKeepsNothingWhenAnEventFails(t *testing.T) {
	db := newFakeDB(0)
	db.due = []transfer.ReconcileTarget{{ID: uuid.New()}, {ID: uuid.New()}}
	db.failAt, db.failErr, db.failLeft = "outbox.Add", errors.New("disk full"), 1

	_, err := newReconciler(db, &fakeFinder{}, io.Discard).Sweep(t.Context())

	if err == nil {
		t.Fatal("Sweep = nil, want the outbox error")
	}
	if len(db.committed.outbox) != 0 {
		t.Errorf("outbox = %v, want nothing committed", db.committed.outbox)
	}
}

func TestCheckExhaustedRaisesAnAlert(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	finder := &fakeFinder{result: transfer.Exhausted{Count: 3, Waited: time.Hour, IDs: ids}}
	var logs bytes.Buffer

	if err := newReconciler(newFakeDB(0), finder, &logs).CheckExhausted(t.Context()); err != nil {
		t.Fatal(err)
	}

	var line struct {
		Level       string      `json:"level"`
		Alert       string      `json:"alert"`
		Severity    string      `json:"severity"`
		Count       int         `json:"count"`
		OldestAge   int         `json:"oldest_age_seconds"`
		TransferIDs []uuid.UUID `json:"transfer_ids"`
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log %q: %v", logs.String(), err)
	}
	if line.Level != "ERROR" || line.Alert != "reconcile_exhausted" || line.Severity != "high" || line.Count != 3 {
		t.Errorf("alert line %+v, want ERROR reconcile_exhausted high with count 3", line)
	}
	if line.OldestAge < 3599 || len(line.TransferIDs) != 2 || line.TransferIDs[0] != ids[0] {
		t.Errorf("alert line %+v, want age about an hour and the two ids", line)
	}
	if len(finder.asked) != 1 || finder.asked[0] != (finderArgs{time.Minute, 8, 10}) {
		t.Errorf("Exhausted called with %+v, want after 1m, 8 tries, 10 ids", finder.asked)
	}
}

func TestCheckExhaustedStaysQuietWhenNoneAreLeft(t *testing.T) {
	var logs bytes.Buffer

	if err := newReconciler(newFakeDB(0), &fakeFinder{}, &logs).CheckExhausted(t.Context()); err != nil {
		t.Fatal(err)
	}

	if logs.Len() != 0 {
		t.Errorf("logged %q, want nothing", logs.String())
	}
}

func TestCheckExhaustedReturnsTheReadError(t *testing.T) {
	finder := &fakeFinder{err: transfer.ErrUnavailable}

	err := newReconciler(newFakeDB(0), finder, io.Discard).CheckExhausted(t.Context())

	if !errors.Is(err, transfer.ErrUnavailable) {
		t.Errorf("CheckExhausted = %v, want ErrUnavailable", err)
	}
}

func TestRunSweepsAgainRightAfterAFullBatch(t *testing.T) {
	db := newFakeDB(0)
	full := make([]transfer.ReconcileTarget, 100)
	for i := range full {
		full[i] = transfer.ReconcileTarget{ID: uuid.New(), Status: transfer.StatusUnknown, Attempt: 1}
	}
	db.due = full
	db.onMark = func(call int) {
		if call == 3 {
			db.due = nil
		}
	}
	// Far shorter than the 30-second tick: every sweep seen here came without waiting for it.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	if err := newReconciler(db, &fakeFinder{}, io.Discard).Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Three full batches, then one that came back short and ended the run of sweeps.
	if got := len(db.markedWith); got != 4 {
		t.Errorf("%d sweeps, want 4", got)
	}
	if got := len(db.committed.outbox); got != 300 {
		t.Errorf("%d events queued, want 300", got)
	}
}
