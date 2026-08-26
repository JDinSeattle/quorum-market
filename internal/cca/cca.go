// Package cca implements the mock credit card authorizer.
//
// It stands in for a real payment gateway: requests are slow, most succeed,
// and a predictable minority are declined so the checkout flow's abort path is
// exercised continuously under load rather than only in a unit test.
package cca

import (
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// DefaultApprovalRate is the share of well-formed cards that are approved.
const DefaultApprovalRate = 0.9

// Server is the authorizer's HTTP surface.
type Server struct {
	approvalRate float64
	delay        busywait.Config
}

// NewServer returns an authorizer approving approvalRate of well-formed cards.
func NewServer(approvalRate float64, delay busywait.Config) *Server {
	if approvalRate < 0 {
		approvalRate = 0
	}
	if approvalRate > 1 {
		approvalRate = 1
	}
	return &Server{approvalRate: approvalRate, delay: delay}
}

// AuthorizeRequest is the body of POST /credit-card-authorizer/authorize.
type AuthorizeRequest struct {
	CardNumber string  `json:"credit_card_number"`
	Amount     float64 `json:"amount"`
	OrderID    string  `json:"order_id,omitempty"`
}

// AuthorizeResponse is returned for both approvals and declines; the HTTP
// status is what the caller branches on.
type AuthorizeResponse struct {
	Authorized bool    `json:"authorized"`
	Code       string  `json:"authorization_code,omitempty"`
	Amount     float64 `json:"amount,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// Routes builds the authorizer's HTTP surface.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /credit-card-authorizer/health")
	rt.Handle("POST /credit-card-authorizer/authorize", s.handleAuthorize)
	return rt.Build(httpx.DefaultMaxInFlight())
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) error {
	var req AuthorizeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	s.delay.Simulate()

	// A malformed card is a client error (400) and is distinct from a
	// well-formed card the issuer refuses (402). The checkout flow treats them
	// differently, so the authorizer has to keep them apart.
	if !ValidCardNumber(req.CardNumber) {
		httpx.JSON(w, http.StatusBadRequest, AuthorizeResponse{
			Authorized: false,
			Reason:     "credit_card_number must be 16 digits",
		})
		return nil
	}

	if rand.Float64() >= s.approvalRate {
		httpx.JSON(w, http.StatusPaymentRequired, AuthorizeResponse{
			Authorized: false,
			Reason:     "credit card declined",
		})
		return nil
	}

	httpx.JSON(w, http.StatusOK, AuthorizeResponse{
		Authorized: true,
		Code:       "AUTH-" + time.Now().UTC().Format("20060102150405.000"),
		Amount:     req.Amount,
	})
	return nil
}

// ValidCardNumber reports whether s is 16 digits, ignoring the spaces and
// dashes people and load generators put between groups.
func ValidCardNumber(s string) bool {
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '-' || r == ' ':
		default:
			return false
		}
	}
	return digits == 16 && strings.TrimSpace(s) != ""
}
