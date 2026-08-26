// Package notification turns domain events into customer-facing messages.
//
// It exists mostly to demonstrate what the event bus buys: this service was
// added without changing a single line in the cart, warehouse or order
// services, because none of them knows or cares that anyone is listening.
package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
)

const inboxPrefix = "notifications:"

// Notification is one message addressed to a customer.
type Notification struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customerId"`
	OrderID    string    `json:"orderId,omitempty"`
	Channel    string    `json:"channel"`
	Subject    string    `json:"subject"`
	Body       string    `json:"body"`
	EventType  string    `json:"eventType"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Service records notifications in a per-customer inbox.
//
// The inbox lives in Redis rather than in the durable store because that is
// what it actually is: a capped, expiring view of recent activity. Nothing
// here is a system of record — the order service owns that — so paying for
// replication and durability would be paying for the wrong property.
type Service struct {
	rdb *redis.Client

	// inboxSize caps how many messages a customer's inbox keeps, so a busy
	// account cannot grow one without limit.
	inboxSize int64
	inboxTTL  time.Duration
}

// NewService returns a Service backed by Redis.
func NewService(rdb *redis.Client, inboxSize int, inboxTTL time.Duration) *Service {
	if inboxSize <= 0 {
		inboxSize = 50
	}
	if inboxTTL <= 0 {
		inboxTTL = 30 * 24 * time.Hour
	}
	return &Service{rdb: rdb, inboxSize: int64(inboxSize), inboxTTL: inboxTTL}
}

// Handle turns one event into a notification.
func (s *Service) Handle(ctx context.Context, envelope events.Envelope) error {
	notification, ok := s.render(envelope)
	if !ok {
		// An event this service has no message for is not a failure. Silently
		// ignoring it is what lets new event types be introduced without
		// breaking every existing subscriber.
		slog.DebugContext(ctx, "no notification for this event", "type", envelope.Type)
		return nil
	}

	if err := s.deliver(ctx, notification); err != nil {
		return err
	}

	obs.ObserveBusinessEvent("notification", string(envelope.Type))
	slog.InfoContext(ctx, "notification sent",
		"customerId", notification.CustomerID,
		"orderId", notification.OrderID,
		"subject", notification.Subject,
	)
	return nil
}

func (s *Service) render(envelope events.Envelope) (*Notification, bool) {
	base := Notification{
		ID:         envelope.ID,
		CustomerID: envelope.CustomerID,
		OrderID:    envelope.OrderID,
		Channel:    "email",
		EventType:  string(envelope.Type),
		CreatedAt:  envelope.OccurredAt,
	}

	switch envelope.Type {
	case events.OrderPlaced:
		var data events.OrderPlacedData
		if err := envelope.Decode(&data); err != nil {
			return nil, false
		}
		base.Subject = fmt.Sprintf("Order %s confirmed", short(data.OrderID))
		base.Body = fmt.Sprintf("Thanks for your order. %d item(s), %.2f total. We will let you know when it ships.",
			len(data.Items), data.TotalPrice)
		return &base, true

	case events.OrderShipped:
		var data events.OrderShippedData
		if err := envelope.Decode(&data); err != nil {
			return nil, false
		}
		base.Subject = fmt.Sprintf("Order %s is on its way", short(data.OrderID))
		base.Body = fmt.Sprintf("%d item(s) have left the warehouse.", len(data.Items))
		return &base, true

	case events.OrderCancelled:
		var data events.OrderCancelledData
		if err := envelope.Decode(&data); err != nil {
			return nil, false
		}
		reason := data.Reason
		if reason == "" {
			reason = "no reason given"
		}
		base.Subject = fmt.Sprintf("Order %s cancelled", short(data.OrderID))
		base.Body = fmt.Sprintf("Your order has been cancelled (%s). Any hold on your card will be released.", reason)
		return &base, true

	default:
		return nil, false
	}
}

func (s *Service) deliver(ctx context.Context, notification *Notification) error {
	if notification.CustomerID == "" {
		return fmt.Errorf("%w: notification has no recipient", events.ErrUndeliverable)
	}

	raw, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("%w: encoding notification: %w", events.ErrUndeliverable, err)
	}

	key := inboxPrefix + notification.CustomerID

	// Pushing, trimming and expiring in one pipeline: three round trips
	// per notification would triple the cost of the busiest path in this
	// service for no benefit.
	pipe := s.rdb.TxPipeline()
	pipe.LPush(ctx, key, raw)
	pipe.LTrim(ctx, key, 0, s.inboxSize-1)
	pipe.Expire(ctx, key, s.inboxTTL)

	if _, err := pipe.Exec(ctx); err != nil {
		// Returned rather than swallowed: the message is still on the queue,
		// so it will be redelivered once Redis is back.
		return fmt.Errorf("delivering notification: %w", err)
	}
	return nil
}

// Inbox returns a customer's recent notifications, newest first.
func (s *Service) Inbox(ctx context.Context, customerID string, limit int) ([]*Notification, error) {
	if customerID == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "customerId is required")
	}
	if limit <= 0 || int64(limit) > s.inboxSize {
		limit = int(s.inboxSize)
	}

	raw, err := s.rdb.LRange(ctx, inboxPrefix+customerID, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "notification store unavailable")
	}

	out := make([]*Notification, 0, len(raw))
	for _, item := range raw {
		var notification Notification
		if err := json.Unmarshal([]byte(item), &notification); err != nil {
			continue // one bad row must not hide the rest of the inbox
		}
		out = append(out, &notification)
	}
	return out, nil
}

func short(orderID string) string {
	if len(orderID) <= 12 {
		return orderID
	}
	return orderID[len(orderID)-12:]
}
