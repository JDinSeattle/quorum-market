package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func testCache(t *testing.T, opts Options) (*Cache, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	if opts.Name == "" {
		opts.Name = "test"
	}
	return New(client, opts), server
}

func TestSecondReadIsServedFromTheCache(t *testing.T) {
	c, _ := testCache(t, Options{Prefix: "p:"})

	var loads atomic.Int32
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte("value"), nil
	}

	for i := 0; i < 3; i++ {
		got, err := c.GetOrLoad(context.Background(), "k", load)
		if err != nil {
			t.Fatalf("GetOrLoad: %v", err)
		}
		if string(got) != "value" {
			t.Fatalf("value = %q", got)
		}
	}

	if got := loads.Load(); got != 1 {
		t.Errorf("the loader ran %d times, want 1", got)
	}
}

// A hot key that does not exist — a crawler walking ids, a stale link — would
// otherwise miss on every request and pass all of it to the database.
func TestAbsencesAreRemembered(t *testing.T) {
	c, _ := testCache(t, Options{Prefix: "p:", NegativeTTL: time.Minute})

	var loads atomic.Int32
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return nil, ErrNotFound
	}

	for i := 0; i < 3; i++ {
		_, err := c.GetOrLoad(context.Background(), "ghost", load)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}

	if got := loads.Load(); got != 1 {
		t.Errorf("the loader ran %d times for a missing key, want 1", got)
	}
}

// When a popular key expires, every in-flight request for it misses at the
// same instant. Without collapsing they all stampede the database at once —
// precisely when it is busiest.
func TestConcurrentMissesCollapseIntoOneLoad(t *testing.T) {
	c, _ := testCache(t, Options{Prefix: "p:"})

	var loads atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		<-release // hold the first loader open so the others pile up behind it
		return []byte("value"), nil
	}

	const callers = 25
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetOrLoad(context.Background(), "hot", load); err != nil {
				t.Errorf("GetOrLoad: %v", err)
			}
		}()
	}

	// Give them all time to arrive and find the key missing.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := loads.Load(); got > 2 {
		t.Errorf("%d concurrent misses caused %d loads, want the stampede collapsed", callers, got)
	}
}

// A cache that takes the site down with it is worse than no cache.
func TestAnUnreachableCacheFallsThroughToTheLoader(t *testing.T) {
	c, server := testCache(t, Options{Prefix: "p:"})
	server.Close() // Redis is gone

	got, err := c.GetOrLoad(context.Background(), "k", func(context.Context) ([]byte, error) {
		return []byte("from the database"), nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad failed when Redis was down: %v", err)
	}
	if string(got) != "from the database" {
		t.Errorf("value = %q", got)
	}
}

func TestInvalidationForcesAReload(t *testing.T) {
	c, _ := testCache(t, Options{Prefix: "p:"})
	ctx := context.Background()

	value := []byte("first")
	load := func(context.Context) ([]byte, error) { return value, nil }

	if _, err := c.GetOrLoad(ctx, "k", load); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}

	value = []byte("second")
	if got, _ := c.GetOrLoad(ctx, "k", load); string(got) != "first" {
		t.Fatalf("value = %q, want the cached first value", got)
	}

	c.Invalidate(ctx, "k")

	if got, _ := c.GetOrLoad(ctx, "k", load); string(got) != "second" {
		t.Errorf("value = %q after invalidation, want second", got)
	}
}

func TestEntriesExpire(t *testing.T) {
	c, server := testCache(t, Options{Prefix: "p:", TTL: time.Minute, Jitter: 0.01})
	ctx := context.Background()

	var loads atomic.Int32
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte("value"), nil
	}

	if _, err := c.GetOrLoad(ctx, "k", load); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	server.FastForward(2 * time.Minute)

	if _, err := c.GetOrLoad(ctx, "k", load); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if got := loads.Load(); got != 2 {
		t.Errorf("the loader ran %d times, want 2: the entry did not expire", got)
	}
}

// Without a backing store the cache has to be transparent, so the same code
// runs unchanged in environments that have no Redis.
func TestANilClientDisablesCaching(t *testing.T) {
	c := New(nil, Options{Name: "disabled"})

	if c.Enabled() {
		t.Fatal("a cache with no client reported itself enabled")
	}

	var loads atomic.Int32
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte("value"), nil
	}

	for i := 0; i < 3; i++ {
		if _, err := c.GetOrLoad(context.Background(), "k", load); err != nil {
			t.Fatalf("GetOrLoad: %v", err)
		}
	}
	if got := loads.Load(); got != 3 {
		t.Errorf("the loader ran %d times, want 3", got)
	}
	c.Invalidate(context.Background(), "k") // must not panic
}

// A loader failure must not be cached, or one database blip poisons the key
// for the whole TTL.
func TestLoaderFailuresAreNotCached(t *testing.T) {
	c, _ := testCache(t, Options{Prefix: "p:"})
	ctx := context.Background()

	boom := errors.New("database is down")
	if _, err := c.GetOrLoad(ctx, "k", func(context.Context) ([]byte, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the loader's error", err)
	}

	got, err := c.GetOrLoad(ctx, "k", func(context.Context) ([]byte, error) {
		return []byte("recovered"), nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if string(got) != "recovered" {
		t.Errorf("value = %q: the failure was cached", got)
	}
}

// A caller that hangs up mid-request is not a cache failure. Counting it as
// one inflates the error rate and fires the "cache is failing open" alert
// every time a burst of clients disconnects.
func TestAnAbandonedRequestIsNotACacheError(t *testing.T) {
	c, _ := testCache(t, Options{Prefix: "p:"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone

	var loaded bool
	_, err := c.GetOrLoad(ctx, "k", func(context.Context) ([]byte, error) {
		loaded = true
		return []byte("value"), nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if loaded {
		t.Error("the loader ran with a context that was already dead")
	}
}
