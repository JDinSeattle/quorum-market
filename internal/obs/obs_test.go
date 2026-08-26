package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatusClassCollapsesCodes(t *testing.T) {
	// Recording the exact code would give a label value per code, and codes
	// are attacker-influenced on some paths.
	cases := map[int]string{
		200: "2xx", 201: "2xx", 301: "3xx",
		400: "4xx", 404: "4xx", 429: "4xx",
		500: "5xx", 503: "5xx",
		0: "error",
	}
	for status, want := range cases {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestRequestIDsRoundTripThroughAContext(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req-123")
	if got := RequestIDFrom(ctx); got != "req-123" {
		t.Errorf("RequestIDFrom = %q, want req-123", got)
	}
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Errorf("an unmarked context returned %q, want empty", got)
	}
}

func TestNewRequestIDsAreDistinct(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if id == "" || id == "unknown" {
			t.Fatalf("NewRequestID returned %q", id)
		}
		if seen[id] {
			t.Fatalf("NewRequestID collided on %q", id)
		}
		seen[id] = true
	}
}

// This is what makes a checkout traceable across services without every call
// site remembering to log the id.
func TestTheContextHandlerAttachesTheRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(ContextHandler{Handler: slog.NewJSONHandler(&buf, nil)})

	logger.InfoContext(WithRequestID(context.Background(), "req-abc"), "something happened")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("the log line is not JSON: %v", err)
	}
	if line["request_id"] != "req-abc" {
		t.Errorf("request_id = %v, want req-abc", line["request_id"])
	}
}

// Adding attributes or opening a group must not strip the wrapper, or the
// request id silently stops appearing on any logger derived from the default.
func TestTheContextHandlerSurvivesDerivation(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(ContextHandler{Handler: slog.NewJSONHandler(&buf, nil)})

	derived := base.With("service", "test").WithGroup("inner")
	derived.InfoContext(WithRequestID(context.Background(), "req-xyz"), "hello")

	if !strings.Contains(buf.String(), "req-xyz") {
		t.Errorf("the request id was lost when the logger was derived: %s", buf.String())
	}
}

func TestBuildReportsSomethingUseful(t *testing.T) {
	build := Build()

	if build.Version == "" {
		t.Error("no version reported")
	}
	if build.GoVersion == "" || build.Platform == "" {
		t.Error("no toolchain or platform reported")
	}
}

// Liveness must not depend on anything external: tying it to a dependency
// means one database outage restarts every instance that talks to it.
func TestHealthWithNoChecksIsUp(t *testing.T) {
	report := NewHealth().Report(context.Background())

	if report.Status != StatusUp {
		t.Errorf("status = %q, want up", report.Status)
	}
}

// A shared dependency failing must degrade the instance, not remove it. Taking
// the whole fleet out of rotation turns a partial failure into a total one.
func TestAFailingOptionalCheckDegradesRatherThanDowns(t *testing.T) {
	health := NewHealth(
		Check{Name: "database", Optional: true, Probe: failing},
		Check{Name: "broker", Optional: true, Probe: succeeding},
	)

	report := health.Report(context.Background())
	if report.Status != StatusDegraded {
		t.Fatalf("status = %q, want degraded", report.Status)
	}
	if report.Checks["database"].Status != StatusDown {
		t.Error("the failing check is not reported as down")
	}
	if report.Checks["database"].Error == "" {
		t.Error("the failing check does not say why")
	}
	if report.Checks["broker"].Status != StatusUp {
		t.Error("the healthy check was marked down")
	}
}

func TestARequiredCheckTakesTheInstanceDown(t *testing.T) {
	health := NewHealth(Check{Name: "local-disk", Probe: failing})

	if got := health.Report(context.Background()).Status; got != StatusDown {
		t.Errorf("status = %q, want down", got)
	}
}

// Draining is the one thing that flips readiness to down, so a deploy takes
// the instance out of rotation before it stops answering.
func TestDrainingReportsDown(t *testing.T) {
	health := NewHealth(Check{Name: "everything", Optional: true, Probe: succeeding})

	if got := health.Report(context.Background()).Status; got != StatusUp {
		t.Fatalf("status = %q before draining, want up", got)
	}

	health.Drain()

	if !health.Draining() {
		t.Error("Draining() is false after Drain()")
	}
	if got := health.Report(context.Background()).Status; got != StatusDown {
		t.Errorf("status = %q while draining, want down", got)
	}
}

// A load balancer polls readiness every few seconds per target; without the
// cache, health checking becomes its own significant load on every dependency.
func TestReportsAreCachedBriefly(t *testing.T) {
	var calls int
	health := NewHealth(Check{
		Name:     "counted",
		Optional: true,
		Probe: func(context.Context) error {
			calls++
			return nil
		},
	})

	for i := 0; i < 5; i++ {
		health.Report(context.Background())
	}
	if calls != 1 {
		t.Errorf("the probe ran %d times for 5 reports, want 1", calls)
	}
}

// A probe that hangs must not hang readiness with it.
func TestSlowProbesTimeOut(t *testing.T) {
	health := NewHealth(Check{
		Name:     "hangs",
		Optional: true,
		Probe: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})

	started := time.Now()
	report := health.Report(context.Background())

	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("readiness took %v against a hanging probe", elapsed)
	}
	if report.Checks["hangs"].Status != StatusDown {
		t.Error("a timed-out probe was not reported as down")
	}
}

func TestTheAdminSurface(t *testing.T) {
	health := NewHealth(Check{Name: "dep", Optional: true, Probe: succeeding})
	handler := AdminHandler(health)

	for path, want := range map[string]int{
		"/healthz":             http.StatusOK,
		"/readyz":              http.StatusOK,
		"/version":             http.StatusOK,
		"/metrics":             http.StatusOK,
		"/debug/pprof/":        http.StatusOK,
		"/debug/pprof/cmdline": http.StatusOK,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s status = %d, want %d", path, rec.Code, want)
		}
	}
}

// Readiness has to answer 503 when the instance should not receive traffic, or
// a deploy gate has nothing to check.
func TestReadyzAnswers503WhenDown(t *testing.T) {
	health := NewHealth()
	health.Drain()

	rec := httptest.NewRecorder()
	AdminHandler(health).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d while draining, want 503", rec.Code)
	}
}

// Degraded still returns 200: a service that can serve most requests should
// keep receiving them.
func TestReadyzAnswers200WhenDegraded(t *testing.T) {
	health := NewHealth(Check{Name: "dep", Optional: true, Probe: failing})

	rec := httptest.NewRecorder()
	AdminHandler(health).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d while degraded, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), StatusDegraded) {
		t.Errorf("body does not report degraded: %s", rec.Body.String())
	}
}

func succeeding(context.Context) error { return nil }
func failing(context.Context) error    { return errors.New("dependency is unreachable") }
