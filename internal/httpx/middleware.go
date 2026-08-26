package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// Middleware wraps a handler with cross-cutting behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware left to right, so the first argument is outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// responseRecorder captures the status so middleware can see what was sent.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.written {
		// A handler that writes without calling WriteHeader has implicitly
		// sent a 200; record that so metrics do not report it as unknown.
		r.status = http.StatusOK
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// Recoverer turns a panicking handler into a 500.
//
// Without it a single nil dereference takes down the process and every other
// request in flight with it. The panic is logged with the request id so it can
// be traced back to the call that caused it.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { //nolint:contextcheck // the response writer has no context to take
			recovered := recover()
			if recovered == nil {
				return
			}
			// A client that hangs up mid-response makes the writer panic with
			// ErrAbortHandler; that is the stdlib's normal signal, not a bug.
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}
			slog.ErrorContext(r.Context(), "handler panicked",
				"path", r.URL.Path, "panic", recovered)
			JSON(w, http.StatusInternalServerError, errorBody{
				Error:     "internal error",
				RequestID: obs.RequestIDFrom(r.Context()),
			})
		}()
		next.ServeHTTP(w, r)
	})
}

// RequestID gives every request a traceable identity.
//
// An inbound id is honoured so a request keeps one identity across all five
// services; otherwise a fresh one is minted at the edge. It goes back on the
// response so a client — or the smoke test — can quote it when reporting a
// failure.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(obs.RequestIDHeader)
		if !validRequestID(id) {
			id = obs.NewRequestID()
		}
		w.Header().Set(obs.RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(obs.WithRequestID(r.Context(), id)))
	})
}

// validRequestID rejects ids that are absent, too long, or contain anything
// but safe characters. The value is echoed into logs and response headers, so
// accepting arbitrary client input here would let a caller forge log lines or
// inject a header.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// RequestLogger logs one structured line per request.
//
// Health checks are skipped: a load balancer probes every target every few
// seconds, and those lines would drown out the traffic worth reading.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		started := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(started)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", elapsed.Milliseconds(),
			"remote", clientIP(r),
		)
	})
}

// Limiter caps how many requests are served concurrently.
//
// Past a certain point extra concurrency does not add throughput, it only adds
// queueing: every request gets slower, timeouts start firing, and clients
// retry, which makes it worse. Rejecting the overflow immediately keeps the
// requests that were admitted fast, and gives the load balancer a clear signal
// to send traffic elsewhere.
//
// A limit of zero disables shedding.
func Limiter(limit int) Middleware {
	if limit <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	slots := make(chan struct{}, limit)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Probes must answer even when the service is saturated, or the
			// load balancer will conclude the instance is dead and kill it
			// exactly when it is busiest.
			if isProbe(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				next.ServeHTTP(w, r)
			default:
				obs.ObserveShedRequest()
				w.Header().Set("Retry-After", "1")
				JSON(w, http.StatusServiceUnavailable, errorBody{
					Error:     "server is at capacity, retry shortly",
					RequestID: obs.RequestIDFrom(r.Context()),
				})
			}
		})
	}
}

// Instrument records metrics for one route. The route is the registered
// pattern, never the raw path, so an id in the URL cannot create a new time
// series per customer.
func Instrument(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		obs.ServerInFlightAdd(1)
		defer obs.ServerInFlightAdd(-1)

		started := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		obs.ObserveServerRequest(r.Method, route, rec.status, time.Since(started))
	})
}

// isProbe reports whether a path is a load balancer health check.
func isProbe(path string) bool {
	const suffix = "/health"
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}

// clientIP prefers the address the load balancer reports, falling back to the
// socket peer when the request did not come through one.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		// The left-most entry is the original client; the rest are proxies.
		for i, c := range forwarded {
			if c == ',' {
				return forwarded[:i]
			}
		}
		return forwarded
	}
	return r.RemoteAddr
}
