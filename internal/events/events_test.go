package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/orders"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	ctx := obs.WithRequestID(context.Background(), "req-abc")

	payload := OrderPlacedData{
		OrderID:    "ord-1",
		CustomerID: "cust-1",
		Items:      []orders.Item{{ProductID: "p1", Quantity: 2}},
		TotalPrice: 19.98,
		PlacedAt:   time.Now().UTC().Truncate(time.Second),
	}

	envelope, err := New(ctx, OrderPlaced, "ord-1", "cust-1", payload)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if envelope.ID == "" {
		t.Error("the envelope has no id, so a redelivery cannot be recognised")
	}
	// The request id travels with the event, so a customer's click can be
	// followed through the queue into whatever reacted to it.
	if envelope.CorrelationID != "req-abc" {
		t.Errorf("correlation id = %q, want req-abc", envelope.CorrelationID)
	}

	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	var got OrderPlacedData
	if err := decoded.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.OrderID != payload.OrderID || got.TotalPrice != payload.TotalPrice {
		t.Errorf("payload = %+v, want %+v", got, payload)
	}
}

func TestDecodeRejectsAnEmptyPayload(t *testing.T) {
	var target OrderPlacedData
	if err := (Envelope{Type: OrderPlaced}).Decode(&target); err == nil {
		t.Error("an envelope with no payload decoded successfully")
	}
}

func TestSubscribeRestoresTheCorrelationID(t *testing.T) {
	var seen string
	handler := Subscribe(func(ctx context.Context, _ Envelope) error {
		seen = obs.RequestIDFrom(ctx)
		return nil
	})

	envelope, _ := New(obs.WithRequestID(context.Background(), "req-xyz"),
		OrderShipped, "ord-1", "cust-1", OrderShippedData{OrderID: "ord-1"})
	raw, _ := json.Marshal(envelope)

	if err := handler(context.Background(), raw); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if seen != "req-xyz" {
		t.Errorf("the subscriber saw request id %q, want req-xyz", seen)
	}
}

// A body that will never parse must be parked, not redelivered forever.
func TestMalformedEventsAreDropped(t *testing.T) {
	handler := Subscribe(func(context.Context, Envelope) error { return nil })

	for _, body := range [][]byte{[]byte("{not json"), []byte(`{"id":"e1"}`)} {
		err := handler(context.Background(), body)
		if !errors.Is(err, rmq.ErrDrop) {
			t.Errorf("body %q: err = %v, want it marked undeliverable", body, err)
		}
	}
}

// Adding a new event type must not break every service already listening, so
// a subscriber has to ignore what it does not recognise.
func TestARouterIgnoresUnknownEventTypes(t *testing.T) {
	called := false
	router := Router{
		OrderPlaced: func(context.Context, Envelope) error {
			called = true
			return nil
		},
	}

	err := router.Handle(context.Background(), Envelope{Type: "order.somethingNew"})
	if err != nil {
		t.Fatalf("an unknown event type produced an error: %v", err)
	}
	if called {
		t.Error("the wrong handler ran")
	}
}

func TestARouterDispatchesByType(t *testing.T) {
	var placed, shipped int
	router := Router{
		OrderPlaced:  func(context.Context, Envelope) error { placed++; return nil },
		OrderShipped: func(context.Context, Envelope) error { shipped++; return nil },
	}

	ctx := context.Background()
	_ = router.Handle(ctx, Envelope{Type: OrderPlaced})
	_ = router.Handle(ctx, Envelope{Type: OrderPlaced})
	_ = router.Handle(ctx, Envelope{Type: OrderShipped})

	if placed != 2 || shipped != 1 {
		t.Errorf("placed = %d, shipped = %d; want 2 and 1", placed, shipped)
	}
}

func TestRouterPatternsAreStableAndComplete(t *testing.T) {
	router := Router{
		OrderShipped: func(context.Context, Envelope) error { return nil },
		OrderPlaced:  func(context.Context, Envelope) error { return nil },
	}

	patterns := router.Patterns()
	if len(patterns) != 2 {
		t.Fatalf("patterns = %v, want one per handled type", patterns)
	}
	// Sorted, so a topology declared from these does not churn between runs
	// purely because Go randomises map iteration.
	if patterns[0] != string(OrderPlaced) || patterns[1] != string(OrderShipped) {
		t.Errorf("patterns = %v, want them sorted", patterns)
	}
}

func TestHandlerErrorsPropagateForRedelivery(t *testing.T) {
	boom := errors.New("the database is down")
	handler := Subscribe(func(context.Context, Envelope) error { return boom })

	envelope, _ := New(context.Background(), OrderPlaced, "ord-1", "cust-1",
		OrderPlacedData{OrderID: "ord-1"})
	raw, _ := json.Marshal(envelope)

	err := handler(context.Background(), raw)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the handler's error so the message is retried", err)
	}
	if errors.Is(err, rmq.ErrDrop) {
		t.Error("a transient failure was marked undeliverable and would be discarded")
	}
}

// A nil publisher has to be a no-op, so a service can run without an event bus.
func TestANilPublisherIsHarmless(t *testing.T) {
	var p *Publisher
	if err := p.Emit(context.Background(), OrderPlaced, "ord-1", "cust-1", OrderPlacedData{}); err != nil {
		t.Errorf("Emit on a nil publisher returned %v", err)
	}
}
