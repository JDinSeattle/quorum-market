package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// ClientConfig describes how one service calls another. Zero values take
// documented defaults.
type ClientConfig struct {
	// Dependency names the callee. It labels metrics and the circuit breaker,
	// so it must be a bounded, human-meaningful value like "warehouse".
	Dependency string

	ConnectTimeout time.Duration // default 500ms
	RequestTimeout time.Duration // default 5s

	// MaxAttempts bounds retries of idempotent requests. Default 3.
	MaxAttempts    int
	RetryBaseDelay time.Duration // default 50ms
	RetryMaxDelay  time.Duration // default 1s

	Breaker        BreakerConfig
	DisableBreaker bool
}

func (c ClientConfig) withDefaults() ClientConfig {
	if c.Dependency == "" {
		c.Dependency = "unknown"
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 500 * time.Millisecond
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.RetryBaseDelay <= 0 {
		c.RetryBaseDelay = 50 * time.Millisecond
	}
	if c.RetryMaxDelay <= 0 {
		c.RetryMaxDelay = time.Second
	}
	return c
}

// Client is a JSON-over-HTTP client for service-to-service calls.
//
// It adds four things to the stdlib client that a call between services needs
// and a bare http.Client does not provide: the caller's request id travels
// with the call, every attempt is measured, idempotent failures are retried
// with jittered backoff, and a persistently failing dependency is short
// circuited rather than queued behind.
type Client struct {
	hc         *http.Client
	dependency string

	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration

	breaker *Breaker
}

// NewClient builds a client from cfg.
func NewClient(cfg ClientConfig) *Client {
	cfg = cfg.withDefaults()

	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: cfg.ConnectTimeout}).DialContext,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 128,
		IdleConnTimeout:     90 * time.Second,
		// Without this, a hung TLS or header phase is only bounded by the
		// overall request timeout, holding a connection the whole time.
		ResponseHeaderTimeout: cfg.RequestTimeout,
	}

	client := &Client{
		hc:          &http.Client{Transport: transport, Timeout: cfg.RequestTimeout},
		dependency:  cfg.Dependency,
		maxAttempts: cfg.MaxAttempts,
		baseDelay:   cfg.RetryBaseDelay,
		maxDelay:    cfg.RetryMaxDelay,
	}
	if !cfg.DisableBreaker {
		client.breaker = NewBreaker(cfg.Dependency, cfg.Breaker)
	}
	return client
}

// GetJSON issues a GET and decodes a 2xx body into out, which may be nil.
func (c *Client) GetJSON(ctx context.Context, url string, out any) error {
	return c.do(ctx, http.MethodGet, url, nil, out)
}

// PutJSON issues a PUT and decodes a 2xx body into out.
func (c *Client) PutJSON(ctx context.Context, url string, body, out any) error {
	return c.do(ctx, http.MethodPut, url, body, out)
}

// PostJSON issues a POST and decodes a 2xx body into out.
//
// POSTs are never retried automatically. A POST that times out may well have
// been applied: retrying an authorization could charge a customer twice, and
// retrying a reservation could hold stock twice. Recovering from that
// ambiguity requires knowing what the specific call means, which is the
// caller's job, not the transport's.
func (c *Client) PostJSON(ctx context.Context, url string, body, out any) error {
	return c.do(ctx, http.MethodPost, url, body, out)
}

func (c *Client) do(ctx context.Context, method, url string, body, out any) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return Wrap(http.StatusInternalServerError, err, "encoding request for %s", url)
		}
		payload = encoded
	}

	attempts := 1
	if isIdempotent(method) {
		attempts = c.maxAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Wrap(http.StatusServiceUnavailable, err, "calling %s", c.dependency)
		}

		if c.breaker != nil {
			if err := c.breaker.Allow(); err != nil {
				return Wrap(http.StatusServiceUnavailable, err,
					"%s is unavailable (circuit open)", c.dependency)
			}
		}

		status, err := c.attempt(ctx, method, url, payload, out)

		if c.breaker != nil {
			var breakerErr error
			if countsAsUnavailable(status, err) {
				breakerErr = err
				if breakerErr == nil {
					breakerErr = errors.New("dependency returned an error status")
				}
			}
			c.breaker.Record(breakerErr)
		}

		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == attempts || !retryable(status, err) {
			break
		}

		obs.ObserveClientRetry(c.dependency)
		slog.DebugContext(ctx, "retrying downstream call",
			"dependency", c.dependency, "attempt", attempt, "status", status, "err", err)

		if waitErr := sleep(ctx, c.backoff(attempt)); waitErr != nil {
			return Wrap(http.StatusServiceUnavailable, waitErr, "calling %s", c.dependency)
		}
	}
	return lastErr
}

// attempt performs one request and returns the response status alongside any
// error, so the caller can tell a transport failure (status 0) from a status
// the server actually chose.
func (c *Client) attempt(ctx context.Context, method, url string, payload []byte, out any) (int, error) {
	var reader io.Reader
	if payload != nil {
		// A fresh reader per attempt: a consumed body cannot be replayed.
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, Wrap(http.StatusInternalServerError, err, "building request for %s", url)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Carry the caller's identity so one customer's checkout can be traced
	// across every service it touches.
	if id := obs.RequestIDFrom(ctx); id != "" {
		req.Header.Set(obs.RequestIDHeader, id)
	}

	started := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		obs.ObserveClientRequest(c.dependency, method, 0, time.Since(started))
		return 0, Wrap(http.StatusServiceUnavailable, err, "calling %s", c.dependency)
	}
	defer func() {
		// Drain before closing so the connection can go back to the idle pool
		// instead of being torn down and redialled on the next call.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	obs.ObserveClientRequest(c.dependency, method, resp.StatusCode, time.Since(started))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The upstream status is preserved so the caller can branch on it.
		return resp.StatusCode, Errorf(resp.StatusCode, "%s %s returned %d", method, url, resp.StatusCode)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBody)).Decode(out); err != nil {
		return resp.StatusCode, Wrap(http.StatusBadGateway, err, "decoding response from %s", c.dependency)
	}
	return resp.StatusCode, nil
}

// backoff returns a full-jitter delay.
//
// Jitter is the point. Without it every caller that failed at the same moment
// retries at the same moment, and the dependency that just came back up is hit
// by a synchronised wave and falls over again.
func (c *Client) backoff(attempt int) time.Duration {
	ceiling := c.baseDelay << (attempt - 1)
	if ceiling > c.maxDelay || ceiling <= 0 {
		ceiling = c.maxDelay
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// isIdempotent reports whether repeating a method is safe.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

// retryable reports whether another attempt could plausibly succeed. A 404 or
// a 409 is a considered answer, and asking again will only produce it faster.
func retryable(status int, err error) bool {
	if status == 0 && err != nil {
		return true // never reached the server
	}
	return status >= 500 || status == http.StatusTooManyRequests
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
