package product

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/cache"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
)

// testService stands up a real single-node KV cluster and, optionally, a real
// in-process Redis, so the cache interaction is exercised rather than mocked.
func testService(t *testing.T, withCache bool) (*Service, *kv.Client) {
	t.Helper()

	cfg := kv.Config{
		NodeID: "product-test-db", Mode: kv.ModeLeaderless,
		WriteQuorum: 1, ReadQuorum: 1, RPCTimeout: time.Second,
	}
	kvSvc := kv.NewService(cfg, kv.NewStore(cfg.NodeID), kv.NewReplicator(time.Second))
	server := httptest.NewServer(
		kv.NewServer(kvSvc, kv.NewTxnManager(time.Minute), busywait.Config{}, false).Routes())
	t.Cleanup(server.Close)

	db := kv.NewClient("product-test-db", server.URL, time.Second, 5*time.Second)

	var catalogue *cache.Cache
	if withCache {
		redisServer := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		catalogue = cache.New(rdb, cache.Options{Name: "test-catalogue", Prefix: "product:"})
	}

	return NewService(db, catalogue), db
}

func TestPutThenGet(t *testing.T) {
	svc, _ := testService(t, true)
	ctx := context.Background()

	want := Product{ProductID: "p1", Name: "Desk Lamp", Weight: 2.5, Price: 19.99}
	if err := svc.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := svc.Get(ctx, "p1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Errorf("product = %+v, want %+v", got, want)
	}
}

func TestUnknownProductsAreNotFound(t *testing.T) {
	svc, _ := testService(t, true)

	_, err := svc.Get(context.Background(), "ghost")
	if status := httpx.StatusOf(err); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestGetAndPutValidateTheirInput(t *testing.T) {
	svc, _ := testService(t, true)
	ctx := context.Background()

	if _, err := svc.Get(ctx, ""); httpx.StatusOf(err) != http.StatusBadRequest {
		t.Error("an empty product id was accepted by Get")
	}
	if err := svc.Put(ctx, Product{Weight: 1, Price: 1}); httpx.StatusOf(err) != http.StatusBadRequest {
		t.Error("a product with no id was accepted by Put")
	}
	if err := svc.Put(ctx, Product{ProductID: "p1", Price: -5}); httpx.StatusOf(err) != http.StatusBadRequest {
		t.Error("a negative price was accepted")
	}
	if err := svc.Put(ctx, Product{ProductID: "p1", Weight: -1}); httpx.StatusOf(err) != http.StatusBadRequest {
		t.Error("a negative weight was accepted")
	}
}

// A write has to be visible on the next read. Without invalidation the old
// value is served until the TTL expires, which for a five minute TTL means a
// price change nobody can see.
func TestAWriteIsVisibleImmediately(t *testing.T) {
	svc, _ := testService(t, true)
	ctx := context.Background()

	_ = svc.Put(ctx, Product{ProductID: "p1", Name: "Lamp", Weight: 1, Price: 10})
	if _, err := svc.Get(ctx, "p1"); err != nil { // populate the cache
		t.Fatalf("Get: %v", err)
	}

	_ = svc.Put(ctx, Product{ProductID: "p1", Name: "Lamp", Weight: 1, Price: 12})

	got, err := svc.Get(ctx, "p1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Price != 12 {
		t.Errorf("price = %v, want 12: the cache served a stale value after a write", got.Price)
	}
}

// The catalogue is the highest-volume read path in the system, so a repeated
// read must not reach the database at all.
func TestRepeatedReadsDoNotReachTheDatabase(t *testing.T) {
	svc, _ := testService(t, true)
	ctx := context.Background()

	_ = svc.Put(ctx, Product{ProductID: "p1", Name: "Lamp", Weight: 1, Price: 10})

	// Count database reads by watching the KV node's own request metrics
	// indirectly: swap in a counting loader instead.
	var loads atomic.Int32
	counted := func(ctx context.Context) ([]byte, error) {
		loads.Add(1)
		return svc.load(ctx, "p1")
	}

	for i := 0; i < 5; i++ {
		if _, err := svc.cache.GetOrLoad(ctx, "p1", counted); err != nil {
			t.Fatalf("GetOrLoad: %v", err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Errorf("the database was read %d times for 5 requests, want 1", got)
	}
}

// Without a cache the service has to behave identically, so the same code runs
// in environments that have no Redis.
func TestTheServiceWorksWithoutACache(t *testing.T) {
	svc, _ := testService(t, false)
	ctx := context.Background()

	if svc.cache.Enabled() {
		t.Fatal("a service built with no cache reported one")
	}

	want := Product{ProductID: "p1", Name: "Lamp", Weight: 2, Price: 9.5}
	if err := svc.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := svc.Get(ctx, "p1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Errorf("product = %+v, want %+v", got, want)
	}
}

// A record that cannot be parsed is a server-side problem, not a missing
// product, and must not be reported as a 404.
func TestAnUnreadableRecordIsAnInternalError(t *testing.T) {
	svc, db := testService(t, false)
	ctx := context.Background()

	if _, err := db.Put(ctx, Key("p1"), "{this is not a product"); err != nil {
		t.Fatalf("seeding a corrupt record: %v", err)
	}

	_, err := svc.Get(ctx, "p1")
	if status := httpx.StatusOf(err); status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}

func TestKeysAreNamespaced(t *testing.T) {
	if got, want := Key("p1"), "product:p1"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

func TestTheHTTPSurface(t *testing.T) {
	svc, _ := testService(t, true)
	handler := NewServer(svc, busywait.Config{}).Routes()

	// A write, then a read, then a miss.
	req := httptest.NewRequest(http.MethodPut, "/product/p1",
		newBody(`{"name":"Lamp","weight":2.5,"price":19.99}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/product/p1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/product/ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/product/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", rec.Code)
	}
}

// The path names the product, so a body claiming a different one must not be
// able to write to it.
func TestThePathWinsOverTheBody(t *testing.T) {
	svc, _ := testService(t, true)
	handler := NewServer(svc, busywait.Config{}).Routes()

	req := httptest.NewRequest(http.MethodPut, "/product/p1",
		newBody(`{"productId":"p999","name":"Lamp","weight":1,"price":10}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if _, err := svc.Get(context.Background(), "p999"); httpx.StatusOf(err) != http.StatusNotFound {
		t.Error("the body's product id was written instead of the path's")
	}
	if _, err := svc.Get(context.Background(), "p1"); err != nil {
		t.Errorf("the path's product was not written: %v", err)
	}
}
