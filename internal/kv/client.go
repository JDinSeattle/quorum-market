package kv

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Client is how a service talks to a KV cluster. Both the product service and
// the cart service use it, against their own cluster, so the wire contract
// lives in exactly one place.
type Client struct {
	hc   *httpx.Client
	base string
}

// NewClient returns a client pointed at any node of a cluster. Under
// leaderless replication that node coordinates the request; under
// leader-follower it must be the leader for writes to be accepted.
//
// dependency names the cluster ("product-db", "cart-db") and labels this
// client's metrics and circuit breaker.
func NewClient(dependency, baseURL string, connectTimeout, requestTimeout time.Duration) *Client {
	return &Client{
		hc: httpx.NewClient(httpx.ClientConfig{
			Dependency:     dependency,
			ConnectTimeout: connectTimeout,
			RequestTimeout: requestTimeout,
		}),
		base: strings.TrimSuffix(baseURL, "/"),
	}
}

// Get reads a key. A missing key is (zero, false, nil), not an error.
func (c *Client) Get(ctx context.Context, key string) (Entry, bool, error) {
	var e Entry
	err := c.hc.GetJSON(ctx, c.base+"/kv?key="+url.QueryEscape(key), &e)
	if err == nil {
		return e, true, nil
	}

	if httpx.IsStatus(err, http.StatusNotFound) {
		return Entry{}, false, nil
	}
	return Entry{}, false, err
}

// Put writes a key that never expires.
func (c *Client) Put(ctx context.Context, key, value string) (int64, error) {
	return c.PutWithTTL(ctx, key, value, 0)
}

// PutWithTTL writes a key that the cluster forgets after ttl.
//
// Used for records whose usefulness has a natural end — an idempotency key is
// only interesting for as long as a client might still retry — so the store
// does not accumulate them forever.
func (c *Client) PutWithTTL(ctx context.Context, key, value string, ttl time.Duration) (int64, error) {
	req := writeRequest{Key: key, Value: value}
	if ttl > 0 {
		req.TTLMs = ttl.Milliseconds()
	}

	var resp writeResponse
	if err := c.hc.PutJSON(ctx, c.base+"/kv", req, &resp); err != nil {
		return 0, err
	}
	return resp.Version, nil
}

// Scan returns entries under a prefix, merged across a read quorum.
func (c *Client) Scan(ctx context.Context, prefix string, limit int) ([]Entry, error) {
	var entries []Entry
	target := fmt.Sprintf("%s/kv/scan?prefix=%s&limit=%d", c.base, url.QueryEscape(prefix), limit)

	if err := c.hc.GetJSON(ctx, target, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// BeginTxn opens a simulated transaction and returns its id.
func (c *Client) BeginTxn(ctx context.Context) (string, error) {
	var txn Txn
	if err := c.hc.PostJSON(ctx, c.base+"/db/begin_transaction", nil, &txn); err != nil {
		return "", err
	}
	if txn.ID == "" {
		return "", httpx.Errorf(http.StatusBadGateway, "begin_transaction returned no transaction_id")
	}
	return txn.ID, nil
}

// CommitTxn closes a transaction successfully.
func (c *Client) CommitTxn(ctx context.Context, id string) error {
	return c.hc.PostJSON(ctx, c.base+"/db/end_transaction", txnRequest{TransactionID: id}, nil)
}

// AbortTxn closes a transaction unsuccessfully. Failures here are returned but
// callers are expected to log rather than propagate them: the abort is already
// running because something else went wrong, and replacing the original cause
// with "abort failed" would hide the reason the checkout was rejected.
func (c *Client) AbortTxn(ctx context.Context, id string) error {
	return c.hc.PostJSON(ctx, c.base+"/db/abort_transaction", txnRequest{TransactionID: id}, nil)
}
