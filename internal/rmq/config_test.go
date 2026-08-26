package rmq

import (
	"strings"
	"testing"
	"time"
)

// Credentials go into a URL, so anything that is special in a URL has to be
// escaped. A password containing a slash or an @ would otherwise silently
// redirect the connection to a different host or vhost.
func TestURLEscapesCredentials(t *testing.T) {
	cfg := Config{
		Host: "broker.internal", Port: 5672,
		User: "svc/cart", Password: "p@ss w:rd", VHost: "/",
	}

	got := cfg.URL()
	if strings.Contains(got, "p@ss") {
		t.Errorf("URL = %q: the raw password leaked into the authority", got)
	}
	if !strings.Contains(got, "broker.internal:5672") {
		t.Errorf("URL = %q, want it to address the configured host", got)
	}
	// A vhost of "/" is the default and is expressed as an empty path, not a
	// literal slash, which would mean a vhost actually named "/".
	if strings.HasSuffix(got, "/%2F") {
		t.Errorf("URL = %q: the default vhost should be an empty path", got)
	}
}

func TestURLEncodesANamedVHost(t *testing.T) {
	cfg := Config{Host: "h", Port: 5672, User: "u", Password: "p", VHost: "prod/orders"}

	if got := cfg.URL(); !strings.HasSuffix(got, "prod%2Forders") {
		t.Errorf("URL = %q, want the vhost path-escaped", got)
	}
}

// Logs and error messages carry the connection string, so it must never be the
// one holding the password.
func TestRedactedHidesThePassword(t *testing.T) {
	cfg := Config{Host: "h", Port: 5672, User: "u", Password: "hunter2", VHost: "/"}

	got := cfg.Redacted()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Redacted() = %q, which contains the password", got)
	}
	if !strings.Contains(got, "u:***@h:5672") {
		t.Errorf("Redacted() = %q, want it to stay readable", got)
	}
}

// Both services have to derive the same dead-letter names from the same work
// queue, or the producer declares one topology and the consumer another.
func TestDeadLetterNamesAreDerivedFromTheQueue(t *testing.T) {
	if got := DeadLetterQueue("orders_queue"); got != "orders_queue.dlq" {
		t.Errorf("DeadLetterQueue = %q", got)
	}
	if got := DeadLetterExchange("orders_queue"); got != "orders_queue.dlx" {
		t.Errorf("DeadLetterExchange = %q", got)
	}
}

func TestConfigFromEnvPrefersTheExplicitNames(t *testing.T) {
	t.Setenv("RABBITMQ_HOST", "from-rabbitmq")
	t.Setenv("RMQ_HOST", "from-rmq")

	if got := ConfigFromEnv("q").Host; got != "from-rabbitmq" {
		t.Errorf("Host = %q, want RABBITMQ_HOST to win", got)
	}
}

func TestConfigFromEnvFallsBackToTheShortNames(t *testing.T) {
	t.Setenv("RMQ_HOST", "only-short")
	t.Setenv("RMQ_PORT", "5673")

	cfg := ConfigFromEnv("orders")
	if cfg.Host != "only-short" {
		t.Errorf("Host = %q", cfg.Host)
	}
	if cfg.Port != 5673 {
		t.Errorf("Port = %d, want 5673", cfg.Port)
	}
	if cfg.Queue != "orders" {
		t.Errorf("Queue = %q, want the supplied default", cfg.Queue)
	}
}

func TestConfigFromEnvHasWorkableDefaults(t *testing.T) {
	cfg := ConfigFromEnv("orders_queue")

	if cfg.DialAttempts < 2 {
		t.Error("a single dial attempt would fail whenever the broker starts second")
	}
	if cfg.ConfirmTimeout <= 0 {
		t.Error("publisher confirms need a timeout, or a stalled broker hangs the publisher")
	}
	if cfg.PoolSize < 1 || cfg.Workers < 1 || cfg.Prefetch < 1 {
		t.Errorf("degenerate defaults: pool=%d workers=%d prefetch=%d",
			cfg.PoolSize, cfg.Workers, cfg.Prefetch)
	}
	if cfg.DialBackoff <= 0 || cfg.DialBackoff > time.Minute {
		t.Errorf("DialBackoff = %v, want a short positive delay", cfg.DialBackoff)
	}
}
