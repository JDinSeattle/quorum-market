// Package redisx wraps the Redis client with the connection handling,
// configuration and health reporting the services share.
package redisx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/JDinSeattle/quorum-market/internal/envx"
)

// Config describes how to reach Redis.
type Config struct {
	Addr     string
	Password string
	DB       int

	PoolSize     int
	MinIdleConns int

	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	DialAttempts int
	DialBackoff  time.Duration
}

// ConfigFromEnv reads REDIS_* settings.
func ConfigFromEnv() Config {
	return Config{
		Addr:     envx.String("REDIS_ADDR", "localhost:6379"),
		Password: envx.String("REDIS_PASSWORD", ""),
		DB:       envx.Int("REDIS_DB", 0),

		PoolSize:     envx.Int("REDIS_POOL_SIZE", 64),
		MinIdleConns: envx.Int("REDIS_MIN_IDLE_CONNS", 8),

		// Short by design. Redis is used for caching and rate limiting here,
		// and both are supposed to make requests faster. A slow Redis that a
		// caller patiently waits on is worse than no Redis at all.
		DialTimeout:  envx.Millis("REDIS_DIAL_TIMEOUT_MS", 2*time.Second),
		ReadTimeout:  envx.Millis("REDIS_READ_TIMEOUT_MS", 200*time.Millisecond),
		WriteTimeout: envx.Millis("REDIS_WRITE_TIMEOUT_MS", 200*time.Millisecond),

		DialAttempts: envx.Int("REDIS_DIAL_ATTEMPTS", 30),
		DialBackoff:  envx.Millis("REDIS_DIAL_BACKOFF_MS", time.Second),
	}
}

// Connect dials Redis, retrying until it answers a PING.
//
// The retry loop is for startup ordering: containers come up in parallel and
// Redis is not reliably ready first. Once running, the client reconnects on
// its own.
func Connect(ctx context.Context, cfg Config) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	})

	var lastErr error
	for attempt := 1; attempt <= cfg.DialAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			_ = client.Close()
			return nil, err
		}

		pingCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
		err := client.Ping(pingCtx).Err()
		cancel()

		if err == nil {
			slog.Info("connected to redis", "addr", cfg.Addr, "attempt", attempt)
			return client, nil
		}
		lastErr = err
		slog.Warn("redis not ready, retrying",
			"addr", cfg.Addr, "attempt", attempt, "of", cfg.DialAttempts, "err", err)

		select {
		case <-ctx.Done():
			_ = client.Close()
			return nil, ctx.Err()
		case <-time.After(cfg.DialBackoff):
		}
	}

	_ = client.Close()
	return nil, fmt.Errorf("redisx: could not reach %s after %d attempts: %w",
		cfg.Addr, cfg.DialAttempts, lastErr)
}

// Probe returns a readiness probe for a Redis client.
func Probe(client *redis.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		if client == nil {
			return fmt.Errorf("redisx: no client")
		}
		return client.Ping(ctx).Err()
	}
}
