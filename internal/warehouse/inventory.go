// Package warehouse tracks single-owner, in-memory inventory and request receipts.
package warehouse

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

// DefaultStock seeds each SKU lazily when it is first referenced.
const DefaultStock = 100

// DefaultTTL limits how long a pending reservation may hold stock.
const DefaultTTL = 30 * time.Second

// MaxCASAttempts bounds version-conflict retries for one reservation.
const MaxCASAttempts = 1000

// InsufficientStockError identifies a well-formed reservation that cannot be filled.
type InsufficientStockError struct {
	ProductID            string
	Requested, Available int
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("insufficient inventory for product %s (requested %d, available %d)", e.ProductID, e.Requested, e.Available)
}

// ReservationState identifies a pending hold or one of its terminal states.
type ReservationState string

// Allowed reservation states; only Reserved can transition.
const (
	Reserved  ReservationState = "reserved"
	Committed ReservationState = "committed"
	Released  ReservationState = "released"
)

// Reservation is an immutable intent with a retained, monotonic lifecycle.
type Reservation struct {
	ID        string           `json:"reservationId"`
	RequestID string           `json:"request_id"`
	State     ReservationState `json:"status"`
	Items     []orders.Item    `json:"items"`
	CreatedAt time.Time        `json:"createdAt"`
	ExpiresAt time.Time        `json:"expiresAt"`
}

// ShipOutcome describes whether a message commits, replays or rejects a hold.
type ShipOutcome string

// Shipment outcomes do not trigger any additional stock decrement.
const (
	ShipCompleted ShipOutcome = "completed"
	ShipDuplicate ShipOutcome = "already-committed"
	ShipRejected  ShipOutcome = "rejected"
)

type stockRecord struct {
	mu       sync.Mutex
	quantity int64
	version  uint64
}
type requestReceipt struct {
	digest        [32]byte
	done          chan struct{}
	reservationID string
	err           error
}

// Inventory requires one warehouse process. Per-SKU mutexes own the atomic
// version comparison, availability check, decrement and version increment.
// Request receipts and terminal reservations are retained until process exit;
// neither receipts nor stock survive restart, and replicas cannot share these locks.
type Inventory struct {
	initial      int64
	ttl          time.Duration
	stockMu      sync.RWMutex
	stock        map[string]*stockRecord
	resMu        sync.Mutex
	reservations map[string]*Reservation
	receipts     map[string]*requestReceipt

	reserved  atomic.Uint64
	rejected  atomic.Uint64
	released  atomic.Uint64
	expired   atomic.Uint64
	shipped   atomic.Uint64
	lateShips atomic.Uint64
	oversold  atomic.Uint64
}

// New constructs an empty, single-owner inventory with lazy initial stock.
func New(initial int, ttl time.Duration) *Inventory {
	if initial < 0 {
		initial = 0
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	inv := &Inventory{initial: int64(initial), ttl: ttl, stock: make(map[string]*stockRecord),
		reservations: make(map[string]*Reservation), receipts: make(map[string]*requestReceipt)}
	registerGauges.Do(func() {
		obs.RegisterGauge("inventory", "pending_reservations", "Reservations currently holding stock.", func() float64 { return float64(gaugeSource.Load().PendingReservations()) })
		obs.RegisterGauge("inventory", "tracked_products", "Distinct products the warehouse has seen.", func() float64 { return float64(gaugeSource.Load().TrackedProducts()) })
	})
	gaugeSource.Store(inv)
	return inv
}

var (
	registerGauges sync.Once
	gaugeSource    atomic.Pointer[Inventory]
)

// Quantity reads the currently available units for a SKU.
func (inv *Inventory) Quantity(productID string) int {
	quantity, _ := inv.Snapshot(productID)
	return quantity
}

// Snapshot returns one locked observation of both fields. A subsequent mutation
// must compare this version under the same SKU mutex; reading alone is not CAS.
func (inv *Inventory) Snapshot(productID string) (int, uint64) {
	stock := inv.counter(productID)
	stock.mu.Lock()
	defer stock.mu.Unlock()
	return int(stock.quantity), stock.version
}

// Reserve creates a fresh request identity for legacy in-process callers.
// Callers retrying a request must use ReserveWithRequestID instead.
func (inv *Inventory) Reserve(items []orders.Item) (*Reservation, error) {
	return inv.ReserveWithRequestID(newReservationID(), items)
}

// ReserveWithRequestID retains one receipt for one normalized intent. Equal
// requests wait for/replay it; changed content conflicts even after resolution.
func (inv *Inventory) ReserveWithRequestID(requestID string, items []orders.Item) (*Reservation, error) {
	if requestID == "" || len(requestID) > 128 {
		return nil, httpx.Errorf(http.StatusBadRequest, "request_id must contain 1..128 characters")
	}
	for _, ch := range requestID {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_', ch == ':', ch == '.':
		default:
			return nil, httpx.Errorf(http.StatusBadRequest, "invalid request_id")
		}
	}
	normalized, err := normalize(items)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(normalized)
	digest := sha256.Sum256(raw)
	inv.resMu.Lock()
	if receipt, ok := inv.receipts[requestID]; ok {
		if receipt.digest != digest {
			inv.resMu.Unlock()
			return nil, httpx.Errorf(http.StatusConflict, "request_id already identifies different items")
		}
		inv.resMu.Unlock()
		<-receipt.done
		inv.resMu.Lock()
		defer inv.resMu.Unlock()
		if receipt.err != nil {
			return nil, receipt.err
		}
		return cloneReservation(inv.reservations[receipt.reservationID]), nil
	}
	receipt := &requestReceipt{digest: digest, done: make(chan struct{})}
	inv.receipts[requestID] = receipt
	inv.resMu.Unlock()
	err = inv.reserveStock(normalized)
	inv.resMu.Lock()
	defer inv.resMu.Unlock()
	receipt.err = err
	defer close(receipt.done)
	if err != nil {
		inv.rejected.Add(1)
		obs.ObserveBusinessEvent("reservation", "rejected")
		return nil, err
	}
	now := time.Now()
	r := &Reservation{ID: newReservationID(), RequestID: requestID, State: Reserved, Items: normalized, CreatedAt: now, ExpiresAt: now.Add(inv.ttl)}
	inv.reservations[r.ID] = r
	receipt.reservationID = r.ID
	inv.reserved.Add(1)
	obs.ObserveBusinessEvent("reservation", "granted")
	return cloneReservation(r), nil
}

// reserveStock holds every SKU lock in canonical order while checking and
// mutating the whole cart, avoiding provisional decrements and compensation.
func (inv *Inventory) reserveStock(items []orders.Item) error {
	stocks := make([]*stockRecord, len(items))
	versions := make([]uint64, len(items))
	for i, item := range items {
		stocks[i] = inv.counter(item.ProductID)
	}
	for range MaxCASAttempts {
		for i, stock := range stocks {
			stock.mu.Lock()
			versions[i] = stock.version
			stock.mu.Unlock()
		}
		for _, stock := range stocks {
			stock.mu.Lock()
		}
		unlock := func() {
			for i := len(stocks) - 1; i >= 0; i-- {
				stocks[i].mu.Unlock()
			}
		}
		conflict := false
		for i, stock := range stocks {
			if stock.version != versions[i] {
				conflict = true
				break
			}
		}
		if conflict {
			unlock()
			continue
		}
		for i, stock := range stocks {
			if stock.quantity < int64(items[i].Quantity) {
				err := &InsufficientStockError{items[i].ProductID, items[i].Quantity, int(stock.quantity)}
				unlock()
				return err
			}
		}
		for i, stock := range stocks {
			stock.quantity -= int64(items[i].Quantity)
			stock.version++
		}
		unlock()
		return nil
	}
	return httpx.Errorf(http.StatusServiceUnavailable, "inventory contention exceeded %d CAS attempts", MaxCASAttempts)
}

// Release is the only explicit compensation: it restores a legitimate pending
// reservation once. A committed or already released reservation cannot reopen.
func (inv *Inventory) Release(id string) bool {
	inv.resMu.Lock()
	r := inv.reservations[id]
	if r == nil || r.State != Reserved {
		inv.resMu.Unlock()
		return false
	}
	r.State = Released
	items := r.Items
	inv.resMu.Unlock()
	inv.restore(items)
	inv.released.Add(1)
	obs.ObserveBusinessEvent("reservation", "released")
	return true
}

// Ship commits a matching live reservation once. Unknown, expired, released or
// content-conflicting messages are rejected without deducting any stock.
func (inv *Inventory) Ship(id string, items []orders.Item) ShipOutcome {
	normalized, err := normalize(items)
	if err != nil {
		inv.lateShips.Add(1)
		return ShipRejected
	}
	inv.resMu.Lock()
	r := inv.reservations[id]
	if r == nil || !sameItems(r.Items, normalized) || r.State == Released {
		inv.resMu.Unlock()
		inv.lateShips.Add(1)
		return ShipRejected
	}
	if r.State == Committed {
		inv.resMu.Unlock()
		return ShipDuplicate
	}
	if !time.Now().Before(r.ExpiresAt) {
		r.State = Released
		inv.resMu.Unlock()
		inv.restore(r.Items)
		inv.expired.Add(1)
		inv.lateShips.Add(1)
		return ShipRejected
	}
	r.State = Committed
	inv.resMu.Unlock()
	inv.shipped.Add(1)
	obs.ObserveBusinessEvent("shipment", "completed")
	return ShipCompleted
}

// Sweep releases each expired pending reservation at most once.
func (inv *Inventory) Sweep() int {
	now := time.Now()
	var stale [][]orders.Item
	inv.resMu.Lock()
	for _, r := range inv.reservations {
		if r.State == Reserved && !now.Before(r.ExpiresAt) {
			r.State = Released
			stale = append(stale, r.Items)
		}
	}
	inv.resMu.Unlock()
	for _, items := range stale {
		inv.restore(items)
		obs.ObserveBusinessEvent("reservation", "expired")
	}
	if len(stale) > 0 {
		inv.expired.Add(uint64(len(stale)))
		slog.Info("reclaimed expired reservations", "count", len(stale))
	}
	return len(stale)
}

// RunSweeper reclaims expired holds until done is closed.
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

// PendingReservations counts holds that have not reached a terminal state.
func (inv *Inventory) PendingReservations() int {
	inv.resMu.Lock()
	defer inv.resMu.Unlock()
	count := 0
	for _, r := range inv.reservations {
		if r.State == Reserved {
			count++
		}
	}
	return count
}

// ReceiptCount counts successful reservations, including terminal receipts.
func (inv *Inventory) ReceiptCount() int {
	inv.resMu.Lock()
	defer inv.resMu.Unlock()
	return len(inv.reservations)
}

// TrackedProducts counts lazily initialized SKU records.
func (inv *Inventory) TrackedProducts() int {
	inv.stockMu.RLock()
	defer inv.stockMu.RUnlock()
	return len(inv.stock)
}

// Stats returns process-local counters and current inventory gauges.
func (inv *Inventory) Stats() map[string]any {
	return map[string]any{"reserved": inv.reserved.Load(), "rejected": inv.rejected.Load(), "released": inv.released.Load(),
		"expired": inv.expired.Load(), "shipped": inv.shipped.Load(), "shipment_rejected": inv.lateShips.Load(),
		"oversold": inv.oversold.Load(), "pending_reservations": inv.PendingReservations(), "receipts": inv.ReceiptCount(),
		"tracked_products": inv.TrackedProducts(), "reservation_ttl": inv.ttl.String()}
}
func (inv *Inventory) counter(id string) *stockRecord {
	inv.stockMu.RLock()
	stock := inv.stock[id]
	inv.stockMu.RUnlock()
	if stock != nil {
		return stock
	}
	inv.stockMu.Lock()
	defer inv.stockMu.Unlock()
	if stock = inv.stock[id]; stock != nil {
		return stock
	}
	stock = &stockRecord{quantity: inv.initial}
	inv.stock[id] = stock
	return stock
}
func (inv *Inventory) restore(items []orders.Item) {
	stocks := make([]*stockRecord, len(items))
	for i, item := range items {
		stocks[i] = inv.counter(item.ProductID)
		stocks[i].mu.Lock()
	}
	for i, stock := range stocks {
		stock.quantity += int64(items[i].Quantity)
		stock.version++
	}
	for i := len(stocks) - 1; i >= 0; i-- {
		stocks[i].mu.Unlock()
	}
}
func cloneReservation(r *Reservation) *Reservation {
	c := *r
	c.Items = append([]orders.Item(nil), r.Items...)
	return &c
}
func sameItems(a, b []orders.Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func normalize(items []orders.Item) ([]orders.Item, error) {
	if len(items) == 0 {
		return nil, httpx.Errorf(http.StatusBadRequest, "items must not be empty")
	}
	totals := make(map[string]int, len(items))
	for _, item := range items {
		if item.ProductID == "" || item.Quantity <= 0 {
			return nil, httpx.Errorf(http.StatusBadRequest, "positive quantity and productId required")
		}
		if totals[item.ProductID] > math.MaxInt-item.Quantity {
			return nil, httpx.Errorf(http.StatusBadRequest, "quantity sum overflows")
		}
		totals[item.ProductID] += item.Quantity
	}
	ids := make([]string, 0, len(totals))
	for id := range totals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	merged := make([]orders.Item, 0, len(ids))
	for _, id := range ids {
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
