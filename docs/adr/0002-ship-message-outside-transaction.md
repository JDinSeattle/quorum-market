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

The worst case is an order that is charged, recorded, and not yet handed to
fulfilment. That is visible in the logs, alertable
(`ShipMessagesNotPublishing`), and recoverable by replaying from the stored
order. The stock stays correctly accounted for in the meantime, because the
reservation still holds it.

The alternative's worst case is goods physically leaving the warehouse for an
order that does not exist, which nothing can undo.

The customer is never told their checkout failed after their card was charged.
That would be a lie, and it would send them to place the order again.

The cost is that the system is not transactionally consistent across the queue
boundary, and cannot be without an outbox table and a sweeper. That is the
right next step if this were carrying real orders; it is deliberately out of
scope here, and the gap is named rather than hidden.
