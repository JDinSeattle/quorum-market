package product

import (
	"net/http"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Server is the catalogue's HTTP surface.
type Server struct {
	svc   *Service
	delay busywait.Config
}

// NewServer returns a Server for svc.
func NewServer(svc *Service, delay busywait.Config) *Server {
	return &Server{svc: svc, delay: delay}
}

// Routes builds the catalogue's HTTP surface. Every path lives under /product
// so the load balancer can route to this service with a single path pattern.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /product/health")
	rt.Handle("GET /product/{productId}", s.handleGet)
	rt.Handle("PUT /product/{productId}", s.handlePut)
	return rt.Build(httpx.DefaultMaxInFlight())
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) error {
	s.delay.Simulate()

	p, err := s.svc.Get(r.Context(), r.PathValue("productId"))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

// handlePut deliberately skips the simulated delay. Catalogue writes come from
// the bulk loader during setup, not from customer traffic, and slowing them
// down only makes seeding the cluster take longer.
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) error {
	var p Product
	if err := httpx.DecodeJSON(r, &p); err != nil {
		return err
	}
	// The path is authoritative, so a mismatched body cannot write to a
	// different product than the URL names.
	p.ProductID = r.PathValue("productId")

	if err := s.svc.Put(r.Context(), p); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, p)
	return nil
}
