# 10. Event handlers are commutative

## Context

The order service learns about an order from two events. `order.placed` comes
from the cart service; `order.shipped` comes from the warehouse. They are
published by different services, over different paths, at almost the same
moment.

The first implementation assumed they would arrive in that order, and returned
an error when a shipment arrived for an order it had not recorded — expecting
the requeue to give the placement event time to land.

It did not work, and the platform smoke test caught it: the requeue was
redelivered four milliseconds later, failed again, and the message was parked
on the dead-letter queue. The order stayed "placed" forever while the goods had
already shipped.

Retrying was the wrong instrument. No number of retries makes the order of two
independent publishers guaranteed.

## Decision

Make the handlers commutative: either event may arrive first, and the result is
the same.

- `HandlePlaced` owns the *details* — items, totals, cart, timestamp — and
  never touches the status.
- `HandleShipped` owns the *status* and creates a stub record if none exists.
- A record with no placement timestamp is marked `awaitingDetails`, so a caller
  seeing an order with no line items knows why.

The cart service also now publishes the placement before queueing the ship
command, so the ordinary case is the ordered one. That is an optimisation, not
the fix.

## Consequences

The ordering dependency is gone rather than mitigated. `TestEventsCanArriveInEitherOrder`
runs the same pair of events both ways round and asserts the same final record.

Retries are left for what they are actually good at: genuine transient
failures, like a database that is briefly unavailable.

This is the general shape for consuming an event stream with no global
ordering. Each handler owns a disjoint part of the state and never overwrites
what it does not own, so the handlers commute and redelivery is harmless. It is
more thought per handler than "assume order and retry", and it is the
difference between a system that works and one that works most of the time.

The stub record is visible to clients for as long as the placement event takes
to arrive — normally milliseconds. Surfacing `awaitingDetails` rather than
hiding it means a client can tell an incomplete record from an empty order.
