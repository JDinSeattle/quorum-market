# Quorum Market

[![CI](https://github.com/JDinSeattle/quorum-market/actions/workflows/ci.yml/badge.svg)](https://github.com/JDinSeattle/quorum-market/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![Evidence](https://img.shields.io/badge/evidence-19%2F19%20gates-brightgreen)](evidence/latest/SUMMARY.md)

A distributed e-commerce backend in Go. Eight microservices behind an API
gateway, three custom key-value clusters with different replication
strategies, Redis doing four different jobs, RabbitMQ carrying both commands
and events, and enough AWS to autoscale under load.

| | |
|---|---|
| **Services** | 8, plus 13 database nodes, Redis and RabbitMQ — 23 containers |
| **Storage** | Three quorum-replicated key-value clusters, two replication strategies |
| **Messaging** | A work queue for commands, a topic exchange for events, dead-letter queues for both |
| **Consistency** | `W + R > N` three times over, with three different answers |
| **Tests** | 195, all under the race detector; two end-to-end suites against a live stack |
| **Verification** | [19 gates recorded per run](evidence/latest/SUMMARY.md), full output committed |

It exists to make distributed systems trade-offs concrete: what a write quorum
costs, why carts and catalogues want opposite storage designs, where a
transaction boundary belongs when there is no real transaction to be had, why
event handlers have to commute, and what actually breaks when inventory
bookkeeping is subtly wrong.

> **quorum** — the smallest number of members that lets an assembly act.
> **market** — an assembly of traders.
>
> Both words are about enough independent parties agreeing to get something
> done, which is the whole problem here. `W + R > N` appears three times in
> this repository with three different answers, and every one of them is a
> judgement about how much agreement a particular kind of data is worth paying
> for.

```
                                internet
                                    │
                      ┌─────────────▼──────────────┐
                      │      ALB :80 (public)      │
                      └─────────────┬──────────────┘
                                everything
                      ┌─────────────▼──────────────┐
                      │     Gateway  :8080         │   verify token
                      │     autoscaled             │   rate limit (Redis)
                      └─┬────┬───────┬───────┬───┬─┘   inject identity
           /identity*   │    │       │       │   │   /notifications*
        ┌───────────────┘    │       │       │   └──────────────┐
        │        /product*   │       │       │  /orders*        │
        │     ┌──────────────┘       │       └────────┐         │
        ▼     ▼                      ▼                ▼         ▼
   ┌─────────┐ ┌───────────┐  ┌─────────────┐  ┌──────────┐ ┌──────────────┐
   │Identity │ │  Product  │  │Shopping Cart│  │  Orders  │ │Notifications │
   │  :8085  │ │   :8081   │  │    :8082    │  │  :8086   │ │    :8087     │
   │         │ │autoscaled │  │ autoscaled  │  │          │ │              │
   └────┬────┘ └─────┬──┬──┘  └──┬───┬───┬──┘  └────┬─────┘ └──────┬───────┘
        │            │  │        │   │   │          │              │
        │      ┌─────▼┐ │ ┌──────▼┐  │   │    ┌─────▼──────┐       │
        └─────►│core  │ │ │cart-db│  │   │    │  core-db   │◄──────┤
               │ -db  │ │ │5 nodes│  │   │    │  3 nodes   │       │
               │3 node│ │ │W3 R3  │  │   │    │  W2 R2     │       │
               └──────┘ │ └───────┘  │   │    └────────────┘       │
                        │            │   │                         │
              ┌─────────▼──┐         │   │                         │
              │ product-db │         │   │                         │
              │ leader + 4 │         │   │                         │
              │ W=5  R=1   │         │   │                         │
              └────────────┘         │   │                         │
                                     │   │                         │
   ┌─────────────────────────────────▼───▼─────────────────────────▼───────┐
   │  Redis        cache · rate limits · sessions · inboxes                │
   ├───────────────────────────────────────────────────────────────────────┤
   │  RabbitMQ     orders_queue (commands) · ecommerce.events (fan-out)    │
   ├───────────────────────────────────────────────────────────────────────┤
   │  Warehouse :8084   reserve/release (sync) · ship (async)              │
   │  Credit Card Authorizer :8083                                         │
   └───────────────────────────────────────────────────────────────────────┘
```

Only the gateway is publicly reachable. Everything else answers on a
VPC-restricted listener, which is what makes the identity headers the services
trust safe to trust.

## Contents

- [Quick start](#quick-start)
- [Services](#services)
- [The three databases](#the-three-databases)
- [What Redis is doing](#what-redis-is-doing)
- [Authentication and the gateway](#authentication-and-the-gateway)
- [Commands, events, and why they differ](#commands-events-and-why-they-differ)
- [Checkout and its transaction boundary](#checkout-and-its-transaction-boundary)
- [Inventory bookkeeping](#inventory-bookkeeping)
- [Retry safety](#retry-safety)
- [Staying up when things go wrong](#staying-up-when-things-go-wrong)
- [Observability](#observability)
- [Simulated latency, and why it burns CPU](#simulated-latency-and-why-it-burns-cpu)
- [API](#api)
- [Measured behaviour](#measured-behaviour)
- [Testing](#testing)
- [Test evidence](#test-evidence)
- [Load testing](#load-testing)
- [Deploying to AWS](#deploying-to-aws)
- [Configuration](#configuration)
- [Repository layout](#repository-layout)
- [What is deliberately not real](#what-is-deliberately-not-real)

## Quick start

Requires Go 1.25+ and Docker.

```bash
make up             # build and start 23 containers, waiting until healthy
make seed           # fill the catalogue with 1000 products
make smoke          # exercise both use cases and the platform, end to end
make observability  # add Prometheus and Grafana
make evidence       # run every gate and record the results
```

A whole customer journey through the front door:

```bash
GW=http://localhost:8080

TOKEN=$(curl -s -X POST $GW/identity/register -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"correct-horse-battery"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["accessToken"])')

curl -s $GW/product/p1                                     # public
CART=$(curl -s -X POST $GW/shopping-cart -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["cartId"])')

curl -s -X POST $GW/shopping-cart/$CART/add-item -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"productId":"p1","quantity":2}'

curl -s -X POST $GW/shopping-cart/$CART/checkout -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" \
  -d '{"creditCard":"4111-1111-1111-1111"}'

curl -s $GW/orders -H "Authorization: Bearer $TOKEN"          # arrived via an event
curl -s $GW/notifications -H "Authorization: Bearer $TOKEN"   # same event, different subscriber
```

| | |
|---|---|
| API | <http://localhost:8080> |
| Grafana | <http://localhost:3000> — the *Quorum Market* dashboard |
| Prometheus | <http://localhost:9490> |
| RabbitMQ | <http://localhost:15672> (guest/guest) |

`make down` tears everything down. `make help` lists every target.

## Services

| Service | Port | Responsibility |
|---|---|---|
| **Gateway** | 8080 | The only public entry: verifies tokens, enforces a shared rate limit, injects identity, routes |
| **Identity** | 8085 | Accounts, credentials, sessions, token issue and rotation |
| **Product** | 8081 | Catalogue, read through a Redis cache |
| **Shopping Cart** | 8082 | Cart state and the checkout orchestration |
| **Orders** | 8086 | Order lifecycle and history, driven entirely by events |
| **Notifications** | 8087 | Turns order events into customer messages |
| **Credit Card Authorizer** | 8083 | Stub payment gateway: approves 90% of well-formed cards |
| **Warehouse** | 8084 | Stock ledger, reservations, and the consumer that ships confirmed orders |

All eight are the same container image; the command selects which one runs.
They share HTTP plumbing, configuration, resilience, auth and instrumentation
through `internal/`, so there is one implementation of each — and a client
lives next to the server it calls, so the two sides of a contract cannot drift.

## The three databases

All three clusters run the same binary (`cmd/kvnode`). `NODE_MODE` picks the
replication strategy, because the workloads want different things.

| | Product DB | Cart DB | Core DB |
|---|---|---|---|
| Strategy | Leader-follower | Leaderless | Leaderless |
| Nodes | 1 leader + 4 followers | 5 peers | 3 peers |
| Quorums | W=5, R=1 | W=3, R=3 | W=2, R=2 |
| Holds | Catalogue | Carts, orders, idempotency keys | Accounts, order history |
| Workload | Read-heavy, written at seed time | Write-heavy, one hot key per shopper | Low volume, must not be lost |

**The catalogue** is read constantly and written almost never. A full-cluster
write is cheap when writes are rare, and it buys reads that are a single local
map lookup with no coordination at all.

**Carts** are the opposite: every shopper writes their own key, constantly. One
leader would build a queue in front of a node with no reason to be a
bottleneck. W=3 and R=3 over 5 nodes keeps `W + R > N`, so the quorums overlap
and a cart read cannot miss a committed cart write, while tolerating two nodes
down.

**Accounts and orders** are written rarely and must survive. Three nodes with
W=2/R=2 still satisfies `W + R > N` at a lower cost per write, and tolerates
one node down. Identity and orders share this cluster — a deliberate
compromise, with separate key namespaces so splitting them later is
configuration rather than migration.

Concurrent writes to one key from different coordinators are resolved by
last-write-wins on `(version, origin_node)`. The origin is the tie-break: two
nodes can independently allocate the same version, and comparing versions alone
would let replicas disagree about the winner and never converge.

Reads that observe a stale replica repair it in the background. Entries can
carry a TTL, which is what keeps idempotency records from accumulating forever.
A prefix scan merges the union of a read quorum's views — correct, because a
key acknowledged by W replicas must appear in any R of them when `W + R > N`.

→ [ADR 1: two replication strategies](docs/adr/0001-two-replication-strategies.md)

## What Redis is doing

Four jobs, separated by key prefix, each with its own answer to "what if Redis
is gone":

| Prefix | Use | On outage |
|---|---|---|
| `product:` | Catalogue read-through cache | **Fails open** — reads go to the database. A cache that takes the site down is worse than no cache |
| `ratelimit:` | Sliding-window counters, shared across gateway instances | **Fails open** — this limiter protects capacity, and refusing everything would turn a cache outage into an API outage |
| `session:` | Refresh tokens and the revocation denylist | **Fails open** on revocation checks — the alternative logs out every customer at once |
| `notifications:` | Per-customer inbox, capped and expiring | Identity and notifications **refuse to start** without it: sessions and inboxes live there |

The cache does three things beyond storing values, each earning its place.
Concurrent misses for the same key are collapsed, so an expiring hot key does
not stampede the database at the moment it is busiest. Absences are remembered,
so a crawler walking product ids stops reaching the database at all. TTLs are
jittered, so a batch of keys loaded together does not expire together.

The rate limiter runs a Lua script, because the check and the record have to be
atomic — as separate commands, concurrent requests all read a count under the
limit and all admit themselves, which is exactly the burst being prevented. It
takes its timestamp from Redis rather than from the caller, so clock skew
between gateway instances cannot shift the window.

→ [ADR 11: Redis for four jobs](docs/adr/0011-redis-for-four-jobs.md)

## Authentication and the gateway

The gateway verifies a JWT locally — no network round trip per request — and
forwards the caller's identity in `X-Customer-Id`. Services read that header
instead of parsing tokens.

**It strips those headers before setting them.** A client that could send
`X-Customer-Id` would be able to act as any customer: read anyone's orders,
check out on anyone's card. Three `Header.Del` calls are the entire reason the
downstream trust is safe, and there is a test whose only job is to prove they
are still there.

Access tokens live fifteen minutes. Refresh tokens are opaque, stored only as a
hash, and **rotated on use** — so a stolen one is usable at most once, and its
use immediately breaks the real customer's session, which is a detectable
signal rather than a silent compromise. Logout adds the session to a denylist
for the remainder of the access token's life, because a signed token cannot be
recalled.

Services still enforce **authorization** themselves. The gateway proves *who*
is calling; only the cart service knows whether a cart is theirs. Conflating
the two is how you end up able to check out someone else's basket by guessing
its id — and there is a test for that too.

→ [ADR 8: the gateway is the only public entry](docs/adr/0008-gateway-is-the-only-public-entry.md)

## Commands, events, and why they differ

```
cart ──command──► orders_queue ─────────────────► warehouse
                 one handler, must happen once

cart ──event────► ecommerce.events ──┬──► order-service.events
warehouse ─event─►  topic exchange   └──► notification-service.events
                 any number of independent subscribers
```

"Ship this order" is a **command**: exactly one correct handler, on a work
queue. "This order was placed" is an **event**: a statement of fact, on a topic
exchange where every subscriber binds its own queue and gets its own copy.

Putting events on a work queue would mean the order service and the
notification service competing — whichever grabbed a message would deny it to
the other, so orders would be recorded *or* notifications sent, alternately, at
random.

The notification service was added after the cart and warehouse services and
neither knows it exists. That is the property the exchange was introduced for.

**Handlers commute.** `order.placed` and `order.shipped` are published by
different services over different paths, so nothing orders them. `HandlePlaced`
owns the details and never the status; `HandleShipped` owns the status and
creates a stub if needed. Either order produces the same record — the first
implementation assumed ordering and lost shipment events to the dead-letter
queue whenever the two raced.

→ [ADR 9: events and commands](docs/adr/0009-events-and-commands.md)
· [ADR 10: handlers are commutative](docs/adr/0010-event-handlers-are-commutative.md)

## Checkout and its transaction boundary

```
begin_transaction
    │
    ├─► reserve inventory ──────── short ──► abort ─────────────────► 409 out of stock
    │        │
    │      held
    │        │
    ├─► authorize card ─────────── declined ► abort + release ──────► 402 declined
    │        │                     malformed► abort + release ──────► 400 bad card
    │        │                     down ────► abort + release ──────► 503 unavailable
    │      approved
    │        │
    ├─► write order, clear cart
    │
end_transaction
    │
    ├─► publish order.placed                        ← outside the boundary
    └─► publish ship command
```

**Both messages go out after the commit.** Publishing first would risk
shipping goods for an order that then fails to commit, and there is no way to
un-ship. Publishing after means the worst case is an order that is paid for and
recorded but not yet queued — visible in the logs, alertable, recoverable, with
the stock still held by its reservation. A broker failure therefore does not
fail the checkout: the customer has already been charged, and telling them it
failed would be a lie.

**Unwinding happens in one place.** A single deferred cleanup releases the
reservation, aborts the transaction and gives back the idempotency key unless
the checkout committed, so no early return can leak any of them. It runs on a
context detached from the request: if the customer's connection drops
mid-checkout, the stock they were holding still has to go back.

→ [ADR 2: the ship message is published after the commit](docs/adr/0002-ship-message-outside-transaction.md)

## Inventory bookkeeping

Stock lives in a map of atomic counters, one per product, so checkouts touching
different products never contend. Reserving uses a compare-and-swap loop, which
makes check-then-decrement atomic without locking the map — and makes
overselling impossible even when a hundred shoppers race for the last unit.

The part that is easy to get wrong is the reservation lifecycle. A reservation
deducts stock immediately and then resolves **exactly once**:

```
reserve ──► deducted, reservation held (30s TTL)
              │
              ├─ release  ──► stock restored
              ├─ ship     ──► reservation retired, stock stays deducted
              └─ expire   ──► stock restored
```

Every path retires the reservation through the same removal under a lock, so
being in the map *is* being pending. That is what stops a released reservation
from also being expired later and handing the same units back twice — a bug
that inflates stock on every declined payment and, over a load test, without
bound.

→ [ADR 3: a reservation resolves exactly once](docs/adr/0003-reservations-resolve-once.md)

## Retry safety

Checkout charges a card. If the response is lost — a timeout, a reset
connection, a phone changing networks — the client cannot tell "it failed" from
"it worked and I did not hear". Retrying is the only sensible thing to do, and
without protection that retry charges the customer twice.

Send an `Idempotency-Key` header and the retry becomes a replay: the same
order, one charge, one reservation. The key is claimed before any work starts
and the receipt recorded before the response is written. A repeat while one is
still running is refused with 409. Failed checkouts release the key, so a
customer whose card was declined can fix it and retry with the same one.

The transport layer is careful too: `GET` and `PUT` are retried with jittered
backoff, and `POST` never is. A POST that times out may already have been
applied, and only the caller knows what repeating it would mean.

→ [ADR 4: checkout is idempotent](docs/adr/0004-idempotent-checkout.md)

## Staying up when things go wrong

| | |
|---|---|
| **Retries** | Idempotent calls only, exponential backoff with full jitter. Without jitter every caller that failed together retries together and knocks the recovering dependency straight back over |
| **Circuit breakers** | Per dependency, and for the databases, per replica — a shared breaker would let one dead replica block writes to the healthy ones. Only genuine unavailability trips them: a 402 decline is the payment service working correctly |
| **Load shedding** | A concurrency limit per instance, defaulting to a multiple of the core count. Past a point extra concurrency adds queueing, not throughput. Health checks are never shed |
| **Rate limiting** | Per identity, shared across gateway instances via Redis. A per-instance counter would multiply the published limit by however many gateways happen to be running |
| **Graceful drain** | SIGTERM marks the instance not-ready, keeps serving for `DRAIN_DELAY_MS` so the load balancer notices, then stops accepting. Skipping that turns every deploy into a burst of 502s |
| **Dead-letter queues** | Both the work queue and every subscription have one. Requeueing forever starves the queue behind it; dropping loses an order somebody paid for |
| **Self-healing connections** | The AMQP client does not reconnect on its own, so a broker restart would otherwise mean a service that silently stops working |

## Observability

Every service exposes an admin endpoint on its own port — never the one behind
the load balancer, because metrics reveal internal topology and pprof can dump
the heap.

| | |
|---|---|
| `GET /metrics` | Prometheus |
| `GET /healthz` | Liveness. Depends on nothing external |
| `GET /readyz` | Readiness, with per-dependency detail |
| `GET /version` | Version, commit, build date, platform |
| `GET /debug/pprof/…` | CPU, heap, goroutine profiles |

Locally: 9210 product, 9211 cca, 9212 warehouse, 9213 cart, 9214 identity,
9215 orders, 9216 notifications, 9217 gateway, 9200-9220 database nodes.

**Tracing.** Every request gets an `X-Request-Id`, honoured if the client sent
one, and it travels with every downstream call *and every published event* as a
correlation id. One customer's checkout is traceable across all eight services
and both queues by a single grep.

**Metrics** cover the usual RED signals plus the ones that say whether the
business is working: checkout outcomes, the reservation lifecycle, cache hit
ratios, rate limiter verdicts, authentication outcomes, order lifecycle, queue
dispositions, breaker states, quorum failures. `make observability` brings up
Prometheus with [alert rules](deploy/prometheus/alerts.yml) and Grafana with a
provisioned [dashboard](deploy/grafana/dashboards/quorum-market.json).

Every alert is about a customer-visible symptom rather than a resource number.
The most important is `OrdersDeadLettered`: a message on a DLQ is an order
somebody paid for that will never ship, and nothing else would notice.

**Readiness reports degraded rather than down** when a shared dependency fails.
Every instance talks to the same database, so failing them all together would
take the service offline entirely and — with ELB health checks — tell the Auto
Scaling Group to replace the whole fleet during someone else's outage.

→ [ADR 5: readiness reports degraded](docs/adr/0005-readiness-reports-degraded.md)
· [Runbook](docs/runbook.md)

## Simulated latency, and why it burns CPU

Every request handler starts by spending a randomly drawn delay. The delay is
**burned**, not slept: a loop of floating-point work plus a rolling buffer of
short-lived allocations.

Sleeping would make requests slow while leaving CPU utilisation flat, so
CloudWatch would see an idle fleet under heavy load and the Auto Scaling Groups
would never scale out. Burning makes offered load show up as real machine load.

Delays are log-normal (`mu=5.5`, `sigma=0.8`, clamped to 50ms–5s): median
~245ms with a long tail past a second. That shape matches real service latency
far better than a uniform draw, and the tail is what makes queueing and
autoscaling behaviour interesting.

→ [ADR 7: burn CPU rather than sleep](docs/adr/0007-burn-cpu-not-sleep.md)

## API

Everything below is reachable through the gateway at `:8080`. The individual
ports are published locally for debugging and would not be exposed anywhere
real.

### Identity — public

| | |
|---|---|
| `POST /identity/register` | `{"email","password"}` → `201` tokens + profile |
| `POST /identity/login` | `{"email","password"}` → `200` tokens. `401` on failure |
| `POST /identity/refresh` | `{"refreshToken"}` → `200` a new rotated pair |
| `POST /identity/logout` | `{"refreshToken"}` → `200`. Revokes the session immediately |
| `GET /identity/me` | Profile for the presented token |

### Product — public

| | |
|---|---|
| `GET /product/{productId}` | `200` product, `404` unknown |
| `PUT /product/{productId}` | Upsert. Body: `{"name","weight","price"}` |

### Shopping Cart — authenticated

| | |
|---|---|
| `POST /shopping-cart` | → `201 {"cartId"}` for the authenticated customer |
| `GET /shopping-cart/{cartId}` | Cart with computed totals |
| `POST /shopping-cart/{cartId}/add-item` | `{"productId","quantity"}`. `404` unknown product, `409` insufficient stock |
| `POST /shopping-cart/{cartId}/checkout` | `{"creditCard"}` → `200` receipt. `400` empty cart or bad card, `402` declined, `409` out of stock or duplicate in flight. Honours `Idempotency-Key` |

Acting on a cart that is not yours returns `404`, not `403` — confirming that a
cart exists but belongs to someone else is itself information worth withholding.

### Orders — authenticated

| | |
|---|---|
| `GET /orders` | The caller's orders, newest first |
| `GET /orders/{orderId}` | One order |
| `POST /orders/{orderId}/cancel` | `{"reason"}`. `409` once shipped |

### Notifications — authenticated

| | |
|---|---|
| `GET /notifications` | The caller's recent messages, newest first |

### Warehouse — internal

| | |
|---|---|
| `GET /warehouse/inventory/{productId}` | Soft availability check; holds nothing |
| `POST /warehouse/reserve` | `{"items":[...]}` → `200 {"reservationId","expiresAt"}`, `409` short |
| `POST /warehouse/release` | `{"reservationId"}` → `200`. Idempotent |
| `GET /warehouse/stats` | Stock and reservation counters, plus consumer stats |

### KV node — internal

| | |
|---|---|
| `GET /kv?key=` | Quorum read |
| `PUT /kv` | `{"key","value","ttlMs"}` → `201 {"key","version"}` |
| `GET /kv/scan?prefix=&limit=` | Quorum-merged prefix scan |
| `GET /kv/local?key=` | This node's own copy, no quorum — for observing replication lag |
| `GET /kv/stats` | Node id, mode, quorums, key count, transaction counters |
| `POST /db/{begin,end,abort}_transaction` | Simulated transaction lifecycle |

## Measured behaviour

The architecture, the load profile and the scaling metrics here are calibrated
against real measurements — but not measurements of *this* code. They come from
the predecessor implementation of the same design: the same use cases, the same
frequency assumptions, the same simulated-work distribution, written in Java
and deployed on AWS as a five-person course project before this rewrite.

Autoscaled services ran on ECS Fargate; the database nodes and the fixed
infrastructure ran on EC2. Locust drove the load through the ALB.

| | 50 users | 100 users | 200 users | 400 users |
|---|---|---|---|---|
| Throughput | 32 req/s | 45 req/s | 76 req/s | **117 req/s** |
| Median | 1.0 s | 1.3 s | 1.6 s | 1.8 s |
| p95 | 3.2 s | 3.2 s | 4.7 s | 7.2 s |
| p99 | — | 4.3 s | 6.1 s | — |
| Autoscaled tasks | 2 | 2 → 4 | 4+ | 4+ |
| Errors | 7% | 7% | 9% | 7% |

39,747 requests over the full ramp. The ~7% error rate is the expected
outcomes — declined cards and out-of-stock conflicts — not failures.

Three findings shaped the rewrite.

**Where it saturates, and in what order.** Tuning aimed for everything to reach
its limit at about the same load rather than one component failing long before
the others:

| Component | Scaling | Saturates around |
|---|---|---|
| Product service | CPU > 60% | 150–200 users |
| Cart service | Memory > 30% | 200–300 users |
| Broker + warehouse + payments (fixed) | none | 300–400 users |
| Database nodes (fixed) | none | 400+ users |

**The bottleneck was the co-located fixed host.** Every checkout passes through
the one instance holding RabbitMQ, the warehouse and the payment stub, and it
cannot autoscale because it holds the broker's state. That is still true here,
and it is why the warehouse's in-memory ledger is called out as the reason it
is a single instance.

**The scaling metric was the wrong one.** Memory-based scaling on the cart
service needed its target dropped from 60% to 30% before it triggered at all —
a JVM on a 512 MB container reclaimed the simulated allocations faster than
they were made. That is a fact about the runtime, not about load. This rewrite
scales the cart service on **requests per instance** instead: it spends most of
a checkout waiting on three other services, so neither CPU nor memory reflects
the pressure, but request count does.

The AWS console screenshots from that deployment are deliberately not included
here — they carry an account id and a university email in the page chrome, and
neither belongs in a public repository.

## Testing

```bash
make test       # everything, with the race detector
make verify     # tidy, lint, vulncheck, tests, terraform — what CI runs
make smoke      # both end-to-end suites against a running stack
make evidence   # every gate, recorded under evidence/
```

The suite is about behaviour that is hard to reason about by reading:

- **`internal/warehouse`** — the reservation lifecycle: release and expiry
  never both restore the same units, shipping does not deduct twice, and a
  hundred concurrent reservations for the last hundred units grant exactly a
  hundred.
- **`internal/kv`** — versioning, deterministic conflict resolution, TTL
  expiry, and integration tests that stand up real multi-node clusters over
  loopback HTTP and assert the quorum guarantees hold.
- **`internal/cart`** — the whole checkout flow against a real database,
  catalogue, warehouse and authorizer, with only the broker stubbed. A declined
  card puts the stock back, a broker outage still returns the customer their
  order, a retried checkout charges once, and one customer cannot touch
  another's cart.
- **`internal/auth`** — the security properties: `alg=none` rejected, wrong
  key rejected, missing expiry rejected, revoked sessions rejected.
- **`internal/gateway`** — spoofed identity headers are stripped, public and
  protected routes behave differently, and an unreachable upstream does not
  leak its address.
- **`internal/cache`** and **`internal/ratelimit`** — against a real Redis
  (in-process): stampede collapsing, negative caching, fail-open, and that
  concurrent requests cannot exceed a shared limit.
- **`internal/order`** — that the two events commute, in both orders.

## Test evidence

`make evidence` runs every gate and writes the full output — not a summary of
it — to a timestamped directory under [`evidence/`](evidence/), with
[`evidence/latest/`](evidence/latest/) always pointing at the most recent run.

Start with `SUMMARY.md`. It records the commit, the toolchain, a pass/fail table
per gate, coverage by package, and every assertion the smoke tests made.

The most recent run:

| | |
|---|---|
| Gates | 19 passed, 0 failed, 0 skipped |
| Tests | 195, all under the race detector |
| Coverage | 51% overall, per-package breakdown in the summary |
| Static analysis | `go vet`, `golangci-lint` (0 issues), `govulncheck` (0 vulnerabilities) |
| Build | Every service cross-compiled for `linux/amd64` |
| Infrastructure | `terraform fmt -check` and `terraform validate` |
| End to end | Both smoke suites against a live 23-container stack |
| Load | 60 s Locust run through the gateway, authenticated |

Three things make it evidence rather than decoration:

- **A skip is not a pass.** Gates needing something absent are recorded as
  `SKIP` with the reason, counted separately, and never folded into the pass
  count.
- **A failing gate does not stop the run.** Everything runs regardless, so one
  failure produces a complete picture rather than hiding what came after.
- **Runs are committed.** A green badge says the tests passed somewhere once; a
  recorded run says which commit, on what, with what output, and what was
  skipped.

## Load testing

```bash
pip install -r loadtest/requirements.txt
locust -f loadtest/locustfile.py --host http://localhost:8080
```

Traffic goes through the gateway, which is the only publicly reachable service
and therefore the only honest thing to measure. Every session registers an
account and carries a token, so the numbers include the cost of verifying it.

| | |
|---|---|
| Browses before each action | log-normal(1.3, 0.1), mode ~3.6 |
| Add-to-cart actions per session | log-normal(1.1, 0.15), mode ~3 |
| Checkout rate | 10% — the other 90% abandon |

Storefront traffic is overwhelmingly browsing. A test that checked out every
session would hammer the expensive distributed path far harder than production
does, while leaving the read path — the one that actually needs to scale —
barely exercised.

Declines, out-of-stock and throttled responses are recorded as successes,
because they are the system working correctly; the run summary reports them
separately.

## Deploying to AWS

```bash
cd terraform
cp example.tfvars terraform.tfvars
# set jwt_secret — openssl rand -base64 48 — and ecr_registry
terraform init && terraform apply
```

Then build and push; the Auto Scaling Groups roll the change out themselves.

```bash
ECR=$(terraform -chdir=terraform output -raw ecr_repository_url)
aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin "${ECR%%/*}"
docker buildx build --platform linux/amd64 -f deploy/Dockerfile \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse --short HEAD)" \
  -t "$ECR:latest" --push .
terraform apply
```

One image and one repository: every service is the same binary bundle selected
by its container command.

→ [ADR 6: one image holds every service](docs/adr/0006-one-image-many-services.md)

### What gets created

- A VPC with two application subnets across two AZs, plus a separate data
  subnet. The databases live apart because their nodes take static private
  IPs — they need to know their peers before booting — and keeping them
  separate means DHCP cannot hand one of those addresses to an autoscaled
  instance first.
- Three KV clusters: 5 + 5 + 3 nodes.
- `services-1`: RabbitMQ, Redis, the authorizer and the warehouse. None scales
  with shopper count, and the warehouse holds its ledger in memory.
- `services-2`: identity, orders and notifications.
- Three Auto Scaling Groups — gateway, product, cart — with rolling instance
  refresh so a launch template change actually reaches running instances.
- Two ALB listeners on one load balancer: `:80` public, forwarding everything
  to the gateway, and `:8081` restricted by security group to the VPC for
  service-to-service routing.
- CloudWatch alarms and a dashboard, optionally wired to an SNS email.

### Autoscaling

| Service | Metric | Target |
|---|---|---|
| Gateway | ALB requests per target | 80 |
| Product | Average CPU | 60% |
| Shopping Cart | ALB requests per target | 50 |

The product service does nothing but read its database and burn its simulated
processing time, so its load is almost purely CPU.

The gateway and cart service are measured differently on purpose. The gateway
does almost no computation per request — verify a signature, check a counter,
proxy a body. The cart service spends most of a checkout waiting on three other
services. For both, CPU can look comfortable while the connection and goroutine
budget is exhausted, so request count is the metric that tracks the pressure.

## Configuration

Everything is environment variables, so the same binary runs unchanged under
`docker compose` and on EC2.

**Shared by every service**

| Variable | Default | Meaning |
|---|---|---|
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | `LOG_FORMAT=text` for human reading |
| `ADMIN_PORT` | `9100` | Metrics, health, pprof. Never expose publicly |
| `MAX_IN_FLIGHT` | `64 × GOMAXPROCS` | Concurrency limit; `0` disables shedding |
| `DRAIN_DELAY_MS` | `5000` | Time between going not-ready and refusing connections |
| `SHUTDOWN_TIMEOUT_MS` | `25000` | Grace period for in-flight requests |
| `DELAY_MU` / `DELAY_SIGMA` | `5.5` / `0.8` | Log-normal delay parameters |
| `DELAY_MIN_MS` / `DELAY_MAX_MS` | `50` / `5000` | Clamp. `DELAY_MAX_MS=0` disables the delay |
| `HTTP_CONNECT_TIMEOUT_MS` | `500` | Dial timeout for downstream calls |
| `HTTP_REQUEST_TIMEOUT_MS` | `5000` | Overall timeout for downstream calls |

**Authentication** — gateway and identity

| Variable | Default | Meaning |
|---|---|---|
| `JWT_SECRET` | — | **Required**, at least 32 bytes. A short HMAC key is brute-forceable offline from one captured token |
| `JWT_ISSUER` | `quorum-market` | Checked on verification |
| `ACCESS_TOKEN_TTL_MS` | `900000` | 15 minutes. Short, because a signed token cannot be recalled |
| `REFRESH_TOKEN_TTL_MS` | `604800000` | 7 days |
| `RATE_LIMIT` | `600` | Requests per window per identity |
| `RATE_LIMIT_WINDOW_MS` | `60000` | The window |

**Redis** — gateway, identity, product, notifications

| Variable | Default |
|---|---|
| `REDIS_ADDR` | `localhost:6379`. Empty disables the cache and the limiter |
| `REDIS_POOL_SIZE` | `64` |
| `REDIS_READ_TIMEOUT_MS` | `200` — short, because Redis is meant to make things faster |
| `CACHE_TTL_MS` / `CACHE_NEGATIVE_TTL_MS` | `300000` / `30000` |

**KV node**

| Variable | Default | Meaning |
|---|---|---|
| `NODE_MODE` | `leader-follower` | Or `leaderless` |
| `ROLE` | `leader` | Leader-follower mode only |
| `FOLLOWER_URLS` / `PEER_URLS` | — | Comma-separated peer list |
| `WRITE_QUORUM_SIZE` / `READ_QUORUM_SIZE` | `1` / `1` | W and R, counting this node |
| `WRITE_DELAY_MS` / `READ_DELAY_MS` | `0` | Simulated storage cost per node |
| `REPLICATION_SEQUENTIAL` | `false` | One peer at a time, widening the window in which stale reads are observable |
| `READ_REPAIR` | `true` | Push the winning value back to stale replicas |
| `STORE_SWEEP_INTERVAL_MS` | `60000` | How often expired entries are reclaimed |

**Warehouse**

| Variable | Default |
|---|---|
| `WAREHOUSE_INITIAL_STOCK` | `100` |
| `RESERVATION_TTL_MS` | `30000` |
| `RMQ_CONSUMER_WORKERS` | `10` |
| `RMQ_PREFETCH_COUNT` | `10` |

## Repository layout

```
cmd/                    one main package per binary
  gatewaysvc/           the public entry
  identitysvc/          accounts and sessions
  productsvc/  cartsvc/  ordersvc/  notificationsvc/  ccasvc/  warehousesvc/
  kvnode/               all three database clusters run this
  productloader/        seeds the catalogue before a load test
  healthcheck/          container probe; the images have no shell to curl with
internal/
  auth/                 token issue and verification, identity headers
  busywait/             log-normal delay that burns CPU and heap
  cache/                read-through cache with stampede collapsing
  cart/                 cart state, checkout orchestration, idempotency
  cca/                  authorizer and its client
  envx/                 typed environment configuration
  events/              domain events, envelope, topic router
  gateway/              routing, auth enforcement, header stripping
  httpx/                errors, router, middleware, resilient client, server
  identity/             accounts, credentials, sessions
  kv/                   store, quorum replication, TTL, scan, transactions
  notification/         event-driven customer messages
  obs/                  metrics, request-scoped logging, readiness, admin
  order/                order lifecycle, driven by events
  orders/               contracts shared by cart and warehouse
  product/              catalogue service and its client
  ratelimit/            distributed sliding-window limiter
  redisx/               connection handling and health
  rmq/                  self-healing connection, publishers, consumers, DLQs
  warehouse/            stock ledger, reservations, shipper
deploy/                 Dockerfile, compose stack, Prometheus, Grafana
terraform/              VPC, ALB, three clusters, three ASGs, alarms
loadtest/               Locust profile, through the gateway with auth
docs/adr/               why things are the way they are
docs/runbook.md         what to do when an alert fires
evidence/               recorded output of every quality gate
scripts/                smoke tests and evidence collection
```

## What is deliberately not real

Worth being explicit about, since some of it looks more finished than it is.

- **The transactions are a shell.** `begin`/`end`/`abort` enforce a real,
  checked lifecycle, but there is no undo log and no two-phase commit.
  Compensation is done by hand — releasing the reservation — which is what the
  boundary is really documenting.
- **The KV stores are in memory.** Restarting a node loses its data; the write
  delay stands in for durability. A recovered node refills through read repair.
- **Warehouse stock is in memory too**, seeded lazily to 100 units per product,
  and that is why the warehouse is a single instance.
- **Payment is a coin flip**, not a gateway.
- **Redis is a single node.** It would be ElastiCache with a replica anywhere
  real. Every caller is written to survive losing it, which is what makes one
  node tolerable here.
- **Idempotency is deduplication, not a distributed lock.** Two genuinely
  simultaneous requests with the same key can both proceed; the store has no
  compare-and-set. The case that actually happens — a retry seconds later — is
  closed.
- **There is no outbox.** An order committed while the broker is unreachable is
  logged and alertable but must be replayed by hand.
- **Services trust the gateway's identity header.** The trust is in the network
  boundary. Anything that can reach a service directly bypasses authentication,
  which is why they sit on a VPC-only listener. Mutual TLS is where this goes
  next.

Each of these is a place where the interesting distributed behaviour is real
and the surrounding machinery is stubbed, which is the point.
