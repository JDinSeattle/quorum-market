package cart

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/cca"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/orders"
	"github.com/JDinSeattle/quorum-market/internal/product"
	"github.com/JDinSeattle/quorum-market/internal/warehouse"
)

// recordingPublisher stands in for the broker so these tests can cover the
// whole checkout flow without one running.
type recordingPublisher struct {
	mu   sync.Mutex
	msgs [][]byte
	err  error
}

func (p *recordingPublisher) Publish(_ context.Context, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.msgs = append(p.msgs, append([]byte(nil), body...))
	return nil
}

func (p *recordingPublisher) shipMessages(t *testing.T) []orders.ShipMessage {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]orders.ShipMessage, 0, len(p.msgs))
	for _, raw := range p.msgs {
		var msg orders.ShipMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("published message is not a ship message: %v", err)
		}
		out = append(out, msg)
	}
	return out
}

// harness runs the real database, catalogue, warehouse and authorizer over
// loopback HTTP. Only the broker is stubbed, so these tests cover the actual
// cross-service behaviour of checkout rather than a mocked-out sketch of it.
type harness struct {
	carts     *Service
	checkout  *CheckoutService
	inv       *warehouse.Inventory
	products  *product.Client
	publisher *recordingPublisher
	db        *kv.Client

	// authorizations counts calls that actually reached the card authorizer,
	// which is how the idempotency tests prove a retry did not charge twice.
	authorizations *atomic.Int32
}

func newHarness(t *testing.T, stock int, approvalRate float64) *harness {
	t.Helper()

	// No simulated delay: these tests are about correctness, and burning CPU
	// per request would make the suite take minutes.
	noDelay := busywait.Config{}

	kvCfg := kv.Config{
		NodeID: "test-db", Mode: kv.ModeLeaderless,
		WriteQuorum: 1, ReadQuorum: 1, RPCTimeout: time.Second,
	}
	kvSvc := kv.NewService(kvCfg, kv.NewStore(kvCfg.NodeID), kv.NewReplicator(time.Second))
	kvSrv := httptest.NewServer(kv.NewServer(kvSvc, kv.NewTxnManager(time.Minute), noDelay, false).Routes())
	t.Cleanup(kvSrv.Close)
	db := kv.NewClient("test-db", kvSrv.URL, time.Second, 5*time.Second)

	productSrv := httptest.NewServer(product.NewServer(product.NewService(db, nil), noDelay).Routes())
	t.Cleanup(productSrv.Close)
	products := product.NewClient(productSrv.URL, time.Second, 5*time.Second)

	inv := warehouse.New(stock, time.Hour)
	whSrv := httptest.NewServer(warehouse.NewServer(inv, noDelay, nil).Routes())
	t.Cleanup(whSrv.Close)
	warehouseClient := warehouse.NewClient(whSrv.URL, time.Second, 5*time.Second)

	var authorizations atomic.Int32
	ccaRoutes := cca.NewServer(approvalRate, noDelay).Routes()
	ccaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			authorizations.Add(1)
		}
		ccaRoutes.ServeHTTP(w, r)
	}))
	t.Cleanup(ccaSrv.Close)
	cards := cca.NewClient(ccaSrv.URL, time.Second, 5*time.Second)

	publisher := &recordingPublisher{}
	carts := NewService(products, warehouseClient, db, noDelay)

	return &harness{
		carts:          carts,
		checkout:       NewCheckoutService(carts, warehouseClient, cards, db, publisher, nil, noDelay),
		inv:            inv,
		products:       products,
		publisher:      publisher,
		db:             db,
		authorizations: &authorizations,
	}
}

func (h *harness) seed(t *testing.T, productID string, weight, price float64) {
	t.Helper()
	err := h.products.Put(context.Background(), product.Product{
		ProductID: productID, Name: "Test Item", Weight: weight, Price: price,
	})
	if err != nil {
		t.Fatalf("seeding %s: %v", productID, err)
	}
}

func (h *harness) add(t *testing.T, customerID, productID string, qty int) *Cart {
	t.Helper()
	cart, err := h.carts.AddItem(context.Background(), customerID,
		AddItemRequest{ProductID: productID, Quantity: qty})
	if err != nil {
		t.Fatalf("AddItem(%s x%d): %v", productID, qty, err)
	}
	return cart
}

const goodCard = "4111-1111-1111-1111"

func TestCheckoutHappyPath(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 2.5, 10.0)
	h.seed(t, "p2", 0.5, 4.25)
	h.add(t, "alice", "p1", 3)
	h.add(t, "alice", "p2", 2)

	receipt, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	if receipt.OrderID == "" {
		t.Error("receipt has no order id")
	}
	if receipt.Status != "confirmed" {
		t.Errorf("status = %q, want confirmed", receipt.Status)
	}
	if want := 38.50; receipt.TotalPrice != want {
		t.Errorf("total price = %v, want %v", receipt.TotalPrice, want)
	}
	if want := 8.5; receipt.TotalWeight != want {
		t.Errorf("total weight = %v, want %v", receipt.TotalWeight, want)
	}

	// Stock came off the shelf exactly once, when it was reserved.
	if got := h.inv.Quantity("p1"); got != 97 {
		t.Errorf("p1 stock = %d, want 97", got)
	}
	if got := h.inv.Quantity("p2"); got != 98 {
		t.Errorf("p2 stock = %d, want 98", got)
	}

	msgs := h.publisher.shipMessages(t)
	if len(msgs) != 1 {
		t.Fatalf("published %d ship messages, want 1", len(msgs))
	}
	// The reservation id has to travel with the order, otherwise the warehouse
	// cannot tell shipping from a fresh deduction.
	if msgs[0].ReservationID == "" {
		t.Error("ship message carries no reservation id")
	}
	if msgs[0].OrderID != receipt.OrderID {
		t.Errorf("ship message order id = %q, want %q", msgs[0].OrderID, receipt.OrderID)
	}
	if len(msgs[0].Items) != 2 {
		t.Errorf("ship message has %d items, want 2", len(msgs[0].Items))
	}

	// The order is durable, and the cart is empty again.
	entry, found, err := h.db.Get(context.Background(), OrderKeyPrefix+receipt.OrderID)
	if err != nil || !found {
		t.Fatalf("order not committed to the store: %v (found=%v)", err, found)
	}
	var stored Order
	if err := json.Unmarshal([]byte(entry.Value), &stored); err != nil {
		t.Fatalf("stored order is unreadable: %v", err)
	}
	if stored.AuthorizationCode == "" {
		t.Error("stored order has no authorization code")
	}

	cart, err := h.carts.Get(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Get cart: %v", err)
	}
	if cart == nil || !cart.IsEmpty() {
		t.Errorf("cart after checkout = %+v, want empty", cart)
	}
}

// A declined card must hand the reserved stock straight back. The original
// implementation of this system leaked it, and every decline permanently
// inflated inventory.
func TestCheckoutDeclinedReleasesTheReservation(t *testing.T) {
	h := newHarness(t, 100, 0.0) // every card is declined
	h.seed(t, "p1", 1, 5)
	h.add(t, "alice", "p1", 4)

	_, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard})
	if err == nil {
		t.Fatal("Checkout succeeded despite a declined card")
	}
	if got := httpx.StatusOf(err); got != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", got)
	}

	if got := h.inv.Quantity("p1"); got != 100 {
		t.Errorf("p1 stock = %d, want 100: the reservation was not released", got)
	}
	if got := h.inv.PendingReservations(); got != 0 {
		t.Errorf("pending reservations = %d, want 0", got)
	}
	if msgs := h.publisher.shipMessages(t); len(msgs) != 0 {
		t.Errorf("published %d ship messages for a declined order, want 0", len(msgs))
	}

	// The customer keeps their cart so they can retry with another card.
	cart, _ := h.carts.Get(context.Background(), "alice")
	if cart == nil || cart.IsEmpty() {
		t.Error("cart was cleared even though checkout failed")
	}
}

func TestCheckoutOutOfStock(t *testing.T) {
	h := newHarness(t, 5, 1.0)
	h.seed(t, "p1", 1, 5)
	h.add(t, "alice", "p1", 5)

	// Somebody else takes the last units between browsing and paying.
	if _, err := h.inv.Reserve([]orders.Item{{ProductID: "p1", Quantity: 5}}); err != nil {
		t.Fatalf("draining stock: %v", err)
	}

	_, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard})
	if err == nil {
		t.Fatal("Checkout succeeded with no stock left")
	}
	if got := httpx.StatusOf(err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
	if msgs := h.publisher.shipMessages(t); len(msgs) != 0 {
		t.Errorf("published %d ship messages, want 0", len(msgs))
	}
	// Only the reservation made by the other customer is outstanding.
	if got := h.inv.PendingReservations(); got != 1 {
		t.Errorf("pending reservations = %d, want 1", got)
	}
}

func TestCheckoutInvalidCardReleasesTheReservation(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 5)
	h.add(t, "alice", "p1", 2)

	_, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: "not-a-card"})
	if err == nil {
		t.Fatal("Checkout accepted a malformed card")
	}
	if got := httpx.StatusOf(err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
	if got := h.inv.Quantity("p1"); got != 100 {
		t.Errorf("p1 stock = %d, want 100", got)
	}
	if got := h.inv.PendingReservations(); got != 0 {
		t.Errorf("pending reservations = %d, want 0", got)
	}
}

func TestCheckoutRejectsEmptyAndMissingCarts(t *testing.T) {
	h := newHarness(t, 100, 1.0)

	_, err := h.checkout.Checkout(context.Background(), IDFor("nobody"),
		CheckoutRequest{CreditCard: goodCard})
	if got := httpx.StatusOf(err); got != http.StatusBadRequest {
		t.Errorf("missing cart: status = %d, want 400", got)
	}

	_, err = h.checkout.Checkout(context.Background(), IDFor("alice"), CheckoutRequest{})
	if got := httpx.StatusOf(err); got != http.StatusBadRequest {
		t.Errorf("missing card: status = %d, want 400", got)
	}
}

// The customer has already been charged by the time the order is queued, so a
// broker failure must not turn a successful purchase into an error.
func TestCheckoutSucceedsEvenIfQueueingFails(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.publisher.err = http.ErrHandlerTimeout
	h.seed(t, "p1", 1, 5)
	h.add(t, "alice", "p1", 2)

	receipt, err := h.checkout.Checkout(context.Background(), IDFor("alice"),
		CheckoutRequest{CreditCard: goodCard})
	if err != nil {
		t.Fatalf("Checkout failed because the broker did: %v", err)
	}
	if receipt.OrderID == "" {
		t.Error("receipt has no order id")
	}
	// The stock stays held: the reservation is what keeps it accounted for
	// until the order is picked up again.
	if got := h.inv.Quantity("p1"); got != 98 {
		t.Errorf("p1 stock = %d, want 98", got)
	}
}

func TestAddItemUnknownProduct(t *testing.T) {
	h := newHarness(t, 100, 1.0)

	_, err := h.carts.AddItem(context.Background(), "alice",
		AddItemRequest{ProductID: "ghost", Quantity: 1})
	if got := httpx.StatusOf(err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}

func TestAddItemRejectsBadQuantities(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 5)

	for _, qty := range []int{0, -3} {
		_, err := h.carts.AddItem(context.Background(), "alice",
			AddItemRequest{ProductID: "p1", Quantity: qty})
		if got := httpx.StatusOf(err); got != http.StatusBadRequest {
			t.Errorf("quantity %d: status = %d, want 400", qty, got)
		}
	}
}

// Availability is judged against what the cart would hold in total, so a
// customer cannot walk to a checkout that is certain to fail by adding the
// same product repeatedly.
func TestAddItemCountsWhatIsAlreadyInTheCart(t *testing.T) {
	h := newHarness(t, 10, 1.0)
	h.seed(t, "p1", 1, 5)
	h.add(t, "alice", "p1", 6)

	_, err := h.carts.AddItem(context.Background(), "alice",
		AddItemRequest{ProductID: "p1", Quantity: 6})
	if got := httpx.StatusOf(err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
}

// Knowing a cart id must not be enough to act on it. Cart ids are derived from
// customer ids, so without this check anyone who learned another customer's id
// could read their basket or check it out on their card.
func TestOneCustomerCannotActOnAnothersCart(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)
	h.add(t, "alice", "p1", 2)

	srv := NewServer(h.carts, h.checkout)
	handler := srv.Routes()

	victimCart := IDFor("alice")

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"read the cart", http.MethodGet, "/shopping-cart/" + victimCart, ""},
		{"add an item", http.MethodPost, "/shopping-cart/" + victimCart + "/add-item", `{"productId":"p1","quantity":1}`},
		{"check it out", http.MethodPost, "/shopping-cart/" + victimCart + "/checkout", `{"creditCard":"` + goodCard + `"}`},
	} {
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, tc.path, body)
		req.Header.Set("Content-Type", "application/json")
		// The gateway would set this from a verified token belonging to mallory.
		req.Header.Set(auth.CustomerIDHeader, "mallory")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", tc.name, rec.Code)
		}
	}

	// Alice's cart is untouched and her card was never charged.
	cart, _ := h.carts.Get(context.Background(), "alice")
	if cart == nil || len(cart.Items) != 1 || cart.Items[0].Quantity != 2 {
		t.Errorf("alice's cart was modified: %+v", cart)
	}
	if got := h.authorizations.Load(); got != 0 {
		t.Errorf("mallory's attempt reached the card authorizer %d times", got)
	}
}

func TestAnAuthenticatedCallerCanUseTheirOwnCart(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	h.seed(t, "p1", 1, 20)

	handler := NewServer(h.carts, h.checkout).Routes()

	req := httptest.NewRequest(http.MethodPost, "/shopping-cart/"+IDFor("alice")+"/add-item",
		strings.NewReader(`{"productId":"p1","quantity":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CustomerIDHeader, "alice")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: alice was refused her own cart", rec.Code)
	}
}

// Whatever the body says, an authenticated caller gets a cart of their own.
func TestCartCreationIgnoresTheBodyWhenAuthenticated(t *testing.T) {
	h := newHarness(t, 100, 1.0)
	handler := NewServer(h.carts, h.checkout).Routes()

	req := httptest.NewRequest(http.MethodPost, "/shopping-cart",
		strings.NewReader(`{"customerId":"someone-else"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CustomerIDHeader, "alice")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), IDFor("alice")) {
		t.Errorf("body = %s, want a cart belonging to the authenticated caller", rec.Body.String())
	}
}
