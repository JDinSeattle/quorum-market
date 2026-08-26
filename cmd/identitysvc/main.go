// Command identitysvc runs the identity service: accounts, credentials and
// sessions.
package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/identity"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/redisx"
)

func main() {
	obs.InitLogging("identity-service")

	ctx, stop := httpx.SignalContext()
	defer stop()

	secret := os.Getenv("JWT_SECRET")
	issuer := envx.String("JWT_ISSUER", "quorum-market")

	signer, err := auth.NewSigner(secret, issuer, envx.Millis("ACCESS_TOKEN_TTL_MS", 15*time.Minute))
	if err != nil {
		slog.Error("invalid JWT configuration", "err", err)
		os.Exit(1)
	}
	verifier, err := auth.NewVerifier(secret, issuer)
	if err != nil {
		slog.Error("invalid JWT configuration", "err", err)
		os.Exit(1)
	}

	dbURL := envx.String("CORE_DB_URL", "http://localhost:9100")
	db := kv.NewClient("core-db", dbURL,
		envx.Millis("HTTP_CONNECT_TIMEOUT_MS", 500*time.Millisecond),
		envx.Millis("HTTP_REQUEST_TIMEOUT_MS", 5*time.Second),
	)

	// Redis is not optional here. Sessions live in it, so without it nobody
	// can log in — running anyway would mean a service that answers health
	// checks and rejects every customer.
	rdb, err := redisx.Connect(ctx, redisx.ConfigFromEnv())
	if err != nil {
		slog.Error("cannot reach redis; sessions cannot be stored", "err", err)
		os.Exit(1)
	}
	defer func() { _ = rdb.Close() }()

	svc := identity.NewService(db, rdb, signer, envx.Millis("REFRESH_TOKEN_TTL_MS", 7*24*time.Hour))
	verifier = verifier.WithRevocationCheck(svc.RevocationCheck())
	srv := identity.NewServer(svc, verifier)

	health := obs.NewHealth(
		obs.Check{Name: "core-db", Optional: true, Probe: httpx.Ping("core-db", dbURL+"/health")},
		obs.Check{Name: "redis", Optional: true, Probe: redisx.Probe(rdb)},
	)
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("identity service starting",
		"core_db", dbURL, "access_token_ttl", signer.TTL(), "build", obs.Build())

	err = httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("PORT", "8085"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
