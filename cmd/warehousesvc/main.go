// Command warehousesvc runs the warehouse service: a stock ledger over HTTP
// plus a RabbitMQ consumer that ships confirmed orders.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
	"github.com/JDinSeattle/quorum-market/internal/warehouse"
)

func main() {
	obs.InitLogging("warehouse-service")

	ctx, stop := httpx.SignalContext()
	defer stop()

	initialStock := envx.Int("WAREHOUSE_INITIAL_STOCK", warehouse.DefaultStock)
	inv := warehouse.New(initialStock, envx.Millis("RESERVATION_TTL_MS", warehouse.DefaultTTL))
	go inv.RunSweeper(ctx.Done(), envx.Millis("RESERVATION_SWEEP_INTERVAL_MS", 5*time.Second))

	// A warehouse that cannot consume ship messages accepts reservations that
	// are never converted into shipments, so refusing to start is better than
	// running in a state that looks healthy and quietly loses every order.
	cfg := rmq.ConfigFromEnv("orders_queue")
	conn, err := rmq.Dial(ctx, cfg)
	if err != nil {
		slog.Error("cannot reach rabbitmq", "url", cfg.Redacted(), "err", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	// Announces what it shipped. The order and notification services both
	// react to this, and neither of them is named anywhere in this file.
	topicPublisher, err := rmq.NewTopicPublisher(conn, cfg, events.Exchange)
	if err != nil {
		slog.Error("cannot prepare the event publisher", "err", err)
		os.Exit(1)
	}
	defer func() { _ = topicPublisher.Close() }()

	shipper := warehouse.NewShipper(inv, events.NewPublisher(topicPublisher))
	consumer := rmq.NewConsumer(conn, cfg, shipper.Handle)
	go consumer.Run(ctx)

	srv := warehouse.NewServer(inv, busywait.FromEnv(), consumer.Stats)

	health := obs.NewHealth(obs.Check{
		Name: "rabbitmq",
		// Degraded, not down: reservations and inventory reads keep working
		// without the broker, and those are what the checkout path needs
		// synchronously. Only fulfilment is delayed, and the queue is durable,
		// so the backlog drains once the broker returns.
		Optional: true,
		Probe:    func(context.Context) error { return conn.Healthy() },
	})
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("warehouse service starting",
		"queue", cfg.Queue,
		"initial_stock", initialStock,
		"consumer_workers", cfg.Workers,
		"build", obs.Build(),
	)

	err = httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("SERVER_PORT", "8084"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
