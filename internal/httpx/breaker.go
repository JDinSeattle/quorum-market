package httpx

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// ErrBreakerOpen is returned when a call is refused without being attempted.
var ErrBreakerOpen = errors.New("circuit breaker is open")

// BreakerState is a circuit breaker's current mode.
type BreakerState int

// Breaker states, numbered to match the exported metric.
const (
	BreakerClosed BreakerState = iota
	BreakerHalfOpen
	BreakerOpen
)

// BreakerConfig tunes a Breaker. Zero values take documented defaults.
type BreakerConfig struct {
	// FailureThreshold is how many consecutive failures open the breaker.
	FailureThreshold int
	// OpenFor is how long it stays open before allowing a probe through.
	OpenFor time.Duration
	// HalfOpenSuccesses is how many probes must succeed to close it again.
	HalfOpenSuccesses int
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.OpenFor <= 0 {
		c.OpenFor = 5 * time.Second
	}
	if c.HalfOpenSuccesses <= 0 {
		c.HalfOpenSuccesses = 2
	}
	return c
}

// Breaker stops a caller from queueing behind a dependency that is already
// failing.
//
// When the warehouse is down, every checkout would otherwise wait out the full
// request timeout before failing. At any real request rate that means the cart
// service's own capacity fills with calls that are certain to fail, and a
// warehouse outage becomes a cart outage. Failing immediately keeps the
// failure contained and lets the caller return a useful error fast.
//
// Only genuine unavailability trips it. A 402 decline or a 409 out-of-stock is
// the dependency working correctly, and counting those would open the breaker
// on the payment service during entirely normal trading.
type Breaker struct {
	name string
	cfg  BreakerConfig

	mu                  sync.Mutex
	state               BreakerState
	consecutiveFailures int
	openedAt            time.Time
	halfOpenSuccesses   int
	halfOpenInFlight    bool
}

// NewBreaker returns a closed breaker named for the dependency it guards.
func NewBreaker(name string, cfg BreakerConfig) *Breaker {
	b := &Breaker{name: name, cfg: cfg.withDefaults()}
	obs.SetBreakerState(name, float64(BreakerClosed))
	return b
}

// Allow reports whether a call may proceed.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerClosed:
		return nil

	case BreakerOpen:
		if time.Since(b.openedAt) < b.cfg.OpenFor {
			obs.ObserveBreakerRejection(b.name)
			return ErrBreakerOpen
		}
		// Cool-off elapsed: let a single probe through to find out whether the
		// dependency recovered.
		b.transition(BreakerHalfOpen)
		b.halfOpenInFlight = true
		return nil

	default: // half-open
		if b.halfOpenInFlight {
			// Exactly one probe at a time. Releasing the full load the instant
			// the timer expires is how a recovering dependency gets knocked
			// straight back over.
			obs.ObserveBreakerRejection(b.name)
			return ErrBreakerOpen
		}
		b.halfOpenInFlight = true
		return nil
	}
}

// Record reports the outcome of a call that Allow permitted. Only failures
// that indicate the dependency itself is unhealthy should be passed as err.
func (b *Breaker) Record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.halfOpenInFlight = false

	if err != nil {
		b.consecutiveFailures++
		if b.state == BreakerHalfOpen || b.consecutiveFailures >= b.cfg.FailureThreshold {
			b.openedAt = time.Now()
			b.halfOpenSuccesses = 0
			b.transition(BreakerOpen)
		}
		return
	}

	b.consecutiveFailures = 0
	if b.state == BreakerHalfOpen {
		b.halfOpenSuccesses++
		if b.halfOpenSuccesses >= b.cfg.HalfOpenSuccesses {
			b.halfOpenSuccesses = 0
			b.transition(BreakerClosed)
		}
		return
	}
	if b.state == BreakerOpen {
		b.transition(BreakerClosed)
	}
}

// State returns the breaker's current mode.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// transition must be called with the lock held.
func (b *Breaker) transition(to BreakerState) {
	if b.state == to {
		return
	}
	b.state = to
	obs.SetBreakerState(b.name, float64(to))
}

// countsAsUnavailable reports whether a call outcome says the dependency is
// unhealthy, as opposed to answering correctly with a rejection.
func countsAsUnavailable(status int, err error) bool {
	if err != nil && status == 0 {
		return true // transport failure: refused, timed out, DNS
	}
	return status >= 500 || status == http.StatusTooManyRequests
}
