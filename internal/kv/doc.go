// Package kv implements the distributed key-value store that backs both
// databases in this system, and the client services use to reach them.
//
// One binary serves two very different workloads by switching replication
// strategy at startup:
//
//   - leader-follower, where one node accepts every write and replicates it
//     out. Suits the product catalogue: reads vastly outnumber writes, so
//     paying for an expensive write to buy a single-node read is a good trade.
//
//   - leaderless, where any node coordinates any write. Suits shopping carts:
//     writes are frequent and spread across many independent keys, so a single
//     leader would only build a queue.
//
// Both share the same quorum machinery. A write is acknowledged once W
// replicas hold it, counting the coordinator; a read consults R replicas and
// returns the highest version among them. Choosing W + R > N makes the two
// quorums overlap, which is what guarantees a read sees the latest
// acknowledged write.
//
// Concurrent writes are resolved by last-write-wins on (version, origin node).
// The origin is the tie-break: two coordinators can independently allocate the
// same version for a key, and ordering by version alone would let replicas
// disagree about the winner and never converge.
package kv
