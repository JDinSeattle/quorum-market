package order

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

func testService(t *testing.T) *Service {
	t.Helper()

	cfg := kv.Config{
		NodeID: "order-test-db", Mode: kv.ModeLeaderless,
		WriteQuorum: 1, ReadQuorum: 1, RPCTimeout: time.Second,
	}
	svc := kv.NewService(cfg, kv.NewStore(cfg.NodeID), kv.NewReplicator(time.Second))
	server := httptest.NewServer(
		kv.NewServer(svc, kv.NewTxnManager(time.Minute), busywait.Config{}, false).Routes())
	t.Cleanup(server.Close)

	db := kv.NewClient("order-test-db", server.URL, time.Second, 5*time.Second)
	// A nil event publisher: these tests are about the records this service
	// keeps, not about what it announces.
	return NewService(db, nil)
}

func placedEvent(t *testing.T, orderID, customerID string, total float64) events.Envelope {
	t.Helper()

	envelope, err := events.New(context.Background(), events.OrderPlaced, orderID, customerID,
		events.OrderPlacedData{
			OrderID:    orderID,
			CartID:     "cart-" + customerID,
			CustomerID: customerID,
			Items:      []orders.Item{{ProductID: "p1", Quantity: 2}},
			TotalPrice: total,
			PlacedAt:   time.Now().UTC(),
		})
	if err != nil {
		t.Fatalf("building the event: %v", err)
	}
	return envelope
}

func shippedEvent(t *testing.T, orderID, customerID string) events.Envelope {
	t.Helper()

	envelope, err := events.New(context.Background(), events.OrderShipped, orderID, customerID,
		events.OrderShippedData{
			OrderID:    orderID,
			CustomerID: customerID,
			Items:      []orders.Item{{ProductID: "p1", Quantity: 2}},
			Outcome:    "completed",
			ShippedAt:  time.Now().UTC(),
		})
	if err != nil {
		t.Fatalf("building the event: %v", err)
	}
	return envelope
}

func TestAPlacedEventBecomesAnOrder(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	if err := svc.HandlePlaced(ctx, placedEvent(t, "ord-1", "cust-1", 42.50)); err != nil {
		t.Fatalf("HandlePlaced: %v", err)
	}

	record, err := svc.Get(ctx, "cust-1", "ord-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.Status != StatusPlaced {
		t.Errorf("status = %q, want placed", record.Status)
	}
	if record.TotalPrice != 42.50 {
		t.Errorf("total = %v, want 42.50", record.TotalPrice)
	}
	if !record.Cancellable() {
		t.Error("a freshly placed order should still be cancellable")
	}
}

// The broker guarantees at-least-once delivery, so the same event will arrive
// twice sooner or later and must not produce two orders or reset one.
func TestRedeliveredPlacedEventsAreIgnored(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	event := placedEvent(t, "ord-1", "cust-1", 42.50)

	if err := svc.HandlePlaced(ctx, event); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := svc.HandleShipped(ctx, shippedEvent(t, "ord-1", "cust-1")); err != nil {
		t.Fatalf("HandleShipped: %v", err)
	}
	// The original placed event turns up again after the order has shipped.
	if err := svc.HandlePlaced(ctx, event); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	record, _ := svc.Get(ctx, "cust-1", "ord-1")
	if record.Status != StatusShipped {
		t.Errorf("status = %q, want shipped: the redelivery reset the order", record.Status)
	}

	records, _ := svc.List(ctx, "cust-1", 10)
	if len(records) != 1 {
		t.Errorf("the customer has %d orders, want 1", len(records))
	}
}

func TestAShippedEventAdvancesTheOrder(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	_ = svc.HandlePlaced(ctx, placedEvent(t, "ord-1", "cust-1", 10))
	if err := svc.HandleShipped(ctx, shippedEvent(t, "ord-1", "cust-1")); err != nil {
		t.Fatalf("HandleShipped: %v", err)
	}

	record, _ := svc.Get(ctx, "cust-1", "ord-1")
	if record.Status != StatusShipped {
		t.Fatalf("status = %q, want shipped", record.Status)
	}
	if record.ShippedAt == nil {
		t.Error("no shipping timestamp was recorded")
	}
	if record.Cancellable() {
		t.Error("a shipped order should no longer be cancellable")
	}
}

func TestOrdersAreListedNewestFirst(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	// Order ids embed their placement time, so their keys sort chronologically.
	for _, id := range []string{"ord-1000", "ord-2000", "ord-3000"} {
		if err := svc.HandlePlaced(ctx, placedEvent(t, id, "cust-1", 10)); err != nil {
			t.Fatalf("HandlePlaced(%s): %v", id, err)
		}
	}

	records, err := svc.List(ctx, "cust-1", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d orders, want 3", len(records))
	}
	if records[0].OrderID != "ord-3000" {
		t.Errorf("first order = %q, want the newest", records[0].OrderID)
	}
}

// One customer must never see another's orders, and must not be able to learn
// that an order id exists by getting a different error for it.
func TestOrdersAreScopedToTheirCustomer(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	_ = svc.HandlePlaced(ctx, placedEvent(t, "ord-1", "cust-alice", 10))

	_, err := svc.Get(ctx, "cust-bob", "ord-1")
	if got := httpx.StatusOf(err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}

	records, _ := svc.List(ctx, "cust-bob", 10)
	if len(records) != 0 {
		t.Errorf("bob can see %d of alice's orders", len(records))
	}
}

func TestCancellingAPlacedOrder(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	_ = svc.HandlePlaced(ctx, placedEvent(t, "ord-1", "cust-1", 10))

	record, err := svc.Cancel(ctx, "cust-1", "ord-1", "changed my mind")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if record.Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled", record.Status)
	}
	if record.CancelReason != "changed my mind" {
		t.Errorf("reason = %q", record.CancelReason)
	}
	if record.CancelledAt == nil {
		t.Error("no cancellation timestamp was recorded")
	}

	// Cancelling twice is the customer clicking again, not an error.
	if _, err := svc.Cancel(ctx, "cust-1", "ord-1", ""); err != nil {
		t.Errorf("cancelling an already-cancelled order failed: %v", err)
	}
}

// Once goods have left the warehouse, calling the order off is a returns
// problem rather than a cancellation.
func TestShippedOrdersCannotBeCancelled(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	_ = svc.HandlePlaced(ctx, placedEvent(t, "ord-1", "cust-1", 10))
	_ = svc.HandleShipped(ctx, shippedEvent(t, "ord-1", "cust-1"))

	_, err := svc.Cancel(ctx, "cust-1", "ord-1", "too late")
	if got := httpx.StatusOf(err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
}

func TestMalformedEventsAreRejected(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	// No order id: nothing sensible can be recorded.
	envelope, _ := events.New(ctx, events.OrderPlaced, "", "", events.OrderPlacedData{})
	if err := svc.HandlePlaced(ctx, envelope); err == nil {
		t.Error("an event with no order or customer id was accepted")
	}
}

func TestKeysAreNamespacedPerCustomer(t *testing.T) {
	if got, want := Key("cust-1", "ord-9"), "order:cust-1:ord-9"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
	if got, want := CustomerPrefix("cust-1"), "order:cust-1:"; got != want {
		t.Errorf("CustomerPrefix = %q, want %q", got, want)
	}
}

// The two events come from different services over different paths, so nothing
// orders them. Handling them has to be commutative, or a race silently loses a
// shipment to the dead-letter queue.
func TestEventsCanArriveInEitherOrder(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		svc := testService(t)
		ctx := context.Background()

		placed := placedEvent(t, "ord-1", "cust-1", 42.50)
		shipped := shippedEvent(t, "ord-1", "cust-1")

		var err error
		if reversed {
			if err = svc.HandleShipped(ctx, shipped); err != nil {
				t.Fatalf("shipped first: %v", err)
			}
			err = svc.HandlePlaced(ctx, placed)
		} else {
			if err = svc.HandlePlaced(ctx, placed); err != nil {
				t.Fatalf("placed first: %v", err)
			}
			err = svc.HandleShipped(ctx, shipped)
		}
		if err != nil {
			t.Fatalf("reversed=%v: %v", reversed, err)
		}

		record, err := svc.Get(ctx, "cust-1", "ord-1")
		if err != nil {
			t.Fatalf("reversed=%v: Get: %v", reversed, err)
		}
		if record.Status != StatusShipped {
			t.Errorf("reversed=%v: status = %q, want shipped", reversed, record.Status)
		}
		if record.TotalPrice != 42.50 {
			t.Errorf("reversed=%v: total = %v, want the placement details applied", reversed, record.TotalPrice)
		}
		if len(record.Items) != 1 {
			t.Errorf("reversed=%v: %d items, want 1", reversed, len(record.Items))
		}
		if record.AwaitingDetails {
			t.Errorf("reversed=%v: the order is still marked as awaiting details", reversed)
		}
		if record.ShippedAt == nil {
			t.Errorf("reversed=%v: no shipping timestamp", reversed)
		}
	}
}

// While only the shipment is known, the record has to say so rather than
// present itself as a complete order with no line items.
func TestAShipmentWithoutItsPlacementIsMarkedIncomplete(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	if err := svc.HandleShipped(ctx, shippedEvent(t, "ord-1", "cust-1")); err != nil {
		t.Fatalf("HandleShipped: %v", err)
	}

	record, err := svc.Get(ctx, "cust-1", "ord-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !record.AwaitingDetails {
		t.Error("the stub record is not marked as awaiting its details")
	}
	if record.HasDetails() {
		t.Error("HasDetails reported true for a record with no placement event")
	}
	if record.Status != StatusShipped {
		t.Errorf("status = %q, want shipped", record.Status)
	}
}

// A late placement event must fill in the details without undoing a status
// that has already moved on.
func TestALatePlacementDoesNotRewindTheStatus(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	_ = svc.HandleShipped(ctx, shippedEvent(t, "ord-1", "cust-1"))
	_ = svc.HandlePlaced(ctx, placedEvent(t, "ord-1", "cust-1", 42.50))

	record, _ := svc.Get(ctx, "cust-1", "ord-1")
	if record.Status != StatusShipped {
		t.Errorf("status = %q, want shipped: the late placement rewound it", record.Status)
	}
}
