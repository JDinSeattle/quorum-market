# Architecture decision records

Short notes on decisions that were not obvious, written down because the code
shows *what* was chosen and almost never *what else was considered*.

Each one states the situation, the decision, and what it costs. A decision with
no cost listed is usually a decision that was not really made.

| # | Decision |
|---|---|
| [0001](0001-two-replication-strategies.md) | Two replication strategies for two workloads |
| [0002](0002-ship-message-outside-transaction.md) | The ship message is published after the commit |
| [0003](0003-reservations-resolve-once.md) | A reservation resolves exactly once |
| [0004](0004-idempotent-checkout.md) | Checkout is idempotent on a client-supplied key |
| [0005](0005-readiness-reports-degraded.md) | Readiness reports degraded, not down, for shared dependencies |
| [0006](0006-one-image-many-services.md) | One image holds every service |
| [0007](0007-burn-cpu-not-sleep.md) | Simulated work burns CPU rather than sleeping |
| [0008](0008-gateway-is-the-only-public-entry.md) | The gateway is the only public entry |
| [0009](0009-events-and-commands.md) | Events on a topic exchange, commands on a work queue |
| [0010](0010-event-handlers-are-commutative.md) | Event handlers are commutative |
| [0011](0011-redis-for-four-jobs.md) | Redis for four jobs, and what happens when it is gone |
