// Package ratelimit implements a distributed sliding-window rate limiter.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// slidingWindow decides and records in a single round trip.
//
// The check and the record have to be atomic. Doing them as separate commands
// lets N concurrent requests all read a count below the limit and all admit
// themselves, which is exactly the burst the limiter exists to prevent. A Lua
// script runs on the server with nothing interleaved.
//
// A sorted set of request timestamps gives a true sliding window rather than
// the fixed buckets a plain counter produces. Fixed buckets allow twice the
// limit across a boundary — the classic failure where a "60 per minute" limit
// passes 120 requests in two seconds.
var slidingWindow = redis.NewScript(`
local key    = KEYS[1]
local window = tonumber(ARGV[1])
local limit  = tonumber(ARGV[2])
local member = ARGV[3]

-- Redis is the clock, not the caller.
--
-- Taking the timestamp from whichever instance happens to handle the request
-- makes the window's edges depend on the clock skew between them: a gateway
-- running a second fast would expire other instances' entries early and hand
-- out extra budget. One authoritative clock removes the whole class of problem.
local t   = redis.call('TIME')
local now = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

-- Drop everything that has aged out of the window.
redis.call('ZREMRANGEBYSCORE', key, 0, now - window)

local used = redis.call('ZCARD', key)
if used < limit then
  redis.call('ZADD', key, now, member)
  -- Expire the key itself so an idle client leaves nothing behind.
  redis.call('PEXPIRE', key, window)
  return {1, limit - used - 1, 0}
end

-- Refused: the caller may retry once the oldest request leaves the window.
local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local retry  = window - (now - tonumber(oldest[2]))
redis.call('PEXPIRE', key, window)
return {0, 0, retry}
`)

// Result is one limiter verdict.
type Result struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

// Options configure a Limiter.
type Options struct {
	// Scope labels the limiter's metrics, e.g. "gateway".
	Scope string
	// Prefix namespaces keys inside a shared Redis instance.
	Prefix string
	// Limit is how many requests are allowed per Window.
	Limit int
	// Window is the period the limit applies over.
	Window time.Duration
	// FailOpen admits traffic when Redis cannot be reached.
	//
	// Defaults to true, which is the right trade for a throughput limiter
	// protecting capacity: a Redis outage should not take the whole API down.
	// A limiter guarding against abuse rather than load wants the opposite,
	// and should set this false so an attacker cannot bypass it by knocking
	// Redis over.
	FailOpen *bool
}

func (o Options) withDefaults() Options {
	if o.Scope == "" {
		o.Scope = "default"
	}
	if o.Limit <= 0 {
		o.Limit = 100
	}
	if o.Window <= 0 {
		o.Window = time.Minute
	}
	if o.FailOpen == nil {
		open := true
		o.FailOpen = &open
	}
	return o
}

// Limiter enforces a request budget shared across every instance of a service.
//
// A per-instance limiter cannot do this job: with four instances behind a load
// balancer, a "100 per minute" limit becomes 400, and it changes every time
// the group scales. The budget has to live somewhere all the instances can see.
type Limiter struct {
	rdb  *redis.Client
	opts Options
}

// New returns a Limiter. A nil client disables limiting, which is what makes
// it optional in environments without Redis.
func New(rdb *redis.Client, opts Options) *Limiter {
	return &Limiter{rdb: rdb, opts: opts.withDefaults()}
}

// Enabled reports whether limiting is active.
func (l *Limiter) Enabled() bool { return l != nil && l.rdb != nil }

// Allow decides whether one request from identity may proceed.
func (l *Limiter) Allow(ctx context.Context, identity string) Result {
	unlimited := Result{Allowed: true, Limit: l.opts.Limit, Remaining: l.opts.Limit}
	if !l.Enabled() {
		return unlimited
	}

	// The member has to be unique per request, or two requests landing in the
	// same millisecond collapse into one sorted-set entry and only cost the
	// budget once.
	member := fmt.Sprintf("%d-%s", time.Now().UnixNano(), randomSuffix())

	raw, err := slidingWindow.Run(ctx, l.rdb,
		[]string{l.opts.Prefix + identity},
		l.opts.Window.Milliseconds(), l.opts.Limit, member,
	).Slice()
	if err != nil {
		obs.ObserveRateLimit(l.opts.Scope, "error")
		slog.WarnContext(ctx, "rate limiter unavailable",
			"scope", l.opts.Scope, "fail_open", *l.opts.FailOpen, "err", err)

		if *l.opts.FailOpen {
			return unlimited
		}
		return Result{Allowed: false, Limit: l.opts.Limit, RetryAfter: time.Second}
	}

	result, err := parse(raw, l.opts.Limit)
	if err != nil {
		obs.ObserveRateLimit(l.opts.Scope, "error")
		return unlimited
	}

	if result.Allowed {
		obs.ObserveRateLimit(l.opts.Scope, "allowed")
	} else {
		obs.ObserveRateLimit(l.opts.Scope, "throttled")
	}
	return result
}

func parse(raw []any, limit int) (Result, error) {
	if len(raw) != 3 {
		return Result{}, errors.New("ratelimit: unexpected script result")
	}

	allowed, ok1 := raw[0].(int64)
	remaining, ok2 := raw[1].(int64)
	retryMS, ok3 := raw[2].(int64)
	if !ok1 || !ok2 || !ok3 {
		return Result{}, errors.New("ratelimit: unexpected script result types")
	}

	return Result{
		Allowed:    allowed == 1,
		Limit:      limit,
		Remaining:  int(remaining),
		RetryAfter: time.Duration(retryMS) * time.Millisecond,
	}, nil
}
