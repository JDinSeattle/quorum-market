// Package busywait simulates variable-latency business logic.
//
// The delay is deliberately spent burning CPU and churning heap rather than
// sleeping: an idle Thread.sleep-style wait keeps the request slow but leaves
// CPU utilisation flat, so CloudWatch never sees load and the Auto Scaling
// Groups never scale out. Every request handler in the system starts with a
// Simulate call so that offered load translates into real machine load.
//
// Delays are drawn from a log-normal distribution, which matches the shape of
// real service latency far better than a uniform draw: a dense body of fast
// responses plus a long, heavy tail. With the defaults (mu=5.5, sigma=0.8) the
// median is ~245ms and the 99th percentile is ~1.6s.
package busywait

import (
	"math"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/envx"
)

// Defaults for the log-normal delay distribution.
const (
	DefaultMu    = 5.5
	DefaultSigma = 0.8
	DefaultMin   = 50 * time.Millisecond
	DefaultMax   = 5 * time.Second
)

// sink absorbs the result of the arithmetic loop so the compiler cannot prove
// the work is dead and delete it. It is atomic because handlers burn
// concurrently and a plain global would be a data race.
var sink atomic.Uint64

// Config describes a log-normal delay distribution clamped to [Min, Max].
type Config struct {
	Mu    float64
	Sigma float64
	Min   time.Duration
	Max   time.Duration
}

// Default returns the tuned distribution used by every service.
func Default() Config {
	return Config{Mu: DefaultMu, Sigma: DefaultSigma, Min: DefaultMin, Max: DefaultMax}
}

// FromEnv reads DELAY_MU, DELAY_SIGMA, DELAY_MIN_MS and DELAY_MAX_MS, falling
// back to Default for anything unset. Setting DELAY_MAX_MS=0 disables the
// simulated delay entirely, which is what the unit tests do to stay fast.
func FromEnv() Config {
	c := Default()
	c.Mu = envx.Float("DELAY_MU", c.Mu)
	c.Sigma = envx.Float("DELAY_SIGMA", c.Sigma)
	c.Min = envx.Millis("DELAY_MIN_MS", c.Min)
	c.Max = envx.Millis("DELAY_MAX_MS", c.Max)
	if c.Min > c.Max {
		c.Min = c.Max
	}
	return c
}

// Delay draws one sample: exp(mu + sigma*Z) milliseconds, Z ~ N(0,1),
// clamped into [Min, Max].
func (c Config) Delay() time.Duration {
	ms := math.Exp(c.Mu + c.Sigma*rand.NormFloat64())
	if math.IsInf(ms, 0) || math.IsNaN(ms) {
		return c.Max
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < c.Min:
		return c.Min
	case d > c.Max:
		return c.Max
	default:
		return d
	}
}

// Simulate draws a delay and burns it. It is a no-op when Max is zero.
func (c Config) Simulate() {
	if c.Max <= 0 {
		return
	}
	Burn(c.Delay())
}

// Burn occupies the calling goroutine for d, consuming CPU with floating point
// work and heap with a rolling 1MB buffer of short-lived allocations. The
// allocation churn is what moves the memory-based scaling metrics; the
// arithmetic is what moves the CPU-based ones.
func Burn(d time.Duration) {
	if d <= 0 {
		return
	}
	deadline := time.Now().Add(d)
	hog := make([][]byte, 0, 1024)

	for round := 0; ; round++ {
		acc := 0.0
		for i := 0; i < 10_000; i++ {
			f := float64(i)
			acc += math.Sqrt(f) * math.Sin(f) * math.Cos(f)
		}
		sink.Store(math.Float64bits(acc))

		if round%100 == 0 {
			hog = append(hog, make([]byte, 1024))
			if len(hog) > 1000 {
				hog = hog[1:]
			}
		}
		// Checking the clock every ~10k iterations keeps the syscall overhead
		// negligible while still landing within a millisecond of the deadline.
		if !time.Now().Before(deadline) {
			break
		}
	}
	runtime.KeepAlive(hog)
}
