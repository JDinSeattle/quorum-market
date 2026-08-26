# 9. Events on a topic exchange, commands on a work queue

## Context

The system sends two kinds of message and they were initially both work queues.

"Ship this order" is a **command**: it has exactly one correct handler, it must
happen once, and the sender is entitled to expect it to be acted on.

"This order was placed" is an **event**: a statement of fact. When the order
service and the notification service were added, both needed to hear it. On a
work queue they would have competed — whichever consumer grabbed a message
would deny it to the other — so orders would have been recorded *or*
notifications sent, alternately, at random.

## Decision

Keep the work queue for commands. Add a topic exchange for events, where each
subscriber declares its own queue and binds the routing keys it cares about.

```
cart ──command──► orders_queue ──────────────► warehouse

cart ──event────► ecommerce.events ──┬──► order-service.events
warehouse ─event─►                   └──► notification-service.events
```

## Consequences

A new subscriber is added without touching any publisher. The notification
service was built after the cart and warehouse services and neither knows it
exists — which is the property the exchange was introduced for, and the
clearest demonstration of what it buys.

Each subscriber owns its queue, so a slow or stopped one builds its own backlog
instead of starving the others.

Publishers no longer know who is listening, which is the point and also the
cost: nothing checks that anyone *is*. A subscriber that silently stops
consuming is invisible to the publisher, and only queue depth reveals it.

The distinction is worth keeping straight because it decides the failure mode.
A lost command means work that will not happen; a lost event means a projection
that is stale. The first needs a dead-letter queue and someone to look at it;
the second is often recoverable by replaying from whatever holds the truth.
