package outbox

import "github.com/autonomousengine/autonomous-engine/internal/metrics"

// Dispatcher metrics. They are registered on the process-wide registry in
// internal/metrics at init time, so any service that imports this package
// exports them from its /metrics endpoint even before a dispatcher runs —
// a counter that is absent until the first event is indistinguishable from a
// broken scrape, while a counter that reads 0 is a fact.
//
// internal/metrics has no labels, so there is one series per name and the
// counters are process-wide rather than per-dispatcher. A process runs one
// dispatcher (Start), so that is the same thing in practice.
var (
	// MetricPublished counts events the bus acknowledged and the store
	// recorded as published.
	MetricPublished = metrics.NewCounter("ae_outbox_published_total",
		"Outbox events published to the bus and marked published")
	// MetricFailed counts failed publish attempts, not failed events: one
	// event that is retried five times increments this five times.
	//
	// It is incremented when the publish fails, before the outcome is
	// written to the row, so it also counts the attempts that were never
	// recorded there — one whose claim had expired (ErrClaimLost) and one
	// whose MarkFailedBy itself failed. It can therefore run ahead of the
	// sum of `attempts` across the table, by design: it measures what this
	// process asked the bus to do and was refused, which is the rate worth
	// alerting on, while `attempts` measures what the queue believes.
	MetricFailed = metrics.NewCounter("ae_outbox_failed_total",
		"Outbox publish attempts that failed (one per attempt, not per event; includes attempts dropped without being recorded on the row, so it can exceed the sum of attempts in the table)")
	// MetricDead counts events that exhausted MaxAttempts and were marked
	// dead. Every increment needs an operator (see the runbook in the spec).
	MetricDead = metrics.NewCounter("ae_outbox_dead_total",
		"Outbox events that exhausted their attempts and were dead-lettered")

	// MetricPending is the dispatch backlog: rows neither published nor dead.
	MetricPending = metrics.NewGauge("ae_outbox_pending",
		"Outbox events neither published nor dead")
	// MetricOldestPendingSeconds is the age of the oldest such row. It is the
	// alerting signal: the count can sit flat at a healthy number while one
	// aggregate's head fails forever and its age climbs.
	MetricOldestPendingSeconds = metrics.NewGauge("ae_outbox_oldest_pending_seconds",
		"Age in seconds of the oldest unpublished, non-dead outbox event (0 when the queue is empty)")
)
