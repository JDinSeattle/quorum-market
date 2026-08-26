package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "a-test-secret-that-is-long-enough-to-be-accepted"

func testSigner(t *testing.T, ttl time.Duration) *Signer {
	t.Helper()
	signer, err := NewSigner(testSecret, "test-issuer", ttl)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

func testVerifier(t *testing.T) *Verifier {
	t.Helper()
	verifier, err := NewVerifier(testSecret, "test-issuer")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}

func TestIssuedTokensVerify(t *testing.T) {
	signer := testSigner(t, time.Hour)

	token, issued, err := signer.Issue("cust-1", "a@example.com", []string{"customer"}, "sess-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.ExpiresAt.Before(time.Now()) {
		t.Fatal("a freshly issued token is already expired")
	}

	claims, err := testVerifier(t).Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.CustomerID() != "cust-1" {
		t.Errorf("customer = %q, want cust-1", claims.CustomerID())
	}
	if claims.Email != "a@example.com" {
		t.Errorf("email = %q", claims.Email)
	}
	if !claims.HasRole("customer") {
		t.Error("the customer role did not survive the round trip")
	}
	if claims.ID != "sess-1" {
		t.Errorf("session id = %q, want sess-1: without it a session cannot be revoked", claims.ID)
	}
}

// A short secret is brute-forceable offline from a single captured token, and
// recovering it lets an attacker mint any identity they like.
func TestShortSecretsAreRefused(t *testing.T) {
	if _, err := NewSigner("too-short", "iss", time.Hour); err == nil {
		t.Error("NewSigner accepted a short secret")
	}
	if _, err := NewVerifier("too-short", "iss"); err == nil {
		t.Error("NewVerifier accepted a short secret")
	}
}

func TestTokensSignedWithAnotherKeyAreRejected(t *testing.T) {
	other, err := NewSigner("a-completely-different-secret-of-sufficient-length", "test-issuer", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	token, _, _ := other.Issue("cust-1", "a@example.com", nil, "sess-1")

	if _, err := testVerifier(t).Verify(context.Background(), token); err == nil {
		t.Fatal("a token signed with a different key was accepted")
	}
}

func TestExpiredTokensAreRejected(t *testing.T) {
	// Built directly rather than through the signer: NewSigner clamps a
	// non-positive TTL to its default, so it will not mint an expired token.
	past := time.Now().Add(-time.Hour)
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "cust-1",
			Issuer:    "test-issuer",
			IssuedAt:  jwt.NewNumericDate(past),
			ExpiresAt: jwt.NewNumericDate(past.Add(time.Minute)),
		},
	}
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("building the expired token: %v", err)
	}

	if _, err := testVerifier(t).Verify(context.Background(), expired); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

// A token with no expiry never stops working, so one that omits the claim has
// to be refused outright rather than treated as valid forever.
func TestTokensWithoutAnExpiryAreRejected(t *testing.T) {
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "cust-1", Issuer: "test-issuer"},
	}
	eternal, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("building the token: %v", err)
	}

	if _, err := testVerifier(t).Verify(context.Background(), eternal); err == nil {
		t.Fatal("a token with no expiry was accepted")
	}
}

// A signature proves the token was minted by the identity service; it says
// nothing about who it is for. A token with no subject must not authenticate
// anyone.
func TestTokensWithoutASubjectAreRejected(t *testing.T) {
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "test-issuer",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	anonymous, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("building the token: %v", err)
	}

	if _, err := testVerifier(t).Verify(context.Background(), anonymous); err == nil {
		t.Fatal("a token with no subject was accepted")
	}
}

func TestTokensFromAnotherIssuerAreRejected(t *testing.T) {
	other, _ := NewSigner(testSecret, "someone-else", time.Hour)
	token, _, _ := other.Issue("cust-1", "a@example.com", nil, "sess-1")

	if _, err := testVerifier(t).Verify(context.Background(), token); err == nil {
		t.Fatal("a token from a different issuer was accepted")
	}
}

// The classic JWT bypass: a token declaring alg=none has no signature to
// check, and a verifier that trusts the header's choice of algorithm accepts
// anything an attacker writes.
func TestAlgNoneIsRejected(t *testing.T) {
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "cust-attacker",
			Issuer:    "test-issuer",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	forged, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("building the forged token: %v", err)
	}

	if _, err := testVerifier(t).Verify(context.Background(), forged); err == nil {
		t.Fatal("a token with alg=none was accepted")
	}
}

func TestMalformedTokensAreRejected(t *testing.T) {
	verifier := testVerifier(t)

	for _, token := range []string{"", "not-a-token", "a.b.c", strings.Repeat("x", 500)} {
		if _, err := verifier.Verify(context.Background(), token); err == nil {
			t.Errorf("accepted a malformed token %q", token)
		}
	}
}

func TestRevokedSessionsAreRejected(t *testing.T) {
	signer := testSigner(t, time.Hour)
	token, _, _ := signer.Issue("cust-1", "a@example.com", nil, "sess-revoked")

	verifier := testVerifier(t).WithRevocationCheck(
		func(_ context.Context, sessionID string) (bool, error) {
			return sessionID == "sess-revoked", nil
		})

	if _, err := verifier.Verify(context.Background(), token); err == nil {
		t.Fatal("a revoked session was accepted")
	}
}

// If the denylist cannot be reached, honouring the signature is the deliberate
// choice: the alternative is that a Redis outage logs out every customer at
// once. The exposure is bounded by the token's short lifetime.
func TestRevocationOutageFailsOpen(t *testing.T) {
	signer := testSigner(t, time.Hour)
	token, _, _ := signer.Issue("cust-1", "a@example.com", nil, "sess-1")

	verifier := testVerifier(t).WithRevocationCheck(
		func(context.Context, string) (bool, error) {
			return false, errors.New("redis is down")
		})

	if _, err := verifier.Verify(context.Background(), token); err != nil {
		t.Fatalf("a valid token was rejected because the denylist was unreachable: %v", err)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
		ok     bool
	}{
		"standard":     {"Bearer abc123", "abc123", true},
		"lowercase":    {"bearer abc123", "abc123", true},
		"missing":      {"", "", false},
		"wrong scheme": {"Basic abc123", "", false},
		"scheme only":  {"Bearer ", "", false},
		"no scheme":    {"abc123", "", false},
	}

	for name, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}

		got, ok := BearerToken(r)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", name, ok, tc.ok)
		}
		if got != tc.want {
			t.Errorf("%s: token = %q, want %q", name, got, tc.want)
		}
	}
}
