package rmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ErrClosed is returned once a Conn has been shut down.
var ErrClosed = errors.New("rmq: connection closed")

// Conn is a self-healing AMQP connection.
//
// The AMQP client does not reconnect on its own: when the broker restarts or a
// network blip drops the TCP connection, every channel opened from it is dead
// and stays dead. Since the broker and the services are separate containers
// that can restart independently, a connection that gives up permanently would
// mean a cart service that silently stops shipping orders until someone
// notices. Conn watches for closure and redials with backoff.
type Conn struct {
	cfg Config

	mu     sync.RWMutex
	conn   *amqp.Connection
	closed bool

	done chan struct{}
	once sync.Once
}

// Dial connects to the broker, retrying until it succeeds, ctx is cancelled,
// or the attempt budget runs out. Retrying matters at startup: containers come
// up in parallel and the broker is rarely ready first.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	c := &Conn{cfg: cfg, done: make(chan struct{})}

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	c.conn = conn

	// The dial context doubles as the connection's lifetime: cancelling it
	// stops the reconnect loop along with everything else in the process.
	go c.watch(ctx)
	return c, nil
}

func (c *Conn) dial(ctx context.Context) (*amqp.Connection, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.DialAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		conn, err := amqp.DialConfig(c.cfg.URL(), amqp.Config{
			Heartbeat: 10 * time.Second,
			Dial:      amqp.DefaultDial(c.cfg.DialTimeout),
		})
		if err == nil {
			slog.Info("connected to rabbitmq", "url", c.cfg.Redacted(), "attempt", attempt)
			return conn, nil
		}

		lastErr = err
		slog.Warn("rabbitmq dial failed, retrying",
			"url", c.cfg.Redacted(), "attempt", attempt, "of", c.cfg.DialAttempts, "err", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.cfg.DialBackoff):
		}
	}
	return nil, fmt.Errorf("rmq: could not reach broker after %d attempts: %w", c.cfg.DialAttempts, lastErr)
}

// watch redials whenever the current connection drops.
func (c *Conn) watch(ctx context.Context) {
	for {
		c.mu.RLock()
		conn := c.conn
		c.mu.RUnlock()
		if conn == nil {
			return
		}

		notify := conn.NotifyClose(make(chan *amqp.Error, 1))
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case reason := <-notify:
			if reason == nil {
				return // closed deliberately
			}
			slog.Warn("rabbitmq connection lost, reconnecting", "reason", reason)
		}

		// Close() must also interrupt an in-progress redial, not just a
		// cancelled parent context.
		dialCtx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-c.done:
				cancel()
			case <-dialCtx.Done():
			}
		}()

		fresh, err := c.dial(dialCtx)
		cancel()
		if err != nil {
			slog.Error("rabbitmq reconnect gave up", "err", err)
			return
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			_ = fresh.Close()
			return
		}
		c.conn = fresh
		c.mu.Unlock()
	}
}

// Channel opens a channel on the current connection.
func (c *Conn) Channel() (*amqp.Channel, error) {
	c.mu.RLock()
	conn, closed := c.conn, c.closed
	c.mu.RUnlock()

	if closed || conn == nil {
		return nil, ErrClosed
	}
	return conn.Channel()
}

// Healthy reports whether this process currently holds a live connection.
//
// It answers for *this* connection, not for the broker in general: the
// reconnect loop may be mid-backoff while the broker is perfectly fine, and
// that is still a reason for this instance to report itself degraded.
func (c *Conn) Healthy() error {
	c.mu.RLock()
	conn, closed := c.conn, c.closed
	c.mu.RUnlock()

	switch {
	case closed:
		return ErrClosed
	case conn == nil || conn.IsClosed():
		return errors.New("rmq: not connected to the broker")
	default:
		return nil
	}
}

// Close shuts the connection down and stops the reconnect loop.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.done) })

	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
