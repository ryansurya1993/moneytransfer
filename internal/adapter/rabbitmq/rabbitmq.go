package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ryansurya1993/moneytransfer/internal/outbox"
)

const (
	transfersQueue     = "transfers"
	deadLetterExchange = "transfers.dlx"
	deadLetterQueue    = "transfers.dlq"
)

// Conn is a connection to RabbitMQ.
type Conn struct {
	conn   *amqp.Connection
	closed chan error
	once   sync.Once
}

// Dial connects to the broker at url. The name labels the connection in the management UI.
func Dial(url, name string) (*Conn, error) {
	cfg := amqp.Config{
		Heartbeat:  10 * time.Second,
		Locale:     "en_US",
		Properties: amqp.NewConnectionProperties(),
	}
	cfg.Properties.SetClientConnectionName(name)
	conn, err := amqp.DialConfig(url, cfg)
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}
	c := &Conn{conn: conn, closed: make(chan error, 1)}
	c.watch(conn.NotifyClose(make(chan *amqp.Error, 1)))
	return c, nil
}

// Closed receives one error when the broker or the network ends the connection or a channel.
// A connection we close ourselves reports nothing.
func (c *Conn) Closed() <-chan error {
	return c.closed
}

// IsOpen reports whether the connection is still usable.
func (c *Conn) IsOpen() bool {
	return !c.conn.IsClosed()
}

// Close closes the connection.
func (c *Conn) Close() error {
	if err := c.conn.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
		return fmt.Errorf("close rabbitmq connection: %w", err)
	}
	return nil
}

func (c *Conn) watch(ch <-chan *amqp.Error) {
	go func() {
		// A clean close closes ch without sending an error.
		if err, ok := <-ch; ok {
			c.once.Do(func() { c.closed <- err })
		}
	}()
}

// DeclareTopology creates the transfers queue and its dead-letter queue if they don't exist.
// Every process that uses the queues declares them at startup, so start order doesn't matter.
// RabbitMQ refuses a declaration whose arguments differ from the existing queue's.
func (c *Conn) DeclareTopology(deliveryLimit int) error {
	ch, err := c.conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer func() { _ = ch.Close() }()

	if err := ch.ExchangeDeclare(deadLetterExchange, amqp.ExchangeFanout, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", deadLetterExchange, err)
	}
	if _, err := ch.QueueDeclare(deadLetterQueue, true, false, false, false, amqp.Table{
		amqp.QueueTypeArg: amqp.QueueTypeQuorum,
	}); err != nil {
		return fmt.Errorf("declare queue %s: %w", deadLetterQueue, err)
	}
	if err := ch.QueueBind(deadLetterQueue, "", deadLetterExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %s: %w", deadLetterQueue, err)
	}
	if _, err := ch.QueueDeclare(transfersQueue, true, false, false, false, amqp.Table{
		amqp.QueueTypeArg:        amqp.QueueTypeQuorum,
		"x-delivery-limit":       int64(deliveryLimit),
		"x-dead-letter-exchange": deadLetterExchange,
		// The default, at-most-once, drops a dead letter if the dead-letter queue can't take it.
		// At-least-once requires reject-publish overflow.
		"x-dead-letter-strategy": "at-least-once",
		"x-overflow":             "reject-publish",
	}); err != nil {
		return fmt.Errorf("declare queue %s: %w", transfersQueue, err)
	}
	return nil
}

var _ outbox.Publisher = (*Publisher)(nil)

// Publisher sends outbox events to the transfers queue and waits for the broker to confirm each.
type Publisher struct {
	mu      sync.Mutex // one message in flight, so a return or confirm always belongs to it
	ch      *amqp.Channel
	returns chan amqp.Return
}

// NewPublisher opens a channel in confirm mode.
func (c *Conn) NewPublisher() (*Publisher, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	c.watch(ch.NotifyClose(make(chan *amqp.Error, 1)))
	return &Publisher{ch: ch, returns: ch.NotifyReturn(make(chan amqp.Return, 1))}, nil
}

// Publish implements outbox.Publisher.
func (p *Publisher) Publish(ctx context.Context, e outbox.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	msgID := strconv.FormatInt(e.ID, 10)
	// mandatory: without it a message that reaches no queue is dropped and still confirmed.
	conf, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, "", transfersQueue, true, false, amqp.Publishing{
		DeliveryMode:  amqp.Persistent,
		ContentType:   "application/json",
		MessageId:     msgID,
		CorrelationId: e.CorrelationID,
		Type:          e.Type,
		Timestamp:     e.CreatedAt,
		Body:          e.Payload,
	})
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	acked, err := conf.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("wait for confirm: %w", err)
	}
	// The broker sends basic.return before the confirm, so a return for this message is already here.
	if r, ok := p.returned(msgID); ok {
		return fmt.Errorf("message returned by broker: %d %s", r.ReplyCode, r.ReplyText)
	}
	if !acked {
		return errors.New("broker rejected the message")
	}
	return nil
}

// returned picks up a return for msgID.
// Returns for earlier messages whose confirm timed out are skipped.
func (p *Publisher) returned(msgID string) (amqp.Return, bool) {
	for {
		select {
		case r := <-p.returns:
			if r.MessageId == msgID {
				return r, true
			}
		default:
			return amqp.Return{}, false
		}
	}
}
