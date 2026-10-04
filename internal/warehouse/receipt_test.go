package warehouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/orders"
	"github.com/JDinSeattle/quorum-market/internal/rmq"
)

// One thousand distinct intents, not retries counted as new reservations.
func TestThousandRequestsAndThreeHundredReceiptReplays(t *testing.T) {
	inv := New(100, time.Hour)
	start := make(chan struct{})
	successes := make(chan *Reservation, 1000)
	var soldOut, unexpected atomic.Int64
	var wg sync.WaitGroup
	for client := range 100 {
		wg.Go(func() {
			<-start
			for request := range 10 {
				id := fmt.Sprintf("client-%d-request-%d", client, request)
				r, err := inv.ReserveWithRequestID(id, items("only-sku", 1))
				if err == nil {
					successes <- r
					continue
				}
				var short *InsufficientStockError
				if errors.As(err, &short) {
					soldOut.Add(1)
				} else {
					unexpected.Add(1)
				}
			}
		})
	}
	close(start)
	wg.Wait()
	close(successes)
	if len(successes) != 100 || soldOut.Load() != 900 || unexpected.Load() != 0 {
		t.Fatalf("success=%d soldout=%d unexpected=%d", len(successes), soldOut.Load(), unexpected.Load())
	}
	reservations := make([]*Reservation, 0, 100)
	for r := range successes {
		reservations = append(reservations, r)
	}
	for _, r := range reservations {
		for range 3 {
			wg.Go(func() {
				replay, err := inv.ReserveWithRequestID(r.RequestID, items("only-sku", 1))
				if err != nil || replay.ID != r.ID {
					t.Errorf("replay %+v %v", replay, err)
				}
			})
		}
	}
	wg.Wait()
	quantity, version := inv.Snapshot("only-sku")
	if quantity != 0 || version != 100 || inv.ReceiptCount() != 100 || inv.Stats()["reserved"].(uint64) != 100 {
		t.Fatalf("quantity=%d version=%d receipts=%d stats=%v", quantity, version, inv.ReceiptCount(), inv.Stats())
	}

	var releases atomic.Int64
	for _, r := range reservations[:20] {
		for range 5 {
			wg.Go(func() {
				if inv.Release(r.ID) {
					releases.Add(1)
				}
			})
		}
	}
	for _, r := range reservations[20:] {
		wg.Go(func() {
			if outcome := inv.Ship(r.ID, r.Items); outcome != ShipCompleted {
				t.Errorf("commit %s", outcome)
			}
		})
	}
	wg.Wait()
	if releases.Load() != 20 || inv.Quantity("only-sku") != 20 || inv.Stats()["shipped"].(uint64) != 80 {
		t.Fatalf("releases=%d stats=%v stock=%d", releases.Load(), inv.Stats(), inv.Quantity("only-sku"))
	}
	for _, r := range reservations[:20] {
		if inv.Ship(r.ID, r.Items) != ShipRejected {
			t.Fatal("released reservation was committed")
		}
	}
	for _, r := range reservations[20:] {
		for range 3 {
			if inv.Ship(r.ID, r.Items) != ShipDuplicate {
				t.Fatal("commit replay lost terminal receipt")
			}
		}
		if inv.Release(r.ID) {
			t.Fatal("committed reservation released")
		}
	}
	if inv.Quantity("only-sku") != 20 || inv.PendingReservations() != 0 || inv.ReceiptCount() != 100 {
		t.Fatal("terminal replay changed conservation")
	}
}

func TestSameRequestContendersShareOneImmutableReceipt(t *testing.T) {
	inv := New(10, time.Hour)
	start := make(chan struct{})
	results := make(chan *Reservation, 64)
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			<-start
			r, e := inv.ReserveWithRequestID("same", items("b", 1, "a", 2))
			if e != nil {
				t.Error(e)
				return
			}
			results <- r
		})
	}
	close(start)
	wg.Wait()
	close(results)
	id := ""
	for r := range results {
		if id == "" {
			id = r.ID
		}
		if r.ID != id {
			t.Fatal("distinct receipts")
		}
		r.Items[0].Quantity = 999
	}
	replay, e := inv.ReserveWithRequestID("same", items("a", 1, "a", 1, "b", 1))
	if e != nil || replay.Items[0].Quantity != 2 || inv.Quantity("a") != 8 || inv.ReceiptCount() != 1 {
		t.Fatalf("mutable or noncanonical receipt: %+v %v", replay, e)
	}
	_, e = inv.ReserveWithRequestID("same", items("a", 3))
	var api *httpx.APIError
	if !errors.As(e, &api) || api.Status != 409 {
		t.Fatalf("content conflict: %v", e)
	}
	if inv.Ship(id, items("a", 2, "b", 2)) != ShipRejected || inv.PendingReservations() != 1 {
		t.Fatal("mismatched ship resolved receipt")
	}
	if inv.Release(id) != true {
		t.Fatal("release failed")
	}
	replay, e = inv.ReserveWithRequestID("same", items("a", 2, "b", 1))
	if e != nil || replay.State != Released || inv.Quantity("a") != 10 {
		t.Fatal("terminal request was reopened")
	}
}

func TestHTTPReceiptAndShipperFailClosed(t *testing.T) {
	inv := New(10, time.Hour)
	srv := httptest.NewServer(NewServer(inv, busywait.Config{}, nil).Routes())
	defer srv.Close()
	client := NewClient(srv.URL, time.Second, time.Second)
	a, e := client.ReserveWithRequestID(context.Background(), "http-replay", items("sku", 1))
	if e != nil {
		t.Fatal(e)
	}
	b, e := client.ReserveWithRequestID(context.Background(), "http-replay", items("sku", 1))
	if e != nil || a.ReservationID != b.ReservationID || a.RequestID != "http-replay" {
		t.Fatalf("HTTP replay %v %+v %+v", e, a, b)
	}
	_, e = client.ReserveWithRequestID(context.Background(), "", items("sku", 1))
	var api *httpx.APIError
	if !errors.As(e, &api) || api.Status != http.StatusBadRequest {
		t.Fatalf("missing request ID: %v", e)
	}
	shipper := NewShipper(inv, nil)
	message := orders.ShipMessage{OrderID: "o", ReservationID: a.ReservationID, Items: items("sku", 1)}
	raw, _ := json.Marshal(message)
	for range 3 {
		if e = shipper.Handle(context.Background(), raw); e != nil {
			t.Fatal(e)
		}
	}
	if inv.Quantity("sku") != 9 || inv.Stats()["shipped"].(uint64) != 1 {
		t.Fatal("queue replay decremented stock")
	}
	message.ReservationID = "unknown"
	raw, _ = json.Marshal(message)
	if e = shipper.Handle(context.Background(), raw); !errors.Is(e, rmq.ErrDrop) {
		t.Fatalf("unknown ship did not fail closed: %v", e)
	}
	if inv.Quantity("sku") != 9 {
		t.Fatal("unknown message changed stock")
	}
}

func TestOppositeTerminalRacesAndOverflow(t *testing.T) {
	inv := New(1000, time.Hour)
	for round := range 100 {
		r, e := inv.ReserveWithRequestID(fmt.Sprintf("race-%d", round), items("sku", 1))
		if e != nil {
			t.Fatal(e)
		}
		var wg sync.WaitGroup
		var released bool
		var outcome ShipOutcome
		wg.Go(func() { released = inv.Release(r.ID) })
		wg.Go(func() { outcome = inv.Ship(r.ID, r.Items) })
		wg.Wait()
		if released == (outcome == ShipCompleted) {
			t.Fatalf("both or neither terminal states: release=%v ship=%s", released, outcome)
		}
	}
	committed := int(inv.Stats()["shipped"].(uint64))
	if inv.Quantity("sku")+committed != 1000 {
		t.Fatal("terminal race lost stock")
	}
	if _, e := inv.ReserveWithRequestID("overflow", items("sku", math.MaxInt, "sku", 1)); e == nil {
		t.Fatal("overflow accepted")
	}
}
