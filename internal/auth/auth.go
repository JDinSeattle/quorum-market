// Package auth issues and verifies the access tokens that identify a customer
// to the rest of the system.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Headers the gateway sets after verifying a token, and that internal services
// read instead of parsing tokens themselves.
const (
	CustomerIDHeader    = "X-Customer-Id"
	CustomerEmailHeader = "X-Customer-Email"
	CustomerRolesHeader = "X-Customer-Roles"
)

// MinSecretLength is the shortest signing secret accepted.
//
// HS256 with a short secret is brute-forceable offline: an attacker with one
// valid token can recover the key and then mint any identity they like. 32
// bytes matches the HMAC output size, past which extra length buys nothing.
const MinSecretLength = 32

// ErrUnauthenticated is returned when a token is missing, malformed, expired,
// or signed with the wrong key. The reasons are deliberately not distinguished
// to the caller: telling an attacker *why* their token failed is free help.
var ErrUnauthenticated = errors.New("auth: not authenticated")

// Claims is the token payload.
type Claims struct {
	jwt.RegisteredClaims
	Email string   `json:"email,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

// CustomerID returns the subject, which is the customer this token speaks for.
func (c *Claims) CustomerID() string { return c.Subject }

// HasRole reports whether the token carries a role.
func (c *Claims) HasRole(role string) bool {
	for _, held := range c.Roles {
		if held == role {
			return true
		}
	}
	return false
}

// Signer mints access tokens. Only the identity service holds one.
type Signer struct {
	secret []byte
	issuer string
	ttl    time.Duration
}

// NewSigner returns a Signer, refusing a secret too short to be safe.
func NewSigner(secret, issuer string, ttl time.Duration) (*Signer, error) {
	if len(secret) < MinSecretLength {
		return nil, fmt.Errorf("auth: signing secret must be at least %d bytes, got %d",
			MinSecretLength, len(secret))
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &Signer{secret: []byte(secret), issuer: issuer, ttl: ttl}, nil
}

// TTL is how long the tokens this Signer issues remain valid.
func (s *Signer) TTL() time.Duration { return s.ttl }

// Issue mints an access token for a customer.
//
// Access tokens are short-lived and are not checked against any store on the
// read path — that is the entire point of signing them, and it is what lets
// the gateway authenticate a request without a network round trip. The cost is
// that revocation is not instant, which is why the lifetime is minutes and why
// a session denylist exists for the cases that cannot wait.
func (s *Signer) Issue(customerID, email string, roles []string, sessionID string) (string, *Claims, error) {
	now := time.Now()
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   customerID,
			Issuer:    s.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
			// The session id lets a specific token be revoked without
			// invalidating every token the customer holds.
			ID: sessionID,
		},
		Email: email,
		Roles: roles,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", nil, fmt.Errorf("auth: signing token: %w", err)
	}
	return signed, claims, nil
}

// Verifier checks tokens. The gateway holds one; so does anything that needs
// to authenticate without calling the identity service.
type Verifier struct {
	secret []byte
	issuer string

	// revoked reports whether a session has been ended early. It is optional:
	// without it, revocation waits for the token to expire.
	revoked func(ctx context.Context, sessionID string) (bool, error)
}

// NewVerifier returns a Verifier.
func NewVerifier(secret, issuer string) (*Verifier, error) {
	if len(secret) < MinSecretLength {
		return nil, fmt.Errorf("auth: signing secret must be at least %d bytes, got %d",
			MinSecretLength, len(secret))
	}
	return &Verifier{secret: []byte(secret), issuer: issuer}, nil
}

// WithRevocationCheck attaches a denylist lookup, so a logged-out session stops
// working before its token would have expired.
func (v *Verifier) WithRevocationCheck(check func(ctx context.Context, sessionID string) (bool, error)) *Verifier {
	v.revoked = check
	return v
}

// Verify parses and validates a token.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	claims := &Claims{}

	parsed, err := jwt.ParseWithClaims(token, claims,
		func(t *jwt.Token) (any, error) {
			// Pinning the algorithm is not optional. Without it a token can
			// declare alg=none, or declare HMAC against a service expecting
			// RSA and have the public key used as an HMAC secret — both are
			// complete authentication bypasses.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
			}
			return v.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !parsed.Valid {
		return nil, ErrUnauthenticated
	}
	if claims.Subject == "" {
		return nil, ErrUnauthenticated
	}

	if v.revoked != nil && claims.ID != "" {
		revoked, err := v.revoked(ctx, claims.ID)
		if err != nil {
			// The denylist is unavailable. Honouring the signature is the
			// deliberate choice: the alternative is that a Redis outage logs
			// out every customer at once. The exposure is bounded by the
			// token's short lifetime.
			//nolint:nilerr // failing open here is the documented trade
			return claims, nil
		}
		if revoked {
			return nil, ErrUnauthenticated
		}
	}
	return claims, nil
}

// BearerToken extracts a token from an Authorization header.
func BearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// Unauthorized renders a 401 with the challenge a client needs to react to.
func Unauthorized(msg string) error {
	if msg == "" {
		msg = "authentication required"
	}
	return httpx.Errorf(http.StatusUnauthorized, "%s", msg)
}
