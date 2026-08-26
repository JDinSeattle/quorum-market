package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/ratelimit"
)

const testSecret = "a-gateway-test-secret-long-enough-to-pass"

// upstream records what the gateway forwarded, so the tests can assert on the
// headers a downstream service would actually see.
type upstream struct {
	server *httptest.Server
	seen   chan http.Header
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()

	u := &upstream{seen: make(chan http.Header, 16)}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) lastHeaders(t *testing.T) http.Header {
	t.Helper()
	select {
	case h := <-u.seen:
		return h
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached the upstream")
		return nil
	}
}

func testGateway(t *testing.T, limiter *ratelimit.Limiter, routes ...Route) http.Handler {
	t.Helper()

	verifier, err := auth.NewVerifier(testSecret, "test-issuer")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	gw, err := New(Config{Routes: routes, Verifier: verifier, Limiter: limiter})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return gw.Routes()
}

func testToken(t *testing.T, customerID string) string {
	t.Helper()

	signer, err := auth.NewSigner(testSecret, "test-issuer", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	token, _, err := signer.Issue(customerID, customerID+"@example.com", []string{"customer"}, "sess-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return token
}

// The single most important test in this package.
//
// Downstream services trust the identity headers completely, because the
// gateway is the only way in. If a client could set them, it could act as any
// customer — read anyone's orders, check out against anyone's cart. The
// gateway must overwrite what the caller sent, never merge with it.
func TestSpoofedIdentityHeadersAreStripped(t *testing.T) {
	up := newUpstream(t)
	handler := testGateway(t, nil,
		Route{Prefix: "/orders", Upstream: up.server.URL, Name: "orders"})

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("Authorization", "Bearer "+testToken(t, "cust-real"))
	req.Header.Set(auth.CustomerIDHeader, "cust-victim")
	req.Header.Set(auth.CustomerEmailHeader, "victim@example.com")
	req.Header.Set(auth.CustomerRolesHeader, "admin")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	forwarded := up.lastHeaders(t)
	if got := forwarded.Get(auth.CustomerIDHeader); got != "cust-real" {
		t.Errorf("customer id = %q, want cust-real: the spoofed header survived", got)
	}
	if got := forwarded.Get(auth.CustomerEmailHeader); got == "victim@example.com" {
		t.Error("the spoofed email header survived")
	}
	if got := forwarded.Get(auth.CustomerRolesHeader); got == "admin" {
		t.Error("a spoofed admin role survived")
	}
}

// The same headers must not leak through on a public route either, where no
// token is verified and nothing overwrites them.
func TestSpoofedHeadersAreStrippedOnPublicRoutes(t *testing.T) {
	up := newUpstream(t)
	handler := testGateway(t, nil,
		Route{Prefix: "/product", Upstream: up.server.URL, Public: true, Name: "product"})

	req := httptest.NewRequest(http.MethodGet, "/product/p1", nil)
	req.Header.Set(auth.CustomerIDHeader, "cust-victim")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := up.lastHeaders(t).Get(auth.CustomerIDHeader); got != "" {
		t.Errorf("customer id = %q on an unauthenticated request, want empty", got)
	}
}

func TestPublicRoutesDoNotRequireAToken(t *testing.T) {
	up := newUpstream(t)
	handler := testGateway(t, nil,
		Route{Prefix: "/product", Upstream: up.server.URL, Public: true, Name: "product"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/product/p1", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: browsing should not require an account", rec.Code)
	}
}

func TestProtectedRoutesRequireAValidToken(t *testing.T) {
	up := newUpstream(t)
	handler := testGateway(t, nil,
		Route{Prefix: "/shopping-cart", Upstream: up.server.URL, Name: "shopping-cart"})

	cases := map[string]string{
		"no token":      "",
		"garbage token": "Bearer not-a-token",
		"wrong scheme":  "Basic abc",
	}
	for name, header := range cases {
		req := httptest.NewRequest(http.MethodPost, "/shopping-cart", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}

	// Nothing should have reached the upstream.
	select {
	case h := <-up.seen:
		t.Errorf("an unauthenticated request was forwarded: %v", h)
	default:
	}
}

func TestVerifiedIdentityIsPassedDownstream(t *testing.T) {
	up := newUpstream(t)
	handler := testGateway(t, nil,
		Route{Prefix: "/orders", Upstream: up.server.URL, Name: "orders"})

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("Authorization", "Bearer "+testToken(t, "cust-42"))

	handler.ServeHTTP(httptest.NewRecorder(), req)

	forwarded := up.lastHeaders(t)
	if got := forwarded.Get(auth.CustomerIDHeader); got != "cust-42" {
		t.Errorf("customer id = %q, want cust-42", got)
	}
	if got := forwarded.Get(auth.CustomerEmailHeader); got != "cust-42@example.com" {
		t.Errorf("email = %q", got)
	}
	if got := forwarded.Get(auth.CustomerRolesHeader); got != "customer" {
		t.Errorf("roles = %q", got)
	}
}

func TestThrottledRequestsAreRefusedWithGuidance(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	limiter := ratelimit.New(client, ratelimit.Options{
		Scope: "test", Prefix: "rl:", Limit: 2, Window: time.Minute,
	})

	up := newUpstream(t)
	handler := testGateway(t, limiter,
		Route{Prefix: "/product", Upstream: up.server.URL, Public: true, Name: "product"})

	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		last = httptest.NewRecorder()
		handler.ServeHTTP(last, httptest.NewRequest(http.MethodGet, "/product/p1", nil))
	}

	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d after exceeding the limit, want 429", last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("a throttled response should say when to retry")
	}
	if last.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("the limit should be advertised so clients can pace themselves")
	}
}

func TestAnUnreachableUpstreamIsReportedAsABadGateway(t *testing.T) {
	handler := testGateway(t, nil,
		Route{Prefix: "/product", Upstream: "http://127.0.0.1:1", Public: true, Name: "product"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/product/p1", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	// The internal address must not be echoed back to whoever asked.
	if body := rec.Body.String(); len(body) > 0 && strings.Contains(body, "127.0.0.1:1") {
		t.Errorf("the upstream address leaked into the response: %s", body)
	}
}

func TestUnroutedPathsAreNotFound(t *testing.T) {
	up := newUpstream(t)
	handler := testGateway(t, nil,
		Route{Prefix: "/product", Upstream: up.server.URL, Public: true, Name: "product"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAGatewayNeedsRoutes(t *testing.T) {
	verifier, _ := auth.NewVerifier(testSecret, "test-issuer")
	if _, err := New(Config{Verifier: verifier}); err == nil {
		t.Error("a gateway with no routes was accepted")
	}
}
