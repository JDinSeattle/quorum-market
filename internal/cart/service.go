package cart

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/product"
	"github.com/JDinSeattle/quorum-market/internal/warehouse"
)

// AddItemRequest is the body of POST /shopping-cart/{cartId}/add-item.
type AddItemRequest struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

// Service owns cart state and the add-to-cart use case.
type Service struct {
	products  *product.Client
	warehouse *warehouse.Client
	db        *kv.Client
	delay     busywait.Config
}

// NewService wires the cart service to its dependencies.
func NewService(products *product.Client, wh *warehouse.Client, db *kv.Client, delay busywait.Config) *Service {
	return &Service{products: products, warehouse: wh, db: db, delay: delay}
}

// Get loads a customer's cart, returning (nil, nil) when there is none.
func (s *Service) Get(ctx context.Context, customerID string) (*Cart, error) {
	entry, found, err := s.db.Get(ctx, IDFor(customerID))
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "cart database unavailable")
	}
	if !found {
		return nil, nil
	}

	var c Cart
	if err := json.Unmarshal([]byte(entry.Value), &c); err != nil {
		return nil, httpx.Wrap(http.StatusInternalServerError, err,
			"cart for customer %s is stored in an unreadable format", customerID)
	}
	return &c, nil
}

// AddItem prices an item against the catalogue, confirms the warehouse has
// enough of it, and merges it into the customer's cart.
//
// The availability check is deliberately soft: it reads stock without holding
// any. Holding stock for a browsing customer would let anyone empty the
// catalogue by filling a cart they never intend to buy. The binding check
// happens once, at checkout.
func (s *Service) AddItem(ctx context.Context, customerID string, req AddItemRequest) (*Cart, error) {
	if req.ProductID == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "productId is required")
	}
	if req.Quantity <= 0 {
		return nil, httpx.Errorf(http.StatusBadRequest, "quantity must be greater than zero")
	}

	p, err := s.products.Get(ctx, req.ProductID)
	if err != nil {
		return nil, translateProductError(req.ProductID, err)
	}

	available, err := s.warehouse.Quantity(ctx, req.ProductID)
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "warehouse unavailable")
	}

	cart, err := s.Get(ctx, customerID)
	if err != nil {
		return nil, err
	}
	if cart == nil {
		cart = NewCart(customerID)
	}

	// Judge availability against what the cart would hold after this add, not
	// against this request alone. Otherwise a customer can add 60 units twice
	// and walk to a checkout that is guaranteed to fail.
	wanted := req.Quantity
	for _, item := range cart.Items {
		if item.ProductID == req.ProductID {
			wanted += item.Quantity
			break
		}
	}
	if available < wanted {
		return nil, httpx.Errorf(http.StatusConflict,
			"insufficient stock for product %s (cart would hold %d, available %d)",
			req.ProductID, wanted, available)
	}

	cart.AddOrMerge(Item{
		ProductID:  p.ProductID,
		Name:       p.Name,
		Quantity:   req.Quantity,
		UnitWeight: p.Weight,
		UnitPrice:  p.Price,
	})

	if err := s.Save(ctx, cart); err != nil {
		return nil, err
	}

	s.delay.Simulate()
	return cart, nil
}

// Save persists a cart as a single JSON value.
func (s *Service) Save(ctx context.Context, cart *Cart) error {
	raw, err := json.Marshal(cart)
	if err != nil {
		return httpx.Wrap(http.StatusInternalServerError, err, "encoding cart %s", cart.CartID)
	}
	if _, err := s.db.Put(ctx, cart.CartID, string(raw)); err != nil {
		return httpx.Wrap(http.StatusServiceUnavailable, err, "could not persist cart %s", cart.CartID)
	}
	return nil
}

// Clear empties a cart after checkout.
//
// The store is append-only with no delete, so an empty cart is written over
// the old one. That is also the safer semantic here: the key keeps its version
// history, so the emptied cart supersedes the full one on every replica
// instead of racing a delete against an in-flight write.
func (s *Service) Clear(ctx context.Context, customerID string) error {
	if err := s.Save(ctx, NewCart(customerID)); err != nil {
		// The order is already committed by the time this runs. Failing the
		// checkout now would tell the customer their paid-for order failed,
		// which is a far worse outcome than a cart that looks stale.
		slog.Error("could not clear cart after checkout", "customerId", customerID, "err", err)
		return err
	}
	return nil
}

// translateProductError maps a catalogue lookup failure onto the status the
// cart's own caller should see.
func translateProductError(productID string, err error) error {
	var apiErr *httpx.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return httpx.Errorf(http.StatusNotFound, "product %s not found", productID)
	}
	return httpx.Wrap(http.StatusServiceUnavailable, err, "product service unavailable")
}
