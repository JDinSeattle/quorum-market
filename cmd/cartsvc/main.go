// Command cartsvc runs the shopping cart service, the orchestrator that fans
// out to the product, warehouse and payment services and owns the checkout
// transaction boundary.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/cart"
	"github.com/JDinSeattle/quorum-market/internal/cca"
	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/product"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
	"github.com/JDinSeattle/quorum-market/internal/warehouse"
)

func main() {
	obs.InitLogging("shopping-cart-service")

	ctx, stop := httpx.SignalContext()
	defer stop()

	// Short timeouts on purpose. This service sits in front of three others,
	// so a downstream that hangs would otherwise pin a goroutine per request
	// and take the orchestrator down with it.
	connectTimeout := envx.Millis("HTTP_CONNECT_TIMEOUT_MS", 500*time.Millisecond)
	requestTimeout := envx.Millis("HTTP_REQUEST_TIMEOUT_MS", 5*time.Second)

	productURL := envx.String("PRODUCT_SERVICE_URL", "http://localhost:8081")
	warehouseURL := envx.String("WAREHOUSE_SERVICE_URL", "http://localhost:8084")
	ccaURL := envx.String("CCA_SERVICE_URL", "http://localhost:8083")
	cartDBURL := envx.String("CART_DB_URL", "http://localhost:9090")

	products := product.NewClient(productURL, connectTimeout, requestTimeout)
	warehouseClient := warehouse.NewClient(warehouseURL, connectTimeout, requestTimeout)
	cards := cca.NewClient(ccaURL, connectTimeout, requestTimeout)
	db := kv.NewClient("cart-db", cartDBURL, connectTimeout, requestTimeout)

	rmqCfg := rmq.ConfigFromEnv("orders_queue")
	conn, err := rmq.Dial(ctx, rmqCfg)
	if err != nil {
		slog.Error("cannot reach rabbitmq", "url", rmqCfg.Redacted(), "err", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	// Two publishers, because two different things are being sent. The ship
	// message is a command with exactly one correct handler and goes on a work
	// queue; order.placed is a statement of fact that any number of services
	// may care about and goes on the topic exchange.
	publisher, err := rmq.NewPublisher(conn, rmqCfg)
	if err != nil {
		slog.Error("cannot prepare the command publisher", "err", err)
		os.Exit(1)
	}
	defer func() { _ = publisher.Close() }()

	topicPublisher, err := rmq.NewTopicPublisher(conn, rmqCfg, events.Exchange)
	if err != nil {
		slog.Error("cannot prepare the event publisher", "err", err)
		os.Exit(1)
	}
	defer func() { _ = topicPublisher.Close() }()

	delay := busywait.FromEnv()
	carts := cart.NewService(products, warehouseClient, db, delay)
	checkout := cart.NewCheckoutService(carts, warehouseClient, cards, db, publisher,
		events.NewPublisher(topicPublisher), delay)
	srv := cart.NewServer(carts, checkout)

	// Every dependency here is shared by the whole fleet, so a failure in one
	// of them says nothing about whether *this* instance should receive
	// traffic. They report degradation; they do not remove the instance.
	health := obs.NewHealth(
		obs.Check{Name: "cart-db", Optional: true, Probe: httpx.Ping("cart-db", cartDBURL+"/health")},
		obs.Check{Name: "product-service", Optional: true, Probe: httpx.Ping("product", productURL+"/product/health")},
		obs.Check{Name: "warehouse-service", Optional: true, Probe: httpx.Ping("warehouse", warehouseURL+"/warehouse/health")},
		obs.Check{Name: "credit-card-authorizer", Optional: true, Probe: httpx.Ping("cca", ccaURL+"/credit-card-authorizer/health")},
		obs.Check{Name: "rabbitmq", Optional: true, Probe: func(context.Context) error { return conn.Healthy() }},
	)
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("shopping cart service starting",
		"product", productURL,
		"warehouse", warehouseURL,
		"cca", ccaURL,
		"cart_db", cartDBURL,
		"queue", rmqCfg.Queue,
		"build", obs.Build(),
	)

	err = httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("PORT", "8082"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
