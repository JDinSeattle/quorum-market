# Runbook

What to do when something is wrong, written for whoever is on the other end of
the alert rather than for whoever wrote the code.

## First three things

Before reaching for a specific alert, these three answers narrow it down
faster than anything else.

Admin ports: 9210 product, 9211 cca, 9212 warehouse, 9213 cart, 9214 identity,
9215 orders, 9216 notifications, 9217 gateway.

```bash
# 1. Which service, and is it up at all?
curl -s localhost:9213/readyz | python3 -m json.tool

# 2. What is it doing right now?
curl -s localhost:9213/metrics | grep -E '^quorum_(http_server_(requests|shed)|circuit_breaker)'

# 3. What did it say about the failing request?
docker logs shopping-cart-service 2>&1 | grep <request-id>
```

Every response carries an `X-Request-Id`, and that id appears on every log line
in every service the request touched. If a user reports a failure, the id from
their response is the fastest path to the cause.

The id also travels with published events as a correlation id, so a trace
continues through the queue into whatever reacted to it.

```bash
for svc in gateway identity-service product-service shopping-cart-service \
           warehouse-service cca-service order-service notification-service; do
  echo "── $svc"; docker logs "$svc" 2>&1 | grep "$1"
done
```

## Is it the gateway or a service behind it?

The gateway answers 502 when an upstream is unreachable and 401/429 on its own
account. Anything else came from a service.

```bash
curl -s localhost:9217/readyz | python3 -m json.tool    # which upstream is unhappy
curl -s localhost:9217/metrics | grep -E 'quorum_(auth|ratelimit)'
docker logs gateway 2>&1 | grep -iE 'upstream call failed|throttled' | tail
```

A 401 that should have worked is usually one of three things: an expired access
token (they last 15 minutes), a session that was logged out, or `JWT_SECRET`
differing between the gateway and the identity service. The last one makes
*every* token fail and is worth ruling out first:

```bash
curl -s localhost:9217/version && curl -s localhost:9214/version
docker logs gateway 2>&1 | grep -i 'invalid JWT' | tail
```

## By alert

### OrdersDeadLettered — critical

Orders that were paid for and could not be shipped. This is money taken for
goods not sent, so it is the one alert here that is unambiguously customer harm.

```bash
# How many, and what do they look like?
curl -su guest:guest localhost:15672/api/queues/%2F/orders_queue.dlq | python3 -m json.tool | head -20
curl -su guest:guest -X POST localhost:15672/api/queues/%2F/orders_queue.dlq/get \
  -H 'Content-Type: application/json' \
  -d '{"count":5,"ackmode":"ack_requeue_true","encoding":"auto"}' | python3 -m json.tool
```

The warehouse logs the reason next to `parking unprocessable message`. Two
causes are likely:

- **Malformed message** — a producer and consumer that disagree about the
  schema. Fix the mismatch, then republish the parked messages.
- **Repeated handler failure** — the message was retried once and failed
  again. The log line has the underlying error.

Once the cause is fixed, replay from the DLQ back onto the work queue using the
RabbitMQ management UI's shovel, or republish the bodies. Do not simply purge:
each message is an order somebody paid for.

### ShipMessagesNotPublishing — critical

Checkout is succeeding, customers are being charged, and nothing is reaching
fulfilment. The orders are committed and recoverable, but nobody is shipping.

```bash
curl -s localhost:9213/readyz | python3 -c 'import json,sys; print(json.load(sys.stdin)["checks"]["rabbitmq"])'
docker logs shopping-cart-service 2>&1 | grep -i 'could not queue shipment' | tail
```

Usually the broker is down or unreachable. The cart service reconnects on its
own once it returns, but orders placed during the outage were never queued:
find them by comparing committed orders against shipped ones, and republish.

If the broker is up and publishes are still failing, check whether the channel
pool is exhausted (`quorum_http_server_shed_requests_total` rising alongside it
suggests general overload rather than a broker problem).

### HighServerErrorRate / target_5xx — critical

```bash
# Which service and which route?
curl -s localhost:9213/metrics | grep 'quorum_http_server_requests_total.*5xx'
docker logs shopping-cart-service 2>&1 | grep 'level=ERROR' | tail -20
```

If the errors are 503s specifically, check whether they are shed (capacity) or
propagated from a dependency:

```bash
curl -s localhost:9213/metrics | grep -E 'shed_requests_total|circuit_breaker_state'
```

### CircuitBreakerOpen — warning

A dependency has failed enough consecutive times that calls are now failing
fast instead of queueing. **The problem is the dependency, not the service
reporting it.**

```bash
curl -s localhost:9213/metrics | grep quorum_circuit_breaker_state
# 0 closed, 1 half-open, 2 open
```

The breaker probes the dependency again after its cool-off and closes itself
when it recovers. There is nothing to do to the caller; go and look at the
named dependency.

### RequestsBeingShed — warning

The service hit its concurrency limit and is rejecting the overflow with 503
and `Retry-After`. This is the system protecting itself, not failing, but it
means capacity has run out.

Either scale out, or raise `MAX_IN_FLIGHT` if the instance genuinely has
headroom. Check whether it does before raising it:

```bash
curl -s localhost:9213/metrics | grep -E 'go_goroutines|process_cpu_seconds_total'
```

Remember that handlers burn real CPU here — a couple of vCPUs retire only a few
requests per second, so a high limit converts throughput into queueing delay
rather than into capacity.

### QuorumFailures — critical

A database cluster cannot reach enough replicas to satisfy its quorum, so it is
refusing operations rather than returning possibly-stale data.

```bash
for port in 9090 9091 9092 9093 9094; do
  printf '%s: ' "$port"; curl -s --max-time 2 "localhost:$port/kv/stats" | head -c 200; echo
done
```

Cart DB is W=3/R=3 over five nodes, so it tolerates two nodes down and fails at
three. Product DB is W=5, so it tolerates **no** follower loss for writes —
reads keep working from the leader throughout.

Restart the missing nodes. Data is in memory and is lost on restart; a
recovered node refills from read repair as keys are read, and the catalogue can
be reseeded with `make seed`.

### ReservationsPilingUp — warning

Checkouts are starting and not finishing, so stock is held against orders
nobody is placing.

```bash
curl -s localhost:8084/warehouse/stats | python3 -m json.tool
```

Compare `reserved` against `released` plus `shipped`. A persistent gap means
checkouts are dying between reserving and resolving — look for the cart service
timing out against the authorizer. Held stock is reclaimed automatically after
`RESERVATION_TTL_MS`, so this degrades rather than breaks, but it makes
products look out of stock while it lasts.

### DeclineRateAbnormal — warning

Steady state is about 10% declines by design. A sharp rise usually means the
authorizer is misbehaving rather than that customers changed.

```bash
curl -s localhost:9211/metrics | grep quorum_http_server_requests_total
docker logs cca-service 2>&1 | tail -20
```

Check that `CCA_APPROVAL_RATE` has not been changed by a deploy.

### InventoryOversold — warning

Stock was deducted below zero: a ship message arrived with no live reservation,
because the queue backed up past `RESERVATION_TTL_MS`. The units really did
ship, so the deduction is correct; the alert is telling you fulfilment is
lagging far enough to matter.

Look at queue depth and consumer throughput. Raise `RMQ_CONSUMER_WORKERS`, or
raise the reservation TTL so holds outlive the backlog.

### AuthenticationFailureSpike — warning

Over half of authentication attempts are being denied. Either an integration
broke or someone is guessing credentials.

```bash
curl -s localhost:9217/metrics | grep quorum_auth_attempts_total
curl -s localhost:9214/metrics | grep quorum_auth_attempts_total
```

The `operation` label separates them: `login` denials are credentials,
`verify` denials are tokens. A spike in `verify` right after a deploy points at
a `JWT_SECRET` mismatch rather than at an attack.

### SustainedThrottling — warning

The gateway is refusing traffic. Find out whose.

```bash
curl -s localhost:9217/metrics | grep quorum_ratelimit_decisions_total
docker logs gateway 2>&1 | grep 'request throttled' | tail -20
```

The log line names the identity. A single customer bucket means one misbehaving
client; broad throttling across many means `RATE_LIMIT` is below legitimate
traffic. Anonymous callers are bucketed by address, so an office behind one NAT
shares a bucket — a common cause of surprising throttling.

### RateLimiterFailingOpen — warning

Redis is unreachable and the limiter is admitting everything. The API is
unprotected until it returns, which is the deliberate trade: refusing all
traffic would turn a cache-tier outage into a full outage.

```bash
docker exec redis redis-cli ping
curl -s localhost:9217/readyz | python3 -c 'import json,sys; print(json.load(sys.stdin)["checks"]["redis"])'
```

If Redis is down, expect three symptoms together: unlimited traffic, a cold
catalogue cache, and failing logins. That combination is Redis, not three
separate incidents.

### CacheHitRatioCollapsed — warning

The catalogue cache has stopped protecting the product database, which is now
taking the full read load.

```bash
curl -s localhost:9210/metrics | grep quorum_cache_operations_total
```

The `result` label says which. Rising `error` means Redis is failing open;
rising `invalidated` means something is writing to the catalogue constantly;
rising `miss` with a healthy Redis means the working set no longer fits and
`maxmemory` is evicting.

```bash
docker exec redis redis-cli info memory | grep -E 'used_memory_human|maxmemory_human|evicted'
```

### Orders stuck as "placed" after shipping

The order service builds its records from two events published by different
services. If a shipment is not reflected, look at its subscription's
dead-letter queue first.

```bash
curl -s localhost:9215/metrics | grep quorum_queue_messages_total
curl -su guest:guest 'localhost:15672/api/queues/%2F/order-service.events.dlq' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["messages"])'
```

The handlers are written to commute, so arrival order is not the cause. A
parked event means the handler failed twice for a real reason — normally the
core database being unreachable — and the log line says which.

An order showing `awaitingDetails: true` is the opposite situation and is
usually transient: a shipment event arrived before its placement event, and the
record fills in within milliseconds.

### Notifications not arriving

Its own subscription, its own backlog. Nothing else is affected.

```bash
curl -s localhost:9216/readyz | python3 -m json.tool
curl -su guest:guest 'localhost:15672/api/queues/%2F?columns=name,messages,consumers' \
  | python3 -m json.tool
docker exec redis redis-cli --scan --pattern 'notifications:*' | head
```

A growing `notification-service.events` queue with zero consumers means the
service is not connected. A growing queue *with* consumers means it is slower
than the event rate.

## Common operations

### Deploy

```bash
ECR=$(terraform -chdir=terraform output -raw ecr_repository_url)
docker buildx build --platform linux/amd64 -f deploy/Dockerfile \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse --short HEAD)" \
  -t "$ECR:$(git describe --tags --always)" -t "$ECR:latest" --push .
terraform -chdir=terraform apply
```

The Auto Scaling Groups have a rolling instance refresh, so a launch template
change replaces instances in batches while keeping at least half serving.
Watch it:

```bash
aws autoscaling describe-instance-refreshes \
  --auto-scaling-group-name quorum-market-cart-asg \
  --query 'InstanceRefreshes[0].[Status,PercentageComplete]'
```

### Confirm what is actually running

```bash
curl -s http://<instance>:9100/version
```

### Rotate the signing secret

Changing `JWT_SECRET` invalidates every access token immediately, which logs
everyone out. Refresh tokens survive, because they are opaque and stored
server-side, so clients recover on their next refresh.

Apply it to the gateway and the identity service together — a window where they
disagree is a window where every request is a 401.

```bash
terraform apply -var="jwt_secret=$(openssl rand -base64 48)"
```

### Take the evidence of a release

```bash
make up && make seed && make evidence
```

Writes a timestamped directory under `evidence/` with the full output of every
gate. Worth doing before a deploy and keeping: the last good run is the fastest
way to see what changed when something regresses.

### Roll back

Retag the previous image as `latest` and start a fresh instance refresh. There
is no database migration to reverse — the stores are in memory — so rollback is
purely the image.

### Drain an instance by hand

Send it SIGTERM. It marks itself not-ready, keeps serving for
`DRAIN_DELAY_MS`, and then stops accepting while finishing what it has.

```bash
docker stop --timeout 40 shopping-cart-service
```

The stop timeout must exceed `DRAIN_DELAY_MS` plus `SHUTDOWN_TIMEOUT_MS`, or
the runtime kills the process mid-drain and undoes the point of it.

### Profile a slow or leaking service

pprof is on the admin port, never the public one.

```bash
go tool pprof -http=:8000 http://localhost:9213/debug/pprof/profile?seconds=30   # CPU
go tool pprof -http=:8000 http://localhost:9213/debug/pprof/heap                 # memory
curl -s 'localhost:9213/debug/pprof/goroutine?debug=1' | head -40                # stuck goroutines
```

A CPU profile that is mostly `busywait.Burn` is the simulation working as
designed, not a bug. Set `DELAY_MAX_MS=0` to take it out of the picture while
investigating something else.

### Inspect the databases directly

```bash
curl -s 'localhost:9090/kv?key=cart-alice'          # quorum read
curl -s 'localhost:9091/kv/local?key=cart-alice'    # one replica's own copy, no quorum
curl -s localhost:9090/kv/stats
```

`/kv/local` against each node in turn is how to see replication lag or a
divergent replica, since it bypasses both the quorum and the read repair that
would otherwise hide it.
