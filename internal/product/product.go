// Package product implements the product catalogue service.
//
// The catalogue is read-heavy and rarely written, which is why it sits on the
// leader-follower cluster: writes pay for strong consistency, reads are served
// by a single node.
package product

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/JDinSeattle/quorum-market/internal/cache"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
)

// KeyPrefix namespaces catalogue keys inside the shared KV store.
const KeyPrefix = "product:"

// Product is one catalogue entry. Weight drives shipping calculations in the
// cart; Price drives the amount sent to the card authorizer.
type Product struct {
	ProductID string  `json:"productId"`
	Name      string  `json:"name,omitempty"`
	Weight    float64 `json:"weight"`
	Price     float64 `json:"price"`
}

// Key returns the KV key holding a product.
func Key(productID string) string { return KeyPrefix + productID }

// Service reads and writes the catalogue through a KV cluster, in front of an
// optional cache.
//
// The catalogue is the highest-volume read path in the system and its contents
// change at seed time and essentially never again — the textbook shape for a
// cache. Every browse that hits it is a quorum read the database never has to
// serve.
type Service struct {
	db    *kv.Client
	cache *cache.Cache
}

// NewService returns a Service backed by db. A nil cache disables caching and
// every read goes to the database, which is what keeps the cache optional.
func NewService(db *kv.Client, c *cache.Cache) *Service {
	return &Service{db: db, cache: c}
}

// Get returns one product, from the cache when possible.
func (s *Service) Get(ctx context.Context, productID string) (Product, error) {
	if productID == "" {
		return Product{}, httpx.Errorf(http.StatusBadRequest, "productId is required")
	}

	raw, err := s.cache.GetOrLoad(ctx, productID, func(ctx context.Context) ([]byte, error) {
		return s.load(ctx, productID)
	})
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			return Product{}, httpx.Errorf(http.StatusNotFound, "product %s not found", productID)
		}
		return Product{}, err
	}

	var p Product
	if err := json.Unmarshal(raw, &p); err != nil {
		return Product{}, httpx.Wrap(http.StatusInternalServerError, err,
			"product %s is stored in an unreadable format", productID)
	}
	p.ProductID = productID
	return p, nil
}

// load fetches a product from the database. Reporting a missing product as
// cache.ErrNotFound is what lets the absence be remembered, so a hot key that
// does not exist stops reaching the database on every request.
func (s *Service) load(ctx context.Context, productID string) ([]byte, error) {
	entry, found, err := s.db.Get(ctx, Key(productID))
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "product database unavailable")
	}
	if !found {
		return nil, cache.ErrNotFound
	}
	return []byte(entry.Value), nil
}

// Put upserts a product.
func (s *Service) Put(ctx context.Context, p Product) error {
	if p.ProductID == "" {
		return httpx.Errorf(http.StatusBadRequest, "productId is required")
	}
	if p.Weight < 0 || p.Price < 0 {
		return httpx.Errorf(http.StatusBadRequest, "weight and price must not be negative")
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return httpx.Wrap(http.StatusInternalServerError, err, "encoding product %s", p.ProductID)
	}
	if _, err := s.db.Put(ctx, Key(p.ProductID), string(raw)); err != nil {
		return httpx.Wrap(http.StatusServiceUnavailable, err, "product database unavailable")
	}

	// Invalidate rather than overwrite. Writing the new value into the cache
	// would look tidier and would be wrong: the database write is replicated
	// asynchronously beyond the write quorum, so a cache primed from this
	// process can be newer than what a subsequent read would find. Dropping
	// the entry makes the next read go and find out.
	s.cache.Invalidate(ctx, p.ProductID)
	return nil
}
