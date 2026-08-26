package warehouse

import (
	"context"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

// Client calls the warehouse service. The cart service uses it to check
// availability while browsing and to hold stock during checkout.
type Client struct {
	hc   *httpx.Client
	base string
}

// NewClient returns a client for the warehouse at baseURL.
func NewClient(baseURL string, connectTimeout, requestTimeout time.Duration) *Client {
	return &Client{
		hc: httpx.NewClient(httpx.ClientConfig{
			Dependency:     "warehouse-service",
			ConnectTimeout: connectTimeout,
			RequestTimeout: requestTimeout,
		}),
		base: strings.TrimSuffix(baseURL, "/"),
	}
}

type quantityResponse struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

// Quantity reports how many units are currently available. This is a soft
// check for the add-to-cart path: it does not hold anything, so the answer can
// be stale by the time the customer checks out. That is the correct trade —
// holding stock for every browsing customer would make the catalogue look
// empty to everyone else.
func (c *Client) Quantity(ctx context.Context, productID string) (int, error) {
	var resp quantityResponse
	if err := c.hc.GetJSON(ctx, c.base+"/warehouse/inventory/"+productID, &resp); err != nil {
		return 0, err
	}
	return resp.Quantity, nil
}

// Reserve holds stock for every item or none. A shortfall surfaces as an
// *httpx.APIError with status 409.
func (c *Client) Reserve(ctx context.Context, items []orders.Item) (orders.ReserveResponse, error) {
	var resp orders.ReserveResponse
	err := c.hc.PostJSON(ctx, c.base+"/warehouse/reserve", orders.ReserveRequest{Items: items}, &resp)
	if err != nil {
		return orders.ReserveResponse{}, err
	}
	return resp, nil
}

// Release hands a reservation back. It is safe to call more than once.
func (c *Client) Release(ctx context.Context, reservationID string) error {
	return c.hc.PostJSON(ctx, c.base+"/warehouse/release",
		orders.ReleaseRequest{ReservationID: reservationID}, nil)
}
