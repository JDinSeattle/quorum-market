package notification

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

func testService(t *testing.T, inboxSize int) *Service {
	t.Helper()

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	return NewService(client, inboxSize, time.Hour)
}

func envelope(t *testing.T, typ events.Type, orderID, customerID string, data any) events.Envelope {
	t.Helper()

	e, err := events.New(context.Background(), typ, orderID, customerID, data)
	if err != nil {
		t.Fatalf("building the event: %v", err)
	}
	return e
}

func TestEachOrderEventProducesAMessage(t *testing.T) {
	svc := testService(t, 50)
	ctx := context.Background()

	all := []events.Envelope{
		envelope(t, events.OrderPlaced, "ord-1", "cust-1", events.OrderPlacedData{
			OrderID: "ord-1", CustomerID: "cust-1",
			Items: []orders.Item{{ProductID: "p1", Quantity: 2}}, TotalPrice: 25,
		}),
		envelope(t, events.OrderShipped, "ord-1", "cust-1", events.OrderShippedData{
			OrderID: "ord-1", CustomerID: "cust-1",
			Items: []orders.Item{{ProductID: "p1", Quantity: 2}}, ShippedAt: time.Now(),
		}),
		envelope(t, events.OrderCancelled, "ord-2", "cust-1", events.OrderCancelledData{
			OrderID: "ord-2", CustomerID: "cust-1", Reason: "changed my mind",
		}),
	}
	for _, e := range all {
		if err := svc.Handle(ctx, e); err != nil {
			t.Fatalf("Handle(%s): %v", e.Type, err)
		}
	}

	inbox, err := svc.Inbox(ctx, "cust-1", 10)
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 3 {
		t.Fatalf("inbox holds %d messages, want 3", len(inbox))
	}
	// Newest first, so the most recent thing that happened is at the top.
	if inbox[0].EventType != string(events.OrderCancelled) {
		t.Errorf("first message = %q, want the most recent event", inbox[0].EventType)
	}
	for _, message := range inbox {
		if message.Subject == "" || message.Body == "" {
			t.Errorf("message %s has no subject or body", message.ID)
		}
	}
}

// Adding a new event type to the system must not break the services already
// listening, so an unrecognised one is ignored rather than failed.
func TestUnrecognisedEventsAreIgnored(t *testing.T) {
	svc := testService(t, 50)
	ctx := context.Background()

	if err := svc.Handle(ctx, events.Envelope{Type: "order.somethingNew", CustomerID: "cust-1"}); err != nil {
		t.Fatalf("an unrecognised event produced an error: %v", err)
	}

	inbox, _ := svc.Inbox(ctx, "cust-1", 10)
	if len(inbox) != 0 {
		t.Errorf("inbox holds %d messages, want 0", len(inbox))
	}
}

// A busy account must not grow an unbounded inbox.
func TestTheInboxIsCapped(t *testing.T) {
	const cap = 5
	svc := testService(t, cap)
	ctx := context.Background()

	for i := 0; i < cap*3; i++ {
		e := envelope(t, events.OrderPlaced, "ord", "cust-1", events.OrderPlacedData{
			OrderID: "ord", CustomerID: "cust-1", TotalPrice: float64(i),
		})
		if err := svc.Handle(ctx, e); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	inbox, _ := svc.Inbox(ctx, "cust-1", 100)
	if len(inbox) != cap {
		t.Errorf("inbox holds %d messages, want it capped at %d", len(inbox), cap)
	}
}

func TestInboxesAreScopedToTheirCustomer(t *testing.T) {
	svc := testService(t, 50)
	ctx := context.Background()

	e := envelope(t, events.OrderPlaced, "ord-1", "cust-alice", events.OrderPlacedData{
		OrderID: "ord-1", CustomerID: "cust-alice",
	})
	if err := svc.Handle(ctx, e); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	inbox, _ := svc.Inbox(ctx, "cust-bob", 10)
	if len(inbox) != 0 {
		t.Errorf("bob can see %d of alice's notifications", len(inbox))
	}
}

// An event with no recipient can never be delivered, so it is parked rather
// than redelivered forever.
func TestEventsWithNoRecipientAreUndeliverable(t *testing.T) {
	svc := testService(t, 50)

	e := envelope(t, events.OrderPlaced, "ord-1", "", events.OrderPlacedData{OrderID: "ord-1"})
	err := svc.Handle(context.Background(), e)
	if err == nil {
		t.Fatal("an event with no recipient was accepted")
	}
	if !errors.Is(err, events.ErrUndeliverable) {
		t.Errorf("err = %v, want it marked undeliverable so it is parked", err)
	}
}

func TestSubscribesToEveryOrderEvent(t *testing.T) {
	patterns := Patterns()
	if len(patterns) != 1 || patterns[0] != "order.*" {
		t.Errorf("patterns = %v, want a wildcard over order events", patterns)
	}
}
