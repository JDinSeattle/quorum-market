// Command gatewaysvc runs the API gateway: the only publicly reachable service.
//
// It verifies tokens, enforces a rate limit shared across every gateway
// instance, and forwards to the internal services with the caller's identity
// attached. Everything behind it can then assume an authenticated caller.
package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/gateway"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/identity"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/ratelimit"
	"github.com/JDinSeattle/quorum-market/internal/redisx"
)

func main() {
	obs.InitLogging("gateway")

	ctx, stop := httpx.SignalContext()
	defer stop()

	secret := os.Getenv("JWT_SECRET")
	verifier, err := auth.NewVerifier(secret, envx.String("JWT_ISSUER", "quorum-market"))
	if err != nil {
		// Starting without a usable signing secret would mean either rejecting
		// every request or, worse, accepting forged ones.
		slog.Error("invalid JWT configuration", "err", err)
		os.Exit(1)
	}

	identityURL := envx.String("IDENTITY_SERVICE_URL", "http://localhost:8085")
	productURL := envx.String("PRODUCT_SERVICE_URL", "http://localhost:8081")
	cartURL := envx.String("CART_SERVICE_URL", "http://localhost:8082")
	orderURL := envx.String("ORDER_SERVICE_URL", "http://localhost:8086")
	notificationURL := envx.String("NOTIFICATION_SERVICE_URL", "http://localhost:8087")

	checks := []obs.Check{
		{Name: "identity-service", Optional: true, Probe: httpx.Ping("identity", identityURL+"/identity/health")},
		{Name: "product-service", Optional: true, Probe: httpx.Ping("product", productURL+"/product/health")},
		{Name: "shopping-cart-service", Optional: true, Probe: httpx.Ping("cart", cartURL+"/shopping-cart/health")},
		{Name: "order-service", Optional: true, Probe: httpx.Ping("order", orderURL+"/orders/health")},
		{Name: "notification-service", Optional: true, Probe: httpx.Ping("notification", notificationURL+"/notifications/health")},
	}

	// The rate limiter's budget has to be shared. A per-instance counter would
	// multiply the published limit by however many gateways happen to be
	// running, and change it every time the group scales.
	var limiter *ratelimit.Limiter
	if addr := envx.String("REDIS_ADDR", ""); addr != "" {
		rdb, redisErr := redisx.Connect(ctx, redisx.ConfigFromEnv())
		if redisErr != nil {
			slog.Error("starting without rate limiting: redis is unreachable", "addr", addr, "err", redisErr)
		} else {
			defer func() { _ = rdb.Close() }()

			limiter = ratelimit.New(rdb, ratelimit.Options{
				Scope:  "gateway",
				Prefix: "ratelimit:gateway:",
				Limit:  envx.Int("RATE_LIMIT", 600),
				Window: envx.Millis("RATE_LIMIT_WINDOW_MS", time.Minute),
			})
			// A logged-out session has to stop working before its token would
			// naturally expire, and the denylist is what makes that immediate.
			verifier = verifier.WithRevocationCheck(identity.RevocationCheck(rdb))
			checks = append(checks, obs.Check{Name: "redis", Optional: true, Probe: redisx.Probe(rdb)})
			slog.Info("rate limiting enabled", "addr", addr)
		}
	}

	gw, err := gateway.New(gateway.Config{
		Routes: []gateway.Route{
			// Registration and login must be reachable without a token, or
			// nobody could ever obtain one.
			{Prefix: "/identity", Upstream: identityURL, Public: true, Name: "identity"},
			// Browsing is public; a catalogue behind a login is not a shop.
			{Prefix: "/product", Upstream: productURL, Public: true, Name: "product"},
			// Everything that belongs to a person requires proving who you are.
			{Prefix: "/shopping-cart", Upstream: cartURL, Name: "shopping-cart"},
			{Prefix: "/orders", Upstream: orderURL, Name: "orders"},
			{Prefix: "/notifications", Upstream: notificationURL, Name: "notifications"},
		},
		Verifier:             verifier,
		Limiter:              limiter,
		AnonymousLimitFactor: envx.Int("ANONYMOUS_LIMIT_FACTOR", 1),
		UpstreamTimeout:      envx.Millis("UPSTREAM_TIMEOUT_MS", 30*time.Second),
	})
	if err != nil {
		slog.Error("invalid gateway configuration", "err", err)
		os.Exit(1)
	}

	health := obs.NewHealth(checks...)
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("gateway starting",
		"identity", identityURL, "product", productURL, "cart", cartURL,
		"orders", orderURL, "notifications", notificationURL,
		"rate_limiting", limiter.Enabled(), "build", obs.Build())

	err = httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("PORT", "8080"),
		Handler: gw.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
