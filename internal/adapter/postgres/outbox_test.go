//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/outbox"
)

// submitN creates n transfers, and with them n outbox events.
func submitN(t *testing.T, db *sqlx.DB, n int) {
	t.Helper()
	svc := newService(db)
	for i := range n {
		if _, err := svc.Submit(t.Context(), request(fmt.Sprintf("k%d", i), 1000)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublishBatchPublishesInOrderAndMarks(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	submitN(t, db, 3)
	store := postgres.NewStore(db)

	var got []outbox.Event
	n, err := store.PublishBatch(t.Context(), 10, func(_ context.Context, e outbox.Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("PublishBatch = %d, %v; want 3, nil", n, err)
	}
	for i, e := range got {
		if i > 0 && e.ID <= got[i-1].ID {
			t.Errorf("events out of order: %d after %d", e.ID, got[i-1].ID)
		}
		var payload map[string]string
		if err := json.Unmarshal(e.Payload, &payload); err != nil || payload["transfer_id"] != e.TransferID.String() {
			t.Errorf("payload %s doesn't match transfer %s", e.Payload, e.TransferID)
		}
		if e.Type != "transfer.created" || e.CorrelationID != fmt.Sprintf("corr-k%d", i) || e.CreatedAt.IsZero() {
			t.Errorf("event %+v has wrong fields", e)
		}
	}
	assertBacklog(t, store, 0)
}

func TestPublishBatchKeepsConfirmedEventsOnFailure(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	submitN(t, db, 3)
	store := postgres.NewStore(db)
	brokerDown := errors.New("broker down")

	calls := 0
	n, err := store.PublishBatch(t.Context(), 10, func(context.Context, outbox.Event) error {
		calls++
		if calls == 2 {
			return brokerDown
		}
		return nil
	})
	if !errors.Is(err, brokerDown) || n != 1 {
		t.Fatalf("PublishBatch = %d, %v; want 1, broker down", n, err)
	}
	assertBacklog(t, store, 2)

	// The next round starts at the event that failed.
	var ids []int64
	if _, err := store.PublishBatch(t.Context(), 10, func(_ context.Context, e outbox.Event) error {
		ids = append(ids, e.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] >= ids[1] {
		t.Errorf("second round published %v, want the remaining two in order", ids)
	}
	assertBacklog(t, store, 0)
}

func TestPublishBatchRespectsLimit(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	submitN(t, db, 5)
	store := postgres.NewStore(db)

	n, err := store.PublishBatch(t.Context(), 2, func(context.Context, outbox.Event) error { return nil })
	if err != nil || n != 2 {
		t.Fatalf("PublishBatch = %d, %v; want 2, nil", n, err)
	}
	assertBacklog(t, store, 3)
}

func TestConcurrentRelaysPublishEachEventOnce(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	submitN(t, db, 40)
	store := postgres.NewStore(db)

	var mu sync.Mutex
	published := make(map[int64]int)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for {
				n, err := store.PublishBatch(context.Background(), 5, func(_ context.Context, e outbox.Event) error {
					time.Sleep(2 * time.Millisecond) // keep the rows locked long enough to overlap
					mu.Lock()
					published[e.ID]++
					mu.Unlock()
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()

	if len(published) != 40 {
		t.Errorf("published %d distinct events, want 40", len(published))
	}
	for id, times := range published {
		if times != 1 {
			t.Errorf("event %d published %d times, want once", id, times)
		}
	}
	assertBacklog(t, store, 0)
}

func TestBacklogReportsOldestUnsent(t *testing.T) {
	t.Parallel()
	db := pg.NewDB(t, 1_000_000)
	store := postgres.NewStore(db)
	assertBacklog(t, store, 0)

	submitN(t, db, 2)
	count, oldest, err := store.Backlog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || time.Since(oldest) > time.Minute {
		t.Errorf("Backlog = %d, oldest %v ago; want 2 and recent", count, time.Since(oldest))
	}
}

func assertBacklog(t *testing.T, store *postgres.Store, want int) {
	t.Helper()
	count, _, err := store.Backlog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Errorf("unsent events = %d, want %d", count, want)
	}
}
