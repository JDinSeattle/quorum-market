// Command notificationsvc runs the notification service.
//
// It subscribes to every order event and turns each one into a message in the
// customer's inbox. Nothing publishes *to* it and nothing calls it to send
// anything — which is the point of the event bus: this service was added
// without a line changing in the services it listens to.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/notification"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/redisx"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
)

func main() {
	obs.InitLogging("notification-service")

	ctx, stop := httpx.SignalContext()
	defer stop()

	// The inbox lives in Redis, so without it there is nowhere to deliver.
	rdb, err := redisx.Connect(ctx, redisx.ConfigFromEnv())
	if err != nil {
		slog.Error("cannot reach redis; notifications have nowhere to go", "err", err)
		os.Exit(1)
	}
	defer func() { _ = rdb.Close() }()

	rmqCfg := rmq.ConfigFromEnv("orders_queue")
	conn, err := rmq.Dial(ctx, rmqCfg)
	if err != nil {
		slog.Error("cannot reach rabbitmq", "url", rmqCfg.Redacted(), "err", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	svc := notification.NewService(rdb,
		envx.Int("NOTIFICATION_INBOX_SIZE", 50),
		envx.Millis("NOTIFICATION_INBOX_TTL_MS", 30*24*time.Hour),
	)
	srv := notification.NewServer(svc)

	// Its own queue, bound with a wildcard. Owning the queue means a slow or
	// stopped notification service builds its own backlog instead of starving
	// anyone else, and the wildcard means a new order event reaches it without
	// a topology change.
	subscription := envx.String("NOTIFICATION_EVENT_QUEUE", "notification-service.events")
	subscriber := rmq.NewSubscriber(conn, rmqCfg, events.Exchange, subscription,
		notification.Patterns(), events.Subscribe(srv.Subscribe()))
	go subscriber.Run(ctx)

	health := obs.NewHealth(
		obs.Check{Name: "redis", Optional: true, Probe: redisx.Probe(rdb)},
		obs.Check{Name: "rabbitmq", Optional: true, Probe: func(context.Context) error { return conn.Healthy() }},
	)
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("notification service starting",
		"subscription", subscription, "patterns", notification.Patterns(), "build", obs.Build())

	err = httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("PORT", "8087"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
