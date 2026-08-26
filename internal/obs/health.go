package obs

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Probe reports whether one dependency is usable right now.
type Probe func(context.Context) error

// Check is a named readiness probe.
type Check struct {
	Name  string
	Probe Probe

	// Optional means a failure degrades the service rather than removing it
	// from the load balancer. The cart service can still serve reads when the
	// broker is down, so the broker is optional; its database is not.
	Optional bool
}

// CheckResult is one probe's outcome.
type CheckResult struct {
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	TookMS   int64  `json:"tookMs"`
}

// Report is the body of a readiness response.
type Report struct {
	Status string                 `json:"status"`
	Checks map[string]CheckResult `json:"checks,omitempty"`
	Build  BuildInfo              `json:"build"`
}

// Readiness statuses.
const (
	StatusUp       = "up"
	StatusDegraded = "degraded"
	StatusDown     = "down"
)

// Health evaluates readiness probes.
//
// Liveness and readiness are kept apart because they answer different
// questions and have opposite failure modes. Liveness asks "is this process
// wedged?" and must not depend on anything external, or one database blip
// restarts the entire fleet. Readiness asks "should traffic come here right
// now?" and is exactly where dependencies belong.
type Health struct {
	checks  []Check
	timeout time.Duration
	ttl     time.Duration

	draining atomic.Bool

	mu       sync.Mutex
	cached   Report
	cachedAt time.Time
}

// NewHealth returns a Health that runs checks with a per-probe timeout and
// caches the result briefly.
//
// Caching matters: a load balancer polls readiness every few seconds per
// target, and without it a fleet of twenty instances would turn health
// checking into its own significant load on every dependency.
func NewHealth(checks ...Check) *Health {
	return &Health{checks: checks, timeout: 2 * time.Second, ttl: 2 * time.Second}
}

// Drain marks the process as not ready without stopping it.
//
// Called first on shutdown so the load balancer takes this instance out of
// rotation while it is still able to finish the requests it already has. Going
// straight to shutdown instead would sever requests that were routed here in
// the moments before the process died.
func (h *Health) Drain() { h.draining.Store(true) }

// Draining reports whether shutdown has begun.
func (h *Health) Draining() bool { return h.draining.Load() }

// Report evaluates every check, subject to the cache.
func (h *Health) Report(ctx context.Context) Report {
	if h.draining.Load() {
		return Report{Status: StatusDown, Build: Build(),
			Checks: map[string]CheckResult{"shutdown": {Status: "draining"}}}
	}

	h.mu.Lock()
	if time.Since(h.cachedAt) < h.ttl && h.cached.Status != "" {
		cached := h.cached
		h.mu.Unlock()
		return cached
	}
	h.mu.Unlock()

	report := h.evaluate(ctx)

	h.mu.Lock()
	h.cached, h.cachedAt = report, time.Now()
	h.mu.Unlock()

	return report
}

func (h *Health) evaluate(ctx context.Context) Report {
	report := Report{Status: StatusUp, Checks: make(map[string]CheckResult, len(h.checks)), Build: Build()}
	if len(h.checks) == 0 {
		return report
	}

	type outcome struct {
		name   string
		result CheckResult
	}
	results := make(chan outcome, len(h.checks))

	// Probes run concurrently so total readiness latency is the slowest probe,
	// not their sum.
	for _, check := range h.checks {
		go func(check Check) {
			probeCtx, cancel := context.WithTimeout(ctx, h.timeout)
			defer cancel()

			started := time.Now()
			err := check.Probe(probeCtx)
			result := CheckResult{
				Status:   StatusUp,
				Optional: check.Optional,
				TookMS:   time.Since(started).Milliseconds(),
			}
			if err != nil {
				result.Status = StatusDown
				result.Error = err.Error()
			}
			results <- outcome{check.Name, result}
		}(check)
	}

	for range h.checks {
		got := <-results
		report.Checks[got.name] = got.result

		if got.result.Status == StatusUp {
			continue
		}
		if got.result.Optional {
			if report.Status == StatusUp {
				report.Status = StatusDegraded
			}
			continue
		}
		report.Status = StatusDown
	}
	return report
}
