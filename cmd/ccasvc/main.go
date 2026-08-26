// Command ccasvc runs the mock credit card authorizer.
package main

import (
	"log/slog"
	"os"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/cca"
	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
)

func main() {
	obs.InitLogging("cca-service")

	ctx, stop := httpx.SignalContext()
	defer stop()

	rate := envx.Float("CCA_APPROVAL_RATE", cca.DefaultApprovalRate)
	srv := cca.NewServer(rate, busywait.FromEnv())

	// The authorizer has no dependencies, so readiness is simply liveness.
	health := obs.NewHealth()
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("credit card authorizer starting", "approval_rate", rate, "build", obs.Build())

	err := httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("SERVER_PORT", "8083"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
