//go:build integration

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/outbox"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type fakeProcessor struct {
	mu           sync.Mutex
	calls        int
	ids          []uuid.UUID
	correlations []string
	process      func(ctx context.Context) error
}

func (p *fakeProcessor) Process(ctx context.Context, id uuid.UUID) error {
	p.mu.Lock()
	p.calls++
	p.ids = append(p.ids, id)
	p.correlations = append(p.correlations, logging.CorrelationID(ctx))
	p.mu.Unlock()
	if p.process == nil {
		return nil
	}
	return p.process(ctx)
}

func (p *fakeProcessor) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// consume runs the consumer until the test ends or stop is called.
func consume(t *testing.T, c *Conn, p Processor, opts ConsumeOptions) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.ConsumeTransfers(ctx, p, opts, slog.New(slog.NewJSONHandler(io.Discard, nil))) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("ConsumeTransfers = %v, want nil after cancel", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func publishTransfer(t *testing.T, c *Conn, id uuid.UUID, body string) {
	t.Helper()
	if body == "" {
		body = fmt.Sprintf(`{"transfer_id":%q,"correlation_id":"corr-1","event_type":"transfer.created"}`, id)
	}
	if err := newPublisher(t, c).Publish(t.Context(), outbox.Event{ID: 1, Payload: []byte(body)}); err != nil {
		t.Fatal(err)
	}
}

func readyMessages(t *testing.T, c *Conn, queue string) int {
	t.Helper()
	q, err := rawChannel(t, c).QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return q.Messages
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var consumeOpts = ConsumeOptions{Concurrency: 4, ShutdownTimeout: time.Second}

func TestConsumeAcksProcessedMessages(t *testing.T) {
	c := dial(t)
	p := &fakeProcessor{}
	id := uuid.New()
	publishTransfer(t, c, id, "")

	stop := consume(t, c, p, consumeOpts)
	eventually(t, "the message to be processed", func() bool { return p.callCount() == 1 })
	stop()

	if p.ids[0] != id || p.correlations[0] != "corr-1" {
		t.Errorf("processed %v with correlation %q, want %v and corr-1", p.ids[0], p.correlations[0], id)
	}
	if n := readyMessages(t, c, transfersQueue) + readyMessages(t, c, deadLetterQueue); n != 0 {
		t.Errorf("%d messages left in the queues, want 0 after ack", n)
	}
}

func TestConsumeDeadLettersWhatCantBeRetried(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		process   func(context.Context) error
		wantCalls int
	}{
		{name: "malformed", body: `{"transfer_id":`, wantCalls: 0},
		{name: "other event type", body: `{"transfer_id":"0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f","event_type":"transfer.reconcile"}`, wantCalls: 0},
		{name: "unknown transfer", process: func(context.Context) error { return fmt.Errorf("claim: %w", transfer.ErrNotFound) }, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := dial(t)
			p := &fakeProcessor{process: tt.process}
			publishTransfer(t, c, uuid.New(), tt.body)

			consume(t, c, p, consumeOpts)

			eventually(t, "the message in the dead-letter queue", func() bool { return readyMessages(t, c, deadLetterQueue) == 1 })
			if got := p.callCount(); got != tt.wantCalls {
				t.Errorf("processed %d times, want %d: retrying can't help", got, tt.wantCalls)
			}
		})
	}
}

// A message that always fails must stop after the delivery limit instead of looping forever.
// This relies on rejects counting toward the limit, which nacks don't.
func TestConsumeDeadLettersAfterTheDeliveryLimit(t *testing.T) {
	c := dial(t)
	p := &fakeProcessor{process: func(context.Context) error { return errors.New("bug") }}
	publishTransfer(t, c, uuid.New(), "")

	consume(t, c, p, consumeOpts)

	eventually(t, "the message in the dead-letter queue", func() bool { return readyMessages(t, c, deadLetterQueue) == 1 })
	if got := p.callCount(); got != deliveryLimit+1 {
		t.Errorf("processed %d times, want %d (first delivery plus %d redeliveries)", got, deliveryLimit+1, deliveryLimit)
	}
}

// Shutdown hands an unfinished message back without counting it as a failed delivery.
func TestConsumeHandsBackUnfinishedMessagesAtShutdown(t *testing.T) {
	c := dial(t)
	p := &fakeProcessor{process: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	publishTransfer(t, c, uuid.New(), "")

	stop := consume(t, c, p, ConsumeOptions{Concurrency: 1, ShutdownTimeout: 100 * time.Millisecond})
	eventually(t, "the message to be picked up", func() bool { return p.callCount() == 1 })
	stop()

	eventually(t, "the message back in the queue", func() bool { return readyMessages(t, c, transfersQueue) == 1 })
	msg, ok, err := rawChannel(t, c).Get(transfersQueue, true)
	if err != nil || !ok {
		t.Fatalf("get message: ok %v, err %v", ok, err)
	}
	if count, _ := msg.Headers["x-delivery-count"].(int64); count != 0 {
		t.Errorf("x-delivery-count = %d, want 0: a shutdown is not a failed delivery", count)
	}
}
