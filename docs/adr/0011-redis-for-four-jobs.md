# 11. Redis for four jobs, and what happens when it is gone

## Context

Four separate needs appeared at roughly the same time: the catalogue was being
read far more than it changed, the rate limit had to be shared across gateway
instances, sessions needed somewhere with expiry built in, and notification
inboxes wanted a capped, throwaway list.

All four are short-lived keyed data with a natural lifetime. None of them wants
the replication and quorum machinery the KV clusters provide.

## Decision

One Redis instance, four uses, separated by key prefix:

| Prefix | Use | Losing it means |
|---|---|---|
| `product:` | Catalogue read-through cache | Slower reads |
| `ratelimit:` | Sliding-window counters | The limit is not enforced |
| `session:` | Refresh tokens and the revocation denylist | Everyone is logged out |
| `notifications:` | Per-customer inbox, capped and expiring | Recent messages are lost |

Each caller decides for itself what an outage means, and the decisions differ:

- **The cache fails open.** Redis unreachable means the loader is called
  directly. A cache that takes the site down with it is worse than no cache.
- **The rate limiter fails open**, because it protects capacity here rather
  than guarding against abuse. Refusing all traffic would turn a cache-tier
  outage into a full API outage. `FailOpen: false` is available for a limiter
  whose job is the other one.
- **The revocation check fails open**, honouring the signature. The
  alternative is that a Redis blip logs out every customer at once, and the
  exposure is bounded by a fifteen-minute token lifetime.
- **Identity and notifications fail closed** — they refuse to start without
  Redis. Sessions and inboxes live there, so a process that came up anyway
  would answer health checks and reject every customer.

Persistence is append-only, because not everything in there is a cache.

## Consequences

One thing to operate instead of four, at the cost of one blast radius: a Redis
outage degrades browsing, unlimits the gateway and stops logins at the same
time. That is acceptable here and would not be past a certain scale, where
sessions in particular would want their own instance.

The failure behaviours are per-caller and deliberate. Writing them down matters
because "fail open" and "fail closed" are both defensible and choosing wrongly
is invisible until an outage.

`allkeys-lru` eviction with a memory ceiling means Redis sheds the least-used
keys rather than refusing writes under pressure. Everything stored sets a TTL,
so nothing untracked accumulates.

The cache does three things beyond storing values, each earning its place:
concurrent misses for the same key are collapsed so an expiring hot key does
not stampede the database; absences are remembered so a crawler walking product
ids stops reaching it at all; and TTLs are jittered so a batch of keys loaded
together does not expire together.
