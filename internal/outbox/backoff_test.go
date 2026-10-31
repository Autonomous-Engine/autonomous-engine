package outbox

import (
	"math"
	"testing"
	"time"
)

var backoffNow = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

// noJitter keeps NextAttempt deterministic: rand() == 0.5 is the midpoint of
// the jitter window, so the delay is exactly the un-jittered backoff.
func noJitter() float64 { return 0.5 }

func TestNextAttempt_DoublesUntilTheCap(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: BackoffBase}, // defensive: treated as the first attempt
		{attempts: 1, want: 2 * time.Second},
		{attempts: 2, want: 4 * time.Second},
		{attempts: 3, want: 8 * time.Second},
		{attempts: 4, want: 16 * time.Second},
		{attempts: 5, want: 32 * time.Second},
		{attempts: 6, want: 64 * time.Second},
		{attempts: 7, want: 128 * time.Second},
		{attempts: 8, want: 256 * time.Second},
		{attempts: 9, want: BackoffCap}, // 512s > 5m
		{attempts: 25, want: BackoffCap},
		{attempts: 1000, want: BackoffCap}, // no shift overflow
	}
	for _, tc := range cases {
		got := NextAttempt(tc.attempts, backoffNow, noJitter).Sub(backoffNow)
		if got != tc.want {
			t.Errorf("NextAttempt(%d) delay = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

func TestNextAttempt_JitterStaysWithinTwentyPercent(t *testing.T) {
	for attempts := 1; attempts <= 12; attempts++ {
		base := NextAttempt(attempts, backoffNow, noJitter).Sub(backoffNow)
		low := NextAttempt(attempts, backoffNow, func() float64 { return 0 }).Sub(backoffNow)
		high := NextAttempt(attempts, backoffNow, func() float64 { return math.Nextafter(1, 0) }).Sub(backoffNow)
		if low != time.Duration(float64(base)*0.8) {
			t.Errorf("attempt %d: lowest delay = %v, want %v", attempts, low, time.Duration(float64(base)*0.8))
		}
		if high > time.Duration(float64(base)*1.2) || high <= base {
			t.Errorf("attempt %d: highest delay = %v, want just under %v", attempts, high, time.Duration(float64(base)*1.2))
		}
		for _, r := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.999} {
			d := NextAttempt(attempts, backoffNow, func() float64 { return r }).Sub(backoffNow)
			if d < low || d > high {
				t.Errorf("attempt %d rand %v: delay %v outside [%v, %v]", attempts, r, d, low, high)
			}
		}
	}
}

func TestNextAttempt_OutOfRangeJitterIsClamped(t *testing.T) {
	base := NextAttempt(3, backoffNow, noJitter).Sub(backoffNow)
	for _, r := range []float64{-5, 5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		d := NextAttempt(3, backoffNow, func() float64 { return r }).Sub(backoffNow)
		if d < time.Duration(float64(base)*0.8) || d > time.Duration(float64(base)*1.2) {
			t.Errorf("rand %v: delay %v escaped the jitter window around %v", r, d, base)
		}
	}
}

func TestNextAttempt_NilRandIsUsable(t *testing.T) {
	base := NextAttempt(4, backoffNow, noJitter).Sub(backoffNow)
	for i := 0; i < 50; i++ {
		d := NextAttempt(4, backoffNow, nil).Sub(backoffNow)
		if d < time.Duration(float64(base)*0.8) || d > time.Duration(float64(base)*1.2) {
			t.Fatalf("nil rand produced %v, outside the jitter window around %v", d, base)
		}
	}
}

func TestNextAttempt_IsAlwaysInTheFuture(t *testing.T) {
	for attempts := 0; attempts <= 30; attempts++ {
		if !NextAttempt(attempts, backoffNow, func() float64 { return 0 }).After(backoffNow) {
			t.Errorf("attempt %d scheduled at or before now", attempts)
		}
	}
}
