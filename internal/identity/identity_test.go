package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/kv"
)

const testSecret = "an-identity-test-secret-long-enough-to-pass"

func testService(t *testing.T) (*Service, *auth.Verifier) {
	t.Helper()

	// A real single-node KV cluster over loopback, so account storage is
	// exercised rather than stubbed.
	cfg := kv.Config{
		NodeID: "identity-test-db", Mode: kv.ModeLeaderless,
		WriteQuorum: 1, ReadQuorum: 1, RPCTimeout: time.Second,
	}
	svc := kv.NewService(cfg, kv.NewStore(cfg.NodeID), kv.NewReplicator(time.Second))
	server := httptest.NewServer(
		kv.NewServer(svc, kv.NewTxnManager(time.Minute), busywait.Config{}, false).Routes())
	t.Cleanup(server.Close)

	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	signer, err := auth.NewSigner(testSecret, "test-issuer", 15*time.Minute)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := auth.NewVerifier(testSecret, "test-issuer")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	db := kv.NewClient("identity-test-db", server.URL, time.Second, 5*time.Second)
	// The minimum cost, because these tests are about the flow rather than
	// about how long a hash takes.
	identityService := NewService(db, rdb, signer, time.Hour).WithBcryptCost(bcrypt.MinCost)

	return identityService, verifier.WithRevocationCheck(identityService.RevocationCheck())
}

const goodPassword = "correct-horse-battery"

func TestRegisterThenLogin(t *testing.T) {
	svc, verifier := testService(t)
	ctx := context.Background()

	registered, err := svc.Register(ctx, "Alice@Example.com ", goodPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if registered.AccessToken == "" || registered.RefreshToken == "" {
		t.Fatal("registration did not return a usable token pair")
	}
	// Email is normalised, so a customer can sign in however they type it.
	if registered.Profile.Email != "alice@example.com" {
		t.Errorf("email = %q, want it lowercased and trimmed", registered.Profile.Email)
	}

	claims, err := verifier.Verify(ctx, registered.AccessToken)
	if err != nil {
		t.Fatalf("the issued token does not verify: %v", err)
	}
	if claims.CustomerID() != registered.Profile.CustomerID {
		t.Errorf("token subject = %q, want %q", claims.CustomerID(), registered.Profile.CustomerID)
	}

	loggedIn, err := svc.Login(ctx, "ALICE@example.com", goodPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if loggedIn.Profile.CustomerID != registered.Profile.CustomerID {
		t.Error("logging in produced a different customer id")
	}
}

func TestPasswordsAreNeverStoredInTheClear(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	registered, err := svc.Register(ctx, "bob@example.com", goodPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	entry, found, err := svc.db.Get(ctx, userKeyPrefix+registered.Profile.CustomerID)
	if err != nil || !found {
		t.Fatalf("loading the stored account: %v (found=%v)", err, found)
	}
	if strings.Contains(entry.Value, goodPassword) {
		t.Fatal("the password was stored in the clear")
	}
	if !strings.Contains(entry.Value, "$2a$") {
		t.Error("the stored credential does not look like a bcrypt hash")
	}
	if strings.Contains(entry.Value, "\"password\"") {
		t.Error("the record has a plaintext password field")
	}
}

func TestDuplicateRegistrationIsRefused(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "carol@example.com", goodPassword); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := svc.Register(ctx, "carol@example.com", "a-completely-different-one")
	if got := httpx.StatusOf(err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
}

// An unknown email and a wrong password must be indistinguishable, or the
// login endpoint becomes an account enumeration oracle.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "dave@example.com", goodPassword); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, wrongPassword := svc.Login(ctx, "dave@example.com", "not-the-password")
	_, unknownEmail := svc.Login(ctx, "nobody@example.com", goodPassword)

	if httpx.StatusOf(wrongPassword) != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", httpx.StatusOf(wrongPassword))
	}
	if httpx.StatusOf(unknownEmail) != http.StatusUnauthorized {
		t.Errorf("unknown email: status = %d, want 401", httpx.StatusOf(unknownEmail))
	}
	if wrongPassword.Error() != unknownEmail.Error() {
		t.Errorf("the two failures are distinguishable:\n  %v\n  %v", wrongPassword, unknownEmail)
	}
}

func TestWeakCredentialsAreRefused(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	cases := map[string][2]string{
		"no email":         {"", goodPassword},
		"malformed email":  {"not-an-email", goodPassword},
		"email with space": {"a b@example.com", goodPassword},
		"short password":   {"e@example.com", "short"},
	}
	for name, tc := range cases {
		if _, err := svc.Register(ctx, tc[0], tc[1]); httpx.StatusOf(err) != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, httpx.StatusOf(err))
		}
	}
}

// Rotation means a stolen refresh token is usable at most once, and its use
// immediately breaks the real customer's session — a detectable signal rather
// than a silent, permanent compromise.
func TestRefreshTokensRotate(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	first, err := svc.Register(ctx, "erin@example.com", goodPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	second, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Error("the refresh token was reused rather than rotated")
	}
	if second.Profile.CustomerID != first.Profile.CustomerID {
		t.Error("refreshing produced a different customer")
	}

	if _, err := svc.Refresh(ctx, first.RefreshToken); httpx.StatusOf(err) != http.StatusUnauthorized {
		t.Errorf("the old refresh token still works: status = %d, want 401", httpx.StatusOf(err))
	}
}

func TestUnknownRefreshTokensAreRefused(t *testing.T) {
	svc, _ := testService(t)

	for _, token := range []string{"", "made-up-token"} {
		if _, err := svc.Refresh(context.Background(), token); httpx.StatusOf(err) != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", token, httpx.StatusOf(err))
		}
	}
}

// An access token cannot be recalled — it verifies on its signature alone — so
// logging out has to denylist its session for the rest of its lifetime.
func TestLogoutRevokesTheSessionImmediately(t *testing.T) {
	svc, verifier := testService(t)
	ctx := context.Background()

	tokens, err := svc.Register(ctx, "frank@example.com", goodPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	claims, err := verifier.Verify(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if err := svc.Logout(ctx, claims.ID, tokens.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := verifier.Verify(ctx, tokens.AccessToken); err == nil {
		t.Error("the access token still works after logout")
	}
	if _, err := svc.Refresh(ctx, tokens.RefreshToken); err == nil {
		t.Error("the refresh token still works after logout")
	}
}

// Logging one customer out must not affect anyone else.
func TestLogoutOnlyEndsItsOwnSession(t *testing.T) {
	svc, verifier := testService(t)
	ctx := context.Background()

	alice, _ := svc.Register(ctx, "alice2@example.com", goodPassword)
	bob, _ := svc.Register(ctx, "bob2@example.com", goodPassword)

	aliceClaims, _ := verifier.Verify(ctx, alice.AccessToken)
	if err := svc.Logout(ctx, aliceClaims.ID, alice.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := verifier.Verify(ctx, bob.AccessToken); err != nil {
		t.Errorf("bob was logged out along with alice: %v", err)
	}
}

func TestProfileLookup(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	registered, _ := svc.Register(ctx, "grace@example.com", goodPassword)

	profile, err := svc.Profile(ctx, registered.Profile.CustomerID)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if profile.Email != "grace@example.com" {
		t.Errorf("email = %q", profile.Email)
	}

	if _, err := svc.Profile(ctx, "cust-nobody"); httpx.StatusOf(err) != http.StatusNotFound {
		t.Errorf("unknown customer: status = %d, want 404", httpx.StatusOf(err))
	}
}
