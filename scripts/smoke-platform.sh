#!/usr/bin/env bash
#
# End-to-end check of the platform layer: the gateway, authentication, rate
# limiting, the catalogue cache, and the event-driven services that no caller
# ever talks to directly.
#
# scripts/smoke.sh covers the commerce path against the services themselves.
# This one covers what sits in front of and behind them.
#
#   make up && make seed && make smoke
#
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
PRODUCT_ADMIN="${PRODUCT_ADMIN:-http://localhost:9210}"
GATEWAY_ADMIN="${GATEWAY_ADMIN:-http://localhost:9217}"
ORDER_ADMIN="${ORDER_ADMIN:-http://localhost:9215}"
NOTIFICATION_ADMIN="${NOTIFICATION_ADMIN:-http://localhost:9216}"
IDENTITY_ADMIN="${IDENTITY_ADMIN:-http://localhost:9214}"

CARD="4111-1111-1111-1111"
PASSWORD="correct-horse-battery-staple"
STAMP="$(date +%s)$$"
EMAIL="smoke-${STAMP}@example.com"
OTHER_EMAIL="smoke-other-${STAMP}@example.com"

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

json() {
  python3 -c '
import json, sys
doc = json.load(sys.stdin)
for key in sys.argv[1].split("."):
    doc = doc[int(key)] if key.isdigit() else doc[key]
print(doc)
' "$1"
}

status_of() { curl -sS -o /dev/null -w '%{http_code}' "$@"; }

step "1. the gateway is up"
for _ in $(seq 1 60); do
  curl -fsS --max-time 2 "$GATEWAY_URL/health" >/dev/null 2>&1 && break
  sleep 2
done
curl -fsS --max-time 2 "$GATEWAY_URL/health" >/dev/null || fail "the gateway never became healthy"
pass "gateway is answering"

step "2. registration and login"
register=$(curl -fsS -X POST "$GATEWAY_URL/identity/register" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")

token=$(echo "$register" | json accessToken)
refresh=$(echo "$register" | json refreshToken)
customer=$(echo "$register" | json profile.customerId)
[ -n "$token" ] && [ -n "$customer" ] || fail "registration did not return a usable session"
pass "registered $customer"

code=$(status_of -X POST "$GATEWAY_URL/identity/register" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
[ "$code" = "409" ] || fail "registering the same email twice returned $code, want 409"
pass "duplicate registration -> 409"

login=$(curl -fsS -X POST "$GATEWAY_URL/identity/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
[ "$(echo "$login" | json profile.customerId)" = "$customer" ] || fail "login returned a different customer"
pass "login returns the same identity"

code=$(status_of -X POST "$GATEWAY_URL/identity/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"definitely-not-the-password\"}")
[ "$code" = "401" ] || fail "a wrong password returned $code, want 401"
pass "wrong password -> 401"

step "3. what the gateway lets through"
# Checked first and reported plainly. The stores are in memory, so a restart
# empties the catalogue, and without this the next assertion fails with a
# confusing 404 that looks like a routing bug.
code=$(status_of "$GATEWAY_URL/product/p1")
if [ "$code" = "404" ]; then
  fail "product p1 does not exist — run 'make seed' first (the catalogue is in memory and does not survive a restart)"
fi
[ "$code" = "200" ] || fail "browsing without a token returned $code, want 200"
pass "browsing is public"

for path in /shopping-cart /orders /notifications; do
  code=$(status_of -X GET "$GATEWAY_URL$path")
  [ "$code" = "401" ] || fail "$path without a token returned $code, want 401"
done
pass "carts, orders and notifications require a token"

code=$(status_of -H 'Authorization: Bearer not-a-real-token' "$GATEWAY_URL/orders")
[ "$code" = "401" ] || fail "a forged token returned $code, want 401"
pass "forged tokens -> 401"

# The gateway must overwrite the identity headers, never merge with them.
# Otherwise a client could act as any customer simply by asserting one.
spoofed=$(curl -fsS "$GATEWAY_URL/orders" \
  -H "Authorization: Bearer $token" \
  -H "X-Customer-Id: cust-somebody-else" | json customerId)
[ "$spoofed" = "$customer" ] || fail "a spoofed X-Customer-Id header reached the backend as $spoofed"
pass "spoofed identity headers are stripped"

step "4. a purchase, end to end"
cart=$(curl -fsS -X POST "$GATEWAY_URL/shopping-cart" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{}' | json cartId)
[ "$cart" = "cart-$customer" ] || fail "the cart id is $cart, want one derived from the authenticated customer"
pass "opened $cart"

curl -fsS -X POST "$GATEWAY_URL/shopping-cart/$cart/add-item" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  -d '{"productId":"p1","quantity":2}' >/dev/null
pass "added 2 units of p1"

order_id=""
for attempt in 1 2 3 4 5 6 7 8; do
  code=$(curl -sS -o /tmp/smoke-platform-checkout.json -w '%{http_code}' \
    -X POST "$GATEWAY_URL/shopping-cart/$cart/checkout" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: platform-$STAMP" \
    -d "{\"creditCard\":\"$CARD\"}")
  if [ "$code" = "200" ]; then
    order_id=$(json orderId < /tmp/smoke-platform-checkout.json)
    break
  fi
  [ "$code" = "402" ] || fail "checkout returned $code: $(cat /tmp/smoke-platform-checkout.json)"
  printf '       card declined, retrying with the same key\n'
done
[ -n "$order_id" ] || fail "checkout never succeeded"
pass "order $order_id placed through the gateway"

step "5. one customer cannot touch another's cart"
other=$(curl -fsS -X POST "$GATEWAY_URL/identity/register" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$OTHER_EMAIL\",\"password\":\"$PASSWORD\"}")
other_token=$(echo "$other" | json accessToken)

code=$(status_of -H "Authorization: Bearer $other_token" "$GATEWAY_URL/shopping-cart/$cart")
[ "$code" = "404" ] || fail "another customer read the cart and got $code, want 404"
pass "reading someone else's cart -> 404"

code=$(status_of -X POST "$GATEWAY_URL/shopping-cart/$cart/checkout" \
  -H "Authorization: Bearer $other_token" -H 'Content-Type: application/json' \
  -d "{\"creditCard\":\"$CARD\"}")
[ "$code" = "404" ] || fail "another customer checked out the cart and got $code, want 404"
pass "checking out someone else's cart -> 404"

step "6. the order service learned about it from an event"
recorded=""
for _ in $(seq 1 30); do
  recorded=$(curl -fsS "$GATEWAY_URL/orders" -H "Authorization: Bearer $token" | json count)
  [ "$recorded" -gt 0 ] && break
  sleep 1
done
[ "${recorded:-0}" -gt 0 ] || fail "the order never reached the order service"
pass "order history has $recorded order(s), populated by order.placed"

listed=$(curl -fsS "$GATEWAY_URL/orders" -H "Authorization: Bearer $token" | json orders.0.orderId)
[ "$listed" = "$order_id" ] || fail "order history shows $listed, want $order_id"
pass "the recorded order matches the receipt"

# The warehouse ships asynchronously and announces it; the order service
# advances the record without the cart service or the warehouse knowing it
# exists.
shipped=""
for _ in $(seq 1 30); do
  shipped=$(curl -fsS "$GATEWAY_URL/orders/$order_id" -H "Authorization: Bearer $token" | json status)
  [ "$shipped" = "shipped" ] && break
  sleep 1
done
[ "$shipped" = "shipped" ] || fail "the order is still '$shipped' after shipping, want shipped"
pass "order advanced to shipped via order.shipped"

step "7. an unrelated subscriber saw the same events"
messages=0
for _ in $(seq 1 20); do
  messages=$(curl -fsS "$GATEWAY_URL/notifications" -H "Authorization: Bearer $token" | json count)
  [ "$messages" -ge 2 ] && break
  sleep 1
done
[ "$messages" -ge 2 ] || fail "the notification service produced $messages messages, want at least 2"
pass "$messages notifications delivered from the same events"

subjects=$(curl -fsS "$GATEWAY_URL/notifications" -H "Authorization: Bearer $token" \
  | python3 -c 'import json,sys; print(",".join(n["eventType"] for n in json.load(sys.stdin)["notifications"]))')
case "$subjects" in
  *order.placed*) pass "order.placed produced a message" ;;
  *) fail "no notification for order.placed (saw: $subjects)" ;;
esac
case "$subjects" in
  *order.shipped*) pass "order.shipped produced a message" ;;
  *) fail "no notification for order.shipped (saw: $subjects)" ;;
esac

step "8. cancelling"
cancel_code=$(status_of -X POST "$GATEWAY_URL/orders/$order_id/cancel" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  -d '{"reason":"smoke test"}')
# The order already shipped, so cancelling it is correctly refused.
[ "$cancel_code" = "409" ] || fail "cancelling a shipped order returned $cancel_code, want 409"
pass "cancelling a shipped order -> 409"

step "9. the catalogue cache"
before=$(curl -fsS "$PRODUCT_ADMIN/metrics" | grep 'quorum_cache_operations_total{cache="catalogue",result="hit"}' | awk '{print $2}')
for _ in 1 2 3 4 5; do curl -fsS "$GATEWAY_URL/product/p7" >/dev/null; done
sleep 1
after=$(curl -fsS "$PRODUCT_ADMIN/metrics" | grep 'quorum_cache_operations_total{cache="catalogue",result="hit"}' | awk '{print $2}')
python3 -c "import sys; sys.exit(0 if float('${after:-0}') > float('${before:-0}') else 1)" \
  || fail "repeated reads of the same product produced no cache hits (${before:-0} -> ${after:-0})"
pass "repeated reads are served from the cache (${before:-0} -> ${after:-0} hits)"

negative=$(curl -fsS "$PRODUCT_ADMIN/metrics" | grep 'result="hit_negative"' | awk '{print $2}' || true)
for _ in 1 2 3; do curl -sS -o /dev/null "$GATEWAY_URL/product/definitely-not-a-product-$STAMP"; done
sleep 1
negative_after=$(curl -fsS "$PRODUCT_ADMIN/metrics" | grep 'result="hit_negative"' | awk '{print $2}' || true)
python3 -c "import sys; sys.exit(0 if float('${negative_after:-0}') > float('${negative:-0}') else 1)" \
  || fail "a repeatedly requested missing product was not negatively cached"
pass "missing products are negatively cached, so they stop reaching the database"

step "10. logout"
curl -fsS -X POST "$GATEWAY_URL/identity/logout" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  -d "{\"refreshToken\":\"$refresh\"}" >/dev/null
pass "logged out"

# The access token is already in the client's hands and verifies on its
# signature alone, so revocation has to be enforced by the denylist.
code=$(status_of -H "Authorization: Bearer $token" "$GATEWAY_URL/orders")
[ "$code" = "401" ] || fail "the access token still works after logout ($code)"
pass "the access token stopped working immediately"

code=$(status_of -X POST "$GATEWAY_URL/identity/refresh" \
  -H 'Content-Type: application/json' -d "{\"refreshToken\":\"$refresh\"}")
[ "$code" = "401" ] || fail "the refresh token still works after logout ($code)"
pass "the refresh token was destroyed"

step "11. observability across the new services"
for name in identity order notification gateway; do
  case $name in
    identity)     admin=$IDENTITY_ADMIN ;;
    order)        admin=$ORDER_ADMIN ;;
    notification) admin=$NOTIFICATION_ADMIN ;;
    gateway)      admin=$GATEWAY_ADMIN ;;
  esac

  ready=$(curl -fsS "$admin/readyz" | json status)
  [ "$ready" = "up" ] || fail "$name readiness is '$ready', want up"

  metrics=$(curl -fsS "$admin/metrics")
  echo "$metrics" | grep -q '^quorum_http_server_requests_total' \
    || fail "$name is not exporting request metrics"

  pass "$name: ready, metrics exported"
done

gateway_metrics=$(curl -fsS "$GATEWAY_ADMIN/metrics")
echo "$gateway_metrics" | grep -q 'quorum_auth_attempts_total' \
  || fail "the gateway is not recording authentication outcomes"
pass "authentication outcomes are recorded"

echo "$gateway_metrics" | grep -q 'quorum_ratelimit_decisions_total' \
  || fail "the gateway is not recording rate limiter decisions"
pass "rate limiter decisions are recorded"

order_metrics=$(curl -fsS "$ORDER_ADMIN/metrics")
echo "$order_metrics" | grep -q 'quorum_domain_events_total{event="order"' \
  || fail "the order service is not recording lifecycle events"
pass "order lifecycle events are recorded"

printf '\n\033[32mall platform smoke checks passed\033[0m\n\n'
