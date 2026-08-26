package cca

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Decision is the outcome of an authorization attempt.
type Decision string

const (
	// Approved means the issuer accepted the charge.
	Approved Decision = "approved"
	// Declined means a well-formed card was refused. The order cannot proceed,
	// but nothing is wrong with the system.
	Declined Decision = "declined"
	// InvalidCard means the number was malformed; the customer must fix it.
	InvalidCard Decision = "invalid_card"
	// Unavailable means the authorizer could not be reached or failed. This is
	// an outage, not a customer problem, and is reported as such.
	Unavailable Decision = "unavailable"
)

// Client calls the credit card authorizer.
type Client struct {
	hc   *httpx.Client
	base string
}

// NewClient returns a client for the authorizer at baseURL.
func NewClient(baseURL string, connectTimeout, requestTimeout time.Duration) *Client {
	return &Client{
		hc: httpx.NewClient(httpx.ClientConfig{
			Dependency:     "credit-card-authorizer",
			ConnectTimeout: connectTimeout,
			RequestTimeout: requestTimeout,
			// Authorization is a POST and so is never retried by the
			// transport anyway; making that explicit here records the reason.
			// A retried charge is a double charge.
			MaxAttempts: 1,
		}),
		base: strings.TrimSuffix(baseURL, "/"),
	}
}

// Authorize charges a card and classifies the result.
//
// The three failure modes are kept apart because the caller must react to them
// differently: a decline is final, a malformed number is worth telling the
// customer about, and an outage is worth alerting on.
func (c *Client) Authorize(ctx context.Context, cardNumber string, amount float64, orderID string) (Decision, string, error) {
	req := AuthorizeRequest{CardNumber: cardNumber, Amount: amount, OrderID: orderID}

	var resp AuthorizeResponse
	err := c.hc.PostJSON(ctx, c.base+"/credit-card-authorizer/authorize", req, &resp)
	if err == nil {
		return Approved, resp.Code, nil
	}

	switch {
	case httpx.IsStatus(err, http.StatusPaymentRequired):
		return Declined, "", nil
	case httpx.IsStatus(err, http.StatusBadRequest):
		return InvalidCard, "", nil
	}
	return Unavailable, "", err
}
