#!/usr/bin/env bash
#
# End-to-end check against a running stack. Exercises both use cases the system
# exists for — adding to a cart and checking out — and then verifies the side
# effects that are easy to get wrong: stock deducted exactly once, the order
# durable in the database, the cart emptied, and the shipment consumed off the
# queue.
#
#   make up && make seed && make smoke
#
set -euo pipefail

PRODUCT_URL="${PRODUCT_URL:-http://localhost:8081}"
CART_URL="${CART_URL:-http://localhost:8082}"
WAREHOUSE_URL="${WAREHOUSE_URL:-http://localhost:8084}"
CCA_URL="${CCA_URL:-http://localhost:8083}"

# Admin ports, where metrics and readiness live. Never the service port.
PRODUCT_ADMIN="${PRODUCT_ADMIN:-http://localhost:9210}"
CART_ADMIN="${CART_ADMIN:-http://localhost:9213}"
WAREHOUSE_ADMIN="${WAREHOUSE_ADMIN:-http://localhost:9212}"

# A card the authorizer always accepts the format of. Declines are random, so
# the checkout step retries a few times rather than failing on an unlucky draw.
CARD="4111-1111-1111-1111"
CUSTOMER="smoke-$(date +%s)"
PRODUCT_ID="smoke-widget-$(date +%s)"

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# json <path> reads a field out of JSON on stdin, e.g. json cartId
json() {
  python3 -c '
import json, sys
doc = json.load(sys.stdin)
for key in sys.argv[1].split("."):
    doc = doc[int(key)] if key.isdigit() else doc[key]
print(doc)
' "$1"
}

wait_for() {
  local name="$1" url="$2"
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      pass "$name is up"
      return 0
    fi
    sleep 2
  done
  fail "$name never became healthy at $url"
}

step "1. waiting for services"
wait_for "product service"  "$PRODUCT_URL/product/health"
wait_for "cart service"     "$CART_URL/shopping-cart/health"
wait_for "cca service"      "$CCA_URL/credit-card-authorizer/health"
wait_for "warehouse"        "$WAREHOUSE_URL/warehouse/health"

step "2. catalogue"
curl -fsS -X PUT "$PRODUCT_URL/product/$PRODUCT_ID" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Smoke Widget","weight":2.5,"price":10.00}' >/dev/null
pass "created $PRODUCT_ID"

weight=$(curl -fsS "$PRODUCT_URL/product/$PRODUCT_ID" | json weight)
[ "$weight" = "2.5" ] || fail "product weight read back as $weight, want 2.5"
pass "read it back through the leader-follower cluster"

step "3. inventory before"
stock_before=$(curl -fsS "$WAREHOUSE_URL/warehouse/inventory/$PRODUCT_ID" | json quantity)
pass "stock is $stock_before"

step "4. add to cart"
cart_id=$(curl -fsS -X POST "$CART_URL/shopping-cart" \
  -H 'Content-Type: application/json' \
  -d "{\"customerId\":\"$CUSTOMER\"}" | json cartId)
pass "opened cart $cart_id"

curl -fsS -X POST "$CART_URL/shopping-cart/$cart_id/add-item" \
  -H 'Content-Type: application/json' \
  -d "{\"productId\":\"$PRODUCT_ID\",\"quantity\":3}" >/dev/null
pass "added 3 units"

total=$(curl -fsS "$CART_URL/shopping-cart/$cart_id" | json totalPrice)
[ "$total" = "30.0" ] || [ "$total" = "30" ] || fail "cart total is $total, want 30"
pass "cart total is $total across the leaderless cluster"

step "5. checkout"
order_id=""
for attempt in 1 2 3 4 5 6 7 8; do
  response=$(curl -sS -o /tmp/smoke-checkout.json -w '%{http_code}' \
    -X POST "$CART_URL/shopping-cart/$cart_id/checkout" \
    -H 'Content-Type: application/json' \
    -d "{\"creditCard\":\"$CARD\"}")

  if [ "$response" = "200" ]; then
    order_id=$(json orderId < /tmp/smoke-checkout.json)
    pass "order $order_id confirmed on attempt $attempt"
    break
  fi
  if [ "$response" = "402" ]; then
    # The authorizer declines 10% of cards by design. A decline deliberately
    # leaves the cart intact so the customer can try another card, so retrying
    # means retrying the checkout and nothing else — re-adding the items here
    # would double the order.
    printf '       card declined (expected ~10%% of the time), retrying\n'
    continue
  fi
  fail "checkout returned $response: $(cat /tmp/smoke-checkout.json)"
done
[ -n "$order_id" ] || fail "checkout was declined 8 times in a row, which should be nearly impossible"

step "6. side effects"
stock_after=$(curl -fsS "$WAREHOUSE_URL/warehouse/inventory/$PRODUCT_ID" | json quantity)
expected=$((stock_before - 3))
[ "$stock_after" = "$expected" ] || fail "stock is $stock_after, want $expected (deducted more than once?)"
pass "stock went $stock_before -> $stock_after, deducted exactly once"

remaining=$(curl -fsS "$CART_URL/shopping-cart/$cart_id" | json totalPrice)
[ "$remaining" = "0" ] || [ "$remaining" = "0.0" ] || fail "cart still totals $remaining after checkout"
pass "cart was emptied"

# The shipment travels through RabbitMQ, so give the consumer a moment.
for _ in $(seq 1 20); do
  shipped=$(curl -fsS "$WAREHOUSE_URL/warehouse/stats" | json shipped)
  [ "$shipped" -gt 0 ] && break
  sleep 1
done
[ "${shipped:-0}" -gt 0 ] || fail "no shipment was consumed off the queue"
pass "warehouse consumed the shipment ($shipped total)"

pending=$(curl -fsS "$WAREHOUSE_URL/warehouse/stats" | json pending_reservations)
[ "$pending" = "0" ] || fail "$pending reservations are still held after a completed order"
pass "no reservation left dangling"

stock_final=$(curl -fsS "$WAREHOUSE_URL/warehouse/inventory/$PRODUCT_ID" | json quantity)
[ "$stock_final" = "$expected" ] || fail "stock drifted to $stock_final after shipping, want $expected"
pass "stock stayed at $stock_final after shipping"

step "7. error paths"
code=$(curl -sS -o /dev/null -w '%{http_code}' "$PRODUCT_URL/product/does-not-exist")
[ "$code" = "404" ] || fail "unknown product returned $code, want 404"
pass "unknown product -> 404"

code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$CART_URL/shopping-cart/$cart_id/checkout" \
  -H 'Content-Type: application/json' -d "{\"creditCard\":\"$CARD\"}")
[ "$code" = "400" ] || fail "checking out an empty cart returned $code, want 400"
pass "empty cart checkout -> 400"

code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$CART_URL/shopping-cart/$cart_id/add-item" \
  -H 'Content-Type: application/json' -d '{"productId":"ghost","quantity":1}')
[ "$code" = "404" ] || fail "adding an unknown product returned $code, want 404"
pass "unknown product in cart -> 404"

step "8. retry safety"
# The failure this guards against: a lost response, a client retry, and a
# customer charged twice for one cart.
idem_customer="smoke-idem-$(date +%s)"
idem_cart=$(curl -fsS -X POST "$CART_URL/shopping-cart" \
  -H 'Content-Type: application/json' \
  -d "{\"customerId\":\"$idem_customer\"}" | json cartId)
curl -fsS -X POST "$CART_URL/shopping-cart/$idem_cart/add-item" \
  -H 'Content-Type: application/json' \
  -d "{\"productId\":\"$PRODUCT_ID\",\"quantity\":2}" >/dev/null

idem_key="smoke-key-$(date +%s)"
stock_pre=$(curl -fsS "$WAREHOUSE_URL/warehouse/inventory/$PRODUCT_ID" | json quantity)

first_order=""
for attempt in 1 2 3 4 5 6 7 8; do
  code=$(curl -sS -o /tmp/smoke-idem.json -w '%{http_code}' \
    -X POST "$CART_URL/shopping-cart/$idem_cart/checkout" \
    -H 'Content-Type: application/json' -H "Idempotency-Key: $idem_key" \
    -d "{\"creditCard\":\"$CARD\"}")
  if [ "$code" = "200" ]; then first_order=$(json orderId < /tmp/smoke-idem.json); break; fi
  # A decline leaves the cart intact and hands the key back, so the retry is
  # the same request again — re-adding the items here would double the order,
  # and a fresh key would stop testing that the key is reusable.
  [ "$code" = "402" ] || fail "keyed checkout returned $code: $(cat /tmp/smoke-idem.json)"
  printf '       card declined, retrying with the same key\n'
done
[ -n "$first_order" ] || fail "keyed checkout never succeeded"
pass "checkout with an idempotency key -> $first_order"

replay=$(curl -fsS -X POST "$CART_URL/shopping-cart/$idem_cart/checkout" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $idem_key" \
  -d "{\"creditCard\":\"$CARD\"}" | json orderId)
[ "$replay" = "$first_order" ] || fail "the retry produced order $replay, want the original $first_order"
pass "retrying with the same key replayed the original order"

stock_post=$(curl -fsS "$WAREHOUSE_URL/warehouse/inventory/$PRODUCT_ID" | json quantity)
expected_idem=$((stock_pre - 2))
[ "$stock_post" = "$expected_idem" ] || fail "stock is $stock_post, want $expected_idem: the retry reserved a second time"
pass "the retry reserved nothing further ($stock_pre -> $stock_post)"

step "9. observability"
for name in product cart warehouse; do
  case $name in
    product)   admin=$PRODUCT_ADMIN ;;
    cart)      admin=$CART_ADMIN ;;
    warehouse) admin=$WAREHOUSE_ADMIN ;;
  esac

  curl -fsS "$admin/healthz" >/dev/null || fail "$name liveness endpoint is not answering"

  ready=$(curl -fsS "$admin/readyz" | json status)
  [ "$ready" = "up" ] || fail "$name readiness is '$ready', want up"

  version=$(curl -fsS "$admin/version" | json version)
  [ -n "$version" ] || fail "$name does not report a version"

  # Captured before matching rather than piped straight into grep: under
  # `set -o pipefail`, grep -q exits on its first match, curl takes SIGPIPE,
  # and the pipeline reports failure for a check that actually succeeded.
  metrics=$(curl -fsS "$admin/metrics")
  echo "$metrics" | grep -q '^quorum_http_server_requests_total' \
    || fail "$name is not exporting request metrics"

  pass "$name: healthy, ready, version $version, metrics exported"
done

# The domain metrics are the ones an operator actually alerts on, so their
# absence is a real regression rather than a cosmetic one.
cart_metrics=$(curl -fsS "$CART_ADMIN/metrics")
echo "$cart_metrics" | grep -q 'quorum_domain_events_total{event="checkout",outcome="confirmed"}' \
  || fail "the cart service is not recording checkout outcomes"
pass "checkout outcomes are being recorded"

warehouse_metrics=$(curl -fsS "$WAREHOUSE_ADMIN/metrics")
echo "$warehouse_metrics" | grep -q 'quorum_domain_events_total{event="reservation"' \
  || fail "the warehouse is not recording reservation outcomes"
pass "reservation outcomes are being recorded"

# A request id must survive every hop, or a customer report cannot be traced.
trace_id="smoketrace$(date +%s)"
headers=$(curl -sS -D- -o /dev/null -H "X-Request-Id: $trace_id" "$PRODUCT_URL/product/$PRODUCT_ID")
echoed=$(echo "$headers" | grep -i '^x-request-id:' | tr -d '\r' | awk '{print $2}')
[ "$echoed" = "$trace_id" ] || fail "the request id came back as '$echoed', want $trace_id"
pass "request ids are honoured and echoed"

step "10. dead-letter queue"
dlq=$(curl -fsS "$WAREHOUSE_URL/warehouse/stats" | json queue.dead_lettered)
[ "$dlq" = "0" ] || fail "$dlq orders were parked on the dead-letter queue"
pass "no orders were dead-lettered"

printf '\n\033[32mall smoke checks passed\033[0m\n\n'
