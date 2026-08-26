# 8. The gateway is the only public entry

## Context

Once the system had accounts, every service needed to know who was calling.
The obvious approach — each service verifies the token itself — means the
signing secret, the algorithm pinning, the revocation lookup and the clock
skew handling are reimplemented seven times. Six of those implementations will
be slightly wrong, and the seventh will be the one nobody updates when the
scheme changes.

## Decision

One gateway is publicly reachable. It verifies the token, enforces the rate
limit, and forwards to the internal services with the caller's identity in
`X-Customer-Id`. Services read that header and trust it.

The load balancer forwards everything on port 80 to the gateway, with no path
rules at all. Services answer on a second listener restricted by security
group to the VPC.

## Consequences

Authentication exists in one place. A service's code is about carts and
inventory rather than about tokens, and there is one implementation to audit.

Verification costs no network round trip: the token is signed, so the gateway
checks it locally. The price is that revocation is not instant, which is why
access tokens live fifteen minutes and why a session denylist exists for
logouts that cannot wait.

**The header must be stripped before it is set.** A client that could send
`X-Customer-Id` would be able to act as any customer — read anyone's orders,
check out on anyone's card. Three `Header.Del` calls at the top of the gateway
handler are the whole reason the downstream trust is safe, and there is a test
whose only job is to prove they are still there.

The trust is in the network boundary, and it is only as good as that boundary.
Anything that can reach a service directly bypasses authentication entirely.
That is why the services are on a VPC-only listener and why the gateway is the
sole target of the public one. In a larger system this is where mutual TLS
between services would go.

Services still enforce **authorization** themselves. The gateway proves *who*
is calling; only the cart service knows whether this cart is theirs. Conflating
the two is how you end up able to check out someone else's basket by guessing
its id.

The gateway is a single point of failure for everything. It is autoscaled and
does very little per request — verify a signature, check a counter, proxy a
body — but it is on the path for every call, and that is the cost of solving
authentication once.
