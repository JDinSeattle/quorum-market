package product

import (
	"context"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Client calls the product service. The cart service uses it to price and
// weigh items; the bulk loader uses it to seed the catalogue.
type Client struct {
	hc   *httpx.Client
	base string
}

// NewClient returns a client for the product service at baseURL.
func NewClient(baseURL string, connectTimeout, requestTimeout time.Duration) *Client {
	return &Client{
		hc: httpx.NewClient(httpx.ClientConfig{
			Dependency:     "product-service",
			ConnectTimeout: connectTimeout,
			RequestTimeout: requestTimeout,
		}),
		base: strings.TrimSuffix(baseURL, "/"),
	}
}

// Get fetches one product. A missing product surfaces as an *httpx.APIError
// with status 404, which the cart service maps straight through to its caller.
func (c *Client) Get(ctx context.Context, productID string) (Product, error) {
	var p Product
	if err := c.hc.GetJSON(ctx, c.base+"/product/"+productID, &p); err != nil {
		return Product{}, err
	}
	return p, nil
}

// Put upserts one product.
func (c *Client) Put(ctx context.Context, p Product) error {
	return c.hc.PutJSON(ctx, c.base+"/product/"+p.ProductID, p, nil)
}
