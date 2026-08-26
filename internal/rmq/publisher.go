package rmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// ErrPoolExhausted means every pooled channel was busy for longer than the
// configured wait. Failing fast here is deliberate: blocking indefinitely on a
// channel would convert broker slowness into unbounded checkout latency.
var ErrPoolExhausted = errors.New("rmq: channel pool exhausted")

// Publisher publishes to one queue using a bounded pool of confirming channels.
//
// Opening a channel per publish is wasteful (it is a synchronous round trip to
// the broker) and sharing one channel across goroutines serialises every
// publish behind a single mutex. A pool gives concurrency with a hard ceiling
// on how many channels the broker has to track.
//
// Every channel runs in confirm mode, so Publish only returns nil once the
// broker has taken responsibility for the message. Without confirms a publish
// is fire-and-forget into a socket buffer and an order can vanish on a broker
// restart with nothing logged.
type Publisher struct {
	conn *Conn

	// exchange is empty for a work queue, which routes on the default
	// exchange by queue name, and set for the topic exchange events go to.
	exchange string
	// routingKey is the queue name for a work queue, or the default event type
	// for a topic publisher.
	routingKey string
	// declare prepares the topology this publisher needs, so whichever side
	// starts first creates it.
	declare func(*amqp.Channel) error
	// name labels this publisher's metrics.
	name string

	// pool holds exactly size slots for the publisher's lifetime. A nil slot
	// is an unused allowance to open a channel, which keeps creation lazy
	// while capping the total at size.
	pool chan *amqp.Channel

	confirmTimeout time.Duration
	poolWait       time.Duration

	closeOnce sync.Once
	closed    chan struct{}
}

// NewPublisher returns a publisher for a work queue: one message, one handler.
func NewPublisher(conn *Conn, cfg Config) (*Publisher, error) {
	queue := cfg.Queue
	return newPublisher(conn, cfg, "", queue, queue, func(ch *amqp.Channel) error {
		return DeclareTopology(ch, queue)
	})
}

// NewTopicPublisher returns a publisher for domain events: one message, any
// number of independent subscribers.
func NewTopicPublisher(conn *Conn, cfg Config, exchange string) (*Publisher, error) {
	return newPublisher(conn, cfg, exchange, "", exchange, func(ch *amqp.Channel) error {
		return DeclareTopic(ch, exchange)
	})
}

func newPublisher(conn *Conn, cfg Config, exchange, routingKey, name string,
	declare func(*amqp.Channel) error) (*Publisher, error) {

	size := cfg.PoolSize
	if size < 1 {
		size = 1
	}

	p := &Publisher{
		conn:           conn,
		exchange:       exchange,
		routingKey:     routingKey,
		declare:        declare,
		name:           name,
		pool:           make(chan *amqp.Channel, size),
		confirmTimeout: cfg.ConfirmTimeout,
		poolWait:       cfg.PoolWait,
		closed:         make(chan struct{}),
	}
	for i := 0; i < size; i++ {
		p.pool <- nil
	}

	// Declaring here means the topology exists before the first message, so a
	// message published before any consumer starts is still durably stored
	// rather than silently discarded by the broker.
	if err := p.declareTopology(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Publisher) declareTopology() error {
	ch, err := p.conn.Channel()
	if err != nil {
		return fmt.Errorf("rmq: opening channel to declare %q: %w", p.name, err)
	}
	defer func() { _ = ch.Close() }()

	return p.declare(ch)
}

// Publish sends body using this publisher's default routing key.
func (p *Publisher) Publish(ctx context.Context, body []byte) error {
	return p.PublishKey(ctx, p.routingKey, body)
}

// PublishKey sends body with an explicit routing key, which is how an event
// publisher chooses the event type subscribers bind to.
func (p *Publisher) PublishKey(ctx context.Context, routingKey string, body []byte) (err error) {
	select {
	case <-p.closed:
		return ErrClosed
	default:
	}

	started := time.Now()
	defer func() { obs.ObserveQueuePublish(p.name, time.Since(started), err) }()

	ch, err := p.borrow()
	if err != nil {
		return err
	}

	healthy := false
	defer func() { p.giveBack(ch, healthy) }()

	confirmCtx, cancel := context.WithTimeout(ctx, p.confirmTimeout)
	defer cancel()

	confirmation, pubErr := ch.PublishWithDeferredConfirmWithContext(confirmCtx,
		p.exchange, // empty for a work queue, the topic exchange for events
		routingKey,
		false, // mandatory
		false, // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
			Body:         body,
		})
	if pubErr != nil {
		return fmt.Errorf("rmq: publishing to %q: %w", p.name, pubErr)
	}

	acked, confirmErr := confirmation.WaitContext(confirmCtx)
	if confirmErr != nil {
		return fmt.Errorf("rmq: waiting for confirm on %q: %w", p.name, confirmErr)
	}
	if !acked {
		return fmt.Errorf("rmq: broker nacked message on %q", p.name)
	}

	healthy = true
	return nil
}

func (p *Publisher) borrow() (*amqp.Channel, error) {
	timer := time.NewTimer(p.poolWait)
	defer timer.Stop()

	var slot *amqp.Channel
	select {
	case slot = <-p.pool:
	case <-timer.C:
		return nil, ErrPoolExhausted
	case <-p.closed:
		return nil, ErrClosed
	}

	if slot != nil && !slot.IsClosed() {
		return slot, nil
	}
	if slot != nil {
		_ = slot.Close() // dead, most likely the connection dropped under it
	}

	ch, err := p.openConfirming()
	if err != nil {
		p.pool <- nil // hand the allowance back so the pool does not shrink
		return nil, err
	}
	return ch, nil
}

func (p *Publisher) openConfirming() (*amqp.Channel, error) {
	ch, err := p.conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("rmq: opening channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("rmq: enabling publisher confirms: %w", err)
	}
	return ch, nil
}

// giveBack returns a channel to the pool, discarding it if the publish that
// borrowed it failed. A channel that errored may have unresolved confirms
// outstanding, and reusing it would attribute the next publish's confirm to
// the wrong message.
func (p *Publisher) giveBack(ch *amqp.Channel, healthy bool) {
	if ch != nil && (!healthy || ch.IsClosed()) {
		_ = ch.Close()
		ch = nil
	}
	select {
	case p.pool <- ch:
	default:
		// Cannot happen: the pool has one slot per outstanding borrow.
		if ch != nil {
			_ = ch.Close()
		}
	}
}

// Close discards every pooled channel.
func (p *Publisher) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })

	for i := 0; i < cap(p.pool); i++ {
		select {
		case ch := <-p.pool:
			if ch != nil {
				_ = ch.Close()
			}
		case <-time.After(time.Second):
			slog.Warn("timed out reclaiming publisher channels")
			return nil
		}
	}
	return nil
}
