package identity

import (
	"net/http"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Server is the identity service's HTTP surface.
type Server struct {
	svc      *Service
	verifier *auth.Verifier
}

// NewServer returns a Server. The verifier is used only on /me, where the
// caller presents its own token rather than being introduced by the gateway.
func NewServer(svc *Service, verifier *auth.Verifier) *Server {
	return &Server{svc: svc, verifier: verifier}
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type logoutRequest struct {
	RefreshToken string `json:"refreshToken,omitempty"`
}

// Routes builds the identity service's HTTP surface.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /identity/health")
	rt.Handle("POST /identity/register", s.handleRegister)
	rt.Handle("POST /identity/login", s.handleLogin)
	rt.Handle("POST /identity/refresh", s.handleRefresh)
	rt.Handle("POST /identity/logout", s.handleLogout)
	rt.Handle("GET /identity/me", s.handleMe)
	return rt.Build(httpx.DefaultMaxInFlight())
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) error {
	var req credentials
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	tokens, err := s.svc.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, tokens)
	return nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	var req credentials
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	tokens, err := s.svc.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, tokens)
	return nil
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) error {
	var req refreshRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	tokens, err := s.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, tokens)
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	var req logoutRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	// The session id comes from the presented access token, so a caller can
	// only ever end its own session.
	var sessionID string
	if token, ok := auth.BearerToken(r); ok {
		if claims, err := s.verifier.Verify(r.Context(), token); err == nil {
			sessionID = claims.ID
		}
	}

	if err := s.svc.Logout(r.Context(), sessionID, req.RefreshToken); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "logged out"})
	return nil
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	token, ok := auth.BearerToken(r)
	if !ok {
		return auth.Unauthorized("a bearer token is required")
	}

	claims, err := s.verifier.Verify(r.Context(), token)
	if err != nil {
		return auth.Unauthorized("the token is not valid")
	}

	profile, err := s.svc.Profile(r.Context(), claims.CustomerID())
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, profile)
	return nil
}
