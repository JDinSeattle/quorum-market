package notification

import (
	"net/http"
	"strconv"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/events"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Server is the notification service's HTTP surface.
type Server struct {
	svc *Service
}

// NewServer returns a Server over svc.
func NewServer(svc *Service) *Server { return &Server{svc: svc} }

// Routes builds the notification service's HTTP surface.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /notifications/health")
	rt.Handle("GET /notifications", s.handleInbox)
	return rt.Build(httpx.DefaultMaxInFlight())
}

// Patterns are the routing keys this service subscribes to.
//
// A wildcard rather than a list of specific types: a notification service
// should hear about everything that happens to an order, and binding to
// order.* means a new order event reaches it without a topology change.
func Patterns() []string { return []string{"order.*"} }

// Subscribe returns the handler for every event this service consumes.
func (s *Server) Subscribe() events.Handler { return s.svc.Handle }

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) error {
	customerID := r.Header.Get(auth.CustomerIDHeader)
	if customerID == "" {
		return auth.Unauthorized("this endpoint requires an authenticated customer")
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	messages, err := s.svc.Inbox(r.Context(), customerID, limit)
	if err != nil {
		return err
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"customerId":    customerID,
		"count":         len(messages),
		"notifications": messages,
	})
	return nil
}
