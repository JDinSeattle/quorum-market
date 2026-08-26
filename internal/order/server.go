package order

import (
	"net/http"
	"strconv"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Server is the order service's HTTP surface.
type Server struct {
	svc *Service
}

// NewServer returns a Server over svc.
func NewServer(svc *Service) *Server { return &Server{svc: svc} }

type cancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// Routes builds the order service's HTTP surface.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /orders/health")
	rt.Handle("GET /orders", s.handleList)
	rt.Handle("GET /orders/{orderId}", s.handleGet)
	rt.Handle("POST /orders/{orderId}/cancel", s.handleCancel)
	return rt.Build(httpx.DefaultMaxInFlight())
}

// Subscriptions maps event types to handlers, so the wiring lives next to the
// service that reacts to them rather than in main.
func (s *Server) Subscriptions() map[events.Type]events.Handler {
	return map[events.Type]events.Handler{
		events.OrderPlaced:  s.svc.HandlePlaced,
		events.OrderShipped: s.svc.HandleShipped,
	}
}

// customerOf reads the caller's identity from the header the gateway sets.
//
// This service trusts that header because it is not publicly reachable: the
// gateway is the only way in, and it strips any inbound copy before setting
// its own from a verified token. The trust is in the network boundary, and it
// is only as good as that boundary — which is why the services are in a
// private subnet and the gateway is the sole target of the load balancer.
func customerOf(r *http.Request) (string, error) {
	customerID := r.Header.Get(auth.CustomerIDHeader)
	if customerID == "" {
		return "", auth.Unauthorized("this endpoint requires an authenticated customer")
	}
	return customerID, nil
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) error {
	customerID, err := customerOf(r)
	if err != nil {
		return err
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	records, err := s.svc.List(r.Context(), customerID, limit)
	if err != nil {
		return err
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"customerId": customerID,
		"count":      len(records),
		"orders":     records,
	})
	return nil
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) error {
	customerID, err := customerOf(r)
	if err != nil {
		return err
	}

	record, err := s.svc.Get(r.Context(), customerID, r.PathValue("orderId"))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, record)
	return nil
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) error {
	customerID, err := customerOf(r)
	if err != nil {
		return err
	}

	var req cancelRequest
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &req); err != nil {
			return err
		}
	}

	record, err := s.svc.Cancel(r.Context(), customerID, r.PathValue("orderId"), req.Reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, record)
	return nil
}
