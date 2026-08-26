package kv

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

type testNode struct {
	cfg Config
	svc *Service
	url string
}

// newCluster starts n real HTTP nodes wired to each other, so these tests
// exercise the actual replication transport rather than a stubbed one.
func newCluster(t *testing.T, mode Mode, n, w, r int) []*testNode {
	t.Helper()

	handlers := make([]http.Handler, n)
	servers := make([]*httptest.Server, n)
	urls := make([]string, n)

	// The listener exists before the server starts, so peer URLs can be known
	// before the handlers they point at are built.
	for i := 0; i < n; i++ {
		idx := i
		servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			handlers[idx].ServeHTTP(w, req)
		}))
		urls[i] = "http://" + servers[i].Listener.Addr().String()
	}

	nodes := make([]*testNode, n)
	for i := 0; i < n; i++ {
		cfg := Config{
			NodeID:      fmt.Sprintf("node-%d", i),
			Mode:        mode,
			Role:        "leader",
			WriteQuorum: w,
			ReadQuorum:  r,
			RPCTimeout:  5 * time.Second,
			ReadRepair:  true,
		}

		switch {
		case mode == ModeLeaderFollower && i == 0:
			cfg.Peers = append([]string(nil), urls[1:]...)
		case mode == ModeLeaderFollower:
			cfg.Role = "follower" // followers hold no peer list of their own
		default:
			for j := 0; j < n; j++ {
				if j != i {
					cfg.Peers = append(cfg.Peers, urls[j])
				}
			}
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("node %d config: %v", i, err)
		}

		svc := NewService(cfg, NewStore(cfg.NodeID), NewReplicator(cfg.RPCTimeout))
		handlers[i] = NewServer(svc, NewTxnManager(time.Minute), busywait.Config{}, false).Routes()
		nodes[i] = &testNode{cfg: cfg, svc: svc, url: urls[i]}

		servers[i].Start()
		t.Cleanup(servers[i].Close)
	}
	return nodes
}

// The cart cluster's configuration: any node coordinates, W=3 and R=3 over 5
// nodes so the two quorums always overlap.
func TestLeaderlessQuorumReadSeesQuorumWrite(t *testing.T) {
	nodes := newCluster(t, ModeLeaderless, 5, 3, 3)
	ctx := context.Background()

	if _, err := nodes[0].svc.Write(ctx, "cart-alice", `{"items":3}`, 0); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// W + R > N, so a read coordinated by any node must intersect the write.
	for i, node := range nodes {
		got, found, err := node.svc.Read(ctx, "cart-alice")
		if err != nil {
			t.Fatalf("node %d Read: %v", i, err)
		}
		if !found {
			t.Fatalf("node %d did not find the key", i)
		}
		if got.Value != `{"items":3}` {
			t.Errorf("node %d value = %q, want the written value", i, got.Value)
		}
	}
}

func TestLeaderlessWritesFromDifferentCoordinatorsConverge(t *testing.T) {
	nodes := newCluster(t, ModeLeaderless, 5, 3, 3)
	ctx := context.Background()

	if _, err := nodes[0].svc.Write(ctx, "cart-bob", "v1", 0); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// A different node coordinates the follow-up, as happens when the load
	// balancer moves a customer between cart instances.
	if _, err := nodes[3].svc.Write(ctx, "cart-bob", "v2", 0); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	for i, node := range nodes {
		got, found, err := node.svc.Read(ctx, "cart-bob")
		if err != nil || !found {
			t.Fatalf("node %d Read: %v (found=%v)", i, err, found)
		}
		if got.Value != "v2" {
			t.Errorf("node %d value = %q, want v2", i, got.Value)
		}
	}
}

// The product cluster's configuration: one leader, W=5 so every replica has
// the write before the call returns, R=1 so reads are a single local lookup.
func TestLeaderFollowerWriteReachesEveryReplica(t *testing.T) {
	nodes := newCluster(t, ModeLeaderFollower, 5, 5, 1)
	ctx := context.Background()

	if _, err := nodes[0].svc.Write(ctx, "product:p1", `{"weight":2.5}`, 0); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for i, node := range nodes {
		got, found := node.svc.LocalRead("product:p1")
		if !found {
			t.Fatalf("node %d has no copy despite W=5", i)
		}
		if got.Value != `{"weight":2.5}` {
			t.Errorf("node %d value = %q", i, got.Value)
		}
	}
}

func TestFollowerRejectsClientWrites(t *testing.T) {
	nodes := newCluster(t, ModeLeaderFollower, 3, 3, 1)

	if _, err := nodes[1].svc.Write(context.Background(), "product:p1", "1.0", 0); err == nil {
		t.Fatal("a follower accepted a client write")
	}
}

func TestWriteFailsWhenQuorumIsUnreachable(t *testing.T) {
	// Three peers that do not exist: the coordinator alone cannot make W=3.
	cfg := Config{
		NodeID: "lonely", Mode: ModeLeaderless,
		Peers:       []string{"http://127.0.0.1:1", "http://127.0.0.1:2", "http://127.0.0.1:3"},
		WriteQuorum: 3, ReadQuorum: 1, RPCTimeout: time.Second,
	}
	svc := NewService(cfg, NewStore(cfg.NodeID), NewReplicator(cfg.RPCTimeout))

	if _, err := svc.Write(context.Background(), "k", "v", 0); err == nil {
		t.Fatal("Write succeeded without a quorum of replicas")
	}
}

// A replica that missed a write should be brought up to date by the next read
// that notices, rather than staying stale until someone writes the key again.
func TestReadRepairHealsStaleReplicas(t *testing.T) {
	// R=5 forces every peer to answer, so every stale replica is observed.
	nodes := newCluster(t, ModeLeaderless, 3, 1, 3)
	ctx := context.Background()

	// Only node 0 learns the new value; W=1 means it does not wait for anyone.
	newest := Entry{Key: "cart-carol", Value: "repaired", Version: 9, Origin: "node-0"}
	nodes[0].svc.store.Apply(newest)
	for _, n := range nodes[1:] {
		n.svc.store.Apply(Entry{Key: "cart-carol", Value: "stale", Version: 2, Origin: "node-9"})
	}

	got, found, err := nodes[1].svc.Read(ctx, "cart-carol")
	if err != nil || !found {
		t.Fatalf("Read: %v (found=%v)", err, found)
	}
	if got.Value != "repaired" {
		t.Fatalf("read returned %q, want the newest value", got.Value)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		healed := true
		for _, n := range nodes {
			if e, ok := n.svc.store.Get("cart-carol"); !ok || e.Version != 9 {
				healed = false
			}
		}
		if healed {
			return
		}
		if time.Now().After(deadline) {
			for i, n := range nodes {
				e, _ := n.svc.store.Get("cart-carol")
				t.Logf("node %d: %+v", i, e)
			}
			t.Fatal("stale replicas were never repaired")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestTransactionLifecycle(t *testing.T) {
	m := NewTxnManager(time.Minute)

	txn := m.Begin()
	if txn.State != TxnPending {
		t.Fatalf("new transaction state = %q, want pending", txn.State)
	}
	if m.Open() != 1 {
		t.Fatalf("open = %d, want 1", m.Open())
	}

	if _, err := m.Commit(txn.ID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if m.Open() != 0 {
		t.Errorf("open = %d after commit, want 0", m.Open())
	}
	// Committing twice is a bug in the caller and is reported as one.
	if _, err := m.Commit(txn.ID); httpx.StatusOf(err) != http.StatusNotFound {
		t.Errorf("second commit status = %d, want 404", httpx.StatusOf(err))
	}
	if _, err := m.Abort("never-existed"); httpx.StatusOf(err) != http.StatusNotFound {
		t.Errorf("aborting an unknown transaction: status = %d, want 404", httpx.StatusOf(err))
	}
	if _, err := m.Abort(""); httpx.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("aborting with no id: status = %d, want 400", httpx.StatusOf(err))
	}
}

func TestTransactionSweepReclaimsAbandoned(t *testing.T) {
	m := NewTxnManager(time.Nanosecond)
	m.Begin()
	m.Begin()

	if reaped := m.Sweep(); reaped != 2 {
		t.Fatalf("swept %d, want 2", reaped)
	}
	if m.Open() != 0 {
		t.Errorf("open = %d, want 0", m.Open())
	}
	if got := m.Stats()["abandoned"].(uint64); got != 2 {
		t.Errorf("abandoned = %d, want 2", got)
	}
}
