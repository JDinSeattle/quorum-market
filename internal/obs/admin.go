package obs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// AdminHandler builds the operational endpoint.
//
// It is served on its own port, never the one behind the load balancer.
// Metrics reveal internal topology and traffic shape, and pprof can dump the
// heap — including anything a customer sent — and can be used to stall a
// process by requesting a long CPU profile. None of that belongs on a public
// listener, and a separate port means a security group rule can gate it
// instead of a routing rule that someone might later widen.
func AdminHandler(health *Health) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /metrics", promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			// A broken collector should show up as a scrape error rather than
			// silently reporting a partial view of the system.
			ErrorHandling: promhttp.HTTPErrorOnError,
		},
	))

	// Liveness answers "is this process wedged?" and so depends on nothing
	// external. Tying it to a dependency would mean one database outage
	// restarts every instance that talks to it, turning a partial failure into
	// a total one.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": StatusUp})
	})

	// Readiness answers "should traffic come here?" and is where dependencies
	// belong. Degraded still returns 200: a cart service that cannot reach the
	// broker can serve every read and most writes, and pulling it out of
	// rotation would turn reduced function into no function.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		report := health.Report(r.Context())

		status := http.StatusOK
		if report.Status == StatusDown {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, report)
	})

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Build())
	})

	// Registered explicitly rather than by importing net/http/pprof for its
	// side effect, which would attach these to DefaultServeMux and risk them
	// being exposed by any handler that happens to use it.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	return mux
}

// ServeAdmin runs the admin endpoint until ctx is cancelled.
func ServeAdmin(ctx context.Context, addr string, health *Health) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           AdminHandler(health),
		ReadHeaderTimeout: 5 * time.Second,
		// Long, because a CPU profile is a 30 second streaming response by
		// default and a shorter write timeout would cut it off.
		WriteTimeout: 3 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		// Without the parent's cancellation, which has already fired.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("admin endpoint listening", "addr", addr,
		"routes", "/metrics /healthz /readyz /version /debug/pprof")

	// A failed admin listener must not take the service down with it: metrics
	// and profiling are how an incident is diagnosed, not how requests are
	// served.
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("admin endpoint stopped", "addr", addr, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
