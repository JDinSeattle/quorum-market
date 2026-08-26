// Package warehouse tracks stock levels and the reservations held against them.
package warehouse

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

// DefaultStock is the quantity a product starts with the first time it is
// touched. Stock is seeded lazily rather than loaded from the catalogue so the
// warehouse has no startup dependency on the product service.
const DefaultStock = 100

// DefaultTTL is how long a reservation is held before it is reclaimed.
const DefaultTTL = 30 * time.Second

// InsufficientStockError names the product that could not be reserved.
type InsufficientStockError struct {
	ProductID string
	Requested int
	Available int
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("insufficient inventory for product %s (requested %d, available %d)",
		e.ProductID, e.Requested, e.Available)
}

// Reservation is a hold on stock that has already been deducted.
type Reservation struct {
	ID        string        `json:"reservationId"`
	Items     []orders.Item `json:"items"`
	CreatedAt time.Time     `json:"createdAt"`
	ExpiresAt time.Time     `json:"expiresAt"`
}

// ShipOutcome describes how a ship message was accounted for.
type ShipOutcome string

const (
	// ShipCompleted means a live reservation was retired. Stock was already
	// deducted when it was created, so nothing else changes.
	ShipCompleted ShipOutcome = "completed"
	// ShipCompensated means no live reservation was found and stock had to be
	// deducted at ship time instead.
	ShipCompensated ShipOutcome = "compensated"
)

// Inventory is the warehouse's stock ledger.
//
// Stock lives in a map of atomic counters rather than behind one mutex so that
// checkouts touching different products never contend: only concurrent
// reservations of the *same* product serialise, and even then through a
// compare-and-swap loop rather than a lock.
//
// The lifecycle is the part worth being careful about. A reservation deducts
// stock immediately and then resolves exactly once — released, shipped, or
// expired — because every path retires it through take(), which removes it
// from the map under a lock. Being in the map *is* being pending. That is what
// stops a released reservation from also being expired later and handing the
// same units back twice.
type Inventory struct {
	initial int64
	ttl     time.Duration

	stockMu sync.RWMutex
	stock   map[string]*atomic.Int64

	resMu        sync.Mutex
	reservations map[string]*Reservation

	reserved  atomic.Uint64
	rejected  atomic.Uint64
	released  atomic.Uint64
	expired   atomic.Uint64
	shipped   atomic.Uint64
	lateShips atomic.Uint64
	oversold  atomic.Uint64
}

// New returns an Inventory seeding unknown products to initial units and
// holding reservations for ttl.
//
// Its size gauges are computed at scrape time rather than mirrored on every
// mutation, so there is no second copy of the truth to drift and no cost on
// the reservation hot path.
func New(initial int, ttl time.Duration) *Inventory {
	if initial < 0 {
		initial = 0
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	inv := &Inventory{
		initial:      int64(initial),
		ttl:          ttl,
		stock:        make(map[string]*atomic.Int64),
		reservations: make(map[string]*Reservation),
	}
	registerGauges.Do(func() {
		obs.RegisterGauge("inventory", "pending_reservations",
			"Reservations currently holding stock.",
			func() float64 { return float64(gaugeSource.Load().PendingReservations()) })
		obs.RegisterGauge("inventory", "tracked_products",
			"Distinct products the warehouse has seen.",
			func() float64 { return float64(gaugeSource.Load().TrackedProducts()) })
	})
	gaugeSource.Store(inv)
	return inv
}

// A process runs exactly one warehouse; the indirection exists only so that
// tests, which construct many, do not panic on duplicate metric registration.
var (
	registerGauges sync.Once
	gaugeSource    atomic.Pointer[Inventory]
)

// Quantity returns the units currently available for a product, seeding it on
// first sight.
func (inv *Inventory) Quantity(productID string) int {
	return int(inv.counter(productID).Load())
}

// Reserve deducts stock for every item or none of them, and records a
// reservation that must later be released, shipped, or left to expire.
func (inv *Inventory) Reserve(items []orders.Item) (*Reservation, error) {
	merged, err := normalize(items)
	if err != nil {
		return nil, err
	}

	applied := make([]orders.Item, 0, len(merged))
	for _, item := range merged {
		counter := inv.counter(item.ProductID)
		if !casDecrement(counter, int64(item.Quantity)) {
			// All-or-nothing: a partially reserved cart would leave stock held
			// against an order that is never going to be placed.
			inv.restore(applied)
			inv.rejected.Add(1)
			obs.ObserveBusinessEvent("reservation", "rejected")
			return nil, &InsufficientStockError{
				ProductID: item.ProductID,
				Requested: item.Quantity,
				Available: int(counter.Load()),
			}
		}
		applied = append(applied, item)
	}

	now := time.Now()
	r := &Reservation{
		ID:        newReservationID(),
		Items:     merged,
		CreatedAt: now,
		ExpiresAt: now.Add(inv.ttl),
	}

	inv.resMu.Lock()
	inv.reservations[r.ID] = r
	inv.resMu.Unlock()

	inv.reserved.Add(1)
	obs.ObserveBusinessEvent("reservation", "granted")
	return r, nil
}

// Release hands a reservation's stock back. It reports whether the reservation
// was still live; releasing one that has already been resolved is a no-op
// rather than an error, so a client that retries a release cannot inflate
// stock by doing so.
func (inv *Inventory) Release(reservationID string) bool {
	r, ok := inv.take(reservationID)
	if !ok {
		return false
	}
	inv.restore(r.Items)
	inv.released.Add(1)
	obs.ObserveBusinessEvent("reservation", "released")
	return true
}

// Ship retires a reservation because its order has shipped.
//
// The stock was already deducted at reserve time, so the common path only
// removes the reservation. If the reservation is gone — it expired before the
// queue drained, or this warehouse restarted and lost it — the units still
// physically left, so they are deducted now instead.
func (inv *Inventory) Ship(reservationID string, items []orders.Item) ShipOutcome {
	if reservationID != "" {
		if _, ok := inv.take(reservationID); ok {
			inv.shipped.Add(1)
			obs.ObserveBusinessEvent("shipment", "completed")
			return ShipCompleted
		}
	}

	inv.compensate(items)
	inv.shipped.Add(1)
	inv.lateShips.Add(1)
	obs.ObserveBusinessEvent("shipment", "compensated")
	slog.Warn("shipping an order with no live reservation; deducting stock now",
		"reservationId", reservationID)
	return ShipCompensated
}

// Sweep reclaims reservations whose hold has expired. Without it a checkout
// that dies between reserving and paying would hold stock forever.
func (inv *Inventory) Sweep() int {
	now := time.Now()

	inv.resMu.Lock()
	var stale []*Reservation
	for id, r := range inv.reservations {
		if now.After(r.ExpiresAt) {
			stale = append(stale, r)
			delete(inv.reservations, id)
		}
	}
	inv.resMu.Unlock()

	for _, r := range stale {
		inv.restore(r.Items)
	}
	if len(stale) > 0 {
		inv.expired.Add(uint64(len(stale)))
		for range stale {
			obs.ObserveBusinessEvent("reservation", "expired")
		}
		slog.Info("reclaimed expired reservations", "count", len(stale))
	}
	return len(stale)
}

// RunSweeper sweeps on a ticker until ctx is done.
func (inv *Inventory) RunSweeper(done <-chan struct{}, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			inv.Sweep()
		}
	}
}

// PendingReservations returns how many reservations are currently held.
func (inv *Inventory) PendingReservations() int {
	inv.resMu.Lock()
	defer inv.resMu.Unlock()
	return len(inv.reservations)
}

// TrackedProducts returns how many distinct products have been touched.
func (inv *Inventory) TrackedProducts() int {
	inv.stockMu.RLock()
	defer inv.stockMu.RUnlock()
	return len(inv.stock)
}

// Stats reports lifetime counters.
func (inv *Inventory) Stats() map[string]any {
	return map[string]any{
		"reserved":              inv.reserved.Load(),
		"rejected":              inv.rejected.Load(),
		"released":              inv.released.Load(),
		"expired":               inv.expired.Load(),
		"shipped":               inv.shipped.Load(),
		"shipped_uncompensated": inv.lateShips.Load(),
		"oversold":              inv.oversold.Load(),
		"pending_reservations":  inv.PendingReservations(),
		"tracked_products":      inv.TrackedProducts(),
		"reservation_ttl":       inv.ttl.String(),
	}
}

// ── internals ────────────────────────────────────────────────────────────────

// counter returns the atomic holding a product's stock, seeding it on first
// use. The read lock covers the common case; the write lock is taken only to
// insert, and re-checks because another goroutine may have won the race.
func (inv *Inventory) counter(productID string) *atomic.Int64 {
	inv.stockMu.RLock()
	c, ok := inv.stock[productID]
	inv.stockMu.RUnlock()
	if ok {
		return c
	}

	inv.stockMu.Lock()
	defer inv.stockMu.Unlock()
	if c, ok := inv.stock[productID]; ok {
		return c
	}
	c = new(atomic.Int64)
	c.Store(inv.initial)
	inv.stock[productID] = c
	return c
}

// take removes a reservation and returns it, which is how a reservation is
// resolved exactly once no matter how many paths race to resolve it.
func (inv *Inventory) take(reservationID string) (*Reservation, bool) {
	if reservationID == "" {
		return nil, false
	}
	inv.resMu.Lock()
	defer inv.resMu.Unlock()

	r, ok := inv.reservations[reservationID]
	if !ok {
		return nil, false
	}
	delete(inv.reservations, reservationID)
	return r, true
}

func (inv *Inventory) restore(items []orders.Item) {
	for _, item := range items {
		inv.counter(item.ProductID).Add(int64(item.Quantity))
	}
}

// compensate deducts stock outside the reserve path, clamping at zero. Going
// negative would be meaningless — a warehouse cannot hold minus three units —
// so the shortfall is counted as an oversell instead.
func (inv *Inventory) compensate(items []orders.Item) {
	for _, item := range items {
		counter := inv.counter(item.ProductID)
		want := int64(item.Quantity)
		for {
			current := counter.Load()
			next := current - want
			if next < 0 {
				next = 0
			}
			if counter.CompareAndSwap(current, next) {
				if current < want {
					inv.oversold.Add(1)
					obs.ObserveBusinessEvent("inventory", "oversold")
				}
				break
			}
		}
	}
}

// casDecrement takes n units if they are available. The loop retries when a
// concurrent reservation moves the counter between the read and the swap,
// which is what makes the check-then-decrement atomic without a lock.
func casDecrement(counter *atomic.Int64, n int64) bool {
	for {
		current := counter.Load()
		if current < n {
			return false
		}
		if counter.CompareAndSwap(current, current-n) {
			return true
		}
	}
}

// normalize validates a request and folds repeated products into one line.
// Reserving the same product twice in a single request would otherwise race
// against itself and make the all-or-nothing rollback ambiguous.
func normalize(items []orders.Item) ([]orders.Item, error) {
	if len(items) == 0 {
		return nil, httpx.Errorf(http.StatusBadRequest, "items must not be empty")
	}

	order := make([]string, 0, len(items))
	totals := make(map[string]int, len(items))
	for _, item := range items {
		if item.ProductID == "" {
			return nil, httpx.Errorf(http.StatusBadRequest, "every item needs a productId")
		}
		if item.Quantity <= 0 {
			return nil, httpx.Errorf(http.StatusBadRequest,
				"quantity for product %s must be greater than zero", item.ProductID)
		}
		if _, seen := totals[item.ProductID]; !seen {
			order = append(order, item.ProductID)
		}
		totals[item.ProductID] += item.Quantity
	}

	merged := make([]orders.Item, 0, len(order))
	for _, id := range order {
		merged = append(merged, orders.Item{ProductID: id, Quantity: totals[id]})
	}
	return merged, nil
}

func newReservationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("res-%d", time.Now().UnixNano())
	}
	return "res-" + hex.EncodeToString(b[:])
}
