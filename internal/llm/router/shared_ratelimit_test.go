package router

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/redis/go-redis/v9"
)

// stubAdapter answers health checks; the tests never complete through it.
type stubAdapter struct{ adapter.ModelAdapter }

func (stubAdapter) HealthCheck(context.Context) error { return nil }
func (stubAdapter) Name() string                      { return "stub" }

func sharedPair(t *testing.T, addr string, key string, max int, perMinute float64) (*SharedRateLimiter, *SharedRateLimiter) {
	t.Helper()
	ca := redis.NewClient(&redis.Options{Addr: addr})
	cb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = ca.Close(); _ = cb.Close() })
	return NewSharedRateLimiter(ca, key, max, perMinute), NewSharedRateLimiter(cb, key, max, perMinute)
}

func checkSharedBucket(t *testing.T, a, b *SharedRateLimiter) {
	t.Helper()
	// Capacity 3, refill 1/min: three calls succeed in total across both
	// replicas, then both are empty.
	got := []bool{a.Allow(), b.Allow(), a.Allow(), b.Allow(), a.Allow()}
	want := []bool{true, true, true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d: allowed=%v, want %v (all: %v)", i+1, got[i], want[i], got)
		}
	}
	if w := b.WaitTime(); w <= 0 || w > time.Minute {
		t.Fatalf("WaitTime = %v, want in (0, 1m]", w)
	}
	// WaitTime does not consume: still empty, still denied.
	if a.Allow() {
		t.Fatal("bucket refilled without time passing")
	}
}

func TestSharedTokenBucketIsSharedAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := sharedPair(t, mr.Addr(), "test:shared", 3, 1)
	checkSharedBucket(t, a, b)

	other, _ := sharedPair(t, mr.Addr(), "test:other", 3, 1)
	if !other.Allow() {
		t.Fatal("a different key must have its own bucket")
	}
}

func TestSharedTokenBucketRefills(t *testing.T) {
	mr := miniredis.RunT(t)
	// 600/min = 10/s: one token back every 100ms.
	a, b := sharedPair(t, mr.Addr(), "test:refill", 1, 600)
	if !a.Allow() {
		t.Fatal("first call denied")
	}
	if b.Allow() {
		t.Fatal("empty bucket allowed a call")
	}
	time.Sleep(150 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("bucket did not refill")
	}
}

func TestSharedTokenBucketFallsBackToLocalWhenRedisIsDown(t *testing.T) {
	mr := miniredis.RunT(t)
	a, _ := sharedPair(t, mr.Addr(), "test:down", 2, 1)
	mr.Close()
	defer a.fallback.Recover() // the tracker is process-wide

	before := a.fallback.Total()
	got := []bool{a.Allow(), a.Allow(), a.Allow()}
	want := []bool{true, true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d: allowed=%v, want %v", i+1, got[i], want[i])
		}
	}
	if a.WaitTime() <= 0 {
		t.Fatal("local fallback WaitTime should be positive when empty")
	}
	if d := a.fallback.Total() - before; d < 3 {
		t.Fatalf("fallback counter delta = %d, want >= 3", d)
	}
}

// Routers are built per Kafka message (NewServiceForWorkItem), so the
// fallback state must be per process: once one router has seen Redis fail,
// another skips Redis during the same cooldown, and the outage is logged
// once, not once per router.
func TestSharedLimitersShareFallbackStateAcrossRouters(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	c := redis.NewClient(&redis.Options{Addr: addr})
	defer c.Close()
	r1 := New(RouterConfig{Primary: stubAdapter{}, RateLimit: 60, SharedStore: c, RateLimitKey: "fb:one"})
	defer r1.Close()
	r2 := New(RouterConfig{Primary: stubAdapter{}, RateLimit: 60, SharedStore: c, RateLimitKey: "fb:two"})
	defer r2.Close()
	l1 := r1.rateLimiter.(*SharedRateLimiter)
	l2 := r2.rateLimiter.(*SharedRateLimiter)

	if l1.fallback != l2.fallback {
		t.Fatal("routers have separate fallback trackers")
	}
	l1.fallback.Recover() // start from a known state; earlier tests share it
	buf.Reset()

	if !l1.Allow() {
		t.Fatal("local fallback denied the first call")
	}
	if l2.fallback.Available() {
		t.Fatal("second router would retry Redis inside the first router's cooldown")
	}
	start := time.Now()
	if !l2.Allow() {
		t.Fatal("local fallback denied the second router's first call")
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("second router waited %v on Redis; it should skip it", d)
	}
	if n := strings.Count(buf.String(), "Redis unreachable"); n != 1 {
		t.Fatalf("outage logged %d times, want 1:\n%s", n, buf.String())
	}
	l1.fallback.Recover()
}

func TestRouterUsesSharedLimiterOnlyWithStoreAndKey(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()

	local := New(RouterConfig{Primary: stubAdapter{}, RateLimit: 60})
	defer local.Close()
	if _, ok := local.rateLimiter.(*RateLimiter); !ok {
		t.Fatalf("no store: limiter = %T, want *RateLimiter", local.rateLimiter)
	}

	noKey := New(RouterConfig{Primary: stubAdapter{}, RateLimit: 60, SharedStore: c})
	defer noKey.Close()
	if _, ok := noKey.rateLimiter.(*RateLimiter); !ok {
		t.Fatalf("no key: limiter = %T, want *RateLimiter", noKey.rateLimiter)
	}

	shared := New(RouterConfig{Primary: stubAdapter{}, RateLimit: 60, SharedStore: c, RateLimitKey: "svc:p:m"})
	defer shared.Close()
	sl, ok := shared.rateLimiter.(*SharedRateLimiter)
	if !ok {
		t.Fatalf("store+key: limiter = %T, want *SharedRateLimiter", shared.rateLimiter)
	}
	if sl.key != "ae:rl:llm:svc:p:m" {
		t.Fatalf("key = %q", sl.key)
	}
	if err := shared.waitForRateLimit(context.Background()); err != nil {
		t.Fatalf("waitForRateLimit: %v", err)
	}
}

// TestSharedTokenBucketLiveRedis runs the same checks against a real Redis
// when TEST_REDIS_URL is set (e.g. redis://localhost:56379/0).
func TestSharedTokenBucketLiveRedis(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	key := "test:live:" + time.Now().Format("150405.000000000")
	a, b := sharedPair(t, opts.Addr, key, 3, 1)
	checkSharedBucket(t, a, b)
}
