package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func testLimiter(t *testing.T, opts Options) (*Limiter, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	if opts.Scope == "" {
		opts.Scope = "test"
	}
	return New(client, opts), server
}

func TestRequestsAreAllowedUpToTheLimit(t *testing.T) {
	limiter, _ := testLimiter(t, Options{Prefix: "rl:", Limit: 5, Window: time.Minute})
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		result := limiter.Allow(ctx, "alice")
		if !result.Allowed {
			t.Fatalf("request %d was refused while under the limit", i)
		}
		if want := 5 - i; result.Remaining != want {
			t.Errorf("request %d: remaining = %d, want %d", i, result.Remaining, want)
		}
	}

	result := limiter.Allow(ctx, "alice")
	if result.Allowed {
		t.Fatal("the sixth request was allowed past a limit of five")
	}
	if result.RetryAfter <= 0 {
		t.Error("a refused request should say when to come back")
	}
}

// The budget is per identity: one customer exhausting theirs must not affect
// anyone else.
func TestBudgetsAreIndependentPerIdentity(t *testing.T) {
	limiter, _ := testLimiter(t, Options{Prefix: "rl:", Limit: 2, Window: time.Minute})
	ctx := context.Background()

	limiter.Allow(ctx, "alice")
	limiter.Allow(ctx, "alice")
	if limiter.Allow(ctx, "alice").Allowed {
		t.Fatal("alice was allowed past her limit")
	}

	if !limiter.Allow(ctx, "bob").Allowed {
		t.Error("bob was refused because alice used up her own budget")
	}
}

// A true sliding window, not fixed buckets. Fixed buckets allow twice the
// limit across a boundary — the classic failure where "60 per minute" passes
// 120 requests in two seconds.
func TestTheWindowSlidesRatherThanResetting(t *testing.T) {
	limiter, server := testLimiter(t, Options{Prefix: "rl:", Limit: 3, Window: time.Minute})
	ctx := context.Background()

	// The script reads the clock from Redis, so the test has to move Redis's
	// clock rather than Go's. FastForward only ages TTLs; SetTime is what
	// TIME reports.
	start := time.Now()
	server.SetTime(start)

	for i := 0; i < 3; i++ {
		limiter.Allow(ctx, "alice")
	}
	if limiter.Allow(ctx, "alice").Allowed {
		t.Fatal("allowed past the limit")
	}

	// Halfway through the window: the earlier requests are still inside it.
	server.SetTime(start.Add(30 * time.Second))
	if limiter.Allow(ctx, "alice").Allowed {
		t.Error("the budget was refilled halfway through the window")
	}

	// Past the window: the earliest requests have aged out of it.
	server.SetTime(start.Add(61 * time.Second))
	if !limiter.Allow(ctx, "alice").Allowed {
		t.Error("the budget did not recover after the window passed")
	}
}

// Two instances with skewed clocks must agree on the window, which is why the
// script takes its timestamp from Redis rather than from whoever is calling.
func TestTheWindowIsMeasuredByRedisNotTheCaller(t *testing.T) {
	limiter, server := testLimiter(t, Options{Prefix: "rl:", Limit: 2, Window: time.Minute})
	ctx := context.Background()

	// A clock far in the past. If the caller's time were used, entries would
	// be written with ancient scores and immediately trimmed, handing out
	// unlimited budget.
	server.SetTime(time.Now().Add(-24 * time.Hour))

	limiter.Allow(ctx, "alice")
	limiter.Allow(ctx, "alice")

	if limiter.Allow(ctx, "alice").Allowed {
		t.Error("the limit was not enforced when Redis's clock differed from the caller's")
	}
}

// The whole point of doing the check and the record in one script. Without
// atomicity, concurrent requests all read a count below the limit and all
// admit themselves, which is exactly the burst the limiter exists to stop.
func TestConcurrentRequestsCannotExceedTheLimit(t *testing.T) {
	const limit = 20
	limiter, _ := testLimiter(t, Options{Prefix: "rl:", Limit: limit, Window: time.Minute})

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < limit*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.Allow(context.Background(), "alice").Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != limit {
		t.Errorf("%d requests were allowed, want exactly %d", got, limit)
	}
}

// A throughput limiter protects capacity. If Redis is down, refusing all
// traffic converts a cache-tier outage into a full API outage.
func TestUnreachableRedisFailsOpenByDefault(t *testing.T) {
	limiter, server := testLimiter(t, Options{Prefix: "rl:", Limit: 1, Window: time.Minute})
	server.Close()

	for i := 0; i < 5; i++ {
		if !limiter.Allow(context.Background(), "alice").Allowed {
			t.Fatal("traffic was refused because the limiter was unreachable")
		}
	}
}

// A limiter guarding against abuse rather than load wants the opposite, or an
// attacker bypasses it by knocking Redis over.
func TestFailClosedIsAvailableWhenAbuseIsTheConcern(t *testing.T) {
	closed := false
	limiter, server := testLimiter(t, Options{
		Prefix: "rl:", Limit: 1, Window: time.Minute, FailOpen: &closed,
	})
	server.Close()

	if limiter.Allow(context.Background(), "alice").Allowed {
		t.Fatal("a fail-closed limiter admitted traffic while Redis was unreachable")
	}
}

func TestANilClientDisablesLimiting(t *testing.T) {
	limiter := New(nil, Options{Scope: "disabled", Limit: 1})

	if limiter.Enabled() {
		t.Fatal("a limiter with no client reported itself enabled")
	}
	for i := 0; i < 10; i++ {
		if !limiter.Allow(context.Background(), "alice").Allowed {
			t.Fatal("a disabled limiter refused a request")
		}
	}
}

// Two requests landing in the same millisecond must each cost the budget. If
// they collapse into one sorted-set member, one of them goes uncounted.
func TestSimultaneousRequestsAreCountedSeparately(t *testing.T) {
	limiter, _ := testLimiter(t, Options{Prefix: "rl:", Limit: 100, Window: time.Minute})
	ctx := context.Background()

	var last Result
	for i := 0; i < 10; i++ {
		last = limiter.Allow(ctx, "alice")
	}

	if want := 90; last.Remaining != want {
		t.Errorf("remaining = %d after 10 requests, want %d", last.Remaining, want)
	}
}
