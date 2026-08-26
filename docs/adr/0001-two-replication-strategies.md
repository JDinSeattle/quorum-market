# 1. Two replication strategies for two workloads

## Context

The system stores two kinds of data with opposite access patterns.

The product catalogue is written once when the environment is seeded and then
read constantly — browsing is the highest-volume path in the system by a wide
margin. Shopping carts are the reverse: every shopper writes to their own key
repeatedly over a session, and each key is read almost exclusively by the one
shopper who owns it.

A single storage design has to be a compromise between these, and the
compromise is bad in both directions: a leader that serialises cart writes
becomes a queue, and a quorum read on every catalogue lookup pays coordination
cost for data that has not changed in hours.

## Decision

Run two clusters of the same binary with different replication strategies,
selected by `NODE_MODE`.

**Catalogue — leader-follower, W=5, R=1.** One leader accepts writes and
replicates to all four followers before acknowledging. Reads are served from a
single node with no coordination at all.

**Carts — leaderless, W=3, R=3 over five nodes.** Any node coordinates any
write. `W + R > N` keeps the quorums overlapping, so a read cannot miss a
committed write. Conflicts are resolved last-write-wins on `(version, origin)`,
with the origin as tie-break so every replica converges on the same winner.

## Consequences

Catalogue writes are as expensive as the slowest replica, which is acceptable
because they happen at seed time and nowhere near the customer path. In
exchange, the hottest path in the system is a local map lookup.

Cart writes have no single bottleneck and survive two nodes being down, at the
cost of touching three nodes per read instead of one.

Two configurations mean two sets of failure behaviour to understand rather than
one. The mitigation is that they share an implementation: the quorum fan-out,
the conflict resolution and the transport are the same code, so a fix to either
is a fix to both.

Last-write-wins silently discards a concurrent update to the same key. For
carts this is acceptable — a customer edits their own cart from one place at a
time — and it would not be for anything with cross-key invariants.
