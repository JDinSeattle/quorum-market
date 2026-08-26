package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

func TestRouterAnswersUnmatchedPaths(t *testing.T) {
	rt := NewRouter()
	rt.Handle("GET /thing/{id}", func(w http.ResponseWriter, r *http.Request) error {
		JSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
		return nil
	})
	handler := rt.Build(0)

	req := httptest.NewRequest(http.MethodGet, "/nowhere", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmatched route did not return the standard error body: %v", err)
	}
	if body.Error == "" {
		t.Error("error body has no message")
	}
	if body.RequestID == "" {
		t.Error("error body has no request id to quote in a bug report")
	}
}

func TestRouterMatchesPathParameters(t *testing.T) {
	rt := NewRouter()
	rt.Handle("GET /thing/{id}", func(w http.ResponseWriter, r *http.Request) error {
		JSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
		return nil
	})

	rec := httptest.NewRecorder()
	rt.Build(0).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thing/abc", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"abc"`) {
		t.Errorf("body = %s, want the path value", rec.Body.String())
	}
}

func TestRouteLabelStripsTheMethod(t *testing.T) {
	cases := map[string]string{
		"GET /product/{productId}": "/product/{productId}",
		"POST /shopping-cart":      "/shopping-cart",
		"/health":                  "/health",
	}
	for pattern, want := range cases {
		if got := routeLabel(pattern); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestRequestIDHonoursAValidInboundID(t *testing.T) {
	var seen string
	handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = obs.RequestIDFrom(r.Context())
	}), RequestID)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(obs.RequestIDHeader, "upstream-id-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if seen != "upstream-id-1" {
		t.Errorf("context id = %q, want the inbound id", seen)
	}
	if got := rec.Header().Get(obs.RequestIDHeader); got != "upstream-id-1" {
		t.Errorf("response header = %q, want it echoed", got)
	}
}

// The id is echoed into logs and response headers, so a caller must not be
// able to smuggle a newline or an oversized value through it.
func TestRequestIDRejectsHostileInput(t *testing.T) {
	hostile := []string{
		"", "has spaces", "line\nbreak", "semi;colon", strings.Repeat("a", 65),
	}

	for _, candidate := range hostile {
		var seen string
		handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = obs.RequestIDFrom(r.Context())
		}), RequestID)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(obs.RequestIDHeader, candidate)
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if seen == candidate {
			t.Errorf("accepted hostile request id %q", candidate)
		}
		if seen == "" {
			t.Errorf("no replacement id was generated for %q", candidate)
		}
	}
}

func TestRecovererTurnsPanicsIntoErrors(t *testing.T) {
	handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went very wrong")
	}), Recoverer, RequestID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// The panic message may name internal state; it belongs in the log, not
	// in a response to whoever triggered it.
	if strings.Contains(rec.Body.String(), "something went very wrong") {
		t.Error("the panic message leaked into the response body")
	}
}

func TestLimiterShedsBeyondCapacity(t *testing.T) {
	const capacity = 2
	release := make(chan struct{})
	admitted := make(chan struct{}, capacity)

	handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted <- struct{}{}
		<-release
	}), RequestID, Limiter(capacity))

	var wg sync.WaitGroup
	codes := make([]int, capacity)
	for i := 0; i < capacity; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/work", nil))
			codes[i] = rec.Code
		}(i)
	}

	// Wait until both slots are genuinely occupied before probing the limit.
	<-admitted
	<-admitted

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/work", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("overflow request status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a shed response should tell the client when to come back")
	}

	close(release)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("admitted request %d got %d, want 200", i, code)
		}
	}
}

// Health checks must answer even when the service is saturated. If they do
// not, the load balancer concludes the instance is dead and kills it at
// exactly the moment it is busiest.
func TestLimiterAlwaysAdmitsProbes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	admitted := make(chan struct{}, 1)

	rt := NewRouter()
	rt.Probe("GET /svc/health")
	rt.Handle("GET /svc/work", func(w http.ResponseWriter, r *http.Request) error {
		admitted <- struct{}{}
		<-release
		return nil
	})
	handler := rt.Build(1)

	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/svc/work", nil))
	<-admitted

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/svc/health", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("probe status = %d while saturated, want 200", rec.Code)
	}
}

func TestLimiterOfZeroIsDisabled(t *testing.T) {
	handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), Limiter(0))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// A 5xx must not echo the underlying error: it can carry an internal hostname,
// a connection string, or a driver detail. The request id is what ties the
// generic response back to the real cause in the logs.
func TestWriteErrorHidesInternalDetail(t *testing.T) {
	handler := Chain(H(func(w http.ResponseWriter, r *http.Request) error {
		return Wrap(http.StatusInternalServerError,
			errors.New("dial tcp 10.0.10.22:5432: connection refused"), "could not reach the ledger")
	}), RequestID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.10.22") {
		t.Error("an internal address leaked into the response body")
	}

	var body errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.RequestID == "" {
		t.Error("no request id to correlate the generic message with the logs")
	}
}

// Client errors keep their message: the caller can act on "quantity must be
// greater than zero", and hiding it would just generate support tickets.
func TestWriteErrorKeepsClientMessages(t *testing.T) {
	handler := Chain(H(func(w http.ResponseWriter, r *http.Request) error {
		return Errorf(http.StatusBadRequest, "quantity must be greater than zero")
	}), RequestID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "greater than zero") {
		t.Errorf("body = %s, want the actionable message", rec.Body.String())
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	var target struct {
		Quantity int `json:"quantity"`
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"qty": 5}`))
	err := DecodeJSON(req, &target)

	if !IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("err = %v, want 400: an unrecognised field must not silently default to zero", err)
	}
}
