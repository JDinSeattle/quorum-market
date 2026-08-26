package httpx

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b := NewBreaker("test", BreakerConfig{FailureThreshold: 3, OpenFor: time.Minute})
	boom := errors.New("boom")

	for i := 0; i < 2; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("call %d was rejected while the breaker should still be closed", i)
		}
		b.Record(boom)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("state = %v after 2 of 3 failures, want closed", b.State())
	}

	if err := b.Allow(); err != nil {
		t.Fatal("third call was rejected before the threshold was reached")
	}
	b.Record(boom)

	if b.State() != BreakerOpen {
		t.Fatalf("state = %v after 3 failures, want open", b.State())
	}
	if err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("Allow() = %v, want ErrBreakerOpen", err)
	}
}

// A single success must clear the count, otherwise unrelated failures spread
// over an hour would eventually trip a breaker on a healthy dependency.
func TestBreakerResetsOnSuccess(t *testing.T) {
	b := NewBreaker("test", BreakerConfig{FailureThreshold: 3, OpenFor: time.Minute})
	boom := errors.New("boom")

	b.Record(boom)
	b.Record(boom)
	b.Record(nil)
	b.Record(boom)
	b.Record(boom)

	if b.State() != BreakerClosed {
		t.Errorf("state = %v, want closed: an intervening success did not reset the count", b.State())
	}
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	b := NewBreaker("test", BreakerConfig{
		FailureThreshold: 1, OpenFor: 10 * time.Millisecond, HalfOpenSuccesses: 2,
	})

	b.Record(errors.New("boom"))
	if b.State() != BreakerOpen {
		t.Fatal("breaker did not open")
	}

	time.Sleep(20 * time.Millisecond)

	// The first call after the cool-off is the probe.
	if err := b.Allow(); err != nil {
		t.Fatalf("probe was rejected after the cool-off: %v", err)
	}
	if b.State() != BreakerHalfOpen {
		t.Fatalf("state = %v, want half-open", b.State())
	}

	// Only one probe at a time: releasing everything at once is how a
	// recovering dependency gets knocked straight back over.
	if err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Error("a second concurrent probe was allowed through")
	}

	b.Record(nil)
	if b.State() != BreakerHalfOpen {
		t.Fatalf("state = %v after 1 of 2 successes, want half-open", b.State())
	}

	if err := b.Allow(); err != nil {
		t.Fatalf("second probe was rejected: %v", err)
	}
	b.Record(nil)

	if b.State() != BreakerClosed {
		t.Errorf("state = %v after enough successes, want closed", b.State())
	}
}

func TestBreakerReopensWhenTheProbeFails(t *testing.T) {
	b := NewBreaker("test", BreakerConfig{FailureThreshold: 1, OpenFor: 10 * time.Millisecond})

	b.Record(errors.New("boom"))
	time.Sleep(20 * time.Millisecond)

	_ = b.Allow()
	b.Record(errors.New("still broken"))

	if b.State() != BreakerOpen {
		t.Errorf("state = %v, want open", b.State())
	}
}

// A dependency answering 402 or 409 is working correctly. Counting those as
// failures would open the breaker on the payment service during entirely
// normal trading, and every subsequent checkout would fail.
func TestOnlyUnavailabilityCountsAgainstTheBreaker(t *testing.T) {
	healthy := []int{
		http.StatusOK, http.StatusCreated, http.StatusBadRequest,
		http.StatusNotFound, http.StatusConflict, http.StatusPaymentRequired,
	}
	for _, status := range healthy {
		if countsAsUnavailable(status, Errorf(status, "x")) {
			t.Errorf("status %d counted as unavailable", status)
		}
	}

	unhealthy := []int{
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests,
	}
	for _, status := range unhealthy {
		if !countsAsUnavailable(status, Errorf(status, "x")) {
			t.Errorf("status %d did not count as unavailable", status)
		}
	}

	if !countsAsUnavailable(0, errors.New("connection refused")) {
		t.Error("a transport failure did not count as unavailable")
	}
}
