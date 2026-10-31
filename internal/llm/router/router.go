package router

import (
	"context"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/redis/go-redis/v9"
)

// RouterConfig holds configuration for the Router.
type RouterConfig struct {
	// Primary is the main adapter to use.
	Primary adapter.ModelAdapter

	// Fallback is used when Primary is unhealthy or returns an error.
	// May be nil.
	Fallback adapter.ModelAdapter

	// MaxConcurrent limits simultaneous in-flight requests. 0 means unlimited.
	MaxConcurrent int

	// RateLimit is the maximum requests per minute. 0 disables rate limiting.
	RateLimit float64

	// SharedStore and RateLimitKey, both set, keep the RateLimit bucket in
	// Redis under "ae:rl:llm:<RateLimitKey>" so every replica using the same
	// key shares it. Either unset (REDIS_URL empty) keeps the bucket in
	// process. A Redis outage falls back to the in-process bucket.
	SharedStore  *redis.Client
	RateLimitKey string

	// HealthCheckInterval controls how frequently adapters are probed. 0 → 30s.
	HealthCheckInterval time.Duration
}

// Router routes completion requests to a primary adapter with optional fallback,
// rate limiting, concurrency control, and health monitoring.
type Router struct {
	primary        adapter.ModelAdapter
	fallback       adapter.ModelAdapter
	rateLimiter    limiter
	primaryHealth  *HealthMonitor
	fallbackHealth *HealthMonitor
	semaphore      chan struct{}
	cancelCtx      context.CancelFunc
}

// New creates and starts a Router from the provided config.
func New(cfg RouterConfig) *Router {
	ctx, cancel := context.WithCancel(context.Background())

	r := &Router{
		primary:   cfg.Primary,
		fallback:  cfg.Fallback,
		cancelCtx: cancel,
	}

	// Rate limiter
	if cfg.RateLimit > 0 {
		if cfg.SharedStore != nil && cfg.RateLimitKey != "" {
			r.rateLimiter = NewSharedRateLimiter(cfg.SharedStore, cfg.RateLimitKey, int(cfg.RateLimit), cfg.RateLimit)
		} else {
			r.rateLimiter = NewRateLimiter(int(cfg.RateLimit), cfg.RateLimit)
		}
	}

	// Concurrency semaphore
	if cfg.MaxConcurrent > 0 {
		r.semaphore = make(chan struct{}, cfg.MaxConcurrent)
	}

	// Health monitors
	r.primaryHealth = NewHealthMonitor(cfg.Primary, cfg.HealthCheckInterval)
	r.primaryHealth.Start(ctx)

	if cfg.Fallback != nil {
		r.fallbackHealth = NewHealthMonitor(cfg.Fallback, cfg.HealthCheckInterval)
		r.fallbackHealth.Start(ctx)
	}

	return r
}

// GetPrimary returns the primary adapter.
func (r *Router) GetPrimary() adapter.ModelAdapter {
	return r.primary
}

// Complete sends a synchronous completion request, trying the primary adapter
// first and falling back to the fallback adapter on error or unhealthy state.
func (r *Router) Complete(ctx context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()

	if err := r.waitForRateLimit(ctx); err != nil {
		return nil, err
	}

	// Try primary first
	if r.primaryHealth.IsHealthy() {
		resp, err := r.primary.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		// Fall through to fallback on error
	}

	// Try fallback
	if r.fallback != nil {
		if r.fallbackHealth == nil || r.fallbackHealth.IsHealthy() {
			return r.fallback.Complete(ctx, req)
		}
		return nil, fmt.Errorf("router: both primary and fallback adapters are unhealthy")
	}

	// No fallback available; try primary directly even if unhealthy
	return r.primary.Complete(ctx, req)
}

// Stream sends a streaming completion request. The returned channel is closed
// when streaming completes. The semaphore slot is released after the stream ends.
func (r *Router) Stream(ctx context.Context, req adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}

	if err := r.waitForRateLimit(ctx); err != nil {
		r.release()
		return nil, err
	}

	var (
		ch  <-chan adapter.StreamChunk
		err error
	)

	if r.primaryHealth.IsHealthy() {
		ch, err = r.primary.Stream(ctx, req)
	}

	if err != nil || ch == nil {
		if r.fallback != nil && (r.fallbackHealth == nil || r.fallbackHealth.IsHealthy()) {
			ch, err = r.fallback.Stream(ctx, req)
		}
	}

	if err != nil {
		r.release()
		return nil, err
	}
	if ch == nil {
		r.release()
		return nil, fmt.Errorf("router: no healthy adapter available for streaming")
	}

	// Wrap the channel so we release the semaphore when the stream ends
	wrapped := make(chan adapter.StreamChunk, 32)
	go func() {
		defer r.release()
		defer close(wrapped)
		for chunk := range ch {
			select {
			case wrapped <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	return wrapped, nil
}

// Close stops the health monitors.
func (r *Router) Close() {
	if r.cancelCtx != nil {
		r.cancelCtx()
	}
	if r.primaryHealth != nil {
		r.primaryHealth.Stop()
	}
	if r.fallbackHealth != nil {
		r.fallbackHealth.Stop()
	}
}

// ---- helpers ----

func (r *Router) acquire(ctx context.Context) error {
	if r.semaphore == nil {
		return nil
	}
	select {
	case r.semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Router) release() {
	if r.semaphore == nil {
		return
	}
	<-r.semaphore
}

func (r *Router) waitForRateLimit(ctx context.Context) error {
	if r.rateLimiter == nil {
		return nil
	}
	for {
		if r.rateLimiter.Allow() {
			return nil
		}
		wait := r.rateLimiter.WaitTime()
		if wait <= 0 {
			wait = 100 * time.Millisecond
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
