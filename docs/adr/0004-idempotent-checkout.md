# 4. Checkout is idempotent on a client-supplied key

## Context

Checkout charges a card. If the response is lost — a timeout, a reset
connection, a phone changing networks — the client cannot tell "it failed" from
"it worked and I did not hear about it". Retrying is the only reasonable thing
for it to do, and without protection that retry charges the customer twice and
reserves the stock twice.

This is not a hypothetical: the cart service sits behind a load balancer with a
request timeout, in front of three services each with their own.

## Decision

Accept an `Idempotency-Key` header. A key is claimed before any work begins and
the receipt is recorded against it before the response is written. A repeat of
a completed checkout replays the stored receipt; a repeat while one is still
running is refused with 409.

Only successful checkouts are recorded. A failed checkout releases the key, so
a customer whose card was declined can fix it and retry with the same key.

Claims are stored in the cart database with a TTL — 60 seconds while in
progress, 24 hours once completed — which is why the store grew a TTL.

## Consequences

A retry after a lost response returns the original order instead of buying the
cart again. `TestRetryWithTheSameKeyReplaysTheReceipt` asserts the card is
authorized exactly once.

This is deduplication, not a distributed lock. Two genuinely simultaneous
requests carrying the same key can both find it unclaimed and proceed; the
store offers no compare-and-set to prevent it. The case that actually happens —
a retry seconds after the original — is closed, and the remaining race is
documented rather than papered over.

If the process dies between charging the card and recording the receipt, the
claim stays in progress until it expires and the retry can then double-charge.
Closing that needs the charge and the record to commit together, which needs a
real transaction across two systems.

The key becomes part of a storage key and appears in logs, so it is validated
to a bounded length and character set.
