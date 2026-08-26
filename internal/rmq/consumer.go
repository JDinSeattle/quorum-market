package rmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// ErrDrop tells the consumer a message can never succeed and must not be
// requeued — a body that fails to parse, for instance. Requeueing such a
// message puts it straight back at the head of the queue and the worker spins
// on it forever, starving every valid order behind it.
var ErrDrop = errors.New("rmq: drop message")

// Handler processes one message body.
type Handler func(ctx context.Context, body []byte) error

// Consumer runs a pool of workers against one queue.
//
// Each worker holds its own channel with its own prefetch window, so a slow
// message blocks only the worker handling it. Acknowledgement is manual and
// happens after the handler returns, so a crash mid-processing leaves the
// message on the queue for another worker to pick up.
type Consumer struct {
	conn     *Conn
	queue    string
	workers  int
	prefetch int
	handler  Handler

	// declare prepares this consumer's topology: a work queue on its own, or a
	// private queue bound to the shared event exchange.
	declare func(*amqp.Channel) error

	processed    atomic.Uint64
	deadLettered atomic.Uint64
	requeued     atomic.Uint64
}

// NewConsumer returns a Consumer that feeds messages to handler.
func NewConsumer(conn *Conn, cfg Config, handler Handler) *Consumer {
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	prefetch := cfg.Prefetch
	if prefetch < 1 {
		prefetch = 1
	}
	queue := cfg.Queue
	return &Consumer{
		conn:     conn,
		queue:    queue,
		workers:  workers,
		prefetch: prefetch,
		handler:  handler,
		declare:  func(ch *amqp.Channel) error { return DeclareTopology(ch, queue) },
	}
}

// NewSubscriber returns a Consumer bound to the shared event exchange.
//
// The queue belongs to this subscriber alone, so every subscriber sees every
// matching event and a slow one builds its own backlog rather than starving
// the others.
func NewSubscriber(conn *Conn, cfg Config, exchange, queue string, patterns []string, handler Handler) *Consumer {
	c := NewConsumer(conn, cfg, handler)
	c.queue = queue
	c.declare = func(ch *amqp.Channel) error {
		return DeclareSubscription(ch, exchange, queue, patterns)
	}
	return c
}

// Run starts the workers and blocks until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < c.workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.worker(ctx, n)
		}(i)
	}
	slog.Info("consumer started", "queue", c.queue, "workers", c.workers, "prefetch", c.prefetch)

	wg.Wait()
	slog.Info("consumer stopped", "queue", c.queue, "stats", c.Stats())
}

// worker keeps one subscription alive, re-establishing it after a channel or
// connection failure rather than exiting and silently reducing throughput.
func (c *Consumer) worker(ctx context.Context, n int) {
	backoff := 500 * time.Millisecond
	const maxBackoff = 10 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		err := c.subscribe(ctx, n)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("consumer worker lost its subscription",
				"worker", n, "queue", c.queue, "retry_in", backoff, "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Consumer) subscribe(ctx context.Context, n int) error {
	ch, err := c.conn.Channel()
	if err != nil {
		return fmt.Errorf("opening channel: %w", err)
	}
	defer func() { _ = ch.Close() }()

	if err := c.declare(ch); err != nil {
		return err
	}
	if err := ch.Qos(c.prefetch, 0, false); err != nil {
		return fmt.Errorf("setting qos: %w", err)
	}

	tag := fmt.Sprintf("%s-worker-%d", c.queue, n)
	deliveries, err := ch.ConsumeWithContext(ctx, c.queue, tag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("subscribing: %w", err)
	}

	closed := ch.NotifyClose(make(chan *amqp.Error, 1))

	for {
		select {
		case <-ctx.Done():
			return nil
		case reason := <-closed:
			return fmt.Errorf("channel closed: %w", reason)
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("delivery stream ended")
			}
			c.handle(ctx, delivery)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, d amqp.Delivery) {
	obs.QueueInFlightAdd(c.queue, 1)
	defer obs.QueueInFlightAdd(c.queue, -1)

	err := c.handler(ctx, d.Body)

	switch {
	case err == nil:
		c.ack(d)
		c.processed.Add(1)
		obs.ObserveQueueMessage(c.queue, "acked")

	case errors.Is(err, ErrDrop):
		// Unprocessable: park it on the dead-letter queue rather than acking
		// it away. The order is still recoverable there once someone works out
		// why it could not be parsed.
		c.deadLetter(d)
		slog.ErrorContext(ctx, "parking unprocessable message",
			"queue", c.queue, "dlq", DeadLetterQueue(c.queue), "err", err)

	case d.Redelivered:
		// Already retried once and failed again. Retrying forever would turn a
		// permanent failure into an infinite loop that starves the queue.
		c.deadLetter(d)
		slog.ErrorContext(ctx, "parking message after a failed retry",
			"queue", c.queue, "dlq", DeadLetterQueue(c.queue), "err", err)

	default:
		if nackErr := d.Nack(false, true); nackErr != nil {
			slog.Error("nack failed", "queue", c.queue, "err", nackErr)
		}
		c.requeued.Add(1)
		obs.ObserveQueueMessage(c.queue, "requeued")
		slog.WarnContext(ctx, "requeueing message after a failure", "queue", c.queue, "err", err)
	}
}

// deadLetter rejects a message without requeueing it, which routes it to the
// dead-letter exchange declared alongside the work queue.
func (c *Consumer) deadLetter(d amqp.Delivery) {
	if err := d.Nack(false, false); err != nil {
		slog.Error("could not dead-letter message", "queue", c.queue, "err", err)
	}
	c.deadLettered.Add(1)
	obs.ObserveQueueMessage(c.queue, "dead_lettered")
}

func (c *Consumer) ack(d amqp.Delivery) {
	if err := d.Ack(false); err != nil {
		slog.Error("ack failed", "queue", c.queue, "err", err)
	}
}

// Stats reports lifetime message counters.
func (c *Consumer) Stats() map[string]any {
	return map[string]any{
		"queue":         c.queue,
		"processed":     c.processed.Load(),
		"requeued":      c.requeued.Load(),
		"dead_lettered": c.deadLettered.Load(),
		"dlq":           DeadLetterQueue(c.queue),
	}
}
