package kv

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the in-memory, versioned key-value map backing a single node.
//
// Durability is deliberately out of scope: the point of this system is
// replication and quorum behaviour, and the configurable write delay stands in
// for the cost of an fsync.
type Store struct {
	mu     sync.RWMutex
	data   map[string]Entry
	nodeID string
}

// NewStore returns an empty store owned by nodeID.
func NewStore(nodeID string) *Store {
	return &Store{data: make(map[string]Entry), nodeID: nodeID}
}

// Get returns the entry held for key.
//
// An expired entry is reported as absent even before the sweeper removes it,
// so a key's lifetime does not depend on how recently the sweeper ran.
func (s *Store) Get(key string) (Entry, bool) {
	s.mu.RLock()
	e, ok := s.data[key]
	s.mu.RUnlock()

	if !ok || e.Expired(time.Now()) {
		return Entry{}, false
	}
	return e, true
}

// CoordinatorWrite allocates the next version for key and stores the value
// locally, all under one lock so two concurrent writers on the same node can
// never hand out the same version.
//
// A ttl of zero means the entry never expires.
func (s *Store) CoordinatorWrite(key, value string, ttl time.Duration) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := int64(1)
	// An expired predecessor still contributes its version. Restarting the
	// clock at 1 would let a stale replica's copy of the old value outrank the
	// new one and win the next conflict resolution.
	if prev, ok := s.data[key]; ok {
		next = prev.Version + 1
	}

	e := Entry{Key: key, Value: value, Version: next, Origin: s.nodeID}
	if ttl > 0 {
		e.ExpiresAt = time.Now().Add(ttl).UnixMilli()
	}
	s.data[key] = e
	return e
}

// Apply stores a replicated entry, but only if it supersedes what is already
// held. Rejecting stale writes is what makes replication idempotent and makes
// out-of-order or duplicated delivery harmless.
func (s *Store) Apply(e Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if prev, ok := s.data[e.Key]; ok && !e.Newer(prev) {
		return false
	}
	s.data[e.Key] = e
	return true
}

// Len returns the number of unexpired keys held.
func (s *Store) Len() int {
	now := time.Now()

	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, e := range s.data {
		if !e.Expired(now) {
			count++
		}
	}
	return count
}

// Scan returns up to limit unexpired entries whose key starts with prefix.
//
// This walks every key, which is honest about what it is: an in-memory map has
// no ordered index to range over. It is bounded by limit and intended for
// collections that are small per prefix — one customer's orders, not the whole
// catalogue. A store meant to serve this pattern at scale would keep a sorted
// index instead.
func (s *Store) Scan(prefix string, limit int) []Entry {
	if limit <= 0 {
		limit = 1000
	}
	now := time.Now()

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, 0, min(limit, len(s.data)))
	for key, entry := range s.data {
		if len(out) >= limit {
			break
		}
		if !strings.HasPrefix(key, prefix) || entry.Expired(now) {
			continue
		}
		out = append(out, entry)
	}

	// Map iteration is randomised, so without this the same scan returns a
	// different order every time and a client cannot page or diff results.
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Sweep removes expired entries.
//
// Lazy expiry on read is enough for correctness but not for memory: keys that
// are written once and never read again — an idempotency record whose retry
// never comes — would otherwise be held forever.
func (s *Store) Sweep() int {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for key, e := range s.data {
		if e.Expired(now) {
			delete(s.data, key)
			removed++
		}
	}
	return removed
}

// RunSweeper sweeps on a ticker until ctx is cancelled.
func (s *Store) RunSweeper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if removed := s.Sweep(); removed > 0 {
				slog.Debug("swept expired entries", "count", removed, "node", s.nodeID)
			}
		}
	}
}
