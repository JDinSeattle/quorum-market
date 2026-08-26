package cart

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
)

// IdempotencyKeyHeader carries a client-chosen key that makes a checkout safe
// to retry.
const IdempotencyKeyHeader = "Idempotency-Key"

const idempotencyPrefix = "idem:"

// Lifetimes for the two states a claim can be in.
const (
	// completedTTL is how long a finished checkout can be replayed. A day
	// comfortably covers a client retrying through a network partition, a
	// mobile app resuming, or an operator re-driving a stuck request.
	completedTTL = 24 * time.Hour

	// inProgressTTL bounds how long a claim blocks a retry. It has to exceed
	// the worst realistic checkout — reserve, authorize and commit, each with
	// its own timeout — or a slow-but-succeeding checkout would be joined by a
	// second attempt.
	inProgressTTL = 60 * time.Second
)

// Claim states.
const (
	claimInProgress = "in_progress"
	claimCompleted  = "completed"
)

// idempotencyRecord is the stored claim on a key.
type idempotencyRecord struct {
	Key       string    `json:"key"`
	CartID    string    `json:"cartId"`
	State     string    `json:"state"`
	Receipt   *Receipt  `json:"receipt,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

// idempotencyStore makes checkout safe to retry.
//
// Checkout charges a card and commits an order. If the response is lost — a
// timeout, a load balancer resetting the connection, a phone changing
// networks — the client has no way to tell "it failed" from "it worked and I
// did not hear". Retrying is the only sensible thing for it to do, and without
// a key that retry charges the customer a second time.
//
// The key turns the retry into a replay: the second request returns the first
// one's receipt rather than performing a second checkout.
//
// This is deduplication, not a distributed lock. Two genuinely simultaneous
// requests carrying the same key can both observe an unclaimed key and
// proceed; making that impossible needs a compare-and-set the store does not
// offer. It closes the case that actually happens — a retry seconds after the
// original — and the narrow race is documented rather than pretended away.
type idempotencyStore struct {
	db *kv.Client
}

func newIdempotencyStore(db *kv.Client) *idempotencyStore {
	return &idempotencyStore{db: db}
}

// claim attempts to take ownership of a key.
//
// It returns a receipt when the key names an already-completed checkout, in
// which case the caller must return that receipt and do nothing else.
func (s *idempotencyStore) claim(ctx context.Context, key, cartID string) (*Receipt, error) {
	existing, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		// A key belongs to the checkout that first used it. Honouring it for a
		// different cart would hand one customer another's receipt.
		if existing.CartID != cartID {
			return nil, httpx.Errorf(http.StatusConflict,
				"idempotency key %q was already used for a different cart", key)
		}

		if existing.State == claimCompleted && existing.Receipt != nil {
			return existing.Receipt, nil
		}

		if time.Since(existing.StartedAt) < inProgressTTL {
			return nil, httpx.Errorf(http.StatusConflict,
				"a checkout with idempotency key %q is already in progress", key)
		}
		// The claim is stale: whoever held it died without finishing. Taking it
		// over is the lesser risk — the alternative is a cart that can never be
		// checked out until the record expires.
		slog.WarnContext(ctx, "taking over an abandoned idempotency claim",
			"key", key, "cartId", cartID, "age", time.Since(existing.StartedAt))
	}

	record := idempotencyRecord{
		Key: key, CartID: cartID, State: claimInProgress, StartedAt: time.Now().UTC(),
	}
	if err := s.save(ctx, record, inProgressTTL); err != nil {
		return nil, err
	}
	return nil, nil
}

// complete records the receipt so a later retry replays it.
//
// Called after the order is committed and before the response is written: that
// ordering is what makes a lost response replayable rather than repeatable.
func (s *idempotencyStore) complete(ctx context.Context, key, cartID string, receipt Receipt) {
	record := idempotencyRecord{
		Key: key, CartID: cartID, State: claimCompleted,
		Receipt: &receipt, StartedAt: time.Now().UTC(),
	}

	if err := s.save(ctx, record, completedTTL); err != nil {
		// The order is already placed, so this cannot fail the checkout. The
		// consequence is that a retry within the claim window gets a 409
		// instead of a replayed receipt — annoying, and far better than a
		// second charge.
		slog.ErrorContext(ctx, "could not record the completed idempotency key",
			"key", key, "orderId", receipt.OrderID, "err", err)
	}
}

// release drops a claim after a checkout failed, so the customer can fix
// whatever was wrong — a declined card, usually — and retry with the same key.
//
// Only successful checkouts are worth replaying. A failure has no charge and
// no order behind it, so there is nothing to protect against repeating.
func (s *idempotencyStore) release(ctx context.Context, key, cartID string) {
	record := idempotencyRecord{
		Key: key, CartID: cartID, State: claimInProgress,
		// Backdated past the claim window so the next attempt sees an
		// abandoned claim and takes it over. The store has no delete, so
		// expiring the claim in place is how it is given up.
		StartedAt: time.Now().UTC().Add(-2 * inProgressTTL),
	}
	if err := s.save(ctx, record, inProgressTTL); err != nil {
		slog.WarnContext(ctx, "could not release the idempotency claim", "key", key, "err", err)
	}
}

func (s *idempotencyStore) load(ctx context.Context, key string) (*idempotencyRecord, error) {
	entry, found, err := s.db.Get(ctx, idempotencyPrefix+key)
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "cart database unavailable")
	}
	if !found {
		return nil, nil
	}

	var record idempotencyRecord
	if err := json.Unmarshal([]byte(entry.Value), &record); err != nil {
		// An unreadable record must not wedge the customer's checkout forever.
		slog.ErrorContext(ctx, "discarding an unreadable idempotency record", "key", key, "err", err)
		return nil, nil
	}
	return &record, nil
}

func (s *idempotencyStore) save(ctx context.Context, record idempotencyRecord, ttl time.Duration) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return httpx.Wrap(http.StatusInternalServerError, err, "encoding idempotency record")
	}
	if _, err := s.db.PutWithTTL(ctx, idempotencyPrefix+record.Key, string(raw), ttl); err != nil {
		return httpx.Wrap(http.StatusServiceUnavailable, err, "cart database unavailable")
	}
	return nil
}

// validateIdempotencyKey bounds what a client may send.
//
// The key becomes part of a storage key and appears in logs, so an unbounded
// or exotic value would let a caller bloat the store or forge log lines.
func validateIdempotencyKey(key string) error {
	if len(key) > 128 {
		return httpx.Errorf(http.StatusBadRequest,
			"%s must be at most 128 characters", IdempotencyKeyHeader)
	}
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == ':', c == '.':
		default:
			return httpx.Errorf(http.StatusBadRequest,
				"%s may only contain letters, digits, and - _ : .", IdempotencyKeyHeader)
		}
	}
	return nil
}
