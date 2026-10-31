package router

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// HealthMonitor runs periodic health checks against a ModelAdapter and exposes
// a thread-safe IsHealthy status.
type HealthMonitor struct {
	adapter  adapter.ModelAdapter
	interval time.Duration
	healthy  atomic.Bool
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewHealthMonitor creates a HealthMonitor that checks the adapter at the given interval.
// A zero or negative interval defaults to 30 seconds.
func NewHealthMonitor(a adapter.ModelAdapter, interval time.Duration) *HealthMonitor {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &HealthMonitor{
		adapter:  a,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start begins the background health check loop. It performs an immediate check
// and then continues on the configured interval.
func (m *HealthMonitor) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	go func() {
		defer close(m.done)

		// Perform an immediate check
		m.check(ctx)

		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.check(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop cancels the health check goroutine and waits for it to exit.
// Safe to call even if Start was never called.
func (m *HealthMonitor) Stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	<-m.done
}

// IsHealthy returns true if the most recent health check succeeded.
func (m *HealthMonitor) IsHealthy() bool {
	return m.healthy.Load()
}

func (m *HealthMonitor) check(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err := m.adapter.HealthCheck(checkCtx)
	m.healthy.Store(err == nil)
}
