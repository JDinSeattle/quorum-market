# 3. A reservation resolves exactly once

Checkout deducts inventory when it reserves, before payment. The warehouse
retains a reservation in exactly one of `reserved`, `committed`, or `released`.
Release, expiry, and shipment compete under the reservation mutex; only
`reserved -> committed` or `reserved -> released` is permitted. Terminal records
are retained for the lifetime of this warehouse process.

A shipment commits the existing reservation and never deducts stock. Repeating
a matching committed shipment returns `already-committed`. An unknown, released,
expired, or item-mismatched shipment is rejected and sent to the dead-letter
path. In particular, a delayed message cannot recreate an expired reservation or
deduct inventory that has already been released. Release and expiry restore a
legitimate reservation once; cancellation after commit cannot restore stock.

The request ID maps to normalized items and the original reservation. Concurrent
retries wait for its initial result, and later retries return the same reservation
with its current state. Reusing an ID with different items returns 409. Rejected
initial attempts also retain their error, so a new purchase attempt needs a new
request ID. Only successful reservations count in the `receipts` statistic.

`TestThousandRequestsAndThreeHundredReceiptReplays` exercises 100 clients with
10 distinct requests each, then three replays of each of the 100 successful
receipts. It checks 100 successful stock decrements, 900 sold-out results, and no
new decrements from 300 replays. Twenty reservations each receive five concurrent
cancellations while 80 commit: there are 20 releases, 80 committed units, and 20
available units. It also rejects released-to-committed transitions and repeats
commits without additional stock movement.

The single-owner locking and retention limits are documented in
[ADR 12](0012-single-owner-inventory-receipts.md). Historical evidence of
"compensated" late shipments describes the previous implementation and does not
specify the current contract.
