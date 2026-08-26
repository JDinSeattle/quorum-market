package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

func testClient(cfg ClientConfig) *Client {
	if cfg.Dependency == "" {
		cfg.Dependency = "test-dep-" + obs.NewRequestID() // unique breaker per test
	}
	if cfg.RetryBaseDelay == 0 {
		cfg.RetryBaseDelay = time.Millisecond
	}
	if cfg.RetryMaxDelay == 0 {
		cfg.RetryMaxDelay = 2 * time.Millisecond
	}
	return NewClient(cfg)
}

func TestClientRetriesIdempotentRequests(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		JSON(w, http.StatusOK, map[string]string{"value": "ok"})
	}))
	defer srv.Close()

	client := testClient(ClientConfig{MaxAttempts: 3, DisableBreaker: true})

	var out map[string]string
	if err := client.GetJSON(context.Background(), srv.URL, &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if out["value"] != "ok" {
		t.Errorf("body = %v", out)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("server saw %d attempts, want 3", got)
	}
}

// A POST that times out may already have been applied. Retrying an
// authorization could charge a customer twice and retrying a reservation could
// hold stock twice, so the transport must never do it on its own.
func TestClientNeverRetriesPost(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := testClient(ClientConfig{MaxAttempts: 5, DisableBreaker: true})

	if err := client.PostJSON(context.Background(), srv.URL, map[string]string{"a": "b"}, nil); err == nil {
		t.Fatal("PostJSON succeeded against a failing server")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("server saw %d attempts, want exactly 1", got)
	}
}

// A 404 or a 409 is a considered answer. Asking again only produces it faster.
func TestClientDoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict, http.StatusPaymentRequired} {
		var attempts atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(status)
		}))

		client := testClient(ClientConfig{MaxAttempts: 4, DisableBreaker: true})
		err := client.GetJSON(context.Background(), srv.URL, nil)

		if !IsStatus(err, status) {
			t.Errorf("status %d: err = %v, want an APIError carrying that status", status, err)
		}
		if got := attempts.Load(); got != 1 {
			t.Errorf("status %d: server saw %d attempts, want 1", status, got)
		}
		srv.Close()
	}
}

func TestClientStopsAtMaxAttempts(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := testClient(ClientConfig{MaxAttempts: 3, DisableBreaker: true})

	if err := client.GetJSON(context.Background(), srv.URL, nil); err == nil {
		t.Fatal("GetJSON succeeded against a failing server")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("server saw %d attempts, want 3", got)
	}
}

// The request id has to survive the hop, or a checkout cannot be traced across
// the five services it touches.
func TestClientPropagatesTheRequestID(t *testing.T) {
	seen := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(obs.RequestIDHeader)
		JSON(w, http.StatusOK, nil)
	}))
	defer srv.Close()

	client := testClient(ClientConfig{DisableBreaker: true})
	ctx := obs.WithRequestID(context.Background(), "abc123")

	if err := client.GetJSON(ctx, srv.URL, nil); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if got := <-seen; got != "abc123" {
		t.Errorf("downstream saw request id %q, want abc123", got)
	}
}

// Once the breaker opens, calls must fail without touching the network. That
// is the whole point: a dead dependency should cost microseconds, not the full
// request timeout multiplied by every caller.
func TestClientShortCircuitsWhenTheBreakerOpens(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := testClient(ClientConfig{
		MaxAttempts: 1,
		Breaker:     BreakerConfig{FailureThreshold: 2, OpenFor: time.Minute},
	})

	for i := 0; i < 2; i++ {
		_ = client.GetJSON(context.Background(), srv.URL, nil)
	}
	before := attempts.Load()

	err := client.GetJSON(context.Background(), srv.URL, nil)
	if !IsStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("err = %v, want a 503 from the open breaker", err)
	}
	if attempts.Load() != before {
		t.Error("the request reached the server despite the breaker being open")
	}
}

func TestClientRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := testClient(ClientConfig{MaxAttempts: 3, DisableBreaker: true, RequestTimeout: time.Minute})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	if err := client.GetJSON(ctx, srv.URL, nil); err == nil {
		t.Fatal("GetJSON succeeded against a hanging server")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("took %v to give up, want the context deadline to win", elapsed)
	}
}

func TestClientReportsMalformedResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	client := testClient(ClientConfig{DisableBreaker: true})

	var out map[string]string
	err := client.GetJSON(context.Background(), srv.URL, &out)
	if !IsStatus(err, http.StatusBadGateway) {
		t.Errorf("err = %v, want 502: an unparseable upstream body is a gateway problem", err)
	}
}

func TestClientIsSafeForConcurrentUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, map[string]int{"n": 1})
	}))
	defer srv.Close()

	client := testClient(ClientConfig{DisableBreaker: true})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]int
			if err := client.GetJSON(context.Background(), srv.URL, &out); err != nil {
				t.Errorf("GetJSON: %v", err)
			}
		}()
	}
	wg.Wait()
}
