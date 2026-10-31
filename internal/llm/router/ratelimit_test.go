package router

import (
	"testing"
	"time"
)

func TestNewRateLimiterStartsWithFullTokens(t *testing.T) {
	rl := NewRateLimiter(5, 60)
	if rl.tokens != 5 {
		t.Errorf("initial tokens = %v, want 5", rl.tokens)
	}
	if rl.maxTokens != 5 {
		t.Errorf("maxTokens = %v, want 5", rl.maxTokens)
	}
}

func TestAllowConsumesTokens(t *testing.T) {
	rl := NewRateLimiter(5, 60)

	for i := 0; i < 5; i++ {
		if !rl.Allow() {
			t.Errorf("Allow() returned false on call %d (expected true)", i+1)
		}
	}
}

func TestAllowReturnsFalseWhenExhausted(t *testing.T) {
	rl := NewRateLimiter(5, 60)

	for i := 0; i < 5; i++ {
		rl.Allow()
	}

	if rl.Allow() {
		t.Error("Allow() returned true after bucket was exhausted, want false")
	}
}

func TestWaitTimeIsZeroWhenTokensAvailable(t *testing.T) {
	rl := NewRateLimiter(5, 60)

	wait := rl.WaitTime()
	if wait != 0 {
		t.Errorf("WaitTime() = %v, want 0 when tokens are available", wait)
	}
}

func TestWaitTimeIsPositiveWhenExhausted(t *testing.T) {
	// Very low refill rate so tokens don't replenish during the test
	rl := NewRateLimiter(3, 1) // 1 token per minute → ~1 per 60 seconds

	for i := 0; i < 3; i++ {
		rl.Allow()
	}

	wait := rl.WaitTime()
	if wait <= 0 {
		t.Errorf("WaitTime() = %v, want > 0 when bucket is exhausted", wait)
	}
}

func TestRefillOverTime(t *testing.T) {
	// 600 tokens/minute = 10 tokens/second
	rl := NewRateLimiter(10, 600)

	// Exhaust all tokens
	for i := 0; i < 10; i++ {
		rl.Allow()
	}

	if rl.Allow() {
		t.Fatal("bucket should be exhausted immediately after 10 calls")
	}

	// Manually advance lastRefill to simulate elapsed time (1 second)
	rl.mu.Lock()
	rl.lastRefill = rl.lastRefill.Add(-1 * time.Second)
	rl.mu.Unlock()

	// After 1 second at 10 tokens/second, ~10 tokens should be available
	if !rl.Allow() {
		t.Error("Allow() should return true after simulated refill time")
	}
}

func TestMaxTokensCapIsRespected(t *testing.T) {
	rl := NewRateLimiter(5, 600)

	// Simulate a very long elapsed time (1 hour)
	rl.mu.Lock()
	rl.lastRefill = rl.lastRefill.Add(-1 * time.Hour)
	rl.mu.Unlock()

	// Trigger refill
	rl.mu.Lock()
	rl.refill()
	tokens := rl.tokens
	rl.mu.Unlock()

	if tokens > rl.maxTokens {
		t.Errorf("tokens after refill = %v, exceeds maxTokens = %v", tokens, rl.maxTokens)
	}
}
