# 5. Readiness reports degraded, not down, for shared dependencies

## Context

Every instance of the cart service talks to the same database, the same
warehouse and the same broker. The obvious readiness check — probe the
dependencies, report unready if any is down — has a bad failure mode when the
dependency is shared.

If the cart database goes down, every instance fails its probe simultaneously.
The load balancer then has no healthy target and returns 503 for everything,
including the requests that did not need the database. Worse, with ELB health
checks the Auto Scaling Group concludes its instances are broken and starts
replacing all of them — during an incident, adding a fleet-wide rebuild to a
database outage.

The instances were fine. The thing that was down was somewhere else, and
removing them helped nobody.

## Decision

Split the two questions and answer them in different places.

`/healthz` is liveness. It depends on nothing external and answers "is this
process wedged?". The load balancer's target group health check uses the
service's own `/health`, which is the same idea.

`/readyz` is readiness, and probes dependencies — but reports them as
`degraded` with HTTP 200 rather than `down`. `down` is reserved for one thing:
the process is draining and should stop receiving traffic.

## Consequences

A dependency outage shows up as `degraded` in the readiness body and in the
alerts, without pulling the fleet out of rotation or triggering a replacement
storm.

Readiness is still useful for what it is good at: confirming a newly launched
instance can actually reach its dependencies before a deploy shifts traffic to
it, and telling an operator at a glance which dependency is unhappy.

The cost is that `/readyz` cannot be wired directly to an aggressive
auto-replacement policy, because it deliberately does not fail when the
dependency does. That is the intent, not an oversight.

Draining is what makes `down` meaningful. On SIGTERM the process marks itself
not-ready, keeps serving for `DRAIN_DELAY_MS`, and only then stops accepting —
so a deploy does not sever requests the balancer routed a moment earlier.
