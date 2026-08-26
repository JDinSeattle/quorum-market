// Package orders holds the contracts shared between the cart service, which
// produces orders, and the warehouse service, which reserves stock for them
// and ships them. Both sides depend on this package so the HTTP bodies and the
// queue message have exactly one definition.
package orders

import "time"

// Item is one line of an order as the warehouse sees it: what to move and how
// much of it. Prices and weights stay in the cart; the warehouse does not need
// them and shipping them would only invite the two sides to disagree.
type Item struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

// ReserveRequest is the body of POST /warehouse/reserve.
type ReserveRequest struct {
	Items []Item `json:"items"`
}

// ReserveResponse is returned when a reservation is granted. The id is what
// makes the reservation releasable and shippable: without it the caller can
// only describe the items it wants back, and the warehouse cannot tell a
// legitimate release from a duplicate one.
type ReserveResponse struct {
	ReservationID string    `json:"reservationId"`
	Status        string    `json:"status"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// ReleaseRequest is the body of POST /warehouse/release.
type ReleaseRequest struct {
	ReservationID string `json:"reservationId"`
}

// ShipMessage is the queue contract between cart and warehouse.
//
// It carries ReservationID as well as the items so the warehouse can retire
// the reservation it already made instead of decrementing stock a second time.
type ShipMessage struct {
	OrderID       string    `json:"orderId"`
	CartID        string    `json:"cartId"`
	CustomerID    string    `json:"customerId"`
	ReservationID string    `json:"reservationId"`
	Items         []Item    `json:"items"`
	TotalWeight   float64   `json:"totalWeight"`
	TotalPrice    float64   `json:"totalPrice"`
	PlacedAt      time.Time `json:"placedAt"`
}
