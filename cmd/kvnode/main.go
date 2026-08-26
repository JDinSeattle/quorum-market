// Command kvnode runs one node of a distributed key-value store.
//
// The same binary backs both clusters in this system; NODE_MODE picks the
// replication strategy:
//
//	NODE_MODE=leader-follower  ROLE=leader|follower  FOLLOWER_URLS=...
//	NODE_MODE=leaderless       PEER_URLS=...
//
// Quorums come from WRITE_QUORUM_SIZE and READ_QUORUM_SIZE, and
// WRITE_DELAY_MS / READ_DELAY_MS stand in for storage latency.
package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
)

func main() {
	obs.InitLogging("kv-node")

	cfg := kv.ConfigFromEnv()
	if err := cfg.Validate(); err != nil {
		// Refusing to start beats starting into a configuration that can never
		// satisfy its own quorum and only reveals that on the first checkout.
		slog.Error("invalid node configuration", "err", err)
		os.Exit(1)
	}

	store := kv.NewStore(cfg.NodeID)
	svc := kv.NewService(cfg, store, kv.NewReplicator(cfg.RPCTimeout))
	txns := kv.NewTxnManager(envx.Millis("TXN_TTL_MS", 5*time.Minute))
	srv := kv.NewServer(svc, txns, busywait.FromEnv(), envx.Bool("TXN_BUSYWAIT", false))

	ctx, stop := httpx.SignalContext()
	defer stop()

	go txns.RunSweeper(ctx, envx.Millis("TXN_SWEEP_INTERVAL_MS", 30*time.Second))
	go store.RunSweeper(ctx, envx.Millis("STORE_SWEEP_INTERVAL_MS", time.Minute))

	obs.RegisterGauge("kv", "keys", "Unexpired keys held by this node.",
		func() float64 { return float64(store.Len()) })

	health := obs.NewHealth(peerChecks(cfg)...)
	go obs.ServeAdmin(ctx, ":"+envx.String("ADMIN_PORT", "9100"), health)

	slog.Info("kv node starting",
		"node_id", cfg.NodeID,
		"mode", cfg.Mode,
		"role", cfg.Role,
		"cluster_size", cfg.ClusterSize(),
		"write_quorum", cfg.WriteQuorum,
		"read_quorum", cfg.ReadQuorum,
		"strongly_consistent", cfg.StronglyConsistent(),
		"build", obs.Build(),
	)

	err := httpx.Serve(ctx, httpx.ServerConfig{
		Addr:    ":" + envx.String("SERVER_PORT", "8080"),
		Handler: srv.Routes(),
		Health:  health,
	})
	if err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// peerChecks reports each peer's reachability.
//
// They are all optional: a node that has lost a peer can still serve every
// request its quorum allows, and taking it out of rotation because a *different*
// node is down would turn one failure into two.
func peerChecks(cfg kv.Config) []obs.Check {
	checks := make([]obs.Check, 0, len(cfg.Peers))
	for _, peer := range cfg.Peers {
		checks = append(checks, obs.Check{
			Name:     peer,
			Optional: true,
			Probe:    httpx.Ping("peer", peer+"/health"),
		})
	}
	return checks
}
