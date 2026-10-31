package health

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/metrics"
)

// Status represents the health status of a service at a point in time.
type Status struct {
	Service   string            `json:"service"`
	Status    string            `json:"status"`
	Checks    map[string]string `json:"checks"`
	Timestamp time.Time         `json:"timestamp"`
}

// Checker holds a set of named health-check functions for a service and
// exposes them through an HTTP handler.
type Checker struct {
	serviceName string
	mu          sync.RWMutex
	checks      map[string]func() error
}

// New creates a new Checker for the named service.
func New(serviceName string) *Checker {
	return &Checker{
		serviceName: serviceName,
		checks:      make(map[string]func() error),
	}
}

// AddCheck registers a named check function. The function should return nil
// when the component is healthy or a non-nil error that describes the failure.
// AddCheck is safe to call concurrently.
func (c *Checker) AddCheck(name string, fn func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks[name] = fn
}

// MetricsHandler returns an http.HandlerFunc that emits Prometheus-format
// metrics for all globally registered counters and gauges. Mount this at
// /metrics alongside /health.
func (c *Checker) MetricsHandler() http.HandlerFunc {
	return metrics.Handler()
}

// Handler returns an http.HandlerFunc that runs all registered checks and
// responds with a JSON-encoded Status.
//
// The HTTP status code is:
//   - 200 OK        — all checks passed
//   - 503 Service Unavailable — one or more checks failed
func (c *Checker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.RLock()
		// Copy the checks map so we don't hold the lock while running them.
		snapshot := make(map[string]func() error, len(c.checks))
		for name, fn := range c.checks {
			snapshot[name] = fn
		}
		c.mu.RUnlock()

		checkResults := make(map[string]string, len(snapshot))
		allHealthy := true

		for name, fn := range snapshot {
			if err := fn(); err != nil {
				checkResults[name] = err.Error()
				allHealthy = false
			} else {
				checkResults[name] = "healthy"
			}
		}

		overallStatus := "healthy"
		httpStatus := http.StatusOK
		if !allHealthy {
			overallStatus = "unhealthy"
			httpStatus = http.StatusServiceUnavailable
		}

		status := Status{
			Service:   c.serviceName,
			Status:    overallStatus,
			Checks:    checkResults,
			Timestamp: time.Now().UTC(),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		json.NewEncoder(w).Encode(status) //nolint:errcheck
	}
}
