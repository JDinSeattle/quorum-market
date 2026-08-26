// Package cart implements the shopping cart service, which orchestrates the
// two customer-facing use cases: adding an item to a cart, and checking out.
package cart

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

const (
	// KeyPrefix namespaces cart records and doubles as the cart id prefix, so
	// a cart id is also its own storage key.
	KeyPrefix = "cart-"
	// OrderKeyPrefix namespaces committed orders in the same store.
	OrderKeyPrefix = "order:"
)

// Item is one line in a cart.
//
// Unit weight and price are copied in at add time rather than looked up again
// at checkout. That is a deliberate snapshot: the customer agreed to the price
// they were shown, and a catalogue update between browsing and paying must not
// silently change what they are charged.
type Item struct {
	ProductID  string  `json:"productId"`
	Name       string  `json:"name,omitempty"`
	Quantity   int     `json:"quantity"`
	UnitWeight float64 `json:"unitWeight"`
	UnitPrice  float64 `json:"unitPrice"`
}

// LineWeight is the total weight this line contributes.
func (i Item) LineWeight() float64 { return i.UnitWeight * float64(i.Quantity) }

// LinePrice is the total price this line contributes.
func (i Item) LinePrice() float64 { return i.UnitPrice * float64(i.Quantity) }

// Order converts the line into the reservation/shipping form.
func (i Item) Order() orders.Item {
	return orders.Item{ProductID: i.ProductID, Quantity: i.Quantity}
}

// Cart is a customer's active cart, stored as one JSON value in the KV store.
//
// Keeping the whole cart in a single key is what lets it live on a leaderless
// cluster: every mutation is a whole-value write, so there are no cross-key
// invariants for replication to break.
type Cart struct {
	CartID     string    `json:"cartId"`
	CustomerID string    `json:"customerId"`
	Items      []Item    `json:"items"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// NewCart returns an empty cart for a customer.
func NewCart(customerID string) *Cart {
	now := time.Now().UTC()
	return &Cart{
		CartID:     IDFor(customerID),
		CustomerID: customerID,
		Items:      []Item{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// IDFor returns the cart id belonging to a customer. The mapping is
// deterministic, so a customer never needs to remember a cart id and a cart
// can be found from the customer alone.
func IDFor(customerID string) string { return KeyPrefix + customerID }

// CustomerFor extracts the customer id from a cart id.
func CustomerFor(cartID string) (string, error) {
	if !strings.HasPrefix(cartID, KeyPrefix) {
		return "", httpx.Errorf(http.StatusBadRequest,
			"invalid cartId %q: expected it to start with %q", cartID, KeyPrefix)
	}
	customerID := strings.TrimPrefix(cartID, KeyPrefix)
	if customerID == "" {
		return "", httpx.Errorf(http.StatusBadRequest, "invalid cartId %q: no customer id", cartID)
	}
	return customerID, nil
}

// AddOrMerge adds a line, folding it into an existing line for the same
// product so a cart never holds the same product twice.
func (c *Cart) AddOrMerge(item Item) {
	for i := range c.Items {
		if c.Items[i].ProductID == item.ProductID {
			c.Items[i].Quantity += item.Quantity
			// Refresh the snapshot so the newly added units carry the price
			// the customer just saw.
			c.Items[i].UnitWeight = item.UnitWeight
			c.Items[i].UnitPrice = item.UnitPrice
			if item.Name != "" {
				c.Items[i].Name = item.Name
			}
			c.UpdatedAt = time.Now().UTC()
			return
		}
	}
	c.Items = append(c.Items, item)
	c.UpdatedAt = time.Now().UTC()
}

// IsEmpty reports whether the cart has nothing in it.
func (c *Cart) IsEmpty() bool { return len(c.Items) == 0 }

// TotalWeight is the cart's combined shipping weight.
func (c *Cart) TotalWeight() float64 {
	var total float64
	for _, item := range c.Items {
		total += item.LineWeight()
	}
	return round2(total)
}

// TotalPrice is the cart's combined price.
func (c *Cart) TotalPrice() float64 {
	var total float64
	for _, item := range c.Items {
		total += item.LinePrice()
	}
	return round2(total)
}

// OrderItems renders the cart in the form the warehouse understands.
func (c *Cart) OrderItems() []orders.Item {
	out := make([]orders.Item, 0, len(c.Items))
	for _, item := range c.Items {
		out = append(out, item.Order())
	}
	return out
}

// round2 keeps money at two decimals so accumulated float error never shows up
// as a price like 41.900000000000006 on a receipt.
func round2(v float64) float64 { return math.Round(v*100) / 100 }
