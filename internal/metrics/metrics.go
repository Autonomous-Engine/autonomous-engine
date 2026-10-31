package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// Simple Prometheus-compatible metrics without importing the full prometheus client.
// Outputs text format that Prometheus can scrape.

// Counter is a monotonically increasing counter metric.
type Counter struct {
	name string
	help string
	// labels is the rendered label set (`{service="x"}`), empty for an
	// unlabelled counter.
	labels string
	value  atomic.Int64
}

// Gauge is a metric that can go up or down.
type Gauge struct {
	name  string
	help  string
	value atomic.Int64
}

// Registry holds all registered metrics.
type Registry struct {
	mu       sync.RWMutex
	counters []*Counter
	gauges   []*Gauge
}

var globalRegistry = &Registry{}

// NewCounter creates and registers a new counter.
func NewCounter(name, help string) *Counter {
	c := &Counter{name: name, help: help}
	globalRegistry.mu.Lock()
	globalRegistry.counters = append(globalRegistry.counters, c)
	globalRegistry.mu.Unlock()
	return c
}

// NewLabeledCounter creates and registers one series of a counter with a
// single label, e.g. ae_snapshot_unpinned_total{service="x"}. Series of the
// same name share one HELP/TYPE header in the output.
func NewLabeledCounter(name, help, label, value string) *Counter {
	c := &Counter{name: name, help: help, labels: fmt.Sprintf("{%s=%q}", label, value)}
	globalRegistry.mu.Lock()
	globalRegistry.counters = append(globalRegistry.counters, c)
	globalRegistry.mu.Unlock()
	return c
}

// Value returns the counter's current value.
func (c *Counter) Value() int64 { return c.value.Load() }

// NewGauge creates and registers a new gauge.
func NewGauge(name, help string) *Gauge {
	g := &Gauge{name: name, help: help}
	globalRegistry.mu.Lock()
	globalRegistry.gauges = append(globalRegistry.gauges, g)
	globalRegistry.mu.Unlock()
	return g
}

// Inc increments the counter by 1.
func (c *Counter) Inc() { c.value.Add(1) }

// Add increments the counter by n.
func (c *Counter) Add(n int64) { c.value.Add(n) }

// Set sets the gauge to exactly n.
func (g *Gauge) Set(n int64) { g.value.Store(n) }

// Inc increments the gauge by 1.
func (g *Gauge) Inc() { g.value.Add(1) }

// Dec decrements the gauge by 1.
func (g *Gauge) Dec() { g.value.Add(-1) }

// Handler returns an http.HandlerFunc that emits the Prometheus text format for
// all registered metrics. Mount this at /metrics on any HTTP mux.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		globalRegistry.mu.RLock()
		defer globalRegistry.mu.RUnlock()
		described := map[string]bool{}
		for _, c := range globalRegistry.counters {
			if !described[c.name] {
				described[c.name] = true
				fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
			}
			fmt.Fprintf(w, "%s%s %d\n", c.name, c.labels, c.value.Load())
		}
		for _, g := range globalRegistry.gauges {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n",
				g.name, g.help, g.name, g.name, g.value.Load())
		}
	}
}

// Standard metrics used across services.
var (
	WorkItemsProcessed = NewCounter("ae_work_items_processed_total", "Total work items processed")
	WorkItemsFailed    = NewCounter("ae_work_items_failed_total", "Total work items that failed")
	ToolCallsExecuted  = NewCounter("ae_tool_calls_executed_total", "Total tool calls executed by agents")
	LLMCallsTotal      = NewCounter("ae_llm_calls_total", "Total LLM API calls")
	LLMTokensUsed      = NewCounter("ae_llm_tokens_used_total", "Total LLM tokens consumed")
	ActiveWorkItems    = NewGauge("ae_active_work_items", "Currently active work items")
	ActiveAgents       = NewGauge("ae_active_agents", "Currently active agent instances")

	// EvidenceCompatLegacyFallback counts implementation.complete messages
	// that carried no evidence and were decided by the legacy success
	// boolean (EVIDENCE_COMPAT_WINDOW=legacy). Shared by every consumer of
	// the topic so the name cannot drift; it should read zero once every
	// producer emits evidence, at which point strict can be enabled.
	EvidenceCompatLegacyFallback = NewCounter("ae_evidence_compat_legacy_fallback_total", "implementation.complete messages without evidence decided by the legacy success boolean")
)
