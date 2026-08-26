package kv

import "time"

// Entry is one versioned record.
//
// Version is a per-key logical clock and Origin is the node that coordinated
// the write. Origin exists to break ties: under leaderless replication two
// nodes can independently allocate the same version for the same key, and
// comparing versions alone would let different replicas pick different winners
// and never converge. Comparing (Version, Origin) makes last-write-wins
// deterministic, so every replica resolves the conflict identically.
type Entry struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int64  `json:"version"`
	Origin  string `json:"origin,omitempty"`

	// ExpiresAt is a Unix millisecond timestamp; zero means the entry never
	// expires. It is replicated with the entry so every node expires it at the
	// same instant rather than each starting its own clock on receipt.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// Newer reports whether e supersedes other under last-write-wins.
//
// Expiry is deliberately not part of this comparison: it is a property of the
// value, not of its position in the write order.
func (e Entry) Newer(other Entry) bool {
	if e.Version != other.Version {
		return e.Version > other.Version
	}
	return e.Origin > other.Origin
}

// Expired reports whether the entry's lifetime has run out.
func (e Entry) Expired(now time.Time) bool {
	return e.ExpiresAt != 0 && now.UnixMilli() >= e.ExpiresAt
}
