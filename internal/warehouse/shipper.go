package warehouse

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/orders"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
)

// Shipper applies ship messages from the queue to the inventory, then tells
// the rest of the system what happened.
type Shipper struct {
	inv    *Inventory
	events *events.Publisher
}

// NewShipper returns a Shipper writing to inv. A nil publisher disables the
// announcement, which keeps the warehouse usable without an event bus.
func NewShipper(inv *Inventory, eventBus *events.Publisher) *Shipper {
	return &Shipper{inv: inv, events: eventBus}
}

// Handle processes one ship message. Errors wrapping rmq.ErrDrop tell the
// consumer the message is unprocessable and must not be requeued.
func (s *Shipper) Handle(ctx context.Context, body []byte) error {
	var msg orders.ShipMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		// Redelivering this will produce the same parse error forever.
		return fmt.Errorf("%w: malformed ship message: %w", rmq.ErrDrop, err)
	}
	if msg.ReservationID == "" && len(msg.Items) == 0 {
		return fmt.Errorf("%w: ship message %q has neither a reservation nor items",
			rmq.ErrDrop, msg.OrderID)
	}

	outcome := s.inv.Ship(msg.ReservationID, msg.Items)

	slog.InfoContext(ctx, "order shipped",
		"orderId", msg.OrderID,
		"cartId", msg.CartID,
		"reservationId", msg.ReservationID,
		"items", len(msg.Items),
		"outcome", outcome,
	)

	s.announce(ctx, msg, outcome)
	return nil
}

// announce publishes order.shipped.
//
// The inventory has already been updated, so a failure here must not cause a
// redelivery: reapplying the ship message would deduct the stock a second
// time. The event is lost, the ledger stays correct, and the log says so.
func (s *Shipper) announce(ctx context.Context, msg orders.ShipMessage, outcome ShipOutcome) {
	if s.events == nil || msg.CustomerID == "" {
		return
	}

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	err := s.events.Emit(pubCtx, events.OrderShipped, msg.OrderID, msg.CustomerID,
		events.OrderShippedData{
			OrderID:    msg.OrderID,
			CustomerID: msg.CustomerID,
			Items:      msg.Items,
			Outcome:    string(outcome),
			ShippedAt:  time.Now().UTC(),
		})
	if err != nil {
		slog.ErrorContext(ctx, "could not announce the shipment",
			"orderId", msg.OrderID, "err", err)
	}
}
