// Package identity owns customer accounts, credentials and sessions.
package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// Key prefixes inside the shared core database.
const (
	userKeyPrefix  = "user:"
	emailKeyPrefix = "user-email:"
)

// Redis key prefixes for session state.
const (
	refreshPrefix = "session:refresh:"
	revokedPrefix = "session:revoked:"
)

// DefaultBcryptCost is deliberately above the library default.
//
// Password hashing is supposed to be slow: the cost is paid once per login by
// one person, and multiplied by billions for anyone brute-forcing a stolen
// database. 12 lands around 250ms on current hardware, which is unnoticeable
// interactively and ruinous at scale.
const DefaultBcryptCost = 12

// User is an account. The hash never leaves this package.
type User struct {
	CustomerID   string    `json:"customerId"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"passwordHash"`
	Roles        []string  `json:"roles,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Profile is the public view of an account.
type Profile struct {
	CustomerID string    `json:"customerId"`
	Email      string    `json:"email"`
	Roles      []string  `json:"roles,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (u *User) profile() Profile {
	return Profile{CustomerID: u.CustomerID, Email: u.Email, Roles: u.Roles, CreatedAt: u.CreatedAt}
}

// Tokens is what a successful authentication returns.
type Tokens struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	TokenType    string    `json:"tokenType"`
	ExpiresIn    int       `json:"expiresIn"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Profile      Profile   `json:"profile"`
}

// Service implements registration, login, refresh and logout.
//
// Accounts live in the durable key-value cluster; sessions live in Redis. The
// split is deliberate: an account must survive a restart, whereas a session is
// short-lived by nature and wants a store with expiry built in.
type Service struct {
	db     *kv.Client
	redis  *redis.Client
	signer *auth.Signer

	refreshTTL time.Duration

	// bcryptCost is a field rather than a constant so tests can turn it down.
	// At the production cost a suite that registers a few dozen accounts
	// spends most of a minute hashing, and a slow suite is a suite people
	// stop running.
	bcryptCost int
}

// NewService wires the identity service to its stores.
func NewService(db *kv.Client, rdb *redis.Client, signer *auth.Signer, refreshTTL time.Duration) *Service {
	if refreshTTL <= 0 {
		refreshTTL = 7 * 24 * time.Hour
	}
	return &Service{
		db: db, redis: rdb, signer: signer,
		refreshTTL: refreshTTL, bcryptCost: DefaultBcryptCost,
	}
}

// WithBcryptCost overrides the password hashing cost. Intended for tests;
// lowering it in production makes a stolen database far cheaper to crack.
func (s *Service) WithBcryptCost(cost int) *Service {
	s.bcryptCost = cost
	return s
}

// Register creates an account and signs the customer in.
func (s *Service) Register(ctx context.Context, email, password string) (*Tokens, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		obs.ObserveAuth("register", "invalid")
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		obs.ObserveAuth("register", "invalid")
		return nil, err
	}

	if existing, err := s.lookupByEmail(ctx, email); err != nil {
		return nil, err
	} else if existing != nil {
		obs.ObserveAuth("register", "duplicate")
		return nil, httpx.Errorf(http.StatusConflict, "an account already exists for that email")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		return nil, httpx.Wrap(http.StatusInternalServerError, err, "could not hash the password")
	}

	user := &User{
		CustomerID:   newCustomerID(),
		Email:        email,
		PasswordHash: string(hash),
		Roles:        []string{"customer"},
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.saveUser(ctx, user); err != nil {
		return nil, err
	}
	// The email index points at the account. Written second so a crash between
	// the two leaves an orphaned account rather than an index entry pointing
	// at nothing, which is the harmless direction to fail in.
	if _, err := s.db.Put(ctx, emailKeyPrefix+email, user.CustomerID); err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "identity database unavailable")
	}

	obs.ObserveAuth("register", "created")
	slog.InfoContext(ctx, "account created", "customerId", user.CustomerID)
	return s.issue(ctx, user)
}

// Login authenticates a customer.
func (s *Service) Login(ctx context.Context, email, password string) (*Tokens, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		obs.ObserveAuth("login", "denied")
		return nil, errInvalidCredentials()
	}

	user, err := s.lookupByEmail(ctx, normalized)
	if err != nil {
		return nil, err
	}
	if user == nil {
		// Comparing against a dummy hash anyway keeps the response time for an
		// unknown email indistinguishable from a wrong password. Skipping it
		// turns the login endpoint into an account enumeration oracle.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		obs.ObserveAuth("login", "denied")
		return nil, errInvalidCredentials()
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		obs.ObserveAuth("login", "denied")
		return nil, errInvalidCredentials()
	}

	obs.ObserveAuth("login", "granted")
	return s.issue(ctx, user)
}

// Refresh exchanges a refresh token for a new pair.
//
// The old refresh token is destroyed in the process. Rotation means a stolen
// token is usable at most once, and its use by the thief immediately breaks
// the real customer's session — which is a detectable signal rather than a
// silent, permanent compromise.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	if refreshToken == "" {
		return nil, auth.Unauthorized("refresh token is required")
	}

	key := refreshPrefix + hashToken(refreshToken)
	customerID, err := s.redis.GetDel(ctx, key).Result()
	switch {
	case errors.Is(err, redis.Nil):
		obs.ObserveAuth("refresh", "denied")
		return nil, auth.Unauthorized("refresh token is not valid")
	case err != nil:
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "session store unavailable")
	}

	user, err := s.loadUser(ctx, customerID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		obs.ObserveAuth("refresh", "denied")
		return nil, auth.Unauthorized("refresh token is not valid")
	}

	obs.ObserveAuth("refresh", "granted")
	return s.issue(ctx, user)
}

// Logout ends a session immediately.
func (s *Service) Logout(ctx context.Context, sessionID, refreshToken string) error {
	if refreshToken != "" {
		if err := s.redis.Del(ctx, refreshPrefix+hashToken(refreshToken)).Err(); err != nil {
			slog.WarnContext(ctx, "could not delete the refresh token", "err", err)
		}
	}
	if sessionID != "" {
		// The access token cannot be recalled — it is already in the client's
		// hands and verifies on its signature alone. Denylisting its session
		// for the remainder of its lifetime is what makes logout immediate.
		if err := s.redis.Set(ctx, revokedPrefix+sessionID, "1", s.signer.TTL()).Err(); err != nil {
			return httpx.Wrap(http.StatusServiceUnavailable, err, "session store unavailable")
		}
	}
	obs.ObserveAuth("logout", "ok")
	return nil
}

// Profile returns an account's public view.
func (s *Service) Profile(ctx context.Context, customerID string) (*Profile, error) {
	user, err := s.loadUser(ctx, customerID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, httpx.Errorf(http.StatusNotFound, "no account for %s", customerID)
	}
	profile := user.profile()
	return &profile, nil
}

// RevocationCheck reports whether a session has been logged out. The gateway
// installs this on its verifier.
func (s *Service) RevocationCheck() func(context.Context, string) (bool, error) {
	return RevocationCheck(s.redis)
}

// RevocationCheck builds a denylist lookup against a Redis client, so the
// gateway can consult it without importing the identity service.
func RevocationCheck(rdb *redis.Client) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, sessionID string) (bool, error) {
		if rdb == nil || sessionID == "" {
			return false, nil
		}
		n, err := rdb.Exists(ctx, revokedPrefix+sessionID).Result()
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
}

// ── internals ────────────────────────────────────────────────────────────────

func (s *Service) issue(ctx context.Context, user *User) (*Tokens, error) {
	sessionID := newSessionID()

	accessToken, claims, err := s.signer.Issue(user.CustomerID, user.Email, user.Roles, sessionID)
	if err != nil {
		return nil, httpx.Wrap(http.StatusInternalServerError, err, "could not issue a token")
	}

	refreshToken := newRefreshToken()
	// Only a hash of the refresh token is stored. A leak of the session store
	// then yields nothing usable, exactly as with password hashes.
	err = s.redis.Set(ctx, refreshPrefix+hashToken(refreshToken), user.CustomerID, s.refreshTTL).Err()
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "session store unavailable")
	}

	return &Tokens{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(s.signer.TTL().Seconds()),
		ExpiresAt:    claims.ExpiresAt.Time,
		Profile:      user.profile(),
	}, nil
}

func (s *Service) lookupByEmail(ctx context.Context, email string) (*User, error) {
	entry, found, err := s.db.Get(ctx, emailKeyPrefix+email)
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "identity database unavailable")
	}
	if !found || entry.Value == "" {
		return nil, nil
	}
	return s.loadUser(ctx, entry.Value)
}

func (s *Service) loadUser(ctx context.Context, customerID string) (*User, error) {
	if customerID == "" {
		return nil, nil
	}
	entry, found, err := s.db.Get(ctx, userKeyPrefix+customerID)
	if err != nil {
		return nil, httpx.Wrap(http.StatusServiceUnavailable, err, "identity database unavailable")
	}
	if !found {
		return nil, nil
	}

	var user User
	if err := json.Unmarshal([]byte(entry.Value), &user); err != nil {
		return nil, httpx.Wrap(http.StatusInternalServerError, err, "account record is unreadable")
	}
	return &user, nil
}

func (s *Service) saveUser(ctx context.Context, user *User) error {
	raw, err := json.Marshal(user)
	if err != nil {
		return httpx.Wrap(http.StatusInternalServerError, err, "encoding the account")
	}
	if _, err := s.db.Put(ctx, userKeyPrefix+user.CustomerID, string(raw)); err != nil {
		return httpx.Wrap(http.StatusServiceUnavailable, err, "identity database unavailable")
	}
	return nil
}

// dummyHash is compared against when an email is unknown, so that path costs
// the same as a real password check.
var dummyHash = []byte("$2a$12$C6UzMDM.H6dfI/f/IKcEe.7bIYQe1J1eF/e0i1kZ0lXG3.6Vy0aLK")

func errInvalidCredentials() error {
	// One message for both "no such account" and "wrong password". Any
	// difference between them tells an attacker which emails are registered.
	return httpx.Errorf(http.StatusUnauthorized, "email or password is incorrect")
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", httpx.Errorf(http.StatusBadRequest, "email is required")
	}
	if len(email) > 254 {
		return "", httpx.Errorf(http.StatusBadRequest, "email is too long")
	}
	at := strings.IndexByte(email, '@')
	if at < 1 || at == len(email)-1 || strings.Contains(email, " ") {
		return "", httpx.Errorf(http.StatusBadRequest, "email is not a valid address")
	}
	return email, nil
}

func validatePassword(password string) error {
	// A length floor and nothing else. Composition rules push people towards
	// predictable substitutions and shorter passwords, which is the opposite
	// of what they are meant to achieve.
	if len(password) < 10 {
		return httpx.Errorf(http.StatusBadRequest, "password must be at least 10 characters")
	}
	if len(password) > 1024 {
		// bcrypt only reads the first 72 bytes anyway; the cap is to stop a
		// caller making the server hash a megabyte.
		return httpx.Errorf(http.StatusBadRequest, "password is too long")
	}
	return nil
}

func newCustomerID() string   { return "cust-" + randomToken(8) }
func newSessionID() string    { return "sess-" + randomToken(8) }
func newRefreshToken() string { return randomToken(32) }

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken stores a refresh token by digest rather than in the clear.
func hashToken(token string) string {
	sum := sha256Sum(token)
	return hex.EncodeToString(sum[:])
}
