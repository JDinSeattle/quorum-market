package cart

import (
	"net/http"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Server is the cart service's HTTP surface.
type Server struct {
	carts    *Service
	checkout *CheckoutService
}

// NewServer returns a Server over the cart and checkout services.
func NewServer(carts *Service, checkout *CheckoutService) *Server {
	return &Server{carts: carts, checkout: checkout}
}

type createCartRequest struct {
	CustomerID string `json:"customerId"`
}

// cartView is the wire form of a cart. Totals are computed rather than stored,
// so they can never drift out of step with the lines they summarise.
type cartView struct {
	CartID      string     `json:"cartId"`
	CustomerID  string     `json:"customerId"`
	Items       []lineView `json:"items"`
	TotalWeight float64    `json:"totalWeight"`
	TotalPrice  float64    `json:"totalPrice"`
}

type lineView struct {
	Item
	LineWeight float64 `json:"lineWeight"`
	LinePrice  float64 `json:"linePrice"`
}

func viewOf(c *Cart) cartView {
	lines := make([]lineView, 0, len(c.Items))
	for _, item := range c.Items {
		lines = append(lines, lineView{
			Item:       item,
			LineWeight: round2(item.LineWeight()),
			LinePrice:  round2(item.LinePrice()),
		})
	}
	return cartView{
		CartID:      c.CartID,
		CustomerID:  c.CustomerID,
		Items:       lines,
		TotalWeight: c.TotalWeight(),
		TotalPrice:  c.TotalPrice(),
	}
}

// Routes builds the cart service's HTTP surface. Everything lives under
// /shopping-cart so one load balancer rule covers the service.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /shopping-cart/health")
	rt.Handle("POST /shopping-cart", s.handleCreate)
	rt.Handle("GET /shopping-cart/{cartId}", s.handleGet)
	rt.Handle("POST /shopping-cart/{cartId}/add-item", s.handleAddItem)
	rt.Handle("POST /shopping-cart/{cartId}/checkout", s.handleCheckout)
	return rt.Build(httpx.DefaultMaxInFlight())
}

// handleCreate mints a cart id. Because the id is derived from the customer id
// there is nothing to store yet: the cart materialises on the first add, which
// keeps the 90% of sessions that never add anything from writing to the store
// at all.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) error {
	var req createCartRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	// An authenticated caller creates a cart for themselves, whatever they
	// put in the body; otherwise there would be no point verifying the token.
	if caller := r.Header.Get(auth.CustomerIDHeader); caller != "" {
		req.CustomerID = caller
	}
	if req.CustomerID == "" {
		return httpx.Errorf(http.StatusBadRequest, "customerId is required")
	}
	httpx.JSON(w, http.StatusCreated, map[string]string{
		"cartId":     IDFor(req.CustomerID),
		"customerId": req.CustomerID,
	})
	return nil
}

// authorize resolves the customer a request is acting on, and refuses to let
// one customer act on another's cart.
//
// Authentication happens at the gateway; this is authorization. The gateway
// verifies the token and sets the identity header, having first stripped
// whatever the client sent. When that header is present, the cart named in the
// path has to belong to it — otherwise knowing (or guessing) a cart id would
// be enough to read someone's basket or check it out on their card.
//
// When the header is absent the request did not come through the gateway,
// which on a correctly deployed system means it came from inside the private
// network. That is a debugging affordance, and it is the weakest link in this
// model: it is safe exactly as far as the network boundary is.
func authorize(r *http.Request, cartID string) (string, error) {
	owner, err := CustomerFor(cartID)
	if err != nil {
		return "", err
	}

	caller := r.Header.Get(auth.CustomerIDHeader)
	if caller == "" {
		return owner, nil
	}
	if caller != owner {
		// 404 rather than 403: confirming that a cart exists but belongs to
		// someone else is itself information worth withholding.
		return "", httpx.Errorf(http.StatusNotFound, "no cart %s for this customer", cartID)
	}
	return owner, nil
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) error {
	customerID, err := authorize(r, r.PathValue("cartId"))
	if err != nil {
		return err
	}

	cart, err := s.carts.Get(r.Context(), customerID)
	if err != nil {
		return err
	}
	if cart == nil {
		return httpx.Errorf(http.StatusNotFound, "no cart for customer %s", customerID)
	}
	httpx.JSON(w, http.StatusOK, viewOf(cart))
	return nil
}

func (s *Server) handleAddItem(w http.ResponseWriter, r *http.Request) error {
	customerID, err := authorize(r, r.PathValue("cartId"))
	if err != nil {
		return err
	}

	var req AddItemRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	cart, err := s.carts.AddItem(r.Context(), customerID, req)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, viewOf(cart))
	return nil
}

func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request) error {
	cartID := r.PathValue("cartId")
	if _, err := authorize(r, cartID); err != nil {
		return err
	}

	var req CheckoutRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	// The header is the conventional place for this and takes precedence; the
	// body field is a fallback for clients that cannot set one.
	if key := r.Header.Get(IdempotencyKeyHeader); key != "" {
		req.IdempotencyKey = key
	}

	receipt, err := s.checkout.Checkout(r.Context(), cartID, req)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, receipt)
	return nil
}
