package outbox_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryansurya1993/moneytransfer/internal/outbox"
)

type fakeStore struct {
	mu        sync.Mutex
	pending   []outbox.Event
	sent      []int64
	rounds    int
	oldestAge time.Duration
}

func newFakeStore(n int) *fakeStore {
	s := &fakeStore{}
	for i := range n {
		s.pending = append(s.pending, outbox.Event{ID: int64(i + 1), CreatedAt: time.Now()})
	}
	return s
}

func (s *fakeStore) PublishBatch(ctx context.Context, limit int, publish func(context.Context, outbox.Event) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rounds++
	n := 0
	for len(s.pending) > 0 && n < limit {
		if err := publish(ctx, s.pending[0]); err != nil {
			return n, err
		}
		s.sent = append(s.sent, s.pending[0].ID)
		s.pending = s.pending[1:]
		n++
	}
	return n, nil
}

func (s *fakeStore) Backlog(context.Context) (int, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), s.oldestAge, nil
}

func (s *fakeStore) state() (pending, sent, rounds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), len(s.sent), s.rounds
}

type fakePublisher struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	deadline  time.Duration
	canceled  bool
	onPublish func()
}

func (p *fakePublisher) Publish(ctx context.Context, _ outbox.Event) error {
	if p.onPublish != nil {
		p.onPublish()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if d, ok := ctx.Deadline(); ok {
		p.deadline = time.Until(d)
	}
	p.canceled = p.canceled || ctx.Err() != nil
	if p.calls <= p.failFirst {
		return errors.New("confirm timed out")
	}
	return nil
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var defaultOptions = outbox.Options{BatchSize: 50, PollInterval: time.Hour, PublishTimeout: 5 * time.Second, AlertAge: 2 * time.Minute}

// start runs the relay in the background and stops it when the test ends.
func start(t *testing.T, store outbox.Store, pub outbox.Publisher, opts outbox.Options) (*outbox.Relay, *syncBuffer, context.CancelFunc) {
	t.Helper()
	logs := &syncBuffer{}
	r := outbox.NewRelay(store, pub, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), opts)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v, want nil after cancel", err)
		}
	})
	return r, logs, cancel
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunDrainsFullBatchesWithoutWaiting(t *testing.T) {
	store := newFakeStore(120)
	start(t, store, &fakePublisher{}, defaultOptions)

	// The poll interval is an hour, so only back-to-back rounds can publish all 120 in time.
	waitFor(t, func() bool { _, sent, _ := store.state(); return sent == 120 })
	if _, _, rounds := store.state(); rounds != 3 {
		t.Errorf("rounds = %d, want 3 (50, 50, 20, then wait)", rounds)
	}
}

func TestRunWaitsAfterPartialBatch(t *testing.T) {
	store := newFakeStore(10)
	start(t, store, &fakePublisher{}, defaultOptions)

	waitFor(t, func() bool { _, sent, _ := store.state(); return sent == 10 })
	time.Sleep(50 * time.Millisecond)
	if _, _, rounds := store.state(); rounds != 1 {
		t.Errorf("rounds = %d, want 1: a partial batch means nothing else is waiting", rounds)
	}
}

func TestFailedRoundIsLoggedAndRetried(t *testing.T) {
	store := newFakeStore(5)
	opts := defaultOptions
	opts.PollInterval = 10 * time.Millisecond
	_, logs, _ := start(t, store, &fakePublisher{failFirst: 1}, opts)

	waitFor(t, func() bool { _, sent, _ := store.state(); return sent == 5 })
	if !strings.Contains(logs.String(), "confirm timed out") || !strings.Contains(logs.String(), `"outbox_id"`) {
		t.Errorf("logs %s, want the failed publish and later successes", logs)
	}
}

func TestPublishGetsItsOwnTimeout(t *testing.T) {
	pub := &fakePublisher{}
	start(t, newFakeStore(1), pub, defaultOptions)

	waitFor(t, func() bool { pub.mu.Lock(); defer pub.mu.Unlock(); return pub.calls == 1 })
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.deadline <= 0 || pub.deadline > 5*time.Second {
		t.Errorf("publish deadline in %v, want within the 5s publish timeout", pub.deadline)
	}
}

func TestShutdownLetsTheRoundFinish(t *testing.T) {
	store := newFakeStore(3)
	pub := &fakePublisher{}
	ready := make(chan struct{})
	var cancel context.CancelFunc
	var once sync.Once
	// Cancel the relay while its first publish is in progress.
	pub.onPublish = func() {
		<-ready
		once.Do(cancel)
	}
	_, _, cancel = start(t, store, pub, defaultOptions)
	close(ready)

	waitFor(t, func() bool { _, sent, _ := store.state(); return sent == 3 })
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.canceled {
		t.Error("publish saw a canceled context during shutdown; confirmed events would be sent again")
	}
}

func TestBacklogAlert(t *testing.T) {
	tests := []struct {
		name      string
		oldestAgo time.Duration
		wantAlert bool
	}{
		{name: "old unsent event", oldestAgo: 10 * time.Minute, wantAlert: true},
		{name: "recent unsent event", oldestAgo: time.Second, wantAlert: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore(1)
			store.oldestAge = tt.oldestAgo
			// Every publish fails, so the event stays unsent.
			_, logs, _ := start(t, store, &fakePublisher{failFirst: 1000}, defaultOptions)

			waitFor(t, func() bool { _, _, rounds := store.state(); return rounds >= 1 })
			time.Sleep(20 * time.Millisecond)
			if got := strings.Contains(logs.String(), `"alert":"outbox_backlog"`); got != tt.wantAlert {
				t.Errorf("alert logged = %v, want %v; logs %s", got, tt.wantAlert, logs)
			}
		})
	}
}

func TestLastRoundAdvances(t *testing.T) {
	opts := defaultOptions
	opts.PollInterval = 10 * time.Millisecond
	r, _, _ := start(t, newFakeStore(0), &fakePublisher{}, opts)
	first := r.LastRound()

	waitFor(t, func() bool { return r.LastRound().After(first) })
}
