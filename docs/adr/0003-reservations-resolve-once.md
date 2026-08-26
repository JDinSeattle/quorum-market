# 3. A reservation resolves exactly once

## Context

Checkout deducts stock at reserve time, before payment. That reservation then
ends in one of three ways: released when the checkout fails, retired when the
order ships, or reclaimed when it has been held too long.

The obvious implementation — release restores stock, and a sweeper also
restores anything old — double-counts. A released reservation is still in the
sweeper's map, so its stock is handed back twice. Every declined payment
inflates inventory a little, and under sustained load it grows without bound.

The mirror-image bug is on the success path: if shipping deducts stock again,
having already deducted it at reserve time, every completed order removes twice
what it sold.

## Decision

Presence in the reservation map *is* pendingness. Every path — release, ship,
expire — retires the reservation by removing it from the map under a lock, and
only acts on the stock if the removal succeeded.

Shipping does not deduct: the stock came off the shelf when it was reserved, so
shipping only retires the hold.

## Consequences

Each reservation affects the ledger exactly once, whichever way it ends, and
the paths cannot race each other because they all go through the same removal.

Releases become idempotent for free, which matters because the cart service
retries them on cleanup paths.

One case still needs handling: a ship message that arrives after its
reservation expired, because the queue was backed up past the TTL. The goods
did leave, so stock is deducted then instead, clamped at zero and counted as an
oversell. That is visible in `/warehouse/stats` and alertable.

The regression tests for both directions are the reason this is written down:
`TestReleaseAndExpiryDoNotBothRestore` and `TestShipDoesNotDeductTwice`.
