// Package events defines the domain events services publish about themselves
// and the envelope they travel in.
//
// A domain event is a statement of fact about something that already happened:
// an order was placed, an order shipped. It is deliberately not a request. The
// publisher does not know or care who is listening, which is what lets a new
// subscriber — notifications, analytics, fraud scoring — be added without the
// publisher changing at all.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/orders"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
)

// Exchange is the topic exchange every domain event is published to.
const Exchange = "ecommerce.events"

// ErrUndeliverable marks an event a subscriber can never act on, so the
// consumer parks it rather than redelivering it forever.
var ErrUndeliverable = rmq.ErrDrop

// Type is an event's routing key. The dotted namespace lets a subscriber bind
// to one event, a family (order.*), or everything (#).
type Type string

// The events this system publishes.
const (
	// OrderPlaced is emitted by the cart service once a checkout has been
	// paid for and committed.
	OrderPlaced Type = "order.placed"
	// OrderShipped is emitted by the warehouse once goods have left.
	OrderShipped Type = "order.shipped"
	// OrderCancelled is emitted by the order service.
	OrderCancelled Type = "order.cancelled"
)

// Envelope wraps every event with the metadata a subscriber needs regardless
// of what the event says.
type Envelope struct {
	ID   string `json:"id"`
	Type Type   `json:"type"`

	OccurredAt time.Time `json:"occurredAt"`

	// CorrelationID carries the request id of whatever caused this event, so a
	// customer's click can be followed from the HTTP request through the queue
	// and into the services that reacted to it.
	CorrelationID string `json:"correlationId,omitempty"`

	OrderID    string `json:"orderId,omitempty"`
	CustomerID string `json:"customerId,omitempty"`

	Data json.RawMessage `json:"data,omitempty"`
}

// OrderPlacedData is the payload of an order.placed event.
type OrderPlacedData struct {
	OrderID       string        `json:"orderId"`
	CartID        string        `json:"cartId"`
	CustomerID    string        `json:"customerId"`
	Items         []orders.Item `json:"items"`
	TotalPrice    float64       `json:"totalPrice"`
	TotalWeight   float64       `json:"totalWeight"`
	ReservationID string        `json:"reservationId,omitempty"`
	PlacedAt      time.Time     `json:"placedAt"`
}

// OrderShippedData is the payload of an order.shipped event.
type OrderShippedData struct {
	OrderID    string        `json:"orderId"`
	CustomerID string        `json:"customerId"`
	Items      []orders.Item `json:"items"`
	Outcome    string        `json:"outcome"`
	ShippedAt  time.Time     `json:"shippedAt"`
}

// OrderCancelledData is the payload of an order.cancelled event.
type OrderCancelledData struct {
	OrderID     string    `json:"orderId"`
	CustomerID  string    `json:"customerId"`
	Reason      string    `json:"reason"`
	CancelledAt time.Time `json:"cancelledAt"`
}

// New builds an envelope, taking the correlation id from the calling context.
func New(ctx context.Context, typ Type, orderID, customerID string, data any) (Envelope, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("events: encoding %s payload: %w", typ, err)
	}

	return Envelope{
		ID:            newEventID(),
		Type:          typ,
		OccurredAt:    time.Now().UTC(),
		CorrelationID: obs.RequestIDFrom(ctx),
		OrderID:       orderID,
		CustomerID:    customerID,
		Data:          payload,
	}, nil
}

// Decode reads an envelope's payload into v.
func (e Envelope) Decode(v any) error {
	if len(e.Data) == 0 {
		return fmt.Errorf("events: %s envelope has no payload", e.Type)
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("events: decoding %s payload: %w", e.Type, err)
	}
	return nil
}

// Publisher publishes envelopes to the shared topic exchange.
type Publisher struct {
	inner *rmq.Publisher
}

// NewPublisher wraps an rmq topic publisher.
func NewPublisher(inner *rmq.Publisher) *Publisher { return &Publisher{inner: inner} }

// Publish sends an envelope, routed by its type.
func (p *Publisher) Publish(ctx context.Context, envelope Envelope) error {
	if p == nil || p.inner == nil {
		return nil
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("events: encoding envelope: %w", err)
	}
	return p.inner.PublishKey(ctx, string(envelope.Type), body)
}

// Emit builds and publishes an event in one step.
func (p *Publisher) Emit(ctx context.Context, typ Type, orderID, customerID string, data any) error {
	envelope, err := New(ctx, typ, orderID, customerID, data)
	if err != nil {
		return err
	}
	return p.Publish(ctx, envelope)
}

// Handler processes one decoded envelope.
type Handler func(ctx context.Context, envelope Envelope) error

// Subscribe adapts an envelope handler to the raw message handler the consumer
// expects, and restores the originating request id so a subscriber's logs join
// up with the request that caused the event.
func Subscribe(handler Handler) rmq.Handler {
	return func(ctx context.Context, body []byte) error {
		var envelope Envelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			// Redelivery cannot fix a body that does not parse.
			return fmt.Errorf("%w: malformed event envelope: %w", rmq.ErrDrop, err)
		}
		if envelope.Type == "" {
			return fmt.Errorf("%w: event %s has no type", rmq.ErrDrop, envelope.ID)
		}

		if envelope.CorrelationID != "" {
			ctx = obs.WithRequestID(ctx, envelope.CorrelationID)
		}
		return handler(ctx, envelope)
	}
}

// Router dispatches envelopes to a handler chosen by event type.
//
// A subscriber binds one queue to several routing keys and demultiplexes here,
// rather than running a queue and a consumer per event type. One queue means
// one backlog to watch and one ordering to reason about.
type Router map[Type]Handler

// Handle dispatches one envelope.
//
// An unrecognised type is not an error. A subscriber must tolerate events it
// does not care about, or adding a new event type to the system becomes a
// breaking change for everyone already listening.
func (r Router) Handle(ctx context.Context, envelope Envelope) error {
	handler, ok := r[envelope.Type]
	if !ok {
		return nil
	}
	return handler(ctx, envelope)
}

// Patterns lists the routing keys a Router needs bound to its queue.
func (r Router) Patterns() []string {
	patterns := make([]string, 0, len(r))
	for typ := range r {
		patterns = append(patterns, string(typ))
	}
	sort.Strings(patterns)
	return patterns
}
