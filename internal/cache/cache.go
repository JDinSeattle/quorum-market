// Package cache provides a read-through cache over Redis.
package cache

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// ErrNotFound tells the cache that the underlying record does not exist, so it
// can remember the absence instead of asking again on every request.
var ErrNotFound = errors.New("cache: record not found")

// tombstone marks a key the loader reported as missing.
const tombstone = "\x00__absent__"

// Options tune a Cache. Zero values take documented defaults.
type Options struct {
	// Name labels this cache's metrics.
	Name string
	// Prefix namespaces keys inside a shared Redis instance.
	Prefix string
	// TTL is how long a loaded value is kept. Default 5 minutes.
	TTL time.Duration
	// NegativeTTL is how long an absence is remembered. Default 30 seconds.
	NegativeTTL time.Duration
	// Jitter is the fraction of TTL randomly subtracted from each entry.
	// Default 0.2.
	Jitter float64
}

func (o Options) withDefaults() Options {
	if o.Name == "" {
		o.Name = "default"
	}
	if o.TTL <= 0 {
		o.TTL = 5 * time.Minute
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = 30 * time.Second
	}
	if o.Jitter <= 0 {
		o.Jitter = 0.2
	}
	return o
}

// Cache is a read-through cache.
//
// Three behaviours here matter more than the caching itself.
//
// It collapses concurrent misses. When a popular key expires, every in-flight
// request for it misses at the same instant and they all stampede the backing
// store — precisely when it is busiest. Singleflight lets one of them load and
// the rest wait for that result.
//
// It remembers absences. A hot key that does not exist — a crawler walking
// product ids, a stale link — would otherwise miss on every single request and
// pass all of it straight through.
//
// It fails open. If Redis is unreachable the loader is called directly, so a
// cache outage costs latency rather than availability. A cache that takes the
// site down with it is worse than no cache.
type Cache struct {
	rdb  *redis.Client
	opts Options

	// group collapses concurrent loads of the same key.
	group singleflight.Group
}

// New returns a Cache backed by rdb. A nil client disables caching entirely
// and every read goes to the loader, which is what makes the cache optional.
func New(rdb *redis.Client, opts Options) *Cache {
	return &Cache{rdb: rdb, opts: opts.withDefaults()}
}

// Enabled reports whether a backing store is configured.
func (c *Cache) Enabled() bool { return c != nil && c.rdb != nil }

// Loader produces a value when it is not cached. Returning ErrNotFound records
// the absence rather than treating it as a failure.
type Loader func(ctx context.Context) ([]byte, error)

// GetOrLoad returns the cached bytes for key, loading and storing them if
// necessary.
func (c *Cache) GetOrLoad(ctx context.Context, key string, load Loader) ([]byte, error) {
	if !c.Enabled() {
		return load(ctx)
	}

	full := c.opts.Prefix + key

	switch cached, err := c.rdb.Get(ctx, full).Bytes(); {
	case err == nil:
		if string(cached) == tombstone {
			obs.ObserveCache(c.opts.Name, "hit_negative")
			return nil, ErrNotFound
		}
		obs.ObserveCache(c.opts.Name, "hit")
		return cached, nil

	case errors.Is(err, redis.Nil):
		obs.ObserveCache(c.opts.Name, "miss")

	case ctx.Err() != nil:
		// The caller hung up. That is not a cache failure, and counting it as
		// one would inflate the error rate — and fire the "cache is failing
		// open" alert — every time a client disconnects mid-request. Falling
		// through to the loader would also be pointless: it would be handed
		// the same dead context.
		obs.ObserveCache(c.opts.Name, "abandoned")
		return nil, ctx.Err()

	default:
		// Redis is genuinely unwell. Say so once and carry on to the loader
		// rather than failing a request the backing store could have served.
		obs.ObserveCache(c.opts.Name, "error")
		slog.WarnContext(ctx, "cache read failed, falling through to the loader",
			"cache", c.opts.Name, "key", key, "err", err)
		return load(ctx)
	}

	// Only one caller loads a given key; the rest share its result.
	value, err, shared := c.group.Do(full, func() (any, error) {
		loaded, loadErr := load(ctx)
		if loadErr != nil {
			if errors.Is(loadErr, ErrNotFound) {
				c.store(ctx, full, []byte(tombstone), c.opts.NegativeTTL)
			}
			return nil, loadErr
		}
		c.store(ctx, full, loaded, c.opts.TTL)
		return loaded, nil
	})
	if shared {
		obs.ObserveCache(c.opts.Name, "coalesced")
	}
	if err != nil {
		return nil, err
	}

	bytes, ok := value.([]byte)
	if !ok {
		return nil, errors.New("cache: loader returned an unexpected type")
	}
	return bytes, nil
}

// Invalidate drops a key, so a write is visible on the next read instead of
// after the TTL.
func (c *Cache) Invalidate(ctx context.Context, key string) {
	if !c.Enabled() {
		return
	}
	if err := c.rdb.Del(ctx, c.opts.Prefix+key).Err(); err != nil {
		// The entry still expires on its own, so this is staleness rather than
		// corruption — worth a log line, not a failed write.
		obs.ObserveCache(c.opts.Name, "error")
		slog.WarnContext(ctx, "cache invalidation failed; the entry will expire on its own",
			"cache", c.opts.Name, "key", key, "err", err)
		return
	}
	obs.ObserveCache(c.opts.Name, "invalidated")
}

func (c *Cache) store(ctx context.Context, key string, value []byte, ttl time.Duration) {
	// Writing on a context detached from the request: the caller already has
	// its answer, and cancelling the store would leave the cache permanently
	// cold for keys whose first requester hung up.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()

	if err := c.rdb.Set(storeCtx, key, value, c.jitter(ttl)).Err(); err != nil {
		obs.ObserveCache(c.opts.Name, "error")
		slog.WarnContext(ctx, "cache write failed", "cache", c.opts.Name, "key", key, "err", err)
	}
}

// jitter spreads expiry times out.
//
// A batch of keys loaded together — everything touched by the first request
// after a deploy — would otherwise expire together and stampede together. A
// fifth of the TTF, randomised, is enough to decorrelate them.
func (c *Cache) jitter(ttl time.Duration) time.Duration {
	spread := float64(ttl) * c.opts.Jitter
	return ttl - time.Duration(rand.Float64()*spread)
}
