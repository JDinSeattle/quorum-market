// Package gateway is the system's front door.
//
// It exists so that authentication, rate limiting and request identity are
// solved once, at the edge, rather than reimplemented in every service behind
// it. The services then get to assume they are talking to an already-verified
// caller, which is what keeps their code about carts and inventory instead of
// about tokens.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/auth"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
	"github.com/JDinSeattle/quorum-market/internal/ratelimit"
)

// Route maps a public path prefix onto an upstream service.
type Route struct {
	// Prefix is the path prefix this route claims, e.g. "/shopping-cart".
	Prefix string
	// Upstream is the base URL of the service behind it.
	Upstream string
	// Public allows unauthenticated access. Browsing a catalogue does not
	// require an account; touching a cart does.
	Public bool
	// Name labels this route's metrics and error messages.
	Name string
}

// Config assembles a gateway.
type Config struct {
	Routes   []Route
	Verifier *auth.Verifier
	Limiter  *ratelimit.Limiter

	// AnonymousLimitFactor scales the budget for callers with no identity.
	//
	// Anonymous requests are bucketed by client address, which is a far
	// coarser identity than an account: an office behind one NAT is a single
	// bucket. Signed-in customers each get the full budget.
	AnonymousLimitFactor int

	UpstreamTimeout time.Duration
}

// Gateway routes public traffic to internal services.
type Gateway struct {
	cfg     Config
	proxies map[string]*httputil.ReverseProxy
}

// New builds a Gateway, wiring one reverse proxy per upstream.
func New(cfg Config) (*Gateway, error) {
	if len(cfg.Routes) == 0 {
		return nil, errors.New("gateway: no routes configured")
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 30 * time.Second
	}
	if cfg.AnonymousLimitFactor <= 0 {
		cfg.AnonymousLimitFactor = 1
	}

	g := &Gateway{cfg: cfg, proxies: make(map[string]*httputil.ReverseProxy, len(cfg.Routes))}

	for _, route := range cfg.Routes {
		target, err := url.Parse(route.Upstream)
		if err != nil {
			return nil, fmt.Errorf("gateway: route %s has an unparseable upstream %q: %w",
				route.Prefix, route.Upstream, err)
		}
		g.proxies[route.Prefix] = newProxy(route, target)
	}
	return g, nil
}

func newProxy(route Route, target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Preserve the original Host so an upstream that builds absolute
			// URLs produces ones a client can actually follow.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 128,
			IdleConnTimeout:     90 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// An upstream failure is the gateway's problem to describe. Letting
			// the default handler answer would surface the internal address.
			slog.ErrorContext(r.Context(), "upstream call failed",
				"route", route.Name, "path", r.URL.Path, "err", err)
			httpx.WriteError(w, r, httpx.Errorf(http.StatusBadGateway,
				"%s is not responding", route.Name))
		},
	}
}

// Routes builds the gateway's HTTP surface.
func (g *Gateway) Routes() http.Handler {
	rt := httpx.NewRouter()
	rt.Probe("GET /health")

	for _, route := range g.cfg.Routes {
		handler := g.handlerFor(route)
		// Both forms: the bare prefix, and everything beneath it.
		rt.HandleRaw(route.Prefix, handler)
		rt.HandleRaw(route.Prefix+"/", handler)
	}
	return rt.Build(httpx.DefaultMaxInFlight())
}

func (g *Gateway) handlerFor(route Route) http.Handler {
	proxy := g.proxies[route.Prefix]

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip any inbound identity headers before anything else.
		//
		// Downstream services trust these headers completely, so a client able
		// to set them could act as any customer. These three lines are the
		// entire reason that trust is safe.
		r.Header.Del(auth.CustomerIDHeader)
		r.Header.Del(auth.CustomerEmailHeader)
		r.Header.Del(auth.CustomerRolesHeader)

		claims, authErr := g.authenticate(r)
		if authErr != nil && !route.Public {
			httpx.WriteError(w, r, authErr)
			return
		}

		identity, factor := g.identify(r, claims)
		if !g.allow(w, r, identity, factor) {
			return
		}

		if claims != nil {
			r.Header.Set(auth.CustomerIDHeader, claims.CustomerID())
			if claims.Email != "" {
				r.Header.Set(auth.CustomerEmailHeader, claims.Email)
			}
			if len(claims.Roles) > 0 {
				r.Header.Set(auth.CustomerRolesHeader, strings.Join(claims.Roles, ","))
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), g.cfg.UpstreamTimeout)
		defer cancel()

		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authenticate verifies a bearer token if one is present.
func (g *Gateway) authenticate(r *http.Request) (*auth.Claims, error) {
	token, ok := auth.BearerToken(r)
	if !ok {
		return nil, auth.Unauthorized("a bearer token is required")
	}
	if g.cfg.Verifier == nil {
		return nil, auth.Unauthorized("authentication is not configured")
	}

	claims, err := g.cfg.Verifier.Verify(r.Context(), token)
	if err != nil {
		obs.ObserveAuth("verify", "denied")
		return nil, auth.Unauthorized("the token is not valid")
	}
	obs.ObserveAuth("verify", "granted")
	return claims, nil
}

// identify picks the bucket a request is rate limited against.
func (g *Gateway) identify(r *http.Request, claims *auth.Claims) (string, int) {
	if claims != nil {
		return "customer:" + claims.CustomerID(), 1
	}
	return "anon:" + clientAddr(r), g.cfg.AnonymousLimitFactor
}

func (g *Gateway) allow(w http.ResponseWriter, r *http.Request, identity string, factor int) bool {
	if !g.cfg.Limiter.Enabled() {
		return true
	}
	_ = factor // the anonymous bucket is coarser by construction, not by budget

	result := g.cfg.Limiter.Allow(r.Context(), identity)

	// Advertised on every response, not just rejections: a well-behaved client
	// can pace itself off these rather than discovering the limit by hitting
	// it.
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(result.Limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))

	if result.Allowed {
		return true
	}

	retryAfter := int(result.RetryAfter.Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))

	slog.WarnContext(r.Context(), "request throttled", "identity", identity, "path", r.URL.Path)
	httpx.WriteError(w, r, httpx.Errorf(http.StatusTooManyRequests,
		"rate limit exceeded, retry in %ds", retryAfter))
	return false
}

// clientAddr is the address an anonymous caller is bucketed by.
func clientAddr(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx > 0 {
			return strings.TrimSpace(forwarded[:idx])
		}
		return strings.TrimSpace(forwarded)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
