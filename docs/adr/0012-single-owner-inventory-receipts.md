# 12. Single-owner inventory receipts and versioned mutation

The warehouse owns an in-memory stock record per SKU: quantity, version, and a
mutex. Reservation normalizes and sorts item lines, checks summed quantities for
overflow, reads versions, then acquires all SKU locks in that canonical order.
Inside the critical section it compares versions, verifies every SKU has enough
stock, decrements quantities, and increments versions. A conflict retries at most
1,000 times and exhaustion returns 503. No provisional decrement is made when
another item is unavailable, so a failed acquisition has nothing to compensate.

`POST /warehouse/reserve` requires a bounded `request_id`:

```json
{"request_id":"purchase-001","items":[{"productId":"sku-1","quantity":1}]}
```

A response includes that ID, `reservationId`, `status`, and `expiresAt`. Duplicate
item lines are merged and sorted before the intent fingerprint is calculated.
Concurrent identical IDs share one receipt; mismatched intent returns 409. The
original successful reservation and failed-result fingerprints are retained until
restart. A replay of a released reservation reports `released` and cannot hold
stock again. Terminal state transitions follow [ADR 3](0003-reservations-resolve-once.md).

Warehouse inventory and receipts are not shared with other warehouse processes,
are not persisted, and have no garbage-collection policy. This is a bounded-load
lab with one warehouse owner, not a multi-owner distributed inventory algorithm.
The successful-receipt count excludes retained failed-result fingerprints; memory
use grows with all distinct request IDs. Restart resets both stock and receipts.
The cart's local key mutex is similarly limited to one checkout process.

## Quorum boundary

Product uses N=5/W=5/R=1, cart N=5/W=3/R=3, and core N=3/W=2/R=2.
The `quorums_intersect` statistic reports W+R>N. `linearizable` and
`compare_and_swap` remain false: these in-memory last-writer-wins replicas do not
supply either guarantee. Historical evidence reporting `strongly_consistent`
used an inaccurate field name that has been removed from current diagnostics.

Read, write, and prefix scan must collect their configured number of successful
replies or return 503. A failed write can still have reached some replicas; 503
is not rollback. A scan is a merged observation, not an atomic snapshot.
`TestFixedEighteenCellQuorumFaultMatrix` tests all three profiles with reachable
and insufficient replicas across the three operations. Each cell uses actual
local HTTP nodes. For product R=1, insufficient read/scan means the serving
coordinator is unavailable; losing a follower alone cannot break a local read.

## Local verification

```sh
go test -race -count=1 ./...
go test -race -count=1 ./internal/warehouse -run 'TestThousandRequests|TestSameRequest|TestHTTPReceipt|TestOppositeTerminal'
go test -race -count=1 ./internal/kv -run 'TestFixedEighteenCell|TestSequentialWrite'
go test -race -count=1 ./internal/cart -run TestConcurrentRequestID
go vet ./...
go build ./...
```

These tests exercise in-process inventory contention and real localhost HTTP
replication. They do not repeat the separate cloud benchmark's CPU/memory,
20 ms injected latency, eight-service RabbitMQ/Redis deployment, or coordinator
kill trials. See [local verification](../local-validation.json) for
this revision's measured test counts and coverage. README cloud results retain
the owner's supplied provenance and are not replaced by local test counts.
