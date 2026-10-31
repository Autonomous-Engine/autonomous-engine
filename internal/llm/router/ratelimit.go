package router

import (
	"sync"
	"time"
)

// RateLimiter implements a token-bucket rate limiter.
// Tokens are refilled at refillRate per minute up to maxTokens.
type RateLimiter struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens added per second
	lastRefill time.Time
}

// NewRateLimiter creates a RateLimiter with the given capacity and refill rate.
// refillPerMinute is the number of tokens added per minute.
func NewRateLimiter(maxTokens int, refillPerMinute float64) *RateLimiter {
	return &RateLimiter{
		tokens:     float64(maxTokens),
		maxTokens:  float64(maxTokens),
		refillRate: refillPerMinute / 60.0,
		lastRefill: time.Now(),
	}
}

// refill adds tokens based on elapsed time since last refill (must be called with mu held).
func (r *RateLimiter) refill() {
	now := time.Now()
	elapsed := now.Sub(r.lastRefill).Seconds()
	r.lastRefill = now
	r.tokens += elapsed * r.refillRate
	if r.tokens > r.maxTokens {
		r.tokens = r.maxTokens
	}
}

// Allow consumes one token and returns true if a request is permitted.
// Returns false if no tokens are available.
func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refill()
	if r.tokens >= 1.0 {
		r.tokens--
		return true
	}
	return false
}

// WaitTime returns how long the caller should wait before the next token
// becomes available. Returns 0 if a token is already available.
func (r *RateLimiter) WaitTime() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refill()
	if r.tokens >= 1.0 {
		return 0
	}
	deficit := 1.0 - r.tokens
	waitSeconds := deficit / r.refillRate
	return time.Duration(waitSeconds * float64(time.Second))
}
