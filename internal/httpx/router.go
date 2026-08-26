package httpx

import (
	"io"
	"net/http"
	"strings"
)

// Router wraps http.ServeMux so that every route is instrumented by
// construction.
//
// Metrics are attached where the route is registered rather than in an outer
// middleware, because only here is the route *pattern* statically known. An
// outer middleware sees the raw path — /shopping-cart/cart-alice/checkout —
// and using that as a metric label would mint a new time series per customer
// and eventually take down the metrics backend.
type Router struct {
	mux    *http.ServeMux
	routes []string
}

// NewRouter returns an empty Router.
func NewRouter() *Router {
	rt := &Router{mux: http.NewServeMux()}

	// Anything that matches no registered pattern is answered here rather than
	// by the mux's bare 404, so unmatched traffic is still counted and still
	// gets the standard error body.
	rt.mux.Handle("/", Instrument("unmatched", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, Errorf(http.StatusNotFound, "no route matches %s %s", r.Method, r.URL.Path))
	})))
	return rt
}

// Handle registers an error-returning handler. The pattern is Go 1.22 syntax,
// e.g. "GET /product/{productId}".
func (rt *Router) Handle(pattern string, h Handler) {
	rt.HandleRaw(pattern, H(h))
}

// HandleRaw registers a plain http.Handler.
func (rt *Router) HandleRaw(pattern string, h http.Handler) {
	route := routeLabel(pattern)
	rt.routes = append(rt.routes, pattern)
	rt.mux.Handle(pattern, Instrument(route, h))
}

// Probe registers a load balancer health endpoint. It is excluded from
// metrics and request logging by the middleware, so probe traffic does not
// distort the latency histograms it would otherwise dominate.
func (rt *Router) Probe(pattern string) {
	rt.routes = append(rt.routes, pattern)
	rt.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "OK")
	}))
}

// Routes lists every registered pattern, for startup logging.
func (rt *Router) Routes() []string { return rt.routes }

// Build wraps the routes in the standard middleware stack.
//
// Order matters, outermost first:
//
//	Recoverer  — must survive a panic in anything below it
//	RequestID  — established early so even a panic logs with an id
//	Logger     — outside the limiter, so shed requests are still logged
//	Limiter    — sheds before any real work is done
func (rt *Router) Build(maxInFlight int) http.Handler {
	return Chain(rt.mux,
		Recoverer,
		RequestID,
		RequestLogger,
		Limiter(maxInFlight),
	)
}

// routeLabel strips the method from a pattern, leaving the path template.
func routeLabel(pattern string) string {
	if idx := strings.IndexByte(pattern, ' '); idx >= 0 {
		return strings.TrimSpace(pattern[idx+1:])
	}
	return pattern
}
