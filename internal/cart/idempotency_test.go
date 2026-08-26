package cart

import (
	"context"
	"net/http"
	"testing"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// The failure this prevents: a client's connection drops after the charge but
// before the response arrives, it retries, and the customer is billed twice.
func TestRetryWithTheSameKeyReplaysTheReceipt(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)
	h.add(t, "alice", "p1", 2)

	req := CheckoutRequest{CreditCard: goodCard, IdempotencyKey: "checkout-attempt-1"}

	first, err := h.checkout.Checkout(context.Background(), IDFor("alice"), req)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}

	second, err := h.checkout.Checkout(context.Background(), IDFor("alice"), req)
	if err != nil {
		t.Fatalf("retry with the same key: %v", err)
	}

	if second.OrderID != first.OrderID {
		t.Errorf("retry produced order %q, want the original %q", second.OrderID, first.OrderID)
	}
	if second.TotalPrice != first.TotalPrice {
		t.Errorf("retry total = %v, want %v", second.TotalPrice, first.TotalPrice)
	}

	if got := h.authorizations.Load(); got != 1 {
		t.Errorf("the card was authorized %d times, want exactly 1", got)
	}
	if got := h.inv.Quantity("p1"); got != 98 {
		t.Errorf("p1 stock = %d, want 98: the retry reserved a second time", got)
	}
	if msgs := h.publisher.shipMessages(t); len(msgs) != 1 {
		t.Errorf("%d ship messages queued, want 1: the retry shipped a second order", len(msgs))
	}
}

// Without a key the system cannot tell a retry from a new purchase, and the
// second request is a genuine second order. Worth pinning down so the
// behaviour is a documented choice rather than an accident.
func TestWithoutAKeyEachRequestIsANewOrder(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)
	h.add(t, "alice", "p1", 2)

	first, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard})
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}

	// The cart was emptied, so a second identical request has nothing to buy.
	h.add(t, "alice", "p1", 2)
	second, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard})
	if err != nil {
		t.Fatalf("second checkout: %v", err)
	}

	if first.OrderID == second.OrderID {
		t.Error("two unkeyed checkouts produced the same order id")
	}
	if got := h.authorizations.Load(); got != 2 {
		t.Errorf("the card was authorized %d times, want 2", got)
	}
}

func TestDifferentKeysProduceDifferentOrders(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)

	h.add(t, "alice", "p1", 1)
	first, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard, IdempotencyKey: "attempt-a"})
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}

	h.add(t, "alice", "p1", 1)
	second, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard, IdempotencyKey: "attempt-b"})
	if err != nil {
		t.Fatalf("second checkout: %v", err)
	}

	if first.OrderID == second.OrderID {
		t.Error("distinct keys replayed the same order")
	}
	if got := h.inv.Quantity("p1"); got != 98 {
		t.Errorf("p1 stock = %d, want 98", got)
	}
}

// A key belongs to the checkout that first used it. Honouring it for another
// cart would hand one customer another customer's receipt.
func TestAKeyCannotBeReusedForAnotherCart(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)
	h.add(t, "alice", "p1", 1)
	h.add(t, "bob", "p1", 1)

	if _, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard, IdempotencyKey: "shared"}); err != nil {
		t.Fatalf("alice's checkout: %v", err)
	}

	_, err := h.checkout.Checkout(context.Background(), IDFor("bob"),
		CheckoutRequest{CreditCard: goodCard, IdempotencyKey: "shared"})
	if got := httpx.StatusOf(err); got != http.StatusConflict {
		t.Fatalf("bob's checkout status = %d, want 409", got)
	}
	if got := h.authorizations.Load(); got != 1 {
		t.Errorf("the card was authorized %d times, want 1", got)
	}
}

// A checkout in flight blocks a concurrent duplicate, so two attempts cannot
// reserve stock and charge a card side by side.
func TestAnInFlightClaimBlocksADuplicate(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)
	h.add(t, "alice", "p1", 1)

	// Stand in for an attempt that is still running.
	if _, err := h.checkout.idem.claim(context.Background(), "busy", IDFor("alice")); err != nil {
		t.Fatalf("claiming the key: %v", err)
	}

	_, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard, IdempotencyKey: "busy"})
	if got := httpx.StatusOf(err); got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}
	if got := h.authorizations.Load(); got != 0 {
		t.Errorf("the card was authorized %d times, want 0", got)
	}
	if got := h.inv.Quantity("p1"); got != 100 {
		t.Errorf("p1 stock = %d, want 100: the blocked attempt still reserved", got)
	}
}

// A decline has no charge and no order behind it, so there is nothing to
// protect against repeating — and the customer must be able to retry with the
// same key once they have sorted their card out.
func TestAFailedCheckoutGivesTheKeyBack(t *testing.T) {
	h := newHarness(t, 100, 0.0) // every card is declined
	h.seed(t, "p1", 1, 20)
	h.add(t, "alice", "p1", 1)

	req := CheckoutRequest{CreditCard: goodCard, IdempotencyKey: "retry-me"}

	_, err := h.checkout.Checkout(context.Background(), IDFor("alice"), req)
	if got := httpx.StatusOf(err); got != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", got)
	}

	// The claim must not be left holding the key hostage.
	_, err = h.checkout.Checkout(context.Background(), IDFor("alice"), req)
	if got := httpx.StatusOf(err); got != http.StatusPaymentRequired {
		t.Fatalf("retry status = %d, want 402 again, not a 409 from a stuck claim", got)
	}
	if got := h.inv.Quantity("p1"); got != 100 {
		t.Errorf("p1 stock = %d, want 100", got)
	}
	if got := h.inv.PendingReservations(); got != 0 {
		t.Errorf("%d reservations left held after two declines", got)
	}
}

// The key is stored as part of a storage key and echoed into logs, so an
// unbounded or exotic value must be refused.
func TestIdempotencyKeysAreValidated(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)
	h.add(t, "alice", "p1", 1)

	hostile := []string{
		"has spaces",
		"line\nbreak",
		"slash/key",
		string(make([]byte, 200)),
	}
	for _, key := range hostile {
		_, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
			CheckoutRequest{CreditCard: goodCard, IdempotencyKey: key})
		if got := httpx.StatusOf(err); got != http.StatusBadRequest {
			t.Errorf("key %q: status = %d, want 400", key, got)
		}
	}
	if got := h.authorizations.Load(); got != 0 {
		t.Errorf("a rejected key still reached the authorizer %d times", got)
	}
}
