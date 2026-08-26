# 7. Simulated work burns CPU rather than sleeping

## Context

The services stand in for ones that would do real work — pricing, inventory
maths, fraud scoring — and the point of the exercise is to watch the system
behave under load and scale itself out.

Simulating that work with `time.Sleep` is the obvious approach and it defeats
the exercise. A sleeping request is slow but costs nothing: CPU utilisation
stays flat, CloudWatch sees an idle fleet, and the Auto Scaling Groups never
scale out no matter how much traffic arrives. The system would look perfectly
healthy while every customer waits.

## Decision

Spend the simulated delay on actual work: a floating-point loop plus a rolling
buffer of short-lived allocations, so both CPU and the allocator are exercised.

Delays are drawn from a log-normal distribution (mu=5.5, sigma=0.8, clamped to
50ms–5s), giving a median near 245ms and a long tail past a second.

## Consequences

Offered load turns into real machine load, so the scaling policies have
something true to react to and the load test measures something real.

The log-normal shape matters as much as the magnitude. A uniform delay makes
every request alike and queueing well-behaved; real latency has a dense body
and a heavy tail, and the tail is what makes capacity planning and timeout
choice interesting.

Throughput per instance is genuinely low — a couple of vCPUs retiring ~245ms of
work per request is single-digit requests per second. That is the intended
shape of the experiment, and it is why the concurrency limiter defaults to a
multiple of the core count rather than a large fixed number.

`DELAY_MAX_MS=0` disables it entirely, which is what the test suite uses;
otherwise the tests would take minutes to assert things that have nothing to do
with latency.
