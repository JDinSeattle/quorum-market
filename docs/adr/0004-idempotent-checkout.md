# 4. Checkout is idempotent on a client-supplied key

Checkout accepts the `Idempotency-Key` header, the `idempotencyKey` body field,
and the `request_id` body alias, in that precedence order. The key is validated
before reserving or charging. Within one `CheckoutService`, requests with the
same key serialize on a process-local mutex. A concurrent retry waits, then
replays the completed receipt. The cart KV records an in-progress claim for
60 seconds and a successful receipt for 24 hours. An incomplete persisted claim
returns 409 until its window ends. Reusing a key for another cart returns 409.

Checkout uses its generated order ID as the warehouse request ID for that
attempt. Warehouse HTTP transport retries therefore reuse the same inventory
receipt. A failed checkout releases its pending reservation and checkout claim;
a subsequent corrected purchase attempt creates a new order/warehouse request ID.

`TestConcurrentRequestIDReplaysOneCheckout` sends 32 concurrent requests using
one key and checks one card authorization, one inventory receipt, one stock
decrement, one ship message, and one shared order ID. The sequential retry and
failed-payment regressions remain in `internal/cart/idempotency_test.go`.

This requires one checkout owner for each key. Multiple checkout processes can
still race because the KV claim uses separate read and write operations; the
store has no distributed CAS. Quorum intersection does not change that. Request
mutexes remain in memory until process exit. A receipt write failure or process
crash between charging and recording can leave an unresolved claim and a later
retry can charge again after expiry. There is no cross-service transaction or
durable payment idempotency guarantee here.
