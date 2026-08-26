package cart

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/cca"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/orders"
	"github.com/JDinSeattle/quorum-market/internal/warehouse"
)

// Publisher hands a committed order to the shipping queue.
//
// It is an interface rather than *rmq.Publisher so the checkout flow can be
// tested against a real database, warehouse and authorizer without also
// needing a live broker.
type Publisher interface {
	Publish(ctx context.Context, body []byte) error
}

// CheckoutRequest is the body of POST /shopping-cart/{cartId}/checkout.
type CheckoutRequest struct {
	CartID     string `json:"cartId,omitempty"`
	CreditCard string `json:"creditCard"`

	// IdempotencyKey makes the request safe to retry. It is normally supplied
	// in the Idempotency-Key header; the body field exists for clients that
	// cannot set headers. The header wins when both are present.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// Order is the record written to the store when a checkout succeeds.
type Order struct {
	OrderID           string    `json:"orderId"`
	CartID            string    `json:"cartId"`
	CustomerID        string    `json:"customerId"`
	Items             []Item    `json:"items"`
	TotalWeight       float64   `json:"totalWeight"`
	TotalPrice        float64   `json:"totalPrice"`
	ReservationID     string    `json:"reservationId"`
	AuthorizationCode string    `json:"authorizationCode"`
	Status            string    `json:"status"`
	PlacedAt          time.Time `json:"placedAt"`
}

// Receipt is what the customer gets back.
type Receipt struct {
	OrderID     string  `json:"orderId"`
	Status      string  `json:"status"`
	TotalPrice  float64 `json:"totalPrice"`
	TotalWeight float64 `json:"totalWeight"`
}

// CheckoutService runs the checkout use case.
//
// Transaction boundary:
//
//	begin_transaction
//	  ├─ reserve inventory ──── short ──► abort ──► 409 out of stock
//	  ├─ authorize card ─────── decline ─► abort + release ──► 402 declined
//	  ├─ write order, clear cart
//	end_transaction
//	  └─ publish ship message      (deliberately outside the boundary)
//
// The ship message is published after the commit, not inside it. Publishing
// first would risk shipping an order that then fails to commit, and there is
// no way to un-ship. Publishing after means the worst case is an order that is
// paid for and recorded but not yet queued — recoverable, and visible in the
// logs — rather than goods that left the warehouse for an order that does not
// exist.
type CheckoutService struct {
	carts     *Service
	warehouse *warehouse.Client
	cards     *cca.Client
	db        *kv.Client
	publisher Publisher
	events    *events.Publisher
	delay     busywait.Config
	idem      *idempotencyStore

	cleanupTimeout time.Duration
}

// NewCheckoutService wires the checkout flow to its dependencies.
func NewCheckoutService(carts *Service, wh *warehouse.Client, cards *cca.Client,
	db *kv.Client, publisher Publisher, eventBus *events.Publisher,
	delay busywait.Config) *CheckoutService {

	return &CheckoutService{
		carts:          carts,
		warehouse:      wh,
		cards:          cards,
		db:             db,
		publisher:      publisher,
		events:         eventBus,
		delay:          delay,
		idem:           newIdempotencyStore(db),
		cleanupTimeout: 5 * time.Second,
	}
}

// Checkout reserves stock, takes payment, commits the order, and queues it for
// shipping.
//
// When the request carries an idempotency key, a repeat of a completed
// checkout replays the original receipt instead of charging the card again.
func (s *CheckoutService) Checkout(ctx context.Context, cartID string, req CheckoutRequest) (Receipt, error) {
	if req.CreditCard == "" {
		return Receipt{}, httpx.Errorf(http.StatusBadRequest, "creditCard is required")
	}

	customerID, err := CustomerFor(cartID)
	if err != nil {
		return Receipt{}, err
	}

	// Claimed before anything is reserved or charged, so a retry that arrives
	// while the first attempt is still running is turned away rather than
	// running a second checkout alongside it.
	claimed := false
	if req.IdempotencyKey != "" {
		if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
			return Receipt{}, err
		}
		replay, err := s.idem.claim(ctx, req.IdempotencyKey, cartID)
		if err != nil {
			return Receipt{}, err
		}
		if replay != nil {
			obs.ObserveBusinessEvent("checkout", "replayed")
			slog.InfoContext(ctx, "replaying a completed checkout",
				"key", req.IdempotencyKey, "orderId", replay.OrderID)
			return *replay, nil
		}
		claimed = true
	}

	cart, err := s.carts.Get(ctx, customerID)
	if err != nil {
		s.releaseClaim(ctx, claimed, req.IdempotencyKey, cartID)
		return Receipt{}, err
	}
	if cart == nil || cart.IsEmpty() {
		s.releaseClaim(ctx, claimed, req.IdempotencyKey, cartID)
		obs.ObserveBusinessEvent("checkout", "empty_cart")
		return Receipt{}, httpx.Errorf(http.StatusBadRequest, "cart %s is empty", cartID)
	}

	order := &Order{
		OrderID:     newOrderID(),
		CartID:      cartID,
		CustomerID:  customerID,
		Items:       cart.Items,
		TotalWeight: cart.TotalWeight(),
		TotalPrice:  cart.TotalPrice(),
		Status:      "confirmed",
		PlacedAt:    time.Now().UTC(),
	}

	// ── begin_transaction ────────────────────────────────────────────────
	txnID, err := s.db.BeginTxn(ctx)
	if err != nil {
		s.releaseClaim(ctx, claimed, req.IdempotencyKey, cartID)
		return Receipt{}, httpx.Wrap(http.StatusServiceUnavailable, err, "could not begin transaction")
	}

	// Unwinding is centralised here so no early return can leak a held
	// reservation or an open transaction. It runs on a context detached from
	// the request: if the customer's connection dropped, the stock they were
	// holding still has to be given back.
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
		defer cancel()

		if order.ReservationID != "" {
			if relErr := s.warehouse.Release(cleanupCtx, order.ReservationID); relErr != nil {
				// The reservation TTL is the backstop: even if this fails the
				// stock returns on its own once the hold expires.
				slog.ErrorContext(ctx, "could not release reservation; leaving it to expire",
					"reservationId", order.ReservationID, "err", relErr)
			}
		}
		if abortErr := s.db.AbortTxn(cleanupCtx, txnID); abortErr != nil {
			slog.ErrorContext(ctx, "could not abort transaction", "txnId", txnID, "err", abortErr)
		}
		// Only completed checkouts are worth replaying. A failure has no
		// charge and no order behind it, so the key is given back and the
		// customer can fix their card and retry with it.
		if claimed {
			s.idem.release(cleanupCtx, req.IdempotencyKey, cartID)
		}
	}()

	// ── step 1: hold the stock ───────────────────────────────────────────
	reservation, err := s.warehouse.Reserve(ctx, cart.OrderItems())
	if err != nil {
		var apiErr *httpx.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			obs.ObserveBusinessEvent("checkout", "out_of_stock")
			slog.InfoContext(ctx, "checkout aborted: out of stock", "txnId", txnID, "cartId", cartID)
			return Receipt{}, httpx.Errorf(http.StatusConflict, "one or more items are out of stock")
		}
		obs.ObserveBusinessEvent("checkout", "warehouse_unavailable")
		return Receipt{}, httpx.Wrap(http.StatusServiceUnavailable, err, "warehouse unavailable")
	}
	order.ReservationID = reservation.ReservationID

	// ── step 2: take the money ───────────────────────────────────────────
	decision, authCode, authErr := s.cards.Authorize(ctx, req.CreditCard, order.TotalPrice, order.OrderID)
	switch decision {
	case cca.Approved:
		order.AuthorizationCode = authCode
	case cca.Declined:
		obs.ObserveBusinessEvent("checkout", "declined")
		slog.InfoContext(ctx, "checkout aborted: card declined", "txnId", txnID, "cartId", cartID)
		return Receipt{}, httpx.Errorf(http.StatusPaymentRequired, "credit card declined")
	case cca.InvalidCard:
		obs.ObserveBusinessEvent("checkout", "invalid_card")
		return Receipt{}, httpx.Errorf(http.StatusBadRequest, "credit card number is not valid")
	default:
		obs.ObserveBusinessEvent("checkout", "payment_unavailable")
		return Receipt{}, httpx.Wrap(http.StatusServiceUnavailable, authErr, "payment system unavailable")
	}

	// ── step 3: commit the order ─────────────────────────────────────────
	if err := s.writeOrder(ctx, order); err != nil {
		return Receipt{}, err
	}
	// Clearing the cart is best-effort: the money is taken and the order is
	// recorded, so a stale cart is not worth failing the checkout over.
	_ = s.carts.Clear(ctx, customerID)

	// ── end_transaction ──────────────────────────────────────────────────
	if err := s.db.CommitTxn(ctx, txnID); err != nil {
		return Receipt{}, httpx.Wrap(http.StatusInternalServerError, err, "could not commit transaction")
	}
	committed = true
	obs.ObserveBusinessEvent("checkout", "confirmed")
	slog.InfoContext(ctx, "checkout committed",
		"txnId", txnID, "orderId", order.OrderID, "total", order.TotalPrice)

	receipt := Receipt{
		OrderID:     order.OrderID,
		Status:      order.Status,
		TotalPrice:  order.TotalPrice,
		TotalWeight: order.TotalWeight,
	}

	// Recorded before the response is written. If the customer never receives
	// it, their retry replays this receipt instead of buying the cart again.
	if claimed {
		s.idem.complete(ctx, req.IdempotencyKey, cartID, receipt)
	}

	s.delay.Simulate()

	// ── after the boundary: tell the rest of the system ──────────────────
	//
	// Two different things, deliberately. The ship message is a command with
	// exactly one correct handler, so it goes on a work queue. The event is a
	// statement of fact that any number of services may care about, so it goes
	// on the topic exchange — and the order and notification services were
	// both added later without this code changing.
	// Announced before the ship command is queued. Nothing guarantees the two
	// arrive in order — different publishers, different paths — and the
	// subscribers are written to tolerate either, but publishing the
	// placement first makes the ordinary case the ordered one.
	s.announcePlaced(ctx, order)
	s.queueShipment(ctx, order)

	return receipt, nil
}

// announcePlaced publishes the order.placed event.
//
// Like the ship message, a failure here is logged rather than returned: the
// order is committed and paid for, and no downstream subscriber's problem is
// worth telling the customer their purchase failed.
func (s *CheckoutService) announcePlaced(ctx context.Context, order *Order) {
	if s.events == nil {
		return
	}

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()

	err := s.events.Emit(pubCtx, events.OrderPlaced, order.OrderID, order.CustomerID,
		events.OrderPlacedData{
			OrderID:       order.OrderID,
			CartID:        order.CartID,
			CustomerID:    order.CustomerID,
			Items:         orderItems(order.Items),
			TotalPrice:    order.TotalPrice,
			TotalWeight:   order.TotalWeight,
			ReservationID: order.ReservationID,
			PlacedAt:      order.PlacedAt,
		})
	if err != nil {
		slog.ErrorContext(ctx, "could not announce the placed order",
			"orderId", order.OrderID, "err", err)
	}
}

// releaseClaim gives an idempotency key back when the checkout is rejected
// before the transaction — and therefore before the deferred unwind — begins.
func (s *CheckoutService) releaseClaim(ctx context.Context, claimed bool, key, cartID string) {
	if !claimed {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	s.idem.release(releaseCtx, key, cartID)
}

func (s *CheckoutService) writeOrder(ctx context.Context, order *Order) error {
	raw, err := json.Marshal(order)
	if err != nil {
		return httpx.Wrap(http.StatusInternalServerError, err, "encoding order %s", order.OrderID)
	}
	if _, err := s.db.Put(ctx, OrderKeyPrefix+order.OrderID, string(raw)); err != nil {
		return httpx.Wrap(http.StatusServiceUnavailable, err, "could not commit order %s", order.OrderID)
	}
	return nil
}

// queueShipment publishes the ship message. A failure here is logged, not
// returned: the customer's order is already paid for and committed, and
// telling them it failed would be a lie. In production this is where a retry
// queue or an outbox sweeper would pick the order back up; the reservation is
// what keeps the stock accounted for until then.
func (s *CheckoutService) queueShipment(ctx context.Context, order *Order) {
	msg := orders.ShipMessage{
		OrderID:       order.OrderID,
		CartID:        order.CartID,
		CustomerID:    order.CustomerID,
		ReservationID: order.ReservationID,
		Items:         orderItems(order.Items),
		TotalWeight:   order.TotalWeight,
		TotalPrice:    order.TotalPrice,
		PlacedAt:      order.PlacedAt,
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		slog.Error("could not encode ship message", "orderId", order.OrderID, "err", err)
		return
	}

	// Detached from the request: the customer's response does not depend on
	// this, and a client that hangs up must not cancel the shipment.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()

	if err := s.publisher.Publish(pubCtx, raw); err != nil {
		slog.Error("could not queue shipment", "orderId", order.OrderID,
			"reservationId", order.ReservationID, "err", err)
		return
	}
	slog.Info("shipment queued", "orderId", order.OrderID)
}

func orderItems(items []Item) []orders.Item {
	out := make([]orders.Item, 0, len(items))
	for _, item := range items {
		out = append(out, item.Order())
	}
	return out
}

// newOrderID returns a time-ordered, collision-resistant order id. The
// millisecond prefix makes ids sort chronologically in the store; the random
// suffix keeps two checkouts in the same millisecond apart.
func newOrderID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ord-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("ord-%d-%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(b[:]))
}
