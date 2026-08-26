package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/envx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// ServerConfig describes a service's public listener.
type ServerConfig struct {
	Addr    string
	Handler http.Handler

	// Health is marked not-ready before shutdown begins, so the load balancer
	// stops sending new work while this instance finishes what it has.
	Health *obs.Health

	// DrainDelay is how long to keep serving after readiness flips to false.
	// It should exceed the load balancer's detection window, otherwise the
	// process is gone before the balancer has noticed and requests routed in
	// the meantime are severed.
	DrainDelay time.Duration

	// ShutdownTimeout bounds how long in-flight requests may take to finish.
	ShutdownTimeout time.Duration
}

func (c ServerConfig) withDefaults() ServerConfig {
	if c.DrainDelay <= 0 {
		c.DrainDelay = envx.Millis("DRAIN_DELAY_MS", 5*time.Second)
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = envx.Millis("SHUTDOWN_TIMEOUT_MS", 25*time.Second)
	}
	return c
}

// DefaultMaxInFlight bounds concurrent requests when nothing is configured.
//
// It scales with the machine because the right ceiling depends on how much
// work the box can actually do: every handler in this system burns real CPU,
// so admitting far more than the cores can retire just converts throughput
// into queueing delay. MAX_IN_FLIGHT overrides it; 0 disables shedding.
func DefaultMaxInFlight() int {
	return envx.Int("MAX_IN_FLIGHT", 64*runtime.GOMAXPROCS(0))
}

// SignalContext returns a context cancelled on SIGINT or SIGTERM.
//
// SIGTERM is what a container runtime and an Auto Scaling Group send before
// they kill an instance; catching it is the difference between a clean drain
// and dropping every request in flight.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Serve runs the public listener until ctx is cancelled, then drains.
//
// The shutdown sequence is deliberate:
//
//  1. mark not-ready, so the load balancer stops routing new requests here
//  2. keep serving for DrainDelay, so requests already in flight — and any the
//     balancer sends before it notices — are answered normally
//  3. stop accepting, and give the remainder until ShutdownTimeout to finish
//
// Skipping step 1 or 2 turns every deploy and every scale-in into a burst of
// customer-visible 502s.
func Serve(ctx context.Context, cfg ServerConfig) error {
	cfg = cfg.withDefaults()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           cfg.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		// Bound how large a request's headers may be; the default is generous
		// enough to be worth tightening on a public listener.
		MaxHeaderBytes: 1 << 16,
	}

	listenErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		listenErr <- err
	}()

	select {
	case err := <-listenErr:
		return err

	case <-ctx.Done():
		if cfg.Health != nil {
			cfg.Health.Drain()
			slog.Info("marked not ready, draining", "for", cfg.DrainDelay)
			select {
			case <-time.After(cfg.DrainDelay):
			case err := <-listenErr:
				return err
			}
		}

		slog.Info("shutting down", "timeout", cfg.ShutdownTimeout)
		// Derived from the parent so request-scoped values survive, but
		// without its cancellation — the parent is already cancelled, which is
		// the reason this code is running at all.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Requests still running when the deadline passed were cut off.
			// That is worth an error line, not a silent exit.
			slog.Error("shutdown did not complete cleanly", "err", err)
			return err
		}
		slog.Info("shutdown complete")
		return nil
	}
}
