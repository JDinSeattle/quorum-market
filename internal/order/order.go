// Package order owns the lifecycle of a placed order.
//
// The cart service is responsible for turning a cart into a paid order and
// then forgets about it. Everything afterwards — what state the order is in,
// what a customer's history looks like, whether it can still be cancelled —
// belongs here, and is driven by events rather than by anyone calling in.
package order

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

// KeyPrefix namespaces order records.
//
// The customer id is part of the key so one customer's orders are a single
// prefix scan. Without that the service would need a secondary index, and
// maintaining one by read-modify-write over an eventually consistent store is
// exactly how order history quietly loses rows.
const KeyPrefix = "order:"

// Status is where an order is in its life.
type Status string

// The states an order moves through.
const (
	StatusPlaced    Status = "placed"
	StatusShipped   Status = "shipped"
	StatusCancelled Status = "cancelled"
)

// Order is the record this service owns.
type Order struct {
	OrderID     string        `json:"orderId"`
	CustomerID  string        `json:"customerId"`
	CartID      string        `json:"cartId,omitempty"`
	Status      Status        `json:"status"`
	Items       []orders.Item `json:"items"`
	TotalPrice  float64       `json:"totalPrice"`
	TotalWeight float64       `json:"totalWeight"`

	PlacedAt    time.Time  `json:"placedAt"`
	ShippedAt   *time.Time `json:"shippedAt,omitempty"`
	CancelledAt *time.Time `json:"cancelledAt,omitempty"`

	CancelReason string `json:"cancelReason,omitempty"`

	// UpdatedAt orders the versions of a record that arrive out of sequence.
	UpdatedAt time.Time `json:"updatedAt"`

	// AwaitingDetails is true while a shipment has been recorded but its
	// placement event has not arrived. Surfaced rather than hidden: a caller
	// seeing an order with no line items deserves to know why.
	AwaitingDetails bool `json:"awaitingDetails,omitempty"`
}

// Cancellable reports whether the order can still be called off. Once goods
// have shipped the answer is no; that is a returns problem, not a cancellation.
func (o *Order) Cancellable() bool { return o.Status == StatusPlaced }

// HasDetails reports whether the placement event has been applied.
//
// A record can exist without them: a shipment event that overtook its
// placement event creates a stub, and this is what distinguishes that stub
// from a fully recorded order.
func (o *Order) HasDetails() bool { return !o.PlacedAt.IsZero() }

// Service maintains order records from the events it receives.
type Service struct {
	db     *kv.Client
	events *events.Publisher
}

// NewService wires the order service to its store and the event bus.
func NewService(db *kv.Client, publisher *events.Publisher) *Service {
	return &Service{db: db, events: publisher}
}

// Key returns the storage key for one order.
func Key(customerID, orderID string) string {
	return KeyPrefix + customerID + ":" + orderID
}

// CustomerPrefix returns the scan prefix covering a customer's orders.
func CustomerPrefix(customerID string) string { return KeyPrefix + customerID + ":" }

// HandlePlaced records the details of an order.
//
// This and HandleShipped are deliberately commutative: either may arrive
// first, and the result is the same. They are published by different services
// over different paths, so nothing orders them, and an earlier version of this
// code that required placed-then-shipped ordering lost shipment events to the
// dead-letter queue whenever the two raced.
//
// The rule is that this handler owns the *details* and never the *status*.
func (s *Service) HandlePlaced(ctx context.Context, envelope events.Envelope) error {
	var data events.OrderPlacedData
	if err := envelope.Decode(&data); err != nil {
		return err
	}
	if data.OrderID == "" || data.CustomerID == "" {
		return errors.New("order.placed event is missing an order or customer id")
	}

	record, err := s.load(ctx, data.CustomerID, data.OrderID)
	if err != nil {
		return err
	}

	switch {
	case record == nil:
		// Nothing known yet: this is the ordinary case.
		record = &Order{OrderID: data.OrderID, CustomerID: data.CustomerID, Status: StatusPlaced}

	case record.HasDetails():
		// Already fully recorded. Redelivery is normal — the broker
		// guarantees at-least-once — so seeing this again is not an error.
		slog.DebugContext(ctx, "ignoring a duplicate order.placed", "orderId", data.OrderID)
		return nil

		// Otherwise a shipment event created the record first and this fills
		// in what it could not know. The status it set is left alone.
	}

	record.CartID = data.CartID
	record.Items = data.Items
	record.TotalPrice = data.TotalPrice
	record.TotalWeight = data.TotalWeight
	record.PlacedAt = data.PlacedAt
	record.UpdatedAt = time.Now().UTC()

	if err := s.save(ctx, record); err != nil {
		return err
	}

	obs.ObserveBusinessEvent("order", "placed")
	slog.InfoContext(ctx, "order recorded",
		"orderId", record.OrderID, "customerId", record.CustomerID,
		"total", record.TotalPrice, "status", record.Status)
	return nil
}

// HandleShipped advances an order to shipped.
func (s *Service) HandleShipped(ctx context.Context, envelope events.Envelope) error {
	var data events.OrderShippedData
	if err := envelope.Decode(&data); err != nil {
		return err
	}
	if data.OrderID == "" || data.CustomerID == "" {
		return errors.New("order.shipped event is missing an order or customer id")
	}

	record, err := s.load(ctx, data.CustomerID, data.OrderID)
	if err != nil {
		return err
	}
	if record == nil {
		// The shipment event overtook the placement event. Record what is
		// known now rather than requeueing and hoping: the two are published
		// by different services, so no amount of retrying makes their order
		// guaranteed. HandlePlaced fills in the details when it arrives.
		slog.InfoContext(ctx, "recording a shipment before its order.placed arrived",
			"orderId", data.OrderID)
		record = &Order{OrderID: data.OrderID, CustomerID: data.CustomerID, Items: data.Items}
	}
	if record.Status == StatusShipped {
		return nil // already applied
	}

	shippedAt := data.ShippedAt
	record.Status = StatusShipped
	record.ShippedAt = &shippedAt
	record.UpdatedAt = time.Now().UTC()

	if err := s.save(ctx, record); err != nil {
		return err
	}

	obs.ObserveBusinessEvent("order", "shipped")
	slog.InfoContext(ctx, "order marked shipped", "orderId", record.OrderID)
	return nil
}

// Get returns one of a customer's orders.
func (s *Service) Get(ctx context.Context, customerID, orderID string) (*Order, error) {
	if customerID == "" || orderID == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "customerId and orderId are required")
	}

	record, err := s.load(ctx, customerID, orderID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		// Deliberately the same answer as an order belonging to someone else:
		// distinguishing them would let a caller probe for other customers'
		// order ids.
		return nil, httpx.Errorf(http.StatusNotFound, "order %s not found", orderID)
	}
	return record, nil
}

// List returns a customer's orders, newest first.
func (s *Service) List(ctx context.Context, customerID string, limit int) ([]*Order, error) {
	if customerID == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "customerId is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	entries, err := s.db.Scan(ctx, CustomerPrefix(customerID), limit)
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "order database unavailable")
	}

	out := make([]*Order, 0, len(entries))
	for _, entry := range entries {
		var record Order
		if err := json.Unmarshal([]byte(entry.Value), &record); err != nil {
			// One unreadable row must not hide the rest of a customer's history.
			slog.ErrorContext(ctx, "skipping an unreadable order record", "key", entry.Key, "err", err)
			continue
		}
		out = append(out, &record)
	}

	// Order ids embed their placement time, so the keys sort chronologically
	// and reversing them is enough to get newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Cancel calls off an order that has not shipped.
func (s *Service) Cancel(ctx context.Context, customerID, orderID, reason string) (*Order, error) {
	record, err := s.Get(ctx, customerID, orderID)
	if err != nil {
		return nil, err
	}
	if record.Status == StatusCancelled {
		return record, nil // already cancelled; saying so again is not an error
	}
	if !record.Cancellable() {
		return nil, httpx.Errorf(http.StatusConflict,
			"order %s has already %s and can no longer be cancelled", orderID, record.Status)
	}

	now := time.Now().UTC()
	record.Status = StatusCancelled
	record.CancelledAt = &now
	record.CancelReason = strings.TrimSpace(reason)
	record.UpdatedAt = now

	if err := s.save(ctx, record); err != nil {
		return nil, err
	}

	// Announced rather than acted on directly: this service does not know who
	// needs to react — the warehouse might restock, notifications might send a
	// confirmation — and it should not have to.
	err = s.events.Emit(ctx, events.OrderCancelled, record.OrderID, record.CustomerID,
		events.OrderCancelledData{
			OrderID:     record.OrderID,
			CustomerID:  record.CustomerID,
			Reason:      record.CancelReason,
			CancelledAt: now,
		})
	if err != nil {
		// The cancellation is already durable; failing the request now would
		// tell the customer it did not happen.
		slog.ErrorContext(ctx, "could not announce the cancellation",
			"orderId", record.OrderID, "err", err)
	}

	obs.ObserveBusinessEvent("order", "cancelled")
	return record, nil
}

func (s *Service) load(ctx context.Context, customerID, orderID string) (*Order, error) {
	entry, found, err := s.db.Get(ctx, Key(customerID, orderID))
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "order database unavailable")
	}
	if !found {
		return nil, nil
	}

	var record Order
	if err := json.Unmarshal([]byte(entry.Value), &record); err != nil {
		return nil, httpx.Wrap(http.StatusInternalServerError, err, "order record is unreadable")
	}
	return &record, nil
}

func (s *Service) save(ctx context.Context, record *Order) error {
	record.AwaitingDetails = !record.HasDetails()

	raw, err := json.Marshal(record)
	if err != nil {
		return httpx.Wrap(http.StatusInternalServerError, err, "encoding order %s", record.OrderID)
	}
	if _, err := s.db.Put(ctx, Key(record.CustomerID, record.OrderID), string(raw)); err != nil {
		return httpx.Wrap(http.StatusServiceUnavailable, err, "order database unavailable")
	}
	return nil
}
