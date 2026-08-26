package warehouse

import (
	"errors"
	"net/http"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/orders"
)

// Server is the warehouse's HTTP surface.
type Server struct {
	inv   *Inventory
	delay busywait.Config
	stats func() map[string]any
}

// NewServer returns a Server over inv. extraStats, when non-nil, is merged
// into the /warehouse/stats response so the queue consumer can report through
// the same endpoint.
func NewServer(inv *Inventory, delay busywait.Config, extraStats func() map[string]any) *Server {
	return &Server{inv: inv, delay: delay, stats: extraStats}
}

// Routes builds the warehouse's HTTP surface.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /warehouse/health")
	rt.Handle("GET /warehouse/stats", s.handleStats)
	rt.Handle("GET /warehouse/inventory/{productId}", s.handleInventory)
	rt.Handle("POST /warehouse/reserve", s.handleReserve)
	rt.Handle("POST /warehouse/release", s.handleRelease)
	return rt.Build(httpx.DefaultMaxInFlight())
}

func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) error {
	s.delay.Simulate()

	productID := r.PathValue("productId")
	if productID == "" {
		return httpx.Errorf(http.StatusBadRequest, "productId is required")
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"productId": productID,
		"quantity":  s.inv.Quantity(productID),
	})
	return nil
}

func (s *Server) handleReserve(w http.ResponseWriter, r *http.Request) error {
	var req orders.ReserveRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	s.delay.Simulate()

	reservation, err := s.inv.Reserve(req.Items)
	if err != nil {
		var stockErr *InsufficientStockError
		if errors.As(err, &stockErr) {
			// 409 rather than 400: the request is well formed, it just lost a
			// race for stock the caller had every reason to expect was there.
			httpx.JSON(w, http.StatusConflict, map[string]any{
				"error":     stockErr.Error(),
				"productId": stockErr.ProductID,
				"available": stockErr.Available,
			})
			return nil
		}
		return err
	}

	httpx.JSON(w, http.StatusOK, orders.ReserveResponse{
		ReservationID: reservation.ID,
		Status:        "reserved",
		ExpiresAt:     reservation.ExpiresAt,
	})
	return nil
}

// handleRelease deliberately skips the simulated delay. It runs on the
// checkout failure path, where the customer is already waiting on a rejection;
// slowing it down holds stock longer for no benefit.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) error {
	var req orders.ReleaseRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.ReservationID == "" {
		return httpx.Errorf(http.StatusBadRequest, "reservationId is required")
	}

	// A reservation that has already expired or been released is reported, not
	// rejected: the caller's intent — "this stock should not be held" — is
	// satisfied either way, and a retry must stay safe.
	released := s.inv.Release(req.ReservationID)
	status := "released"
	if !released {
		status = "already-resolved"
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"reservationId": req.ReservationID,
		"status":        status,
	})
	return nil
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) error {
	out := s.inv.Stats()
	if s.stats != nil {
		out["queue"] = s.stats()
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}
