package llm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// RetryPolicy controls how Service.Execute re-attempts transient failures.
// The defaults favour local LLM setups (Ollama) that can briefly drop
// connections or return 502s under load without the call itself being
// truly broken — patterns we saw in production where a single HTTP hiccup
// cost multi-hour tool-loop runs.
type RetryPolicy struct {
	// MaxAttempts is inclusive of the first try. 1 disables retries.
	MaxAttempts int
	// InitialBackoff is the first wait after a transient failure.
	InitialBackoff time.Duration
	// MaxBackoff caps the exponential wait between retries.
	MaxBackoff time.Duration
	// OnRetry fires before sleeping between attempts. Use it to emit
	// heartbeat events to the dashboard so operators see progress
	// instead of a silent multi-minute pause. Safe to leave nil.
	OnRetry func(attempt int, err error, nextDelay time.Duration)
}

// DefaultRetryPolicy is what Service.Execute uses unless overridden.
// 3 attempts with 1s → 4s → 16s backoff keeps the floor modest for
// local-model setups. Callers running against cloud providers with
// their own retry logic can set MaxAttempts=1 to disable.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Second,
		MaxBackoff:     16 * time.Second,
	}
}

// withRetry runs op() and retries on transient errors per the policy.
// Returns the final non-nil error after exhausting attempts, or nil on
// success. Respects ctx cancellation — sleeps between retries are
// interruptible. Non-transient errors return immediately.
func withRetry(ctx context.Context, policy RetryPolicy, op func() error) error {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	if policy.InitialBackoff <= 0 {
		policy.InitialBackoff = time.Second
	}
	if policy.MaxBackoff < policy.InitialBackoff {
		policy.MaxBackoff = policy.InitialBackoff
	}

	backoff := policy.InitialBackoff
	var lastErr error
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			if lastErr != nil {
				return fmt.Errorf("context canceled after %d attempts; last error: %w", attempt, lastErr)
			}
			return ctx.Err()
		}
		err := op()
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsTransientLLMError(err) {
			return err
		}
		if attempt+1 >= policy.MaxAttempts {
			return fmt.Errorf("transient error after %d attempts: %w", policy.MaxAttempts, err)
		}
		if policy.OnRetry != nil {
			policy.OnRetry(attempt+1, err, backoff)
		}
		log.Printf("llm: transient error on attempt %d/%d (%v); retrying in %s", attempt+1, policy.MaxAttempts, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return fmt.Errorf("context canceled during retry wait; last error: %w", err)
		}
		backoff *= 2
		if backoff > policy.MaxBackoff {
			backoff = policy.MaxBackoff
		}
	}
	return lastErr
}

// IsTransientLLMError classifies an error as transient (retryable) or
// terminal. Exported so the tool-loop in package tools can use it.
func IsTransientLLMError(err error) bool {
	if err == nil {
		return false
	}
	// Context cancellations propagated from the caller are NOT transient —
	// the caller explicitly wanted to stop. Only treat inner deadline
	// exceeded (HTTP-level timeout inside the adapter) as transient.
	if errors.Is(err, context.Canceled) {
		return false
	}
	msg := strings.ToLower(err.Error())
	transientSubstrings := []string{
		"context deadline exceeded",      // HTTP client timeout or per-call deadline
		"connection reset",                // mid-stream network blip
		"connection refused",              // server briefly down / restarting
		"eof",                             // connection closed mid-read
		"broken pipe",
		"timeout",
		"temporary failure",
		"service unavailable",             // 503
		"bad gateway",                     // 502
		"gateway timeout",                 // 504
		"too many requests",               // 429
		"rate limit",
		"status 429",
		"status 502",
		"status 503",
		"status 504",
		"status 529",                      // anthropic capacity
		"overloaded",                      // anthropic capacity text
	}
	for _, sub := range transientSubstrings {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
