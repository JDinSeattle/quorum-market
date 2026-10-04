# 2. The ship message is published after the commit

## Context

Checkout reserves stock, charges a card, commits an order, and then has to tell
the warehouse to ship it. The last step goes over a message queue, and it has
to sit on one side or the other of the transaction boundary.

Publishing inside the boundary risks the order failing to commit after the
warehouse has already been told to ship. Publishing after risks the order
committing and the publish failing, leaving an order that is paid for and
recorded but not queued.

## Decision

Publish after `end_transaction`, and treat a publish failure as a logged error
rather than a failed checkout.

## Consequences

A committed order can remain unqueued if the coordinator dies or publishing
fails after the core order write. The failure is logged when the process survives;
there is no transactional outbox or automatic replay. A reservation only holds
stock until its TTL. After release or expiry, the warehouse rejects a delayed
shipment instead of deducting stock again. Recovery requires reconciling the
order, payment, and current stock; blindly replaying an expired hold is unsafe.

The owner-confirmed cloud experiment in the README killed the coordinator in
this gap in ten trials: ten orders were readable and no ship messages arrived.
That is a separate cloud result, not a claim that this local regression run
repeated the kill experiment. In-memory quorum acknowledgements also do not
survive a full cluster restart.

Publishing after commit keeps a failed order from being shipped by a premature
message, but does not make order storage and RabbitMQ atomic. A durable outbox
and delivery/reconciliation protocol remain outside this implementation.
