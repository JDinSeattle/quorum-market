package kv

import (
	"testing"
	"time"
)

func TestCoordinatorWriteIncrementsVersion(t *testing.T) {
	s := NewStore("node-a")

	first := s.CoordinatorWrite("k", "v1", 0)
	if first.Version != 1 {
		t.Fatalf("first version = %d, want 1", first.Version)
	}
	second := s.CoordinatorWrite("k", "v2", 0)
	if second.Version != 2 {
		t.Fatalf("second version = %d, want 2", second.Version)
	}
	if second.Origin != "node-a" {
		t.Errorf("origin = %q, want node-a", second.Origin)
	}

	got, ok := s.Get("k")
	if !ok || got.Value != "v2" {
		t.Errorf("Get = %+v, %v; want v2", got, ok)
	}
}

func TestApplyRejectsStaleWrites(t *testing.T) {
	s := NewStore("node-a")
	s.Apply(Entry{Key: "k", Value: "new", Version: 5, Origin: "node-b"})

	if s.Apply(Entry{Key: "k", Value: "old", Version: 3, Origin: "node-c"}) {
		t.Error("Apply accepted an older version")
	}
	got, _ := s.Get("k")
	if got.Value != "new" {
		t.Errorf("value = %q, want new: a stale replication overwrote a newer value", got.Value)
	}
}

// Two nodes can independently allocate the same version for the same key under
// leaderless replication. Every replica has to resolve that the same way, or
// the cluster never converges.
func TestApplyBreaksVersionTiesDeterministically(t *testing.T) {
	fromA := Entry{Key: "k", Value: "from-a", Version: 7, Origin: "node-a"}
	fromB := Entry{Key: "k", Value: "from-b", Version: 7, Origin: "node-b"}

	aFirst := NewStore("x")
	aFirst.Apply(fromA)
	aFirst.Apply(fromB)

	bFirst := NewStore("x")
	bFirst.Apply(fromB)
	bFirst.Apply(fromA)

	got1, _ := aFirst.Get("k")
	got2, _ := bFirst.Get("k")
	if got1.Value != got2.Value {
		t.Fatalf("replicas diverged: %q vs %q", got1.Value, got2.Value)
	}
	if got1.Value != "from-b" {
		t.Errorf("winner = %q, want from-b (higher origin)", got1.Value)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	s := NewStore("node-a")
	e := Entry{Key: "k", Value: "v", Version: 2, Origin: "node-b"}

	if !s.Apply(e) {
		t.Fatal("first Apply was rejected")
	}
	if s.Apply(e) {
		t.Error("re-applying the same entry reported a change")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestConfigValidate(t *testing.T) {
	tests := map[string]struct {
		cfg     Config
		wantErr bool
	}{
		"leaderless quorum fits": {
			Config{Mode: ModeLeaderless, Peers: []string{"a", "b", "c", "d"}, WriteQuorum: 3, ReadQuorum: 3}, false,
		},
		"write quorum exceeds cluster": {
			Config{Mode: ModeLeaderless, Peers: []string{"a"}, WriteQuorum: 5, ReadQuorum: 1}, true,
		},
		"read quorum exceeds cluster": {
			Config{Mode: ModeLeaderless, Peers: []string{"a"}, WriteQuorum: 1, ReadQuorum: 5}, true,
		},
		"unknown mode": {
			Config{Mode: "gossip", WriteQuorum: 1, ReadQuorum: 1}, true,
		},
		"unknown role": {
			Config{Mode: ModeLeaderFollower, Role: "captain", WriteQuorum: 1, ReadQuorum: 1}, true,
		},
		"follower needs no peers": {
			Config{Mode: ModeLeaderFollower, Role: "follower", WriteQuorum: 5, ReadQuorum: 1}, false,
		},
		"zero write quorum": {
			Config{Mode: ModeLeaderless, WriteQuorum: 0, ReadQuorum: 1}, true,
		},
	}

	for name, tc := range tests {
		err := tc.cfg.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: Validate() = %v, want nil", name, err)
		}
	}
}

func TestStronglyConsistent(t *testing.T) {
	// Cart cluster: W=3, R=3 over 5 nodes. 3+3 > 5, so reads see writes.
	cart := Config{Mode: ModeLeaderless, Peers: []string{"a", "b", "c", "d"}, WriteQuorum: 3, ReadQuorum: 3}
	if !cart.StronglyConsistent() {
		t.Error("W=3/R=3 over 5 nodes should be strongly consistent")
	}

	// W=1/R=1 over 5 nodes: 1+1 < 5, so a read can miss a write.
	loose := Config{Mode: ModeLeaderless, Peers: []string{"a", "b", "c", "d"}, WriteQuorum: 1, ReadQuorum: 1}
	if loose.StronglyConsistent() {
		t.Error("W=1/R=1 over 5 nodes should not be strongly consistent")
	}

	// Product cluster: every read is served by the leader, which holds every
	// write it coordinated, so R=1 is still safe.
	product := Config{Mode: ModeLeaderFollower, Role: "leader", Peers: []string{"a", "b", "c", "d"}, WriteQuorum: 5, ReadQuorum: 1}
	if !product.StronglyConsistent() {
		t.Error("leader-follower reads from the leader should be strongly consistent")
	}
}

func TestEntriesExpireAfterTheirTTL(t *testing.T) {
	s := NewStore("node-a")
	s.CoordinatorWrite("session", "value", 30*time.Millisecond)

	if _, ok := s.Get("session"); !ok {
		t.Fatal("the key was not readable immediately after being written")
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := s.Get("session"); ok {
		t.Error("an expired key was still readable")
	}
}

// Expiry has to hold even if the sweeper has not run, otherwise a key's
// lifetime silently depends on the sweep interval.
func TestExpiredEntriesAreInvisibleBeforeTheSweep(t *testing.T) {
	s := NewStore("node-a")
	s.CoordinatorWrite("k", "v", time.Nanosecond)

	if _, ok := s.Get("k"); ok {
		t.Error("an expired key was visible before any sweep")
	}
	if got := s.Len(); got != 0 {
		t.Errorf("Len = %d, want 0: expired keys should not be counted", got)
	}
	if removed := s.Sweep(); removed != 1 {
		t.Errorf("Sweep removed %d, want 1", removed)
	}
}

func TestSweepLeavesLiveEntriesAlone(t *testing.T) {
	s := NewStore("node-a")
	s.CoordinatorWrite("permanent", "v", 0)
	s.CoordinatorWrite("long", "v", time.Hour)
	s.CoordinatorWrite("gone", "v", time.Nanosecond)

	if removed := s.Sweep(); removed != 1 {
		t.Fatalf("Sweep removed %d, want 1", removed)
	}
	for _, key := range []string{"permanent", "long"} {
		if _, ok := s.Get(key); !ok {
			t.Errorf("%q was swept but should have survived", key)
		}
	}
}

// A replacement must outrank the expired value it replaces on every replica.
// Restarting versions at 1 would let a stale replica's copy of the old value
// win the next conflict resolution and resurrect it.
func TestVersionsKeepClimbingPastAnExpiredValue(t *testing.T) {
	s := NewStore("node-a")
	s.CoordinatorWrite("k", "first", time.Nanosecond)

	replacement := s.CoordinatorWrite("k", "second", 0)
	if replacement.Version != 2 {
		t.Errorf("version = %d, want 2", replacement.Version)
	}

	stale := Entry{Key: "k", Value: "first", Version: 1, Origin: "node-z"}
	if s.Apply(stale) {
		t.Error("a stale copy of the expired value was accepted")
	}
	got, _ := s.Get("k")
	if got.Value != "second" {
		t.Errorf("value = %q, want second", got.Value)
	}
}

// The expiry instant travels with the entry so every replica forgets the key
// at the same moment, rather than each starting its own clock on receipt.
func TestExpiryReplicatesAsAnAbsoluteInstant(t *testing.T) {
	source := NewStore("node-a")
	written := source.CoordinatorWrite("k", "v", 40*time.Millisecond)

	replica := NewStore("node-b")
	replica.Apply(written)

	time.Sleep(60 * time.Millisecond)

	if _, ok := replica.Get("k"); ok {
		t.Error("the replica still holds a key that expired on the coordinator")
	}
}
