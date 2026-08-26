package busywait

import (
	"testing"
	"time"
)

func TestDelayStaysWithinBounds(t *testing.T) {
	c := Config{Mu: 5.5, Sigma: 0.8, Min: 50 * time.Millisecond, Max: 5 * time.Second}

	for i := 0; i < 5000; i++ {
		d := c.Delay()
		if d < c.Min || d > c.Max {
			t.Fatalf("Delay() = %v, outside [%v, %v]", d, c.Min, c.Max)
		}
	}
}

// The distribution should put most samples well under a second while still
// producing the occasional slow request; a flat delay would not exercise the
// tail-latency behaviour the load test is meant to reveal.
func TestDelayIsSkewedTowardTheFastEnd(t *testing.T) {
	c := Default()

	const n = 20000
	var underHalfSecond int
	for i := 0; i < n; i++ {
		if c.Delay() < 500*time.Millisecond {
			underHalfSecond++
		}
	}

	ratio := float64(underHalfSecond) / n
	if ratio < 0.6 || ratio > 0.95 {
		t.Errorf("%.2f of samples were under 500ms, want a clear majority but not all", ratio)
	}
}

func TestSimulateIsDisabledWhenMaxIsZero(t *testing.T) {
	c := Config{Mu: 5.5, Sigma: 0.8}

	start := time.Now()
	c.Simulate()
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("Simulate took %v with Max=0, want a no-op", elapsed)
	}
}

func TestBurnSpendsRoughlyTheRequestedTime(t *testing.T) {
	start := time.Now()
	Burn(120 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 100*time.Millisecond {
		t.Errorf("Burn(120ms) returned after %v, too early", elapsed)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("Burn(120ms) took %v, far longer than asked", elapsed)
	}
}
