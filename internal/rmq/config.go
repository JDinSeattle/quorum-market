// Package rmq wraps the AMQP client with the pieces this system needs:
// a connection that redials itself, a pooled confirming publisher, and a
// multi-worker consumer.
package rmq

import (
	"fmt"
	"net/url"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/envx"
)

// Config describes how to reach the broker and which queue to use.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	VHost    string
	Queue    string

	DialTimeout    time.Duration
	DialAttempts   int
	DialBackoff    time.Duration
	ConfirmTimeout time.Duration
	PoolSize       int
	PoolWait       time.Duration
	Workers        int
	Prefetch       int
}

// ConfigFromEnv reads broker settings, accepting either the RABBITMQ_* or the
// shorter RMQ_* variable names so both services can be configured the same way.
func ConfigFromEnv(defaultQueue string) Config {
	return Config{
		Host:     first(envx.String("RABBITMQ_HOST", ""), envx.String("RMQ_HOST", "localhost")),
		Port:     firstInt(envx.Int("RABBITMQ_PORT", 0), envx.Int("RMQ_PORT", 5672)),
		User:     first(envx.String("RABBITMQ_USER", ""), envx.String("RMQ_USER", "guest")),
		Password: first(envx.String("RABBITMQ_PASS", ""), envx.String("RMQ_PASS", "guest")),
		VHost:    first(envx.String("RABBITMQ_VHOST", ""), envx.String("RMQ_VHOST", "/")),
		Queue:    first(envx.String("RABBITMQ_QUEUE", ""), envx.String("RMQ_QUEUE", defaultQueue)),

		DialTimeout:    envx.Millis("RMQ_DIAL_TIMEOUT_MS", 10*time.Second),
		DialAttempts:   envx.Int("RMQ_DIAL_ATTEMPTS", 30),
		DialBackoff:    envx.Millis("RMQ_DIAL_BACKOFF_MS", 2*time.Second),
		ConfirmTimeout: envx.Millis("RMQ_CONFIRM_TIMEOUT_MS", 5*time.Second),
		PoolSize:       envx.Int("RMQ_POOL_MAX_TOTAL", 50),
		PoolWait:       envx.Millis("RMQ_POOL_MAX_WAIT_MS", 1*time.Second),
		Workers:        envx.Int("RMQ_CONSUMER_WORKERS", 10),
		Prefetch:       envx.Int("RMQ_PREFETCH_COUNT", 10),
	}
}

// URL renders the AMQP connection string, escaping credentials so a password
// containing a slash or an @ cannot corrupt the URL.
func (c Config) URL() string {
	vhost := c.VHost
	if vhost == "/" {
		vhost = ""
	}
	return fmt.Sprintf("amqp://%s:%s@%s:%d/%s",
		url.QueryEscape(c.User), url.QueryEscape(c.Password),
		c.Host, c.Port, url.PathEscape(vhost))
}

// Redacted renders the connection string with the password removed, for logs.
func (c Config) Redacted() string {
	return fmt.Sprintf("amqp://%s:***@%s:%d/%s", c.User, c.Host, c.Port, c.VHost)
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstInt(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
