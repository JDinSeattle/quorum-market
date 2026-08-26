package obs

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metric names follow the Prometheus convention of base units and a _total
// suffix on counters. The service's own identity is deliberately absent from
// the labels: that belongs to the scrape target, and baking it into every
// series would duplicate it across the whole time series database.
const namespace = "quorum"

// Latency buckets are chosen around the simulated work this system does. The
// delay distribution has a median near 245ms and a tail past a second, so the
// buckets are dense between 50ms and 2s where the interesting mass sits, with
// coarse buckets either side to catch cache-fast responses and timeouts.
var latencyBuckets = []float64{
	0.005, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10,
}

var (
	// Inbound HTTP: the RED metrics, meaning rate, errors and duration.
	serverRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http_server", Name: "requests_total",
		Help: "Requests handled, by route and response status.",
	}, []string{"method", "route", "status"})

	serverDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "http_server", Name: "request_duration_seconds",
		Help: "Time to serve a request, by route.", Buckets: latencyBuckets,
	}, []string{"method", "route"})

	serverInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "http_server", Name: "in_flight_requests",
		Help: "Requests currently being served.",
	})

	serverShed = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http_server", Name: "shed_requests_total",
		Help: "Requests rejected because the in-flight limit was reached.",
	})

	// Outbound HTTP: one service calling another.
	clientRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http_client", Name: "requests_total",
		Help: "Downstream calls made, by dependency and outcome.",
	}, []string{"dependency", "method", "status"})

	clientDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "http_client", Name: "request_duration_seconds",
		Help: "Time for a downstream call to complete.", Buckets: latencyBuckets,
	}, []string{"dependency", "method"})

	clientRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http_client", Name: "retries_total",
		Help: "Downstream calls retried after a failure.",
	}, []string{"dependency"})

	breakerState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "circuit_breaker", Name: "state",
		Help: "Circuit breaker state: 0 closed, 1 half-open, 2 open.",
	}, []string{"dependency"})

	breakerRejections = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "circuit_breaker", Name: "rejected_total",
		Help: "Calls refused without being attempted because the breaker was open.",
	}, []string{"dependency"})

	// Domain metrics: what an operator actually alerts on. A rising decline
	// rate or a growing pool of expired reservations is a business incident
	// that no amount of CPU and latency graphs would reveal.
	businessEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "domain", Name: "events_total",
		Help: "Domain outcomes worth alerting on.",
	}, []string{"event", "outcome"})

	queueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "queue", Name: "messages_in_flight",
		Help: "Messages received from the broker and not yet acknowledged.",
	}, []string{"queue"})

	queueMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "queue", Name: "messages_total",
		Help: "Queue messages by disposition.",
	}, []string{"queue", "disposition"})

	queuePublishDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "queue", Name: "publish_duration_seconds",
		Help: "Time to publish and receive a broker confirmation.", Buckets: latencyBuckets,
	}, []string{"queue"})

	// Cache behaviour. The hit ratio is the headline, but the miss reasons
	// matter as much: coalesced misses mean the stampede protection is
	// earning its keep, and errors mean the cache is failing open.
	cacheOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "cache", Name: "operations_total",
		Help: "Cache lookups by outcome.",
	}, []string{"cache", "result"})

	// Rate limiting. Throttled traffic is a deliberate rejection rather than a
	// failure, so it is counted separately from errors.
	rateLimitDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ratelimit", Name: "decisions_total",
		Help: "Rate limiter verdicts by scope.",
	}, []string{"scope", "decision"})

	// Authentication. A rising failure rate is either an integration that
	// broke or an attack, and both are worth knowing about.
	authAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "auth", Name: "attempts_total",
		Help: "Authentication attempts by outcome.",
	}, []string{"operation", "result"})

	// Replication between database nodes.
	replicationOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "kv", Name: "replication_total",
		Help: "Replication attempts between database nodes.",
	}, []string{"operation", "outcome"})

	quorumFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "kv", Name: "quorum_failures_total",
		Help: "Operations that could not reach their quorum.",
	}, []string{"operation"})

	readRepairs = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "kv", Name: "read_repairs_total",
		Help: "Stale replicas corrected by a read.",
	})
)

// ── Recording helpers ────────────────────────────────────────────────────────
//
// Every metric is written through a function here rather than by touching the
// collectors directly, so label cardinality stays governed in one file.

// ObserveServerRequest records one handled request.
func ObserveServerRequest(method, route string, status int, elapsed time.Duration) {
	serverRequests.WithLabelValues(method, route, statusClass(status)).Inc()
	serverDuration.WithLabelValues(method, route).Observe(elapsed.Seconds())
}

// ServerInFlightAdd adjusts the in-flight gauge.
func ServerInFlightAdd(delta float64) { serverInFlight.Add(delta) }

// ObserveShedRequest records a request rejected by the concurrency limiter.
func ObserveShedRequest() { serverShed.Inc() }

// ObserveClientRequest records one downstream call.
func ObserveClientRequest(dependency, method string, status int, elapsed time.Duration) {
	clientRequests.WithLabelValues(dependency, method, statusClass(status)).Inc()
	clientDuration.WithLabelValues(dependency, method).Observe(elapsed.Seconds())
}

// ObserveClientRetry records a retried downstream call.
func ObserveClientRetry(dependency string) { clientRetries.WithLabelValues(dependency).Inc() }

// SetBreakerState publishes a breaker's current state.
func SetBreakerState(dependency string, state float64) {
	breakerState.WithLabelValues(dependency).Set(state)
}

// ObserveBreakerRejection records a call refused by an open breaker.
func ObserveBreakerRejection(dependency string) { breakerRejections.WithLabelValues(dependency).Inc() }

// ObserveBusinessEvent records a domain outcome, e.g. ("checkout", "declined").
func ObserveBusinessEvent(event, outcome string) {
	businessEvents.WithLabelValues(event, outcome).Inc()
}

// QueueInFlightAdd adjusts the unacknowledged-message gauge.
func QueueInFlightAdd(queue string, delta float64) { queueDepth.WithLabelValues(queue).Add(delta) }

// ObserveQueueMessage records a message's disposition: acked, requeued, dropped.
func ObserveQueueMessage(queue, disposition string) {
	queueMessages.WithLabelValues(queue, disposition).Inc()
}

// ObserveQueuePublish records a publish round trip.
func ObserveQueuePublish(queue string, elapsed time.Duration, err error) {
	queuePublishDuration.WithLabelValues(queue).Observe(elapsed.Seconds())
	if err != nil {
		queueMessages.WithLabelValues(queue, "publish_failed").Inc()
		return
	}
	queueMessages.WithLabelValues(queue, "published").Inc()
}

// ObserveReplication records one replication attempt.
func ObserveReplication(operation string, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	replicationOps.WithLabelValues(operation, outcome).Inc()
}

// ObserveQuorumFailure records an operation that missed its quorum.
func ObserveQuorumFailure(operation string) { quorumFailures.WithLabelValues(operation).Inc() }

// ObserveReadRepair records a stale replica corrected by a read.
func ObserveReadRepair() { readRepairs.Inc() }

// ObserveCache records a cache lookup outcome: hit, hit_negative, miss,
// coalesced, invalidated, abandoned, or error.
//
// "abandoned" and "error" are deliberately separate. A caller that hangs up
// mid-request is not a cache failure, and folding the two together would make
// a burst of client disconnections look like Redis falling over.
func ObserveCache(cache, result string) { cacheOps.WithLabelValues(cache, result).Inc() }

// ObserveRateLimit records a limiter verdict: allowed, throttled, or error.
func ObserveRateLimit(scope, decision string) {
	rateLimitDecisions.WithLabelValues(scope, decision).Inc()
}

// ObserveAuth records an authentication attempt, e.g. ("login", "denied").
func ObserveAuth(operation, result string) { authAttempts.WithLabelValues(operation, result).Inc() }

// statusClass collapses a status code to its class.
//
// Recording the exact code would give a label value per code, and codes are
// attacker-influenced on some paths. The class is what alerting rules actually
// group on, and the exact code is still in the logs when it matters.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	case status > 0:
		return strconv.Itoa(status/100) + "xx"
	default:
		return "error"
	}
}

// RegisterGauge publishes a value that is computed when Prometheus scrapes,
// rather than pushed on every change.
//
// For a value the process can already answer cheaply and exactly — the number
// of held reservations, the size of a store — this is strictly better than
// mirroring it into a gauge on every mutation: there is no second copy to
// drift, and no hot-path cost.
func RegisterGauge(subsystem, name, help string, read func() float64) {
	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: name, Help: help,
	}, read))
}
