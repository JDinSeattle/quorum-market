// Command ordersvc runs the order service: the lifecycle of an order after it
// has been paid for.
//
// It is driven by events rather than by anyone calling it. The cart service
// announces that an order was placed and the warehouse announces that it
// shipped; this service assembles those into a record a customer can look at.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/order"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
)

func main() {
	obs.InitLogging("order-service")

	ctx, stop := httpx.SignalContext()
	defer stop()

	dbURL := envx.String("CORE_DB_URL", "http://localhost:9100")
	db := kv.NewClient("core-db", dbURL,
		envx.Millis("HTTP_CONNECT_TIMEOUT_MS", 500*time.Millisecond),
		envx.Millis("HTTP_REQUEST_TIMEOUT_MS", 5*time.Second),
	)

	// The event bus is this service's only source of input, so a broker it
	// cannot reach means a service that will never learn about a single order.
	rmqCfg := rmq.ConfigFromEnv("orders_queue")
	conn, err := rmq.Dial(ctx, rmqCfg)
	if err != nil {
		slog.Error("cannot reach rabbitmq", "url", rmqCfg.Redacted(), "err", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	topicPublisher, err := rmq.NewTopicPublisher(conn, rmqCfg, events.Exchange)
	if err != nil {
		slog.Error("cannot prepare the event publisher", "err", err)
		os.Exit(1)
	}
	defer func() { _ = topicPublisher.Close() }()

	svc := order.NewService(db, events.NewPublisher(topicPublisher))
	srv := order.NewServer(svc)

	// One queue bound to several routing keys, demultiplexed by type. A queue
	// per event type would mean several backlogs to watch and no useful
	// ordering between them.
	router := events.Router(srv.Subscriptions())
	subscription := envx.String("ORDER_EVENT_QUEUE", "order-service.events")

	subscriber := rmq.NewSubscriber(conn, rmqCfg, events.Exchange, subscription,
		router.Patterns(), events.Subscribe(router.Handle))
	go subscriber.Run(ctx)

	health := obs.NewHealth(
		obs.Check{Name: "core-db", Optional: true, Probe: httpx.Ping("core-db", dbURL+"/health")},
		obs.Check{Name: "rabbitmq", Optional: true, Probe: func(context.Context) error { return conn.Healthy() }},
	)
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("order service starting",
		"core_db", dbURL, "subscription", subscription,
		"events", router.Patterns(), "build", obs.Build())

	err = httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("PORT", "8086"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
