package outbox

import (
	"math"
	"math/rand"
	"time"
)

// Retry schedule for a failed publish (M1-01 delivery semantics): exponential
// backoff from BackoffBase, doubling each attempt, clamped at BackoffCap, with
// ±BackoffJitter of proportional jitter so a fleet of dispatchers that failed
// on the same broker outage does not retry in lockstep.
const (
	// BackoffBase is the delay after the first failed attempt.
	BackoffBase = 2 * time.Second
	// BackoffFactor multiplies the delay on every further attempt.
	BackoffFactor = 2
	// BackoffCap is the longest un-jittered delay between attempts.
	BackoffCap = 5 * time.Minute
	// BackoffJitter is the proportional jitter applied to the delay: the
	// scheduled delay lies in [(1-j)*d, (1+j)*d].
	BackoffJitter = 0.2
)

// NextAttempt returns the time at which a row that has now failed `attempts`
// times becomes eligible again. It is pure: all randomness comes from rand,
// which must return a value in [0, 1) (math/rand.Float64's contract). A nil
// rand uses the default source; values outside [0, 1] and NaN are clamped so a
// misbehaving source can never schedule a retry outside the jitter window or,
// worse, in the past.
func NextAttempt(attempts int, now time.Time, rnd func() float64) time.Time {
	return now.Add(backoffDelay(attempts, jitterFraction(rnd)))
}

// backoffDelay is the jittered delay for the given attempt count. Split out so
// the exponential growth and the jitter can be reasoned about separately.
func backoffDelay(attempts int, r float64) time.Duration {
	if attempts < 1 {
		// Defensive: MarkFailed always passes the post-increment count, so
		// this only happens if a caller asks about a never-attempted row.
		attempts = 1
	}
	delay := BackoffBase
	for i := 1; i < attempts; i++ {
		delay *= BackoffFactor
		if delay >= BackoffCap {
			break
		}
	}
	if delay > BackoffCap {
		delay = BackoffCap
	}
	// r == 0 -> -jitter, r == 1 -> +jitter, r == 0.5 -> no jitter.
	offset := int64(float64(delay) * BackoffJitter * (2*r - 1))
	return delay + time.Duration(offset)
}

// jitterFraction reads the random source and clamps it into [0, 1].
func jitterFraction(rnd func() float64) float64 {
	if rnd == nil {
		rnd = rand.Float64
	}
	r := rnd()
	switch {
	case math.IsNaN(r):
		return 0.5
	case r < 0:
		return 0
	case r > 1:
		return 1
	default:
		return r
	}
}
