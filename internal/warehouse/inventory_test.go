package warehouse

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/orders"
)

func items(pairs ...any) []orders.Item {
	out := make([]orders.Item, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, orders.Item{ProductID: pairs[i].(string), Quantity: pairs[i+1].(int)})
	}
	return out
}

func TestQuantitySeedsUnknownProducts(t *testing.T) {
	inv := New(100, DefaultTTL)
	if got := inv.Quantity("p1"); got != 100 {
		t.Fatalf("Quantity(p1) = %d, want 100", got)
	}
}

func TestReserveDeductsStock(t *testing.T) {
	inv := New(100, DefaultTTL)

	res, err := inv.Reserve(items("p1", 3, "p2", 5))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.ID == "" {
		t.Fatal("Reserve returned an empty reservation id")
	}
	if got := inv.Quantity("p1"); got != 97 {
		t.Errorf("p1 = %d, want 97", got)
	}
	if got := inv.Quantity("p2"); got != 95 {
		t.Errorf("p2 = %d, want 95", got)
	}
}

func TestReserveIsAllOrNothing(t *testing.T) {
	inv := New(10, DefaultTTL)

	// p1 succeeds, p2 cannot: p1 must be handed back before returning.
	_, err := inv.Reserve(items("p1", 4, "p2", 99))
	if err == nil {
		t.Fatal("Reserve succeeded despite insufficient stock for p2")
	}

	var stockErr *InsufficientStockError
	if !errors.As(err, &stockErr) {
		t.Fatalf("error = %v, want *InsufficientStockError", err)
	}
	if stockErr.ProductID != "p2" {
		t.Errorf("failed product = %q, want p2", stockErr.ProductID)
	}
	if got := inv.Quantity("p1"); got != 10 {
		t.Errorf("p1 = %d, want 10: the partial reservation was not rolled back", got)
	}
	if got := inv.PendingReservations(); got != 0 {
		t.Errorf("pending reservations = %d, want 0", got)
	}
}

func TestReserveMergesDuplicateProducts(t *testing.T) {
	inv := New(10, DefaultTTL)

	res, err := inv.Reserve(items("p1", 3, "p1", 4))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Quantity != 7 {
		t.Fatalf("reservation items = %+v, want a single line of 7", res.Items)
	}
	if got := inv.Quantity("p1"); got != 3 {
		t.Errorf("p1 = %d, want 3", got)
	}
}

func TestReserveRejectsBadInput(t *testing.T) {
	inv := New(10, DefaultTTL)

	for name, in := range map[string][]orders.Item{
		"empty":         {},
		"zero quantity": items("p1", 0),
		"negative":      items("p1", -2),
		"missing id":    {{ProductID: "", Quantity: 1}},
	} {
		if _, err := inv.Reserve(in); err == nil {
			t.Errorf("Reserve(%s) succeeded, want an error", name)
		}
	}
}

// A released reservation must not also be reclaimed by the expiry sweeper.
// Restoring the same units twice would let stock grow every time a payment is
// declined, which over a load test inflates the catalogue without bound.
func TestReleaseAndExpiryDoNotBothRestore(t *testing.T) {
	inv := New(100, time.Nanosecond) // every reservation is immediately expired

	res, err := inv.Reserve(items("p1", 10))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got := inv.Quantity("p1"); got != 90 {
		t.Fatalf("after reserve p1 = %d, want 90", got)
	}

	if !inv.Release(res.ID) {
		t.Fatal("Release reported the reservation was not live")
	}
	if got := inv.Quantity("p1"); got != 100 {
		t.Fatalf("after release p1 = %d, want 100", got)
	}

	if reclaimed := inv.Sweep(); reclaimed != 0 {
		t.Errorf("sweep reclaimed %d reservations, want 0: the release already resolved it", reclaimed)
	}
	if got := inv.Quantity("p1"); got != 100 {
		t.Errorf("after sweep p1 = %d, want 100: stock was restored twice", got)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	inv := New(100, DefaultTTL)

	res, _ := inv.Reserve(items("p1", 10))
	if !inv.Release(res.ID) {
		t.Fatal("first Release reported the reservation was not live")
	}
	if inv.Release(res.ID) {
		t.Error("second Release reported the reservation was live")
	}
	if got := inv.Quantity("p1"); got != 100 {
		t.Errorf("p1 = %d, want 100: a repeated release inflated stock", got)
	}
}

// Shipping consumes the reservation made at checkout. Stock came off the shelf
// when the reservation was taken, so shipping must not take it off again.
func TestShipDoesNotDeductTwice(t *testing.T) {
	inv := New(100, DefaultTTL)

	res, _ := inv.Reserve(items("p1", 10))
	if got := inv.Ship(res.ID, res.Items); got != ShipCompleted {
		t.Fatalf("Ship outcome = %q, want %q", got, ShipCompleted)
	}
	if got := inv.Quantity("p1"); got != 90 {
		t.Errorf("p1 = %d, want 90: shipping deducted the same units twice", got)
	}
	if got := inv.PendingReservations(); got != 0 {
		t.Errorf("pending reservations = %d, want 0", got)
	}
}

func TestShipAfterExpiryCompensates(t *testing.T) {
	inv := New(100, time.Nanosecond)

	res, _ := inv.Reserve(items("p1", 10))
	if reclaimed := inv.Sweep(); reclaimed != 1 {
		t.Fatalf("sweep reclaimed %d reservations, want 1", reclaimed)
	}
	if got := inv.Quantity("p1"); got != 100 {
		t.Fatalf("after expiry p1 = %d, want 100", got)
	}

	// The goods still shipped, so the units have to come off now.
	if got := inv.Ship(res.ID, res.Items); got != ShipCompensated {
		t.Fatalf("Ship outcome = %q, want %q", got, ShipCompensated)
	}
	if got := inv.Quantity("p1"); got != 90 {
		t.Errorf("p1 = %d, want 90", got)
	}
}

func TestCompensationClampsAtZero(t *testing.T) {
	inv := New(5, DefaultTTL)

	inv.Ship("unknown-reservation", items("p1", 12))
	if got := inv.Quantity("p1"); got != 0 {
		t.Errorf("p1 = %d, want 0: stock went negative", got)
	}
	if got := inv.Stats()["oversold"].(uint64); got != 1 {
		t.Errorf("oversold = %d, want 1", got)
	}
}

func TestSweepReclaimsOnlyExpired(t *testing.T) {
	inv := New(100, time.Hour)

	fresh, _ := inv.Reserve(items("p1", 5))
	stale, _ := inv.Reserve(items("p2", 7))

	// Age one reservation past its hold without waiting an hour.
	inv.resMu.Lock()
	inv.reservations[stale.ID].ExpiresAt = time.Now().Add(-time.Minute)
	inv.resMu.Unlock()

	if reclaimed := inv.Sweep(); reclaimed != 1 {
		t.Fatalf("sweep reclaimed %d, want 1", reclaimed)
	}
	if got := inv.Quantity("p2"); got != 100 {
		t.Errorf("p2 = %d, want 100", got)
	}
	if got := inv.Quantity("p1"); got != 95 {
		t.Errorf("p1 = %d, want 95: a live reservation was reclaimed", got)
	}
	if inv.Release(fresh.ID) != true {
		t.Error("the live reservation is no longer releasable")
	}
}

// Concurrent checkouts for the same product must never oversell it. This is
// the property the compare-and-swap loop exists for.
func TestConcurrentReservesNeverOversell(t *testing.T) {
	const stock = 100
	inv := New(stock, DefaultTTL)

	var granted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < stock*3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := inv.Reserve(items("p1", 1)); err == nil {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := granted.Load(); got != stock {
		t.Errorf("granted %d reservations, want exactly %d", got, stock)
	}
	if got := inv.Quantity("p1"); got != 0 {
		t.Errorf("p1 = %d, want 0", got)
	}
}

// Reserving and releasing concurrently must conserve stock exactly.
func TestConcurrentReserveReleaseConservesStock(t *testing.T) {
	const stock = 500
	inv := New(stock, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := inv.Reserve(items("p1", 2))
			if err != nil {
				return
			}
			inv.Release(res.ID)
		}()
	}
	wg.Wait()

	if got := inv.Quantity("p1"); got != stock {
		t.Errorf("p1 = %d, want %d", got, stock)
	}
	if got := inv.PendingReservations(); got != 0 {
		t.Errorf("pending reservations = %d, want 0", got)
	}
}
