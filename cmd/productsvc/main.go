// Command productsvc runs the product catalogue service.
package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/cache"
	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/product"
	"github.com/JDinSeattle/quorum-market/internal/redisx"
)

func main() {
	obs.InitLogging("product-service")

	ctx, stop := httpx.SignalContext()
	defer stop()

	dbURL := envx.String("PRODUCT_DB_URL", "http://localhost:9080")
	db := kv.NewClient("product-db", dbURL,
		envx.Millis("HTTP_CONNECT_TIMEOUT_MS", 500*time.Millisecond),
		envx.Millis("HTTP_REQUEST_TIMEOUT_MS", 5*time.Second),
	)

	checks := []obs.Check{{
		Name: "product-db",
		// Optional on purpose. Every instance shares this database, so a
		// database outage would mark the entire fleet unready at once — which
		// tells the load balancer to send traffic nowhere and, with ELB health
		// checks, tells the Auto Scaling Group to replace every instance while
		// the real problem is elsewhere. Degraded is the honest signal.
		Optional: true,
		Probe:    httpx.Ping("product-db", dbURL+"/health"),
	}}

	// The cache is genuinely optional: a catalogue read is correct with or
	// without it, so a Redis that will not come up costs latency rather than
	// availability and must not stop the service from starting.
	var catalogueCache *cache.Cache
	if addr := envx.String("REDIS_ADDR", ""); addr != "" {
		redisCfg := redisx.ConfigFromEnv()
		rdb, err := redisx.Connect(ctx, redisCfg)
		if err != nil {
			slog.Error("starting without a cache: redis is unreachable", "addr", addr, "err", err)
		} else {
			defer func() { _ = rdb.Close() }()
			catalogueCache = cache.New(rdb, cache.Options{
				Name:        "catalogue",
				Prefix:      "product:",
				TTL:         envx.Millis("CACHE_TTL_MS", 5*time.Minute),
				NegativeTTL: envx.Millis("CACHE_NEGATIVE_TTL_MS", 30*time.Second),
			})
			checks = append(checks, obs.Check{Name: "redis", Optional: true, Probe: redisx.Probe(rdb)})
			slog.Info("catalogue cache enabled", "addr", addr)
		}
	}

	srv := product.NewServer(product.NewService(db, catalogueCache), busywait.FromEnv())

	health := obs.NewHealth(checks...)
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("product service starting",
		"product_db", dbURL, "cache", catalogueCache.Enabled(), "build", obs.Build())

	err := httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("PORT", "8081"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
