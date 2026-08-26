package kv

import (
	"fmt"
	"os"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/envx"
)

// Mode selects a replication strategy.
type Mode string

const (
	// ModeLeaderFollower routes every write through one leader. Used by the
	// product cluster: reads vastly outnumber writes, so paying for a strongly
	// consistent write to buy a single-node read is the right trade.
	ModeLeaderFollower Mode = "leader-follower"

	// ModeLeaderless lets any node coordinate a write. Used by the cart
	// cluster: writes are frequent and spread across many independent carts,
	// so funnelling them through one node would just build a queue.
	ModeLeaderless Mode = "leaderless"
)

// Config is a KV node's full runtime configuration.
type Config struct {
	NodeID      string
	Mode        Mode
	Role        string // "leader" or "follower"; leader-follower mode only
	Peers       []string
	WriteQuorum int
	ReadQuorum  int
	WriteDelay  time.Duration
	ReadDelay   time.Duration
	Sequential  bool
	ReadRepair  bool
	RPCTimeout  time.Duration
}

// ConfigFromEnv builds a Config from the environment.
func ConfigFromEnv() Config {
	mode := Mode(envx.String("NODE_MODE", string(ModeLeaderFollower)))

	// The two cluster flavours use different variable names for the same idea,
	// so accept either and prefer the one that matches the mode.
	peers := envx.List("PEER_URLS")
	followers := envx.List("FOLLOWER_URLS")
	if mode == ModeLeaderFollower {
		peers, followers = followers, peers
	}
	if len(peers) == 0 {
		peers = followers
	}

	return Config{
		NodeID:      envx.String("NODE_ID", defaultNodeID()),
		Mode:        mode,
		Role:        envx.String("ROLE", "leader"),
		Peers:       peers,
		WriteQuorum: envx.Int("WRITE_QUORUM_SIZE", 1),
		ReadQuorum:  envx.Int("READ_QUORUM_SIZE", 1),
		WriteDelay:  envx.Millis("WRITE_DELAY_MS", 0),
		ReadDelay:   envx.Millis("READ_DELAY_MS", 0),
		Sequential:  envx.Bool("REPLICATION_SEQUENTIAL", false),
		ReadRepair:  envx.Bool("READ_REPAIR", true),
		RPCTimeout:  envx.Millis("REPLICATION_TIMEOUT_MS", 5*time.Second),
	}
}

func defaultNodeID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "kv-node"
}

// IsLeader reports whether this node accepts client writes in leader-follower
// mode. Every node is a coordinator under leaderless replication.
func (c Config) IsLeader() bool {
	return c.Mode == ModeLeaderless || c.Role == "leader"
}

// ClusterSize is the number of replicas including this node.
func (c Config) ClusterSize() int { return len(c.Peers) + 1 }

// Validate rejects configurations that could never satisfy their own quorums,
// so a misconfigured cluster fails at boot rather than at the first checkout.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeLeaderFollower, ModeLeaderless:
	default:
		return fmt.Errorf("unknown NODE_MODE %q (want %q or %q)", c.Mode, ModeLeaderFollower, ModeLeaderless)
	}
	if c.Mode == ModeLeaderFollower && c.Role != "leader" && c.Role != "follower" {
		return fmt.Errorf("unknown ROLE %q (want leader or follower)", c.Role)
	}
	if c.WriteQuorum < 1 {
		return fmt.Errorf("WRITE_QUORUM_SIZE must be >= 1, got %d", c.WriteQuorum)
	}
	if c.ReadQuorum < 1 {
		return fmt.Errorf("READ_QUORUM_SIZE must be >= 1, got %d", c.ReadQuorum)
	}
	// A follower holds no peer list of its own, so it cannot and need not
	// satisfy a quorum locally; only nodes that coordinate are checked.
	if c.Mode == ModeLeaderFollower && c.Role == "follower" {
		return nil
	}
	if c.WriteQuorum > c.ClusterSize() {
		return fmt.Errorf("WRITE_QUORUM_SIZE %d exceeds cluster size %d", c.WriteQuorum, c.ClusterSize())
	}
	if c.ReadQuorum > c.ClusterSize() {
		return fmt.Errorf("READ_QUORUM_SIZE %d exceeds cluster size %d", c.ReadQuorum, c.ClusterSize())
	}
	return nil
}

// StronglyConsistent reports whether the configured quorums guarantee that a
// read observes the most recent acknowledged write (W + R > N).
func (c Config) StronglyConsistent() bool {
	if c.Mode == ModeLeaderFollower {
		// All client reads are served by the leader, which applies every write
		// it coordinates, so the guarantee holds regardless of R.
		return true
	}
	return c.WriteQuorum+c.ReadQuorum > c.ClusterSize()
}
