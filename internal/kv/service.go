package kv

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// Service implements both replication strategies over a single Store. The
// strategies differ in who may coordinate a write and in how the quorums are
// configured; the fan-out machinery underneath is identical, so it is shared.
type Service struct {
	cfg   Config
	store *Store
	repl  *Replicator
}

// NewService wires a node's config, store and replicator together.
func NewService(cfg Config, store *Store, repl *Replicator) *Service {
	return &Service{cfg: cfg, store: store, repl: repl}
}

// Config exposes the node's configuration for status reporting.
func (s *Service) Config() Config { return s.cfg }

// Keys returns how many keys this node currently holds.
func (s *Service) Keys() int { return s.store.Len() }

// Write stores a value and replicates it until the write quorum is met.
//
// The coordinator's own copy is the first acknowledgement, so only W-1 peer
// acks are required. Once quorum is reached the call returns and the remaining
// replicas are updated in the background: waiting for the slowest replica
// would hand every write the tail latency of the worst node in the cluster.
//
// A ttl of zero stores the value indefinitely.
func (s *Service) Write(ctx context.Context, key, value string, ttl time.Duration) (Entry, error) {
	if key == "" {
		return Entry{}, httpx.Errorf(http.StatusBadRequest, "key must not be empty")
	}
	if s.cfg.Mode == ModeLeaderFollower && !s.cfg.IsLeader() {
		return Entry{}, httpx.Errorf(http.StatusBadRequest,
			"this node is a follower; writes must be sent to the leader")
	}

	// Stands in for the cost of making the write durable on this node.
	if err := sleepCtx(ctx, s.cfg.WriteDelay); err != nil {
		return Entry{}, httpx.Wrap(http.StatusServiceUnavailable, err, "write cancelled")
	}

	entry := s.store.CoordinatorWrite(key, value, ttl)

	if err := s.replicate(ctx, entry, s.cfg.WriteQuorum-1); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Read returns the newest value visible to a read quorum.
//
// With R=1 this is a purely local read. With R>1 the node polls peers in
// parallel, stops as soon as R replicas (itself included) have answered, and
// returns the highest-versioned entry among them.
func (s *Service) Read(ctx context.Context, key string) (Entry, bool, error) {
	if key == "" {
		return Entry{}, false, httpx.Errorf(http.StatusBadRequest, "key must not be empty")
	}
	if err := sleepCtx(ctx, s.cfg.ReadDelay); err != nil {
		return Entry{}, false, httpx.Wrap(http.StatusServiceUnavailable, err, "read cancelled")
	}

	local, haveLocal := s.store.Get(key)

	need := s.cfg.ReadQuorum - 1
	if need <= 0 || len(s.cfg.Peers) == 0 {
		return local, haveLocal, nil
	}
	return s.readQuorum(ctx, key, local, haveLocal, need)
}

// Scan returns entries under a prefix, merged across a read quorum.
//
// Taking the union of what R replicas hold is what makes this correct rather
// than best-effort. A key is only acknowledged once W replicas have it, and
// W + R > N means any R replicas intersect every such set — so every committed
// key appears in at least one of the responses. Where two replicas hold
// different versions of the same key, the newer wins, exactly as in a
// point read.
//
// This works because keys are added and never deleted. A store with deletes
// would need tombstones for the union to be able to tell "not written yet"
// from "written and removed".
func (s *Service) Scan(ctx context.Context, prefix string, limit int) ([]Entry, error) {
	if err := sleepCtx(ctx, s.cfg.ReadDelay); err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "scan cancelled")
	}

	merged := make(map[string]Entry)
	for _, entry := range s.store.Scan(prefix, limit) {
		merged[entry.Key] = entry
	}

	need := s.cfg.ReadQuorum - 1
	if need > 0 && len(s.cfg.Peers) > 0 {
		s.mergePeerScans(ctx, prefix, limit, need, merged)
	}

	out := make([]Entry, 0, len(merged))
	for _, entry := range merged {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	if len(out) > limit && limit > 0 {
		out = out[:limit]
	}
	return out, nil
}

func (s *Service) mergePeerScans(ctx context.Context, prefix string, limit, need int, merged map[string]Entry) {
	peers := s.cfg.Peers

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RPCTimeout)
	var wg sync.WaitGroup

	type scanResult struct {
		entries []Entry
		err     error
	}
	results := make(chan scanResult, len(peers))

	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			entries, err := s.repl.Scan(rctx, peer, prefix, limit)
			results <- scanResult{entries: entries, err: err}
		}(peer)
	}
	go func() { wg.Wait(); cancel() }()

	answered := 0
	for range peers {
		result := <-results
		if result.err != nil {
			continue
		}
		for _, entry := range result.entries {
			if held, ok := merged[entry.Key]; !ok || entry.Newer(held) {
				merged[entry.Key] = entry
			}
		}
		if answered++; answered >= need {
			return
		}
	}
}

// ApplyReplication handles an entry pushed by another node.
func (s *Service) ApplyReplication(ctx context.Context, e Entry) error {
	if e.Key == "" {
		return httpx.Errorf(http.StatusBadRequest, "key must not be empty")
	}
	// Every replica pays the same simulated durability cost as the coordinator,
	// which is what makes the write quorum cost something to satisfy.
	if err := sleepCtx(ctx, s.cfg.WriteDelay); err != nil {
		return httpx.Wrap(http.StatusServiceUnavailable, err, "replication cancelled")
	}
	s.store.Apply(e)
	return nil
}

// LocalScan returns this node's own view of a prefix, with no fan-out.
func (s *Service) LocalScan(prefix string, limit int) []Entry {
	return s.store.Scan(prefix, limit)
}

// LocalRead returns this node's own copy with no delay and no quorum. It backs
// the /kv/local endpoint, which exists so tests and demos can observe a
// replica that has not caught up yet.
func (s *Service) LocalRead(key string) (Entry, bool) { return s.store.Get(key) }

// ── replication fan-out ──────────────────────────────────────────────────────

func (s *Service) replicate(ctx context.Context, e Entry, need int) error {
	peers := s.cfg.Peers
	if len(peers) == 0 {
		if need > 0 {
			return httpx.Errorf(http.StatusServiceUnavailable,
				"write quorum of %d unreachable: node has no peers", s.cfg.WriteQuorum)
		}
		return nil
	}

	// Replication must outlive the request that triggered it, otherwise
	// returning early at quorum would cancel the replicas still in flight.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RPCTimeout)

	if need <= 0 {
		// Quorum is already satisfied by the coordinator alone (W=1).
		var wg sync.WaitGroup
		s.pushAsync(rctx, &wg, peers, e)
		go func() { wg.Wait(); cancel() }()
		return nil
	}

	if s.cfg.Sequential {
		return s.replicateSequential(rctx, cancel, peers, e, need)
	}
	return s.replicateParallel(rctx, cancel, peers, e, need)
}

func (s *Service) replicateParallel(ctx context.Context, cancel context.CancelFunc,
	peers []string, e Entry, need int) error {

	var wg sync.WaitGroup
	acks := make(chan error, len(peers))
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			acks <- s.push(ctx, peer, e)
		}(peer)
	}
	go func() { wg.Wait(); cancel() }()

	acked, failed := 0, 0
	for range peers {
		if err := <-acks; err != nil {
			failed++
			// Stop early once success has become arithmetically impossible.
			if len(peers)-failed < need-acked {
				obs.ObserveQuorumFailure("write")
				return httpx.Wrap(http.StatusServiceUnavailable, err,
					"write quorum of %d not met for key %q", s.cfg.WriteQuorum, e.Key)
			}
			continue
		}
		acked++
		if acked >= need {
			// The remaining pushes keep running; their results land in the
			// buffered channel and are simply never read.
			return nil
		}
	}
	return nil
}

// replicateSequential pushes to one peer at a time. It is slower by design:
// widening the window in which some replicas hold the new value and others
// still hold the old one is what makes stale reads observable in a load test.
func (s *Service) replicateSequential(ctx context.Context, cancel context.CancelFunc,
	peers []string, e Entry, need int) error {

	acked, failed := 0, 0
	for i, peer := range peers {
		if err := s.push(ctx, peer, e); err != nil {
			failed++
			if len(peers)-failed < need-acked {
				cancel()
				obs.ObserveQuorumFailure("write")
				return httpx.Wrap(http.StatusServiceUnavailable, err,
					"write quorum of %d not met for key %q", s.cfg.WriteQuorum, e.Key)
			}
			continue
		}
		acked++
		if acked >= need {
			var wg sync.WaitGroup
			s.pushAsync(ctx, &wg, peers[i+1:], e)
			go func() { wg.Wait(); cancel() }()
			return nil
		}
	}
	cancel()
	return nil
}

func (s *Service) pushAsync(ctx context.Context, wg *sync.WaitGroup, peers []string, e Entry) {
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			_ = s.push(ctx, peer, e)
		}(peer)
	}
}

func (s *Service) push(ctx context.Context, peer string, e Entry) error {
	err := s.repl.Push(ctx, peer, e)
	if err != nil {
		slog.Warn("replication failed", "peer", peer, "key", e.Key, "version", e.Version, "err", err)
	}
	return err
}

// ── read fan-out ─────────────────────────────────────────────────────────────

type peerRead struct {
	peer  string
	entry Entry
	found bool
	err   error
}

func (s *Service) readQuorum(ctx context.Context, key string, local Entry, haveLocal bool, need int) (Entry, bool, error) {
	peers := s.cfg.Peers

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RPCTimeout)
	var wg sync.WaitGroup
	results := make(chan peerRead, len(peers))
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			e, found, err := s.repl.Fetch(rctx, peer, key)
			results <- peerRead{peer: peer, entry: e, found: found, err: err}
		}(peer)
	}
	go func() { wg.Wait(); cancel() }()

	best, bestFound := local, haveLocal
	collected := make([]peerRead, 0, len(peers))

	answered, failed := 0, 0
	for range peers {
		r := <-results
		if r.err != nil {
			failed++
			if len(peers)-failed < need-answered {
				obs.ObserveQuorumFailure("read")
				return Entry{}, false, httpx.Wrap(http.StatusServiceUnavailable, r.err,
					"read quorum of %d not met for key %q", s.cfg.ReadQuorum, key)
			}
			continue
		}
		answered++
		collected = append(collected, r)
		if r.found && (!bestFound || r.entry.Newer(best)) {
			best, bestFound = r.entry, true
		}
		if answered >= need {
			break
		}
	}

	if bestFound {
		// Deliberately not rctx: that context is cancelled as soon as the last
		// fetch returns, which would kill every repair before it was sent.
		s.repair(ctx, local, haveLocal, best, collected)
	}
	return best, bestFound, nil
}

// repair brings replicas that answered with stale data back up to date.
//
// Repairing the local store is unconditional and free: this node just learned
// a newer version, so there is no reason to keep serving the old one. Pushing
// the winner back out to stale peers is optional (READ_REPAIR) because it adds
// write traffic to the read path.
func (s *Service) repair(ctx context.Context, local Entry, haveLocal bool, best Entry, collected []peerRead) {
	if !haveLocal || best.Newer(local) {
		s.store.Apply(best)
	}
	if !s.cfg.ReadRepair {
		return
	}

	stale := make([]string, 0, len(collected))
	for _, r := range collected {
		if r.found && !best.Newer(r.entry) {
			continue // already current
		}
		stale = append(stale, r.peer)
	}
	if len(stale) == 0 {
		return
	}

	// Repairs outlive the read that noticed the divergence, so they get their
	// own detached context rather than the request's.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RPCTimeout)
	var wg sync.WaitGroup
	for _, peer := range stale {
		obs.ObserveReadRepair()
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			if err := s.repl.Push(rctx, peer, best); err != nil {
				slog.Debug("read repair failed", "peer", peer, "key", best.Key, "err", err)
			}
		}(peer)
	}
	go func() { wg.Wait(); cancel() }()
}

// ── helpers ──────────────────────────────────────────────────────────────────

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
