//go:build integration

package rabbitmq

import (
	"context"
	"log"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/ryansurya1993/moneytransfer/internal/outbox"
)

const deliveryLimit = 5

var brokerURL string

// TestMain starts one broker for the package. The tests share its queues, so they don't run in parallel.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	// Same as in the postgres adapter's tests: a busy Docker pipe on Windows must not look like no Docker.
	if runtime.GOOS == "windows" && os.Getenv("DOCKER_HOST") == "" {
		if err := os.Setenv("DOCKER_HOST", "npipe:////./pipe/docker_engine"); err != nil {
			log.Printf("set DOCKER_HOST: %v", err)
			return 1
		}
	}
	container, err := tcrabbitmq.Run(ctx, "rabbitmq:4.3.7-management-alpine")
	if err != nil {
		log.Printf("start rabbitmq container: %v", err)
		return 1
	}
	defer func() { _ = container.Terminate(ctx) }()

	if brokerURL, err = container.AmqpURL(ctx); err != nil {
		log.Printf("amqp url: %v", err)
		return 1
	}
	return m.Run()
}

// dial connects with a declared, empty topology.
func dial(t *testing.T) *Conn {
	t.Helper()
	c, err := Dial(brokerURL, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.DeclareTopology(deliveryLimit); err != nil {
		t.Fatal(err)
	}
	ch := rawChannel(t, c)
	for _, q := range []string{transfersQueue, deadLetterQueue} {
		if _, err := ch.QueuePurge(q, false); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func rawChannel(t *testing.T, c *Conn) *amqp.Channel {
	t.Helper()
	ch, err := c.conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	return ch
}

func newPublisher(t *testing.T, c *Conn) *Publisher {
	t.Helper()
	p, err := c.NewPublisher()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var event = outbox.Event{
	ID:            42,
	TransferID:    uuid.MustParse("0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f"),
	Type:          "transfer.created",
	Payload:       []byte(`{"transfer_id":"0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f"}`),
	CorrelationID: "corr-1",
	CreatedAt:     time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC),
}

func TestPublishSetsMessageProperties(t *testing.T) {
	c := dial(t)

	if err := newPublisher(t, c).Publish(t.Context(), event); err != nil {
		t.Fatal(err)
	}

	msg, ok, err := rawChannel(t, c).Get(transfersQueue, true)
	if err != nil || !ok {
		t.Fatalf("get message: ok %v, err %v", ok, err)
	}
	if string(msg.Body) != string(event.Payload) {
		t.Errorf("body = %s, want %s", msg.Body, event.Payload)
	}
	if msg.DeliveryMode != amqp.Persistent || msg.ContentType != "application/json" {
		t.Errorf("delivery mode %d, content type %q; want persistent JSON", msg.DeliveryMode, msg.ContentType)
	}
	if msg.MessageId != "42" || msg.CorrelationId != "corr-1" || msg.Type != "transfer.created" {
		t.Errorf("message id %q, correlation id %q, type %q", msg.MessageId, msg.CorrelationId, msg.Type)
	}
	if !msg.Timestamp.Equal(event.CreatedAt) {
		t.Errorf("timestamp = %v, want %v", msg.Timestamp, event.CreatedAt)
	}
}

func TestPublishToMissingQueueFails(t *testing.T) {
	c := dial(t)
	p := newPublisher(t, c)
	if _, err := rawChannel(t, c).QueueDelete(transfersQueue, false, false, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeclareTopology(deliveryLimit) })

	err := p.Publish(t.Context(), event)

	// The broker still confirms an unroutable message; only the return reveals it went nowhere.
	if err == nil || !strings.Contains(err.Error(), "returned") {
		t.Errorf("Publish = %v, want a returned-message error", err)
	}

	// Same message ID again: a leftover return from the first publish would wrongly fail this one.
	if err := c.DeclareTopology(deliveryLimit); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(t.Context(), event); err != nil {
		t.Errorf("publish after the queue is back = %v, want nil", err)
	}
}

func TestDeclareTopologyIsIdempotent(t *testing.T) {
	c := dial(t)

	if err := c.DeclareTopology(deliveryLimit); err != nil {
		t.Errorf("declaring again: %v", err)
	}
	// Different arguments for an existing queue are refused, which proves the limit was set.
	if err := c.DeclareTopology(deliveryLimit + 1); err == nil || !strings.Contains(err.Error(), "PRECONDITION_FAILED") {
		t.Errorf("declaring with another delivery limit = %v, want PRECONDITION_FAILED", err)
	}
}

func TestClosedReportsBrokerErrors(t *testing.T) {
	c := dial(t)
	p := newPublisher(t, c)

	// Publishing to an exchange that doesn't exist makes the broker close the channel.
	_ = p.ch.PublishWithContext(t.Context(), "no-such-exchange", "", false, false, amqp.Publishing{})

	select {
	case err := <-c.Closed():
		if !strings.Contains(err.Error(), "NOT_FOUND") {
			t.Errorf("Closed reported %v, want NOT_FOUND", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Closed reported nothing after the broker closed the channel")
	}
}

func TestCloseReportsNothing(t *testing.T) {
	c, err := Dial(brokerURL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-c.Closed():
		t.Errorf("Closed reported %v after our own Close", err)
	case <-time.After(200 * time.Millisecond):
	}
	if c.IsOpen() {
		t.Error("IsOpen = true after Close")
	}
}

func TestPublishIgnoresReturnsForEarlierMessages(t *testing.T) {
	c := dial(t)
	p := newPublisher(t, c)
	// A return that arrived after an earlier publish had already given up waiting.
	p.returns <- amqp.Return{MessageId: "41", ReplyCode: 312, ReplyText: "NO_ROUTE"}

	if err := p.Publish(t.Context(), event); err != nil {
		t.Errorf("Publish = %v, want nil: the stale return belongs to message 41", err)
	}
}
